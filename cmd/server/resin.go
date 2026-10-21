package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	proxyEngineBuiltin = "builtin"
	proxyEngineResin   = "resin"

	proxyEngineSettingKey       = "proxy_engine"
	resinGatewayURLSettingKey   = "resin_gateway_url"
	resinProxyTokenSettingKey   = "resin_proxy_token"
	resinPlatformSettingKey     = "resin_platform"
	resinFallbackActiveSetting  = "resin_fallback_active"
	resinFallbackSinceSetting   = "resin_fallback_since"
	resinFallbackReasonSetting  = "resin_fallback_reason"
	resinLastCheckedAtSetting   = "resin_last_checked_at"
	resinLastCheckErrorSetting  = "resin_last_check_error"
	resinFailureWindow          = 30 * time.Second
	resinFailureThreshold       = 3
	resinMaxAttempts            = 10
	resinControlPlaneAccountKey = "relaydesk-control-plane"
	// A Resin account is a sticky exit: once the upstream answers 429 for it,
	// drawing the same account again spends a full attempt (seconds of upstream
	// latency) on an exit that is already known to be limited. The floor keeps
	// a short or missing Retry-After from putting it straight back in rotation;
	// the ceiling stops one bad response from parking an exit for the day.
	resinAccountCooldownFloor = 45 * time.Second
	resinAccountCooldownCeil  = 5 * time.Minute
	// How many alternative accounts one draw may probe before it gives up and
	// reuses a cooling one, and how many cooled accounts are remembered.
	resinAccountProbeLimit = 8
	resinAccountCoolLimit  = 4096
)

type proxyEngineConfig struct {
	Engine          string
	ResinGatewayURL string
	ResinProxyToken string
	ResinPlatform   string
	FallbackActive  bool
	FallbackSince   *time.Time
	FallbackReason  string
	LastCheckedAt   *time.Time
	LastCheckError  string
}

type proxyEngineStatus struct {
	Engine              string     `json:"engine"`
	EffectiveEngine     string     `json:"effective_engine"`
	ResinGatewayURL     string     `json:"resin_gateway_url"`
	ResinPlatform       string     `json:"resin_platform"`
	HasResinProxyToken  bool       `json:"has_resin_proxy_token"`
	ResinConfigured     bool       `json:"resin_configured"`
	ResinFallbackActive bool       `json:"resin_fallback_active"`
	ResinFallbackSince  *time.Time `json:"resin_fallback_since,omitempty"`
	ResinFallbackReason string     `json:"resin_fallback_reason,omitempty"`
	ResinLastCheckedAt  *time.Time `json:"resin_last_checked_at,omitempty"`
	ResinLastCheckError string     `json:"resin_last_check_error,omitempty"`
	ResinHealth         string     `json:"resin_health"`
	// ResinCooledAccounts is how many exits are parked after a 429 right now.
	ResinCooledAccounts int `json:"resin_cooled_accounts"`
}

