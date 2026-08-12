package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ClientKey struct {
	ID                 int64      `json:"id"`
	Name               string     `json:"name"`
	Hint               string     `json:"hint"`
	Enabled            bool       `json:"enabled"`
	MaintenanceAllowed bool       `json:"maintenance_allowed"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	RPMLimit           int        `json:"rpm_limit"`
	TPMLimit           int        `json:"tpm_limit"`
	LastUsedAt         *time.Time `json:"last_used_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	lastTouch          *atomic.Int64
}

type keyWindow struct {
	requests    []time.Time
	requestHead int
	tokens      []tokenSample
	tokenHead   int
	tokenTotal  int64
	lastAt      time.Time
}

type tokenSample struct {
	at    time.Time
	value int64
}

type limiterShard struct {
	mu          sync.Mutex
	windows     map[int64]*keyWindow
	lastCleanup time.Time
}

type clientKeyLimiter struct {
	shards [64]limiterShard
}

func newClientKeyLimiter() *clientKeyLimiter {
	limiter := &clientKeyLimiter{}
	for i := range limiter.shards {
		limiter.shards[i].windows = make(map[int64]*keyWindow)
	}
	return limiter
}

func (l *clientKeyLimiter) allow(key *ClientKey, now time.Time) (bool, time.Duration, string) {
	if key.RPMLimit == 0 && key.TPMLimit == 0 {
		return true, 0, ""
	}
	shard := &l.shards[uint64(key.ID)%uint64(len(l.shards))]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	w := shard.windows[key.ID]
	if w == nil {
		w = &keyWindow{}
		shard.windows[key.ID] = w
	}
	w.lastAt = now
	cutoff := now.Add(-time.Minute)
	for w.requestHead < len(w.requests) && !w.requests[w.requestHead].After(cutoff) {
		w.requestHead++
	}
	for w.tokenHead < len(w.tokens) && !w.tokens[w.tokenHead].at.After(cutoff) {
		w.tokenTotal -= w.tokens[w.tokenHead].value
		w.tokenHead++
	}
	if key.RPMLimit > 0 && len(w.requests)-w.requestHead >= key.RPMLimit {
		return false, time.Until(w.requests[w.requestHead].Add(time.Minute)), "key_rpm_limit"
	}
	if key.TPMLimit > 0 && w.tokenTotal >= int64(key.TPMLimit) {
		return false, time.Until(w.tokens[w.tokenHead].at.Add(time.Minute)), "key_tpm_limit"
	}
	w.requests = append(w.requests, now)
	if w.requestHead > 128 && w.requestHead*2 >= len(w.requests) {
		w.requests = append([]time.Time(nil), w.requests[w.requestHead:]...)
		w.requestHead = 0
	}
	if w.tokenHead > 128 && w.tokenHead*2 >= len(w.tokens) {
		w.tokens = append([]tokenSample(nil), w.tokens[w.tokenHead:]...)
		w.tokenHead = 0
	}
	if len(shard.windows) > 1024 && now.Sub(shard.lastCleanup) >= 5*time.Minute {
		for id, item := range shard.windows {
			if now.Sub(item.lastAt) >= 5*time.Minute && item.requestHead >= len(item.requests) && item.tokenHead >= len(item.tokens) {
				delete(shard.windows, id)
			}
		}
		shard.lastCleanup = now
	}
	return true, 0, ""
}

func (l *clientKeyLimiter) recordTokens(keyID int64, usage *tokenUsage, now time.Time) {
	if keyID == 0 || usage == nil || usage.Total == nil || *usage.Total <= 0 {
		return
	}
	shard := &l.shards[uint64(keyID)%uint64(len(l.shards))]
	shard.mu.Lock()
	defer shard.mu.Unlock()
	w := shard.windows[keyID]
	if w == nil {
		w = &keyWindow{}
		shard.windows[keyID] = w
	}
	w.tokens = append(w.tokens, tokenSample{at: now, value: *usage.Total})
	w.tokenTotal += *usage.Total
	w.lastAt = now
}

