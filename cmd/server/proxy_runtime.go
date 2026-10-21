package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	proxyExitProbeURL       = "https://api.ipify.org?format=json"
	proxyProbeJobRetention  = 15 * time.Minute
	proxyProbeExitInterval  = 15 * time.Minute
	proxyProbeUpstreamEvery = 60 * time.Minute
	// maxProxyCooldown bounds the exponential exit cooldown so a persistently
	// limited exit is retried eventually instead of being abandoned.
	maxProxyCooldown = 30 * time.Minute
	// modelBanConfirmations is how many refusals for the same exit and model
	// turn a suspect pairing into a banned one.
	modelBanConfirmations = 2
	// modelBanWindow is how long a banned exit and model pairing stays out of
	// rotation. It is short because a refusal usually follows the upstream's
	// own free-tier bookkeeping, not the exit.
	modelBanWindow = 10 * time.Minute
)

type cachedClient struct {
	client    *http.Client
	transport *http.Transport
	lastUsed  atomic.Int64
	dynamic   bool
}

type proxySnapshot struct {
	proxies []ProxyRecord
}

type proxyRuntime struct {
	snapshot  atomic.Pointer[proxySnapshot]
	loadMu    sync.Mutex
	clientsMu sync.RWMutex
	clients   map[string]*cachedClient
}

func newProxyRuntime() *proxyRuntime {
	return &proxyRuntime{clients: make(map[string]*cachedClient)}
}

func (p *proxyRuntime) invalidate() {
	p.snapshot.Store(nil)
	p.clientsMu.Lock()
	for _, cached := range p.clients {
		if cached.transport != nil {
			cached.transport.CloseIdleConnections()
		}
	}
	p.clients = make(map[string]*cachedClient)
	p.clientsMu.Unlock()
}

func (p *proxyRuntime) available(a *App) ([]ProxyRecord, error) {
	snapshot := p.snapshot.Load()
	if snapshot == nil {
		p.loadMu.Lock()
		snapshot = p.snapshot.Load()
		if snapshot == nil {
			proxies, err := a.loadAvailableProxiesFromDB()
			if err != nil {
				p.loadMu.Unlock()
				return nil, err
			}
			snapshot = &proxySnapshot{proxies: proxies}
			p.snapshot.Store(snapshot)
		}
		p.loadMu.Unlock()
	}
	now := time.Now().UTC()
	out := make([]ProxyRecord, 0, len(snapshot.proxies))
	for _, proxy := range snapshot.proxies {
		if !proxy.Enabled || (proxy.CooldownUntil != nil && proxy.CooldownUntil.After(now)) || (proxy.ExpiresAt != nil && !proxy.ExpiresAt.After(now)) {
			continue
		}
		out = append(out, proxy)
	}
	return out, nil
}

func (p *proxyRuntime) replaceHealth(id int64, status string, failures int, cooldown *time.Time) {
	for {
		current := p.snapshot.Load()
		if current == nil {
			return
		}
		proxies := append([]ProxyRecord(nil), current.proxies...)
		found := false
		for i := range proxies {
			if proxies[i].ID != id {
				continue
			}
			proxies[i].HealthStatus = status
			proxies[i].FailureCount = failures
			proxies[i].CooldownUntil = cloneTime(cooldown)
			found = true
			break
		}
		if !found {
			return
		}
		updated := &proxySnapshot{proxies: proxies}
		if p.snapshot.CompareAndSwap(current, updated) {
			return
		}
	}
}

func clientCacheKey(proxy ProxyRecord) string {
	return strconv.FormatInt(proxy.ID, 10) + "\x00" + proxy.URI + "\x00" + proxy.Username + "\x00" + proxy.Password
}