func settingValue(db *sql.DB, key string) (string, error) {
	var value string
	err := db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func parseStoredTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func (a *App) loadProxyEngine() (proxyEngineConfig, error) {
	a.runtimeCaches.engine.mu.RLock()
	if a.runtimeCaches.engine.loaded {
		cfg := cloneProxyEngineConfig(a.runtimeCaches.engine.cfg)
		a.runtimeCaches.engine.mu.RUnlock()
		return cfg, nil
	}
	a.runtimeCaches.engine.mu.RUnlock()

	a.runtimeCaches.engine.mu.Lock()
	defer a.runtimeCaches.engine.mu.Unlock()
	if a.runtimeCaches.engine.loaded {
		return cloneProxyEngineConfig(a.runtimeCaches.engine.cfg), nil
	}
	cfg, err := a.loadProxyEngineFromDB()
	if err != nil {
		return cfg, err
	}
	a.runtimeCaches.engine.cfg = cloneProxyEngineConfig(cfg)
	a.runtimeCaches.engine.loaded = true
	return cloneProxyEngineConfig(cfg), nil
}

func (a *App) loadProxyEngineFromDB() (proxyEngineConfig, error) {
	cfg := proxyEngineConfig{Engine: proxyEngineBuiltin, ResinPlatform: "Default"}
	var err error
	if raw, readErr := settingValue(a.db, proxyEngineSettingKey); readErr != nil {
		return cfg, readErr
	} else if raw != "" {
		cfg.Engine = raw
	}
	if cfg.Engine != proxyEngineBuiltin && cfg.Engine != proxyEngineResin {
		return cfg, errors.New("stored proxy engine is invalid")
	}
	if cfg.ResinGatewayURL, err = settingValue(a.db, resinGatewayURLSettingKey); err != nil {
		return cfg, err
	}
	if cfg.ResinPlatform, err = settingValue(a.db, resinPlatformSettingKey); err != nil {
		return cfg, err
	}
	if cfg.ResinPlatform == "" {
		cfg.ResinPlatform = "Default"
	}
	var encryptedToken string
	if encryptedToken, err = settingValue(a.db, resinProxyTokenSettingKey); err != nil {
		return cfg, err
	}
	if encryptedToken != "" {
		if cfg.ResinProxyToken, err = a.decrypt(encryptedToken); err != nil {
			return cfg, errors.New("could not decrypt Resin proxy token")
		}
	}
	var checkedAt string
	if cfg.FallbackReason, err = settingValue(a.db, resinFallbackReasonSetting); err != nil {
		return cfg, err
	}
	if checkedAt, err = settingValue(a.db, resinLastCheckedAtSetting); err != nil {
		return cfg, err
	}
	if cfg.LastCheckError, err = settingValue(a.db, resinLastCheckErrorSetting); err != nil {
		return cfg, err
	}
	// Resin remains the active engine even after repeated gateway failures.
	// Ignore legacy persisted fallback markers from earlier releases.
	cfg.FallbackActive = false
	cfg.FallbackSince = nil
	cfg.FallbackReason = ""
	cfg.LastCheckedAt = parseStoredTime(checkedAt)
	return cfg, nil
}

func validateResinGatewayURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New("invalid Resin gateway URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" && parsed.Scheme != "socks5" && parsed.Scheme != "socks5h" {
		return "", errors.New("Resin gateway must use http, https, socks5, or socks5h")
	}
	if parsed.Port() == "" {
		return "", errors.New("Resin gateway port is required")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("Resin gateway URL must not contain credentials, a path, query parameters, or a fragment")
	}
	parsed.Path = ""
	return parsed.String(), nil
}

func validateResinPlatform(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("Resin platform is required")
	}
	if strings.EqualFold(value, "api") {
		return "", errors.New("Resin platform must not use the reserved name api")
	}
	if strings.ContainsAny(value, ".:|/\\@?#%~ \t\r\n") {
		return "", errors.New("Resin platform contains unsupported characters")
	}
	return value, nil
}

func (a *App) proxyEngineStatus() (proxyEngineStatus, error) {
	cfg, err := a.loadProxyEngine()
	if err != nil {
		return proxyEngineStatus{}, err
	}
	configured := false
	if cfg.ResinGatewayURL != "" {
		_, urlErr := validateResinGatewayURL(cfg.ResinGatewayURL)
		_, platformErr := validateResinPlatform(cfg.ResinPlatform)
		configured = urlErr == nil && platformErr == nil
	}
	health := "unknown"
	if configured && cfg.LastCheckError == "" && cfg.LastCheckedAt != nil {
		health = "healthy"
	} else if cfg.LastCheckError != "" {
		health = "degraded"
	}
	return proxyEngineStatus{
		Engine:              cfg.Engine,
		EffectiveEngine:     cfg.Engine,
		ResinGatewayURL:     cfg.ResinGatewayURL,
		ResinPlatform:       cfg.ResinPlatform,
		HasResinProxyToken:  cfg.ResinProxyToken != "",
		ResinConfigured:     configured,
		ResinFallbackActive: cfg.FallbackActive,
		ResinFallbackSince:  cfg.FallbackSince,
		ResinFallbackReason: cfg.FallbackReason,
		ResinLastCheckedAt:  cfg.LastCheckedAt,
		ResinLastCheckError: cfg.LastCheckError,
		ResinHealth:         health,
		ResinCooledAccounts: a.cooledResinAccounts(),
	}, nil
}