func clientKeyHint(key string) string {
	if len(key) <= 10 {
		return key
	}
	return key[:7] + "..." + key[len(key)-4:]
}

func (a *App) initializeRuntimeServices() {
	a.runtimeInitOnce.Do(func() {
		a.keyLimiter = newClientKeyLimiter()
		a.proxyRuntime = newProxyRuntime()
		a.probeJobs = newProbeJobStore(a)
		a.alerts = newAlertDispatcher(a)
	})
}

func (a *App) migrateLegacyClientKey() error {
	var legacyHash string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='client_key'").Scan(&legacyHash); err != nil {
		return err
	}
	if legacyHash == "" {
		return errors.New("legacy client key hash is empty")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := a.db.Exec("INSERT INTO client_keys(name,key_hash,key_hint,enabled,created_at,updated_at) VALUES('Legacy',?,'legacy',1,?,?) ON CONFLICT(key_hash) DO NOTHING", legacyHash, now, now)
	if err == nil {
		a.invalidateClientKeyCache()
	}
	return err
}

func (a *App) authenticateClient(r *http.Request) (*ClientKey, error) {
	supplied := clientCredential(r)
	if supplied == "" {
		return nil, errors.New("missing client key")
	}
	hash := hashToken(supplied)
	cached, err := a.cachedClientKey(hash)
	if err != nil {
		return nil, err
	}
	if cached == nil {
		var legacyHash string
		if legacyErr := a.db.QueryRow("SELECT value FROM settings WHERE key='client_key'").Scan(&legacyHash); legacyErr == nil && subtle.ConstantTimeCompare([]byte(hash), []byte(legacyHash)) == 1 {
			now := time.Now().UTC().Format(time.RFC3339)
			result, _ := a.db.Exec("UPDATE client_keys SET key_hash=?,key_hint='legacy',enabled=1,updated_at=? WHERE name='Legacy'", hash, now)
			if updated, _ := result.RowsAffected(); updated == 0 {
				_, _ = a.db.Exec("INSERT INTO client_keys(name,key_hash,key_hint,enabled,created_at,updated_at) VALUES('Legacy',?,'legacy',1,?,?)", hash, now, now)
			}
			a.invalidateClientKeyCache()
			if cached, err = a.cachedClientKey(hash); err == nil && cached != nil {
				return clientKeyCopy(cached), nil
			}
		}
		return nil, errors.New("invalid client key")
	}
	key := clientKeyCopy(cached)
	if !key.Enabled {
		return nil, errors.New("client key is disabled")
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(time.Now().UTC()) {
		return nil, errors.New("client key has expired")
	}
	return key, nil
}

func clientKeyCopy(cached *cachedClientKey) *ClientKey {
	if cached == nil {
		return nil
	}
	key := cached.key
	key.ExpiresAt = cloneTime(key.ExpiresAt)
	key.LastUsedAt = cloneTime(key.LastUsedAt)
	return &key
}

type clientKeyScanner interface {
	Scan(dest ...any) error
}

func scanClientKey(row clientKeyScanner) (*ClientKey, error) {
	var key ClientKey
	var enabled, maintenanceAllowed int
	var expires, lastUsed, created string
	if err := row.Scan(&key.ID, &key.Name, &key.Hint, &enabled, &maintenanceAllowed, &expires, &key.RPMLimit, &key.TPMLimit, &lastUsed, &created); err != nil {
		return nil, err
	}
	key.Enabled = enabled == 1
	key.MaintenanceAllowed = maintenanceAllowed == 1
	key.ExpiresAt = parseStoredTime(expires)
	key.LastUsedAt = parseStoredTime(lastUsed)
	key.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &key, nil
}

func (a *App) validClient(r *http.Request) bool {
	_, err := a.authenticateClient(r)
	return err == nil
}

func (a *App) enforceClientLimit(w http.ResponseWriter, key *ClientKey) bool {
	a.initializeRuntimeServices()
	ok, retryAfter, code := a.keyLimiter.allow(key, time.Now())
	if ok {
		return true
	}
	seconds := int(retryAfter.Round(time.Second).Seconds())
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	a.emitAlert("client_key_rate_limited", "warning", "Client key rate limit exceeded", map[string]any{"client_key_id": key.ID, "client_key_name": key.Name, "limit": code})
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "client key rate limit exceeded", "code": code})
	return false
}