func (p *proxyRuntime) clientFor(a *App, proxy ProxyRecord) (*http.Client, error) {
	key := clientCacheKey(proxy)
	now := time.Now()
	p.clientsMu.RLock()
	if existing, ok := p.clients[key]; ok {
		existing.lastUsed.Store(now.UnixNano())
		client := existing.client
		p.clientsMu.RUnlock()
		return client, nil
	}
	p.clientsMu.RUnlock()
	client, transport, err := a.buildHTTPClient(proxy)
	if err != nil {
		return nil, err
	}
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	if existing, ok := p.clients[key]; ok {
		if transport != nil {
			transport.CloseIdleConnections()
		}
		existing.lastUsed.Store(now.UnixNano())
		return existing.client, nil
	}
	for cachedKey, existing := range p.clients {
		if now.Sub(time.Unix(0, existing.lastUsed.Load())) < 10*time.Minute {
			continue
		}
		if existing.transport != nil {
			existing.transport.CloseIdleConnections()
		}
		delete(p.clients, cachedKey)
	}
	// Resin accounts are dynamic and can be numerous. Static pool clients may
	// scale with the configured pool, while dynamic Resin clients remain bounded.
	if proxy.ID == 0 && proxy.URI != "" {
		dynamicCount := 0
		oldestKey := ""
		var oldest time.Time
		for cachedKey, existing := range p.clients {
			if !existing.dynamic {
				continue
			}
			dynamicCount++
			lastUsed := time.Unix(0, existing.lastUsed.Load())
			if oldestKey == "" || lastUsed.Before(oldest) {
				oldestKey, oldest = cachedKey, lastUsed
			}
		}
		if dynamicCount >= 256 && oldestKey != "" {
			oldest := p.clients[oldestKey]
			if oldest.transport != nil {
				oldest.transport.CloseIdleConnections()
			}
			delete(p.clients, oldestKey)
		}
	}
	cached := &cachedClient{client: client, transport: transport, dynamic: proxy.ID == 0 && proxy.URI != ""}
	cached.lastUsed.Store(now.UnixNano())
	p.clients[key] = cached
	return client, nil
}

// upstreamOutcome is the single classification of an upstream HTTP status. Every
// health decision in the gateway goes through it, so a status can never be
// interpreted two different ways by two call sites.
type upstreamOutcome int

const (
	// upstreamOutcomeSuccess means the exit reached the upstream and the
	// upstream accepted the request.
	upstreamOutcomeSuccess upstreamOutcome = iota
	// upstreamOutcomeLimited means the exit's quota is exhausted (429). The
	// quota is metered per exit IP, so the exit - not the model - is cooled.
	upstreamOutcomeLimited
	// upstreamOutcomeRefused means this exit is refused for this model
	// (401/403). The link itself is healthy, so the exit must not be cooled.
	upstreamOutcomeRefused
	// upstreamOutcomeProxyAuth means the proxy credentials are rejected (407).
	upstreamOutcomeProxyAuth
	// upstreamOutcomeServerError means the upstream or the model failed behind
	// a healthy exit. It says nothing about the exit, so health is untouched.
	upstreamOutcomeServerError
	// upstreamOutcomeClientError means the request itself was rejected.
	upstreamOutcomeClientError
)

func classifyUpstreamOutcome(status int) upstreamOutcome {
	switch {
	case status >= 200 && status < 300:
		return upstreamOutcomeSuccess
	case status == http.StatusTooManyRequests:
		return upstreamOutcomeLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return upstreamOutcomeRefused
	case status == http.StatusProxyAuthRequired:
		return upstreamOutcomeProxyAuth
	case status >= 500:
		return upstreamOutcomeServerError
	default:
		return upstreamOutcomeClientError
	}
}