func (a *App) getProxyEngine(w http.ResponseWriter, _ *http.Request) {
	status, err := a.proxyEngineStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load proxy engine"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) putProxyEngine(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Engine          string  `json:"engine"`
		ResinGatewayURL *string `json:"resin_gateway_url"`
		ResinPlatform   *string `json:"resin_platform"`
		ResinProxyToken *string `json:"resin_proxy_token"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid proxy engine configuration"})
		return
	}
	old, err := a.loadProxyEngine()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load proxy engine"})
		return
	}
	engine := strings.TrimSpace(in.Engine)
	if engine == "" {
		engine = old.Engine
	}
	if engine != proxyEngineBuiltin && engine != proxyEngineResin {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "engine must be builtin or resin"})
		return
	}
	gatewayURL := old.ResinGatewayURL
	if in.ResinGatewayURL != nil {
		gatewayURL = *in.ResinGatewayURL
	}
	platform := old.ResinPlatform
	if in.ResinPlatform != nil {
		platform = *in.ResinPlatform
	}
	if engine == proxyEngineResin {
		if gatewayURL, err = validateResinGatewayURL(gatewayURL); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if platform, err = validateResinPlatform(platform); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	token := old.ResinProxyToken
	if in.ResinProxyToken != nil {
		token = strings.TrimSpace(*in.ResinProxyToken)
	}
	encryptedToken, err := a.encrypt(token)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not encrypt Resin proxy token"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save proxy engine"})
		return
	}
	defer func() { _ = tx.Rollback() }()
	values := [][2]string{
		{proxyEngineSettingKey, engine},
		{resinGatewayURLSettingKey, gatewayURL},
		{resinPlatformSettingKey, platform},
		{resinProxyTokenSettingKey, encryptedToken},
	}
	if engine == proxyEngineBuiltin {
		values = append(values,
			[2]string{resinFallbackActiveSetting, "false"},
			[2]string{resinFallbackSinceSetting, ""},
			[2]string{resinFallbackReasonSetting, ""},
		)
	}
	for _, value := range values {
		if _, err = tx.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", value[0], value[1]); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save proxy engine"})
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save proxy engine"})
		return
	}
	a.invalidateProxyEngineCache()
	status, err := a.proxyEngineStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy engine was saved but could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (a *App) resinProxy(cfg proxyEngineConfig, account string) (ProxyRecord, error) {
	if _, err := validateResinGatewayURL(cfg.ResinGatewayURL); err != nil {
		return ProxyRecord{}, err
	}
	if _, err := validateResinPlatform(cfg.ResinPlatform); err != nil {
		return ProxyRecord{}, err
	}
	p, err := parseProxy(cfg.ResinGatewayURL)
	if err != nil {
		return ProxyRecord{}, err
	}
	p.Username = cfg.ResinPlatform
	if account != "" {
		p.Username += "." + account
	}
	p.Password = cfg.ResinProxyToken
	p.Engine = proxyEngineResin
	return p, nil
}

func (a *App) resinProxyForSession(sessionKey string) (ProxyRecord, error) {
	cfg, err := a.loadProxyEngine()
	if err != nil {
		return ProxyRecord{}, err
	}
	if cfg.Engine != proxyEngineResin {
		return ProxyRecord{}, errors.New("Resin is not the active proxy engine")
	}
	account, err := a.nextResinAccount(sessionKey)
	if err != nil {
		return ProxyRecord{}, err
	}
	return a.resinProxy(cfg, account)
}

// resinRequestRoute keeps account rotation local to a request when the client
// does not provide a session ID. Persisted session routes retain their existing
// sticky behavior, while anonymous requests avoid creating unbounded DB rows.
//
// A route also remembers which exit it last used so the gateway can learn from
// a 429: the account that answered 429 is parked in the cooldown table, and the
// next draw skips it instead of spending a slow attempt on it again.
type resinRequestRoute struct {
	sessionKey          string
	ephemeralSeed       string
	ephemeralGeneration uint64
	// retryNonce re-randomizes the account name without touching the sticky
	// session generation, so a retry re-rolls the exit instead of walking to
	// the next one (which can stay inside the same limited pool bucket).
	retryNonce  string
	lastAccount string
}

func newResinRequestRoute(sessionKey string) (*resinRequestRoute, error) {
	route := &resinRequestRoute{sessionKey: sessionKey}
	if sessionKey != "" {
		return route, nil
	}
	seed, err := randomKey()
	if err != nil {
		return nil, err
	}
	route.ephemeralSeed = seed
	return route, nil
}

// baseAccount is the sticky identity for this request: the persisted session
// route, or the seed and generation of an anonymous one.
func (route *resinRequestRoute) baseAccount(a *App) (string, error) {
	if route.sessionKey != "" {
		return a.nextResinAccount(route.sessionKey)
	}
	return hashToken(route.ephemeralSeed + "\x00" + strconv.FormatUint(route.ephemeralGeneration, 10)), nil
}

// accountFor folds the retry nonce into the sticky identity, so the first
// attempt of a session keeps its cached exit while a retry is unrelated to it.
func (route *resinRequestRoute) accountFor(base string) string {
	if route.retryNonce == "" {
		return base
	}
	return hashToken(base + "\x00" + route.retryNonce)
}

func (route *resinRequestRoute) proxy(a *App, cfg proxyEngineConfig) (ProxyRecord, error) {
	if route == nil {
		return ProxyRecord{}, errors.New("Resin request route is not initialized")
	}
	base, err := route.baseAccount(a)
	if err != nil {
		return ProxyRecord{}, err
	}
	account := route.accountFor(base)
	// Skip every account the gateway has just seen a 429 for. Probing is local:
	// it costs a hash, not an upstream round trip.
	for probes := 0; probes < resinAccountProbeLimit; probes++ {
		if _, cooling := a.resinAccountCooling(account); !cooling {
			break
		}
		nonce, nonceErr := randomKey()
		if nonceErr != nil {
			break
		}
		route.retryNonce = nonce
		account = route.accountFor(base)
	}
	route.lastAccount = account
	return a.resinProxy(cfg, account)
}

func (route *resinRequestRoute) advance(a *App) error {
	if route == nil {
		return errors.New("Resin request route is not initialized")
	}
	nonce, err := randomKey()
	if err != nil {
		return err
	}
	route.retryNonce = nonce
	if route.sessionKey != "" {
		return a.advanceResinAccount(route.sessionKey)
	}
	route.ephemeralGeneration++
	return nil
}

// resinAccountOf returns the account name a proxy record was built from.
func resinAccountOf(p ProxyRecord) string {
	if _, account, ok := strings.Cut(p.Username, "."); ok {
		return account
	}
	return p.Username
}

// resinAccountCooling reports how long an account is still parked. An expired
// entry is dropped on read, so the table cleans itself under load.
func (a *App) resinAccountCooling(account string) (time.Duration, bool) {
	if account == "" {
		return 0, false
	}
	now := time.Now()
	a.resinCoolMu.Lock()
	defer a.resinCoolMu.Unlock()
	until, ok := a.resinCooledAccounts[account]
	if !ok {
		return 0, false
	}
	if !until.After(now) {
		delete(a.resinCooledAccounts, account)
		return 0, false
	}
	return until.Sub(now), true
}

// coolResinAccount parks one account for the requested time, clamped so a bad
// or missing Retry-After can neither un-park it immediately nor park it for the
// rest of the day.
func (a *App) coolResinAccount(account string, cooldown time.Duration) {
	if account == "" {
		return
	}
	if cooldown < resinAccountCooldownFloor {
		cooldown = resinAccountCooldownFloor
	}
	if cooldown > resinAccountCooldownCeil {
		cooldown = resinAccountCooldownCeil
	}
	until := time.Now().Add(cooldown)
	a.resinCoolMu.Lock()
	defer a.resinCoolMu.Unlock()
	if a.resinCooledAccounts == nil {
		a.resinCooledAccounts = make(map[string]time.Time)
	}
	if len(a.resinCooledAccounts) >= resinAccountCoolLimit {
		for key, expiry := range a.resinCooledAccounts {
			if !expiry.After(time.Now()) {
				delete(a.resinCooledAccounts, key)
			}
		}
		if len(a.resinCooledAccounts) >= resinAccountCoolLimit {
			// Still full of live entries: forget the one that frees up first.
			var oldestKey string
			var oldest time.Time
			for key, expiry := range a.resinCooledAccounts {
				if oldestKey == "" || expiry.Before(oldest) {
					oldestKey, oldest = key, expiry
				}
			}
			delete(a.resinCooledAccounts, oldestKey)
		}
	}
	a.resinCooledAccounts[account] = until
}

// releaseResinAccount clears a park as soon as the same exit answers well
// again, so a recovered account does not sit out the rest of its cooldown.
func (a *App) releaseResinAccount(account string) {
	if account == "" {
		return
	}
	a.resinCoolMu.Lock()
	defer a.resinCoolMu.Unlock()
	delete(a.resinCooledAccounts, account)
}

// cooledResinAccounts reports how many exits are parked right now.
func (a *App) cooledResinAccounts() int {
	now := time.Now()
	a.resinCoolMu.Lock()
	defer a.resinCoolMu.Unlock()
	count := 0
	for _, until := range a.resinCooledAccounts {
		if until.After(now) {
			count++
		}
	}
	return count
}

// resinAccountCooldownFor turns the upstream's Retry-After into a park time.
func resinAccountCooldownFor(resp *http.Response) time.Duration {
	return retryAfterDuration(resp)
}

func (a *App) resinControlPlaneProxy() (ProxyRecord, error) {
	cfg, err := a.loadProxyEngine()
	if err != nil {
		return ProxyRecord{}, err
	}
	return a.resinProxy(cfg, hashToken(resinControlPlaneAccountKey))
}

func (a *App) controlPlaneProxy() (ProxyRecord, string, error) {
	cfg, err := a.loadProxyEngine()
	if err != nil {
		return ProxyRecord{}, "", err
	}
	engine := cfg.Engine
	if engine == proxyEngineResin {
		proxy, err := a.resinProxy(cfg, hashToken(resinControlPlaneAccountKey))
		return proxy, proxyEngineResin, err
	}
	// Keep the legacy configuration flow intact: model discovery and the
	// administrator's direct comparison stay direct in the built-in engine.
	// Resin mode always uses its configured gateway.
	return ProxyRecord{}, "direct", nil
}

func (a *App) nextResinAccount(sessionKey string) (string, error) {
	if sessionKey == "" {
		return "", nil
	}
	lock := &a.routingLocks[sessionRouteLockIndex(sessionKey)]
	lock.Lock()
	defer lock.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	var generation, requestCount int
	err := a.db.QueryRow("SELECT generation,request_count FROM resin_session_routes WHERE session_key=?", sessionKey).Scan(&generation, &requestCount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	limit := a.sessionProxyRequestLimit()
	if errors.Is(err, sql.ErrNoRows) {
		generation = 0
		requestCount = 1
		_, err = a.db.Exec("INSERT INTO resin_session_routes(session_key,generation,request_count,updated_at) VALUES(?,?,?,?)", sessionKey, generation, requestCount, now)
	} else if limit > 0 && requestCount >= limit {
		generation++
		requestCount = 1
		_, err = a.db.Exec("UPDATE resin_session_routes SET generation=?,request_count=?,updated_at=? WHERE session_key=?", generation, requestCount, now, sessionKey)
	} else {
		requestCount++
		_, err = a.db.Exec("UPDATE resin_session_routes SET request_count=?,updated_at=? WHERE session_key=?", requestCount, now, sessionKey)
	}
	if err != nil {
		return "", err
	}
	return hashToken(sessionKey + "\x00" + strconv.Itoa(generation)), nil
}

func (a *App) advanceResinAccount(sessionKey string) error {
	if sessionKey == "" {
		return nil
	}
	lock := &a.routingLocks[sessionRouteLockIndex(sessionKey)]
	lock.Lock()
	defer lock.Unlock()
	_, err := a.db.Exec("UPDATE resin_session_routes SET generation=generation+1,request_count=0,updated_at=? WHERE session_key=?", time.Now().UTC().Format(time.RFC3339), sessionKey)
	return err
}

func sessionRouteLockIndex(sessionKey string) int {
	var hash uint64 = 1469598103934665603
	for i := 0; i < len(sessionKey); i++ {
		hash ^= uint64(sessionKey[i])
		hash *= 1099511628211
	}
	return int(hash % 64)
}

func (a *App) clearStaleResinSessionRoutes() {
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	if _, err := a.db.Exec("DELETE FROM resin_session_routes WHERE updated_at < ?", cutoff); err != nil {
		log.Printf("delete stale Resin session routes failed: %v", err)
	}
}

func (a *App) resinFailure(err error) {
	if err == nil {
		return
	}
	now := time.Now()
	a.resinMu.Lock()
	if a.resinFailureStart.IsZero() || now.Sub(a.resinFailureStart) > resinFailureWindow {
		a.resinFailureStart = now
		a.resinFailureCount = 0
	}
	a.resinFailureCount++
	a.resinMu.Unlock()
	a.saveResinProbeResult(err)
	a.scheduleResinHealthRecheck()
	a.emitAlert("resin_unavailable", "error", "Resin gateway request failed", map[string]any{"error": truncateError(err.Error())})
}

func (a *App) scheduleResinHealthRecheck() {
	a.resinProbeMu.Lock()
	if time.Since(a.resinLastAutoProbe) < 10*time.Second {
		a.resinProbeMu.Unlock()
		return
	}
	a.resinLastAutoProbe = time.Now()
	a.resinProbeMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		_, err := a.resinProbe(ctx)
		a.saveResinProbeResult(err)
	}()
}

func (a *App) resinSuccess() {
	a.resinMu.Lock()
	wasFailing := a.resinFailureCount > 0
	a.resinFailureCount = 0
	a.resinFailureStart = time.Time{}
	shouldPersist := wasFailing || a.resinLastSuccessPersist.IsZero() || time.Since(a.resinLastSuccessPersist) >= time.Minute
	if shouldPersist {
		a.resinLastSuccessPersist = time.Now()
	}
	a.resinMu.Unlock()
	if shouldPersist {
		a.saveResinProbeResult(nil)
	}
}

func (a *App) resinProbe(ctx context.Context) (map[string]any, error) {
	cfg, err := a.loadProxyEngine()
	if err != nil {
		return nil, err
	}
	if _, err = validateResinGatewayURL(cfg.ResinGatewayURL); err != nil {
		return nil, err
	}
	if _, err = validateResinPlatform(cfg.ResinPlatform); err != nil {
		return nil, err
	}
	result := map[string]any{"gateway_url": cfg.ResinGatewayURL, "platform": cfg.ResinPlatform}
	parsed, _ := url.Parse(cfg.ResinGatewayURL)
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		healthURL := parsed.Scheme + "://" + parsed.Host + "/healthz"
		healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		req, requestErr := http.NewRequestWithContext(healthCtx, http.MethodGet, healthURL, nil)
		if requestErr != nil {
			cancel()
			return nil, requestErr
		}
		resp, requestErr := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		cancel()
		if requestErr != nil {
			return nil, fmt.Errorf("Resin health check failed: %w", requestErr)
		}
		result["health_status_code"] = resp.StatusCode
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			return result, fmt.Errorf("Resin health check returned HTTP %d", resp.StatusCode)
		}
	}
	upstream, err := a.loadUpstream()
	if err != nil || upstream.BaseURL == "" {
		return result, errors.New("configure the upstream before testing Resin")
	}
	p, err := a.resinControlPlaneProxy()
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamEndpoint(upstream.BaseURL, "/models"), nil)
	if err != nil {
		return result, err
	}
	applyLaneHeaders(req, a.catalogSession())
	client, err := a.httpClient(p)
	if err != nil {
		return result, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, fmt.Errorf("Resin proxy request failed: %w", err)
	}
	defer resp.Body.Close()
	result["upstream_status_code"] = resp.StatusCode
	if resp.StatusCode >= 300 {
		return result, fmt.Errorf("upstream through Resin returned HTTP %d", resp.StatusCode)
	}
	return result, nil
}

func (a *App) saveResinProbeResult(err error) {
	message := ""
	if err != nil {
		message = truncateError(err.Error())
	}
	if _, saveErr := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?),(?,?)", resinLastCheckedAtSetting, time.Now().UTC().Format(time.RFC3339), resinLastCheckErrorSetting, message); saveErr != nil {
		log.Printf("save Resin probe result failed: %v", saveErr)
		return
	}
	a.invalidateProxyEngineCache()
}

func (a *App) testResin(w http.ResponseWriter, r *http.Request) {
	result, err := a.resinProbe(r.Context())
	a.saveResinProbeResult(err)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "result": result})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

func (a *App) recoverResin(w http.ResponseWriter, r *http.Request) {
	result, err := a.resinProbe(r.Context())
	a.saveResinProbeResult(err)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "result": result})
		return
	}
	a.resinSuccess()
	status, err := a.proxyEngineStatus()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Resin was rechecked but status could not be read"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result, "status": status})
}