func (a *App) touchClientKey(key *ClientKey) {
	if key == nil {
		return
	}
	now := time.Now().UTC()
	if key.lastTouch != nil {
		last := key.lastTouch.Load()
		if last != 0 && now.Sub(time.Unix(0, last)) < time.Minute {
			return
		}
		if !key.lastTouch.CompareAndSwap(last, now.UnixNano()) {
			return
		}
		defer func() {
			if key.LastUsedAt == nil || !key.LastUsedAt.Equal(now) {
				key.lastTouch.CompareAndSwap(now.UnixNano(), last)
			}
		}()
	} else if key.LastUsedAt != nil && now.Sub(*key.LastUsedAt) < time.Minute {
		return
	}
	if _, err := a.db.Exec("UPDATE client_keys SET last_used_at=?,updated_at=? WHERE id=?", now.Format(time.RFC3339), now.Format(time.RFC3339), key.ID); err == nil {
		key.LastUsedAt = &now
	}
}

func (a *App) listClientKeys(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query("SELECT id,name,key_hint,enabled,maintenance_allowed,COALESCE(expires_at,''),rpm_limit,tpm_limit,COALESCE(last_used_at,''),created_at FROM client_keys ORDER BY id")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query client keys"})
		return
	}
	defer rows.Close()
	out := []*ClientKey{}
	for rows.Next() {
		key, scanErr := scanClientKey(rows)
		if scanErr != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read client keys"})
			return
		}
		out = append(out, key)
	}
	writeJSON(w, 200, out)
}

func parseClientKeyExpiry(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil || !parsed.After(time.Now().UTC()) {
		return nil, errors.New("expires_at must be a future RFC3339 timestamp")
	}
	return &parsed, nil
}

func validateClientKeyLimits(rpm, tpm int) error {
	if rpm < 0 || rpm > 100000 || tpm < 0 || tpm > 100000000 {
		return errors.New("rpm_limit or tpm_limit is outside the allowed range")
	}
	return nil
}

func (a *App) createClientKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		ExpiresAt string `json:"expires_at"`
		RPMLimit  int    `json:"rpm_limit"`
		TPMLimit  int    `json:"tpm_limit"`
	}
	if readJSON(r, &in) != nil || strings.TrimSpace(in.Name) == "" || len(strings.TrimSpace(in.Name)) > 80 {
		writeJSON(w, 400, map[string]string{"error": "name is required and must be at most 80 characters"})
		return
	}
	if err := validateClientKeyLimits(in.RPMLimit, in.TPMLimit); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	expires, err := parseClientKeyExpiry(in.ExpiresAt)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	plain, err := randomKey()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not generate client key"})
		return
	}
	now := time.Now().UTC()
	var expiresValue any
	if expires != nil {
		expiresValue = expires.Format(time.RFC3339)
	}
	result, err := a.db.Exec("INSERT INTO client_keys(name,key_hash,key_hint,enabled,expires_at,rpm_limit,tpm_limit,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)", strings.TrimSpace(in.Name), hashToken(plain), clientKeyHint(plain), 1, expiresValue, in.RPMLimit, in.TPMLimit, now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not create client key"})
		return
	}
	id, _ := result.LastInsertId()
	a.invalidateClientKeyCache()
	writeJSON(w, 201, map[string]any{"id": id, "client_key": plain, "warning": "copy this key now; it will not be shown again"})
}