// shouldRotateUpstream reports whether another exit may fix the status. A 401 or
// 403 is rotated because the refusal is scoped to the exit that received it; the
// last attempt still hands the original status to the client.
func shouldRotateUpstream(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// parseRetryAfter reads the Retry-After header as either delta-seconds or an
// HTTP date and clamps it to the maximum exit cooldown.
func parseRetryAfter(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	delay := time.Duration(0)
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds <= 0 {
			return 0, false
		}
		delay = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(raw); err == nil {
		delay = time.Until(at)
	}
	if delay <= 0 {
		return 0, false
	}
	if delay > maxProxyCooldown {
		delay = maxProxyCooldown
	}
	return delay, true
}

// retryAfterDuration is the cooldown floor an exit earns from a 429: whatever
// the upstream asked for, or zero so the local exponential backoff decides.
func retryAfterDuration(resp *http.Response) time.Duration {
	if delay, ok := parseRetryAfter(resp); ok {
		return delay
	}
	return 0
}

func retryAfterDelay(resp *http.Response, attempt int) time.Duration {
	// The in-request wait only bridges the gap to the next exit; the real
	// backoff lives on the cooled exit, so this is deliberately short.
	if delay, ok := parseRetryAfter(resp); ok {
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		return delay
	}
	delay := 100 * time.Millisecond
	if attempt > 0 {
		delay = 300 * time.Millisecond
	}
	return delay
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type proxyProbeResult struct {
	ProxyID        int64        `json:"proxy_id"`
	URI            string       `json:"uri"`
	ExitOK         bool         `json:"exit_ok"`
	ExitIP         string       `json:"exit_ip,omitempty"`
	ExitLatencyMS  int64        `json:"exit_latency_ms,omitempty"`
	UpstreamStatus string       `json:"upstream_status,omitempty"`
	Error          string       `json:"error,omitempty"`
	Verdict        probeVerdict `json:"verdict,omitempty"`
	exitProbed     bool
	upstreamProbed bool
}

// probeVerdict is the probe-side twin of upstreamOutcome. It exists because a
// probe result must never be interpreted as "the exit is broken" unless the link
// itself failed: the echo endpoint is a third party whose own outages say
// nothing about reaching the real upstream.
type probeVerdict int

const (
	probeVerdictUnknown probeVerdict = iota
	// probeVerdictOK means the exit reached the echo endpoint and answered.
	probeVerdictOK
	// probeVerdictEchoUnavailable means the echo endpoint answered badly or was
	// unreachable in a way that does not prove the link is dead.
	probeVerdictEchoUnavailable
	// probeVerdictTransport means the link itself failed.
	probeVerdictTransport
	// probeVerdictUnreadable means the echo endpoint answered 2xx but returned
	// no exit address, so the result cannot judge the exit.
	probeVerdictUnreadable
)

func (v probeVerdict) String() string {
	switch v {
	case probeVerdictOK:
		return "ok"
	case probeVerdictEchoUnavailable:
		return "echo-unavailable"
	case probeVerdictTransport:
		return "transport"
	case probeVerdictUnreadable:
		return "unreadable"
	default:
		return ""
	}
}

func (v probeVerdict) MarshalText() ([]byte, error) {
	return []byte(v.String()), nil
}

func (v *probeVerdict) UnmarshalText(text []byte) error {
	switch strings.ToLower(strings.TrimSpace(string(text))) {
	case "":
		*v = probeVerdictUnknown
	case "ok":
		*v = probeVerdictOK
	case "echo-unavailable":
		*v = probeVerdictEchoUnavailable
	case "transport":
		*v = probeVerdictTransport
	case "unreadable":
		*v = probeVerdictUnreadable
	default:
		return fmt.Errorf("unknown probe verdict %q", text)
	}
	return nil
}

func (v probeVerdict) healthy() bool {
	return v == probeVerdictOK
}

// touchesExitHealth reports whether the verdict is allowed to change the health
// of the probed exit. Echo-endpoint trouble and unreadable payloads only record
// a reason.
func (v probeVerdict) touchesExitHealth() bool {
	return v == probeVerdictOK || v == probeVerdictTransport
}

func classifyExitProbe(httpStatus int, transportErr error, exitIP string) probeVerdict {
	if transportErr != nil {
		if isProxyTransportError(transportErr) {
			return probeVerdictTransport
		}
		return probeVerdictEchoUnavailable
	}
	if httpStatus < 200 || httpStatus >= 300 {
		return probeVerdictEchoUnavailable
	}
	if strings.TrimSpace(exitIP) == "" {
		return probeVerdictUnreadable
	}
	return probeVerdictOK
}

type proxyProbeJob struct {
	ID        string             `json:"id"`
	Status    string             `json:"status"`
	Total     int                `json:"total"`
	Completed int                `json:"completed"`
	CreatedAt time.Time          `json:"created_at"`
	ExpiresAt time.Time          `json:"expires_at"`
	Results   []proxyProbeResult `json:"results"`
}

type probeJobStore struct {
	app *App
}

func newProbeJobStore(app *App) *probeJobStore {
	return &probeJobStore{app: app}
}

func (s *probeJobStore) add(job *proxyProbeJob, mode string) error {
	_, err := s.app.db.Exec("INSERT INTO proxy_probe_jobs(id,status,mode,total,completed,created_at,expires_at) VALUES(?,?,?,?,?,?,?)", job.ID, job.Status, mode, job.Total, job.Completed, job.CreatedAt.UTC().Format(time.RFC3339), job.ExpiresAt.UTC().Format(time.RFC3339))
	return err
}

func (s *probeJobStore) snapshot(id string) (*proxyProbeJob, bool) {
	now := time.Now().UTC().Format(time.RFC3339)
	_, _ = s.app.db.Exec("DELETE FROM proxy_probe_jobs WHERE expires_at<=?", now)
	job := &proxyProbeJob{ID: id}
	var created, expires string
	if err := s.app.db.QueryRow("SELECT status,total,completed,created_at,expires_at FROM proxy_probe_jobs WHERE id=?", id).Scan(&job.Status, &job.Total, &job.Completed, &created, &expires); err != nil {
		return nil, false
	}
	job.CreatedAt, _ = time.Parse(time.RFC3339, created)
	job.ExpiresAt, _ = time.Parse(time.RFC3339, expires)
	rows, err := s.app.db.Query("SELECT proxy_id,uri,exit_ok,COALESCE(exit_ip,''),COALESCE(exit_latency_ms,0),COALESCE(upstream_status,''),COALESCE(error,''),COALESCE(verdict,'') FROM proxy_probe_results WHERE job_id=? ORDER BY proxy_id", id)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var result proxyProbeResult
		var exitOK int
		var verdict string
		if err := rows.Scan(&result.ProxyID, &result.URI, &exitOK, &result.ExitIP, &result.ExitLatencyMS, &result.UpstreamStatus, &result.Error, &verdict); err != nil {
			return nil, false
		}
		result.ExitOK = exitOK == 1
		_ = result.Verdict.UnmarshalText([]byte(verdict))
		job.Results = append(job.Results, result)
	}
	return job, rows.Err() == nil
}

func (s *probeJobStore) appendResult(id string, result proxyProbeResult) {
	tx, err := s.app.db.Begin()
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec("INSERT OR REPLACE INTO proxy_probe_results(job_id,proxy_id,uri,exit_ok,exit_ip,exit_latency_ms,upstream_status,error,verdict) VALUES(?,?,?,?,?,?,?,?,?)", id, result.ProxyID, result.URI, boolInt(result.ExitOK), result.ExitIP, result.ExitLatencyMS, result.UpstreamStatus, result.Error, result.Verdict.String()); err != nil {
		return
	}
	if _, err = tx.Exec("UPDATE proxy_probe_jobs SET completed=completed+1,status=CASE WHEN completed+1>=total THEN 'completed' ELSE status END WHERE id=? AND status='running'", id); err != nil {
		return
	}
	_ = tx.Commit()
}

func (a *App) createProxyProbeJob(w http.ResponseWriter, r *http.Request) {
	a.initializeRuntimeServices()
	var in struct {
		IDs  []int64 `json:"ids"`
		Mode string  `json:"mode"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid probe request"})
		return
	}
	if in.Mode == "" {
		in.Mode = "both"
	}
	if in.Mode != "exit" && in.Mode != "upstream" && in.Mode != "both" {
		writeJSON(w, 400, map[string]string{"error": "mode must be exit, upstream, or both"})
		return
	}
	if len(in.IDs) > 1000 {
		writeJSON(w, 400, map[string]string{"error": "at most 1000 proxies may be probed"})
		return
	}
	proxies, err := a.proxiesForProbe(in.IDs)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load proxies"})
		return
	}
	if len(proxies) == 0 {
		writeJSON(w, 400, map[string]string{"error": "no proxies available for probing"})
		return
	}
	id, err := randomKey()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not create probe job"})
		return
	}
	now := time.Now()
	job := &proxyProbeJob{ID: id, Status: "running", Total: len(proxies), CreatedAt: now, ExpiresAt: now.Add(proxyProbeJobRetention), Results: []proxyProbeResult{}}
	if err := a.probeJobs.add(job, in.Mode); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not save probe job"})
		return
	}
	go a.runProbeJob(id, proxies, in.Mode)
	writeJSON(w, http.StatusAccepted, job)
}

func (a *App) getProxyProbeJob(w http.ResponseWriter, r *http.Request) {
	a.initializeRuntimeServices()
	job, ok := a.probeJobs.snapshot(r.PathValue("id"))
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "probe job not found or expired"})
		return
	}
	writeJSON(w, 200, job)
}

func (a *App) runProbeJob(jobID string, proxies []ProxyRecord, mode string) {
	sem := make(chan struct{}, 10)
	var wg sync.WaitGroup
	for _, proxy := range proxies {
		proxy := proxy
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			a.probeJobs.appendResult(jobID, a.probeProxy(context.Background(), proxy, mode))
		}()
	}
	wg.Wait()
}

func (a *App) proxiesForProbe(ids []int64) ([]ProxyRecord, error) {
	all, err := a.loadProxiesForProbeFromDB()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return all, nil
	}
	selected := map[int64]struct{}{}
	for _, id := range ids {
		selected[id] = struct{}{}
	}
	out := []ProxyRecord{}
	for _, proxy := range all {
		if _, ok := selected[proxy.ID]; ok {
			out = append(out, proxy)
		}
	}
	return out, nil
}

func (a *App) probeProxy(parent context.Context, proxy ProxyRecord, mode string) proxyProbeResult {
	result := proxyProbeResult{ProxyID: proxy.ID, URI: proxy.URI}
	if mode == "exit" || mode == "both" {
		result.exitProbed = true
		started := time.Now()
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		var exitErr error
		httpStatus := 0
		request, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, proxyExitProbeURL, nil)
		if requestErr != nil {
			exitErr = requestErr
		} else if client, clientErr := a.httpClient(proxy); clientErr != nil {
			exitErr = clientErr
		} else if response, doErr := client.Do(request); doErr != nil {
			exitErr = doErr
		} else {
			httpStatus = response.StatusCode
			body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			if httpStatus >= 200 && httpStatus < 300 {
				var payload struct {
					IP string `json:"ip"`
				}
				_ = json.Unmarshal(body, &payload)
				result.ExitIP = strings.TrimSpace(payload.IP)
			}
		}
		cancel()
		result.ExitLatencyMS = time.Since(started).Milliseconds()
		result.Verdict = classifyExitProbe(httpStatus, exitErr, result.ExitIP)
		result.ExitOK = result.Verdict.healthy()
		if !result.ExitOK {
			switch {
			case exitErr != nil:
				result.Error = truncateError(exitErr.Error())
			case httpStatus != 0:
				result.Error = fmt.Sprintf("exit probe returned HTTP %d", httpStatus)
			default:
				result.Error = "exit probe returned no exit address"
			}
		}
		if result.Verdict == probeVerdictTransport {
			// The link is dead, so probing the upstream through it would only
			// spend the probe budget on a result we already know.
			a.persistProbeResult(proxy.ID, result)
			return result
		}
	}
	if mode == "upstream" || mode == "both" {
		result.upstreamProbed = true
		cfg, err := a.loadUpstream()
		if err != nil || cfg.BaseURL == "" {
			result.UpstreamStatus = "not_configured"
		} else {
			ctx, cancel := context.WithTimeout(parent, 30*time.Second)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstreamEndpoint(cfg.BaseURL, "/models"), nil)
			applyUpstreamHeaders(req, cfg)
			client, clientErr := a.httpClient(proxy)
			if clientErr != nil {
				err = clientErr
			} else {
				resp, requestErr := client.Do(req)
				if requestErr != nil {
					err = requestErr
				} else {
					_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
					_ = resp.Body.Close()
					result.UpstreamStatus = strconv.Itoa(resp.StatusCode)
				}
			}
			cancel()
			if err != nil && result.Error == "" {
				result.Error = truncateError(err.Error())
				result.UpstreamStatus = "unreachable"
			}
		}
	}
	a.persistProbeResult(proxy.ID, result)
	return result
}

func (a *App) persistProbeResult(id int64, result proxyProbeResult) {
	now := time.Now().UTC()
	if result.exitProbed {
		var latency any
		if result.ExitLatencyMS > 0 {
			latency = result.ExitLatencyMS
		}
		errorMessage := ""
		if !result.ExitOK {
			errorMessage = result.Error
		}
		_, _ = a.db.Exec("UPDATE proxies SET last_probe_at=?,last_probe_latency_ms=?,last_exit_ip=?,last_probe_error=?,updated_at=? WHERE id=?", now.Format(time.RFC3339), latency, result.ExitIP, errorMessage, now.Format(time.RFC3339), id)
	}
	if result.upstreamProbed {
		_, _ = a.db.Exec("UPDATE proxies SET upstream_probe_at=?,upstream_probe_status=?,updated_at=? WHERE id=?", now.Format(time.RFC3339), result.UpstreamStatus, now.Format(time.RFC3339), id)
	}
	verdict := result.Verdict
	if verdict == probeVerdictUnknown && result.ExitOK {
		// Results restored from storage predate the verdict column.
		verdict = probeVerdictOK
	}
	if !result.exitProbed || !verdict.touchesExitHealth() {
		// Echo-endpoint trouble and unreadable payloads only record a reason.
		// An upstream probe failure never speaks about the exit either: a 500
		// behind a healthy exit must not take the exit out of rotation.
		return
	}
	if verdict.healthy() {
		a.markProxySuccess(id)
		return
	}
	a.markProxyFailure(id)
}

func (a *App) proxyProbeJanitor() {
	for {
		settings, err := a.loadProbeSettings()
		interval := proxyProbeExitInterval
		if err == nil {
			interval = time.Duration(settings.ExitMinutes) * time.Minute
		}
		// time.After rather than a reused timer: the interval can change between
		// rounds, and a stopped timer would have to be rebuilt anyway.
		<-time.After(interval)
		settings, err = a.loadProbeSettings()
		if err != nil || !settings.Enabled {
			continue
		}
		proxies, err := a.proxiesForProbe(nil)
		if err != nil || len(proxies) == 0 {
			continue
		}
		mode := "exit"
		if time.Now().UTC().Sub(settings.LastUpstreamProbe) >= time.Duration(settings.UpstreamMinutes)*time.Minute {
			mode = "both"
			_, _ = a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('proxy_probe_last_upstream',?)", time.Now().UTC().Format(time.RFC3339))
		}
		id := "scheduled-" + strconv.FormatInt(time.Now().UnixNano(), 10)
		job := &proxyProbeJob{ID: id, Status: "running", Total: len(proxies), CreatedAt: time.Now(), ExpiresAt: time.Now().Add(proxyProbeJobRetention)}
		a.initializeRuntimeServices()
		if err := a.probeJobs.add(job, mode); err != nil {
			log.Printf("save scheduled probe job failed: %v", err)
			continue
		}
		go a.runProbeJob(id, proxies, mode)
	}
}

type probeSettings struct {
	Enabled           bool      `json:"enabled"`
	ExitMinutes       int       `json:"exit_minutes"`
	UpstreamMinutes   int       `json:"upstream_minutes"`
	LastUpstreamProbe time.Time `json:"-"`
}

func (a *App) loadProbeSettings() (probeSettings, error) {
	settings := probeSettings{Enabled: true, ExitMinutes: 15, UpstreamMinutes: 60}
	var enabled, exit, upstream, last string
	for _, item := range []struct {
		key  string
		dest *string
	}{{"proxy_probes_enabled", &enabled}, {"proxy_probe_exit_minutes", &exit}, {"proxy_probe_upstream_minutes", &upstream}, {"proxy_probe_last_upstream", &last}} {
		value, err := settingValue(a.db, item.key)
		if err != nil {
			return settings, err
		}
		*item.dest = value
	}
	if enabled != "" {
		settings.Enabled, _ = strconv.ParseBool(enabled)
	}
	if n, err := strconv.Atoi(exit); err == nil && n >= 5 && n <= 1440 {
		settings.ExitMinutes = n
	}
	if n, err := strconv.Atoi(upstream); err == nil && n >= 15 && n <= 10080 {
		settings.UpstreamMinutes = n
	}
	settings.LastUpstreamProbe, _ = time.Parse(time.RFC3339, last)
	return settings, nil
}

func (a *App) getProbeSettings(w http.ResponseWriter, _ *http.Request) {
	settings, err := a.loadProbeSettings()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load probe settings"})
		return
	}
	writeJSON(w, 200, settings)
}

func (a *App) putProbeSettings(w http.ResponseWriter, r *http.Request) {
	var in probeSettings
	if readJSON(r, &in) != nil || in.ExitMinutes < 5 || in.ExitMinutes > 1440 || in.UpstreamMinutes < 15 || in.UpstreamMinutes > 10080 {
		writeJSON(w, 400, map[string]string{"error": "invalid probe settings"})
		return
	}
	_, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('proxy_probes_enabled',?),('proxy_probe_exit_minutes',?),('proxy_probe_upstream_minutes',?)", strconv.FormatBool(in.Enabled), strconv.Itoa(in.ExitMinutes), strconv.Itoa(in.UpstreamMinutes))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not save probe settings"})
		return
	}
	writeJSON(w, 200, in)
}

func sortedProxyIDs(proxies []ProxyRecord) []int64 {
	ids := make([]int64, 0, len(proxies))
	for _, proxy := range proxies {
		ids = append(ids, proxy.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// proxyTransportMarkers are the wordings Go's HTTP stack produces when the exit
// itself failed. They are matched explicitly instead of on a bare "connect"
// substring, which also appears in upstream error bodies and in statuses that
// say nothing about the exit.
var proxyTransportMarkers = []string{
	"proxyconnect", // net/http proxy dial failure
	"proxy: ",      // golang.org/x/net/proxy
	"connection refused",
	"connection reset",
	"broken pipe",
	"no route to host",
	"network is unreachable",
	"i/o timeout",
	"tls handshake timeout",
	"tls: handshake failure",
	"unexpected eof",
	"transport connection broken",
	"http2: client connection lost",
	"server closed idle connection",
	"use of closed network connection",
}

// isProxyTransportError reports whether err proves the exit link failed. Only
// this class may cool an exit: a refused request, an upstream 5xx or a client
// cancellation says nothing about the link.
func isProxyTransportError(err error) bool {
	if err == nil {
		return false
	}
	// A cancelled request is the downstream client hanging up, not a dead exit.
	if errors.Is(err, context.Canceled) {
		return false
	}
	lower := strings.ToLower(err.Error())
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	// A whole-request deadline means the upstream was slow. Only a dial that
	// timed out proves the exit could not be reached.
	if errors.Is(err, context.DeadlineExceeded) && !strings.Contains(lower, "proxyconnect") && !strings.Contains(lower, "dial ") {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	for _, marker := range proxyTransportMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