func (a *App) clientKeyID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (a *App) patchClientKey(w http.ResponseWriter, r *http.Request) {
	id, err := a.clientKeyID(r)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid client key id"})
		return
	}
	var in struct {
		Name               *string `json:"name"`
		Enabled            *bool   `json:"enabled"`
		MaintenanceAllowed *bool   `json:"maintenance_allowed"`
		ExpiresAt          *string `json:"expires_at"`
		RPMLimit           *int    `json:"rpm_limit"`
		TPMLimit           *int    `json:"tpm_limit"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid client key update"})
		return
	}
	current, err := a.loadClientKey(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "client key not found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load client key"})
		return
	}
	if in.Name != nil {
		current.Name = strings.TrimSpace(*in.Name)
	}
	if current.Name == "" || len(current.Name) > 80 {
		writeJSON(w, 400, map[string]string{"error": "name is required and must be at most 80 characters"})
		return
	}
	if in.Enabled != nil {
		current.Enabled = *in.Enabled
	}
	if in.MaintenanceAllowed != nil {
		current.MaintenanceAllowed = *in.MaintenanceAllowed
	}
	if in.RPMLimit != nil {
		current.RPMLimit = *in.RPMLimit
	}
	if in.TPMLimit != nil {
		current.TPMLimit = *in.TPMLimit
	}
	if err := validateClientKeyLimits(current.RPMLimit, current.TPMLimit); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if in.ExpiresAt != nil {
		current.ExpiresAt, err = parseClientKeyExpiry(*in.ExpiresAt)
		if err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
	}
	var expiry any
	if current.ExpiresAt != nil {
		expiry = current.ExpiresAt.Format(time.RFC3339)
	}
	_, err = a.db.Exec("UPDATE client_keys SET name=?,enabled=?,maintenance_allowed=?,expires_at=?,rpm_limit=?,tpm_limit=?,updated_at=? WHERE id=?", current.Name, boolInt(current.Enabled), boolInt(current.MaintenanceAllowed), expiry, current.RPMLimit, current.TPMLimit, time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update client key"})
		return
	}
	a.invalidateClientKeyCache()
	writeJSON(w, 200, current)
}

func (a *App) loadClientKey(id int64) (*ClientKey, error) {
	return scanClientKey(a.db.QueryRow("SELECT id,name,key_hint,enabled,maintenance_allowed,COALESCE(expires_at,''),rpm_limit,tpm_limit,COALESCE(last_used_at,''),created_at FROM client_keys WHERE id=?", id))
}

func (a *App) deleteClientKey(w http.ResponseWriter, r *http.Request) {
	id, err := a.clientKeyID(r)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid client key id"})
		return
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM client_keys").Scan(&count); err != nil || count <= 1 {
		writeJSON(w, 400, map[string]string{"error": "at least one client key must remain"})
		return
	}
	result, err := a.db.Exec("DELETE FROM client_keys WHERE id=?", id)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete client key"})
		return
	}
	if n, _ := result.RowsAffected(); n == 0 {
		writeJSON(w, 404, map[string]string{"error": "client key not found"})
		return
	}
	a.invalidateClientKeyCache()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) rotateNamedClientKey(w http.ResponseWriter, r *http.Request) {
	id, err := a.clientKeyID(r)
	if err != nil || id <= 0 {
		writeJSON(w, 400, map[string]string{"error": "invalid client key id"})
		return
	}
	key, err := a.loadClientKey(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "client key not found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load client key"})
		return
	}
	plain, err := randomKey()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not generate client key"})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec("UPDATE client_keys SET key_hash=?,key_hint=?,updated_at=? WHERE id=?", hashToken(plain), clientKeyHint(plain), now, id); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not rotate client key"})
		return
	}
	a.invalidateClientKeyCache()
	if key.Name == "Legacy" {
		_, _ = a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('client_key',?)", hashToken(plain))
	}
	writeJSON(w, 200, map[string]any{"client_key": plain, "warning": "copy this key now; it will not be shown again"})
}

func (a *App) rotateClientKey(w http.ResponseWriter, r *http.Request) {
	if err := a.migrateLegacyClientKey(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not migrate the legacy client key"})
		return
	}
	var id int64
	if err := a.db.QueryRow("SELECT id FROM client_keys WHERE name='Legacy' ORDER BY id LIMIT 1").Scan(&id); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not find the legacy client key"})
		return
	}
	clone := r.Clone(r.Context())
	clone.SetPathValue("id", strconv.FormatInt(id, 10))
	a.rotateNamedClientKey(w, clone)
}

func (a *App) recordGatewayUsage(key *ClientKey, requestedModel, resolvedModel string, proxyID *int64, proxyURI, engine, status string, code int, latency time.Duration, firstToken *time.Duration, retries int, tokens *tokenUsage, attempts any, requestErr error) {
	a.recordGatewayUsageWithStream(key, requestedModel, resolvedModel, proxyID, proxyURI, engine, status, code, latency, firstToken, retries, tokens, attempts, "", false, requestErr)
}

func (a *App) recordGatewayUsageWithStream(key *ClientKey, requestedModel, resolvedModel string, proxyID *int64, proxyURI, engine, status string, code int, latency time.Duration, firstToken *time.Duration, retries int, tokens *tokenUsage, attempts any, clientUserAgent string, stream bool, requestErr error) {
	a.recordGatewayUsageWithOriginAndStream(key, requestedModel, resolvedModel, proxyID, proxyURI, engine, status, code, latency, firstToken, retries, tokens, attempts, clientUserAgent, stream, requestErr, "")
}

func (a *App) recordGatewayUsageWithOrigin(key *ClientKey, requestedModel, resolvedModel string, proxyID *int64, proxyURI, engine, status string, code int, latency time.Duration, firstToken *time.Duration, retries int, tokens *tokenUsage, attempts any, requestErr error, originOverride string) {
	a.recordGatewayUsageWithOriginAndStream(key, requestedModel, resolvedModel, proxyID, proxyURI, engine, status, code, latency, firstToken, retries, tokens, attempts, "", false, requestErr, originOverride)
}

func (a *App) recordGatewayUsageWithOriginAndStream(key *ClientKey, requestedModel, resolvedModel string, proxyID *int64, proxyURI, engine, status string, code int, latency time.Duration, firstToken *time.Duration, retries int, tokens *tokenUsage, attempts any, clientUserAgent string, stream bool, requestErr error, originOverride string) {
	var prompt, completion, total *int64
	if tokens != nil {
		prompt, completion, total = tokens.Prompt, tokens.Completion, tokens.Total
	}
	var firstMS any
	if firstToken != nil {
		firstMS = firstToken.Milliseconds()
	}
	var proxy any
	if proxyID != nil {
		proxy = *proxyID
	}
	var keyID any
	keyName := ""
	if key != nil {
		keyID = key.ID
		keyName = key.Name
	}
	encoded, _ := json.Marshal(attempts)
	origin := usageErrorOrigin("chat", status, code, lastErrString(requestErr))
	if originOverride != "" {
		origin = originOverride
	}
	args := []any{time.Now().UTC().Format(time.RFC3339), "chat", requestedModel, resolvedModel, keyID, keyName, "", clientUserAgent, stream, proxy, proxyURI, status, code, latency.Milliseconds(), firstMS, retries, prompt, completion, total, lastErrString(requestErr), origin, engine, string(encoded)}
	var err error
	if a.usageInsertStmt != nil {
		_, err = a.usageInsertStmt.Exec(args...)
	} else {
		_, err = a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,resolved_model,client_key_id,client_key_name,client_name,client_user_agent,stream,proxy_id,proxy_uri,status,status_code,latency_ms,first_token_latency_ms,retry_count,prompt_tokens,completion_tokens,total_tokens,error_message,error_origin,route_engine,attempt_summary) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", args...)
	}
	if err != nil {
		log.Printf("record gateway usage failed: %v", err)
	}
	if key != nil {
		a.initializeRuntimeServices()
		a.keyLimiter.recordTokens(key.ID, tokens, time.Now())
	}
}

func clientKeyError(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "disabled") || strings.Contains(err.Error(), "expired") {
		return err.Error()
	}
	return "invalid client key"
}

func clientKeyIDFromUsage(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if id, ok := v.(int64); ok && id > 0 {
		return id, nil
	}
	return nil, fmt.Errorf("invalid client key id")
}
