package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/proxy"
	moderncsqlite "modernc.org/sqlite"
)

type App struct {
	db                      *sql.DB
	key                     []byte
	session                 string
	admin                   string
	cookieSecure            bool
	loginMu                 sync.Mutex
	loginAttempts           map[string]loginAttempt
	routingLocks            [64]sync.Mutex
	resinMu                 sync.Mutex
	resinFailureCount       int
	resinFailureStart       time.Time
	resinLastSuccessPersist time.Time
	resinProbeMu            sync.Mutex
	resinLastAutoProbe      time.Time
	resinCoolMu             sync.Mutex
	resinCooledAccounts     map[string]time.Time
	resinBreakerMu          sync.Mutex
	resinBreakerHits        int
	resinBreakerSince       time.Time
	resinBreakerOpenUntil   time.Time
	gatewaySem              chan struct{}
	rr                      atomic.Uint64
	proxyRuntime            *proxyRuntime
	keyLimiter              *clientKeyLimiter
	probeJobs               *probeJobStore
	alerts                  *alertDispatcher
	runtimeInitOnce         sync.Once
	runtimeCaches           runtimeCaches
	usageInsertStmt         *sql.Stmt
	laneSessions            *laneSessionMemo
	laneSessionsOnce        sync.Once
	logs                    *logSink
	logsOnce                sync.Once
	logAuthMu               sync.Mutex
	logAuthFails            map[string]loginAttempt
	logKeyMu                sync.Mutex
	logKeyCache             map[string]time.Time
}

type loginAttempt struct {
	Failures     int
	WindowStart  time.Time
	BlockedUntil time.Time
}

const (
	adminSessionPrefix        = "admin_session_"
	usageRetentionSettingKey  = "usage_retention_days"
	defaultUsageRetentionDays = 90
	upstreamRequestTimeout    = 120 * time.Second
	statusClientClosedRequest = 499
	// gatewayRetryBudget bounds how long one request may spend rotating exits.
	// Past it no new attempt is started and the last upstream status is relayed
	// as-is, so a caller is never made to wait for a chain of retries that the
	// gateway itself has already given up on.
	gatewayRetryBudget = 25 * time.Second
)

var (
	chinaLocation         = time.FixedZone("Asia/Shanghai", 8*60*60)
	usageRetentionOptions = map[int]struct{}{7: {}, 30: {}, 90: {}, 180: {}}
)

type ProxyRecord struct {
	ID                  int64      `json:"id"`
	URI                 string     `json:"uri"`
	Scheme              string     `json:"scheme"`
	Host                string     `json:"host"`
	Port                int        `json:"port"`
	Username            string     `json:"username,omitempty"`
	Password            string     `json:"-"`
	Engine              string     `json:"-"`
	Enabled             bool       `json:"enabled"`
	HealthStatus        string     `json:"health_status"`
	FailureCount        int        `json:"failure_count"`
	UsageState          string     `json:"usage_state,omitempty"`
	CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	LastProbeAt         *time.Time `json:"last_probe_at,omitempty"`
	LastProbeMS         *int64     `json:"last_probe_latency_ms,omitempty"`
	LastExitIP          string     `json:"last_exit_ip,omitempty"`
	LastProbeError      string     `json:"last_probe_error,omitempty"`
	UpstreamProbeAt     *time.Time `json:"upstream_probe_at,omitempty"`
	UpstreamProbeStatus string     `json:"upstream_probe_status,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

type ModelRecord struct {
	ID           int64           `json:"id"`
	ModelID      string          `json:"model_id"`
	DisplayName  string          `json:"display_name"`
	IsFree       bool            `json:"is_free"`
	FreeReason   string          `json:"free_reason"`
	Pricing      json.RawMessage `json:"pricing_metadata,omitempty"`
	Raw          json.RawMessage `json:"raw_metadata,omitempty"`
	AdminEnabled bool            `json:"admin_enabled"`
	RefreshedAt  time.Time       `json:"refreshed_at"`
}

type upstreamConfig struct {
	// BaseURL and APIKey are the fixed Zen lane (upstream_lane.go), carried in
	// the struct so request construction reads them from one place.
	BaseURL        string
	APIKey         string
	VisionBaseURL  string
	VisionAPIKey   string
	VisionModel    string
	VisionUseProxy bool
	UpdatedAt      time.Time
	LastRefresh    *time.Time
	LastError      string
}

type usageRequest struct {
	ID                  int64           `json:"id"`
	CreatedAt           time.Time       `json:"created_at"`
	RequestKind         string          `json:"request_kind"`
	Model               string          `json:"model"`
	ResolvedModel       string          `json:"resolved_model,omitempty"`
	ClientKeyID         *int64          `json:"client_key_id,omitempty"`
	ClientKeyName       string          `json:"client_key_name,omitempty"`
	ClientName          string          `json:"client_name,omitempty"`
	ClientUserAgent     string          `json:"client_user_agent,omitempty"`
	Stream              *bool           `json:"stream,omitempty"`
	ProxyID             *int64          `json:"proxy_id,omitempty"`
	ProxyURI            string          `json:"proxy_uri,omitempty"`
	Status              string          `json:"status"`
	StatusCode          int             `json:"status_code"`
	LatencyMS           int64           `json:"latency_ms"`
	FirstTokenLatencyMS *int64          `json:"first_token_latency_ms,omitempty"`
	RetryCount          int             `json:"retry_count"`
	PromptTokens        *int64          `json:"prompt_tokens,omitempty"`
	CompletionTokens    *int64          `json:"completion_tokens,omitempty"`
	TotalTokens         *int64          `json:"total_tokens,omitempty"`
	ErrorMessage        string          `json:"error_message,omitempty"`
	ErrorOrigin         string          `json:"error_origin"`
	RouteEngine         string          `json:"route_engine"`
	AttemptSummary      json.RawMessage `json:"attempt_summary,omitempty"`
}

type tokenUsage struct {
	Prompt     *int64
	Completion *int64
	Total      *int64
}

type chatEnvelope struct {
	Model    string          `json:"model"`
	User     json.RawMessage `json:"user"`
	Messages []any           `json:"messages"`
	Stream   bool            `json:"stream"`
}

func parseChatEnvelope(body []byte) (chatEnvelope, map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return chatEnvelope{}, nil, err
	}
	model, _ := payload["model"].(string)
	messages, messagesOK := payload["messages"].([]any)
	if rawMessages, present := payload["messages"]; present && rawMessages != nil && !messagesOK {
		return chatEnvelope{}, nil, errors.New("messages must be an array")
	}
	user, _ := json.Marshal(payload["user"])
	stream, _ := payload["stream"].(bool)
	return chatEnvelope{Model: model, User: user, Messages: messages, Stream: stream}, payload, nil
}

var blockedDownstreamHeaders = map[string]struct{}{
	"connection":          {},
	"content-length":      {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"set-cookie":          {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func main() {
	port := getenv("PORT", "8080")
	dbPath := getenv("DATABASE_PATH", "./data/app.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o750); err != nil {
		log.Fatal(err)
	}
	moderncsqlite.RegisterConnectionHook(func(conn moderncsqlite.ExecQuerierContext, _ string) error {
		_, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL", nil)
		return err
	})
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	// WAL allows concurrent readers while SQLite still serializes writers. The
	// per-connection hook above applies the lock timeout to every pooled conn.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err := migrate(db); err != nil {
		log.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		log.Fatal(err)
	}
	usageInsertStmt, err := db.Prepare("INSERT INTO usage_requests(created_at,request_kind,model,resolved_model,client_key_id,client_key_name,client_name,client_user_agent,stream,proxy_id,proxy_uri,status,status_code,latency_ms,first_token_latency_ms,retry_count,prompt_tokens,completion_tokens,total_tokens,error_message,error_origin,route_engine,attempt_summary) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		log.Fatal(err)
	}
	defer usageInsertStmt.Close()
	appKey := []byte(requireSecret("APP_ENCRYPTION_KEY", os.Getenv("APP_ENCRYPTION_KEY")))
	if looksLikePlaceholderSecret(string(appKey)) {
		log.Print("SECURITY WARNING: APP_ENCRYPTION_KEY appears to be a placeholder; migrate encrypted credentials to a random key")
	}
	sessionSecret := requireSecret("SESSION_SECRET", os.Getenv("SESSION_SECRET"))
	if looksLikePlaceholderSecret(sessionSecret) {
		log.Print("SECURITY WARNING: SESSION_SECRET appears to be a placeholder and should be replaced")
	}
	cookieSecure, err := envBool("COOKIE_SECURE", false)
	if err != nil {
		log.Fatal(err)
	}
	maxConcurrent, err := envInt("MAX_CONCURRENT_REQUESTS", 100, 1, 10000)
	if err != nil {
		log.Fatal(err)
	}
	h := sha256.Sum256(appKey)
	app := &App{
		db:              db,
		key:             h[:],
		session:         sessionSecret,
		admin:           os.Getenv("ADMIN_PASSWORD"),
		cookieSecure:    cookieSecure,
		loginAttempts:   make(map[string]loginAttempt),
		gatewaySem:      make(chan struct{}, maxConcurrent),
		usageInsertStmt: usageInsertStmt,
	}
	if err := app.ensureAdmin(); err != nil {
		log.Fatal(err)
	}
	if previousSecret := os.Getenv("APP_ENCRYPTION_KEY_PREVIOUS"); previousSecret != "" {
		requireSecret("APP_ENCRYPTION_KEY_PREVIOUS", previousSecret)
		previousHash := sha256.Sum256([]byte(previousSecret))
		migrated, err := app.rotateEncryptionKey(previousHash[:])
		if err != nil {
			log.Fatalf("APP_ENCRYPTION_KEY migration failed: %v", err)
		}
		log.Printf("APP_ENCRYPTION_KEY migration completed; re-encrypted %d values", migrated)
	}
	if err := app.deleteExpiredAdminSessions(); err != nil {
		log.Fatal(err)
	}
	if err := app.ensureClientKey(); err != nil {
		log.Fatal(err)
	}
	if err := app.migrateLegacyClientKey(); err != nil {
		log.Fatal(err)
	}
	app.initializeRuntimeServices()
	app.startLogging()
	app.deleteExpiredProxies()
	if err := app.deleteExpiredUsage(); err != nil {
		log.Fatal(err)
	}
	go app.expiredProxyJanitor()
	go app.proxyProbeJanitor()
	go app.modelRefreshJanitor()
	go app.alerts.run()
	go app.alertEvaluationJanitor()
	mux := http.NewServeMux()
	app.routes(mux)
	if logAddr := strings.TrimSpace(os.Getenv("LOG_API_ADDR")); logAddr != "" {
		app.serveLogAPI(logAddr)
	}
	server := &http.Server{Addr: ":" + port, Handler: app.withMiddleware(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: upstreamRequestTimeout, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	app.logEvent(logEvent{Level: logLevelInfo, Source: logSourceSystem, Event: "service.listen", Message: fmt.Sprintf("listening on :%s", port), Detail: map[string]any{"addr": ":" + port, "web_dir": getenv("WEB_DIR", "./web/dist")}})
	log.Printf("opencode proxy pool listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}

// requireSecret returns a configured secret, refusing to start on a value too
// short to be a real key. The message names the variable and its observed
// length, never the value, and explains the usual cause.
func requireSecret(name, value string) string {
	if len(value) >= 32 {
		return value
	}
	log.Fatalf("%s must contain at least 32 characters (got %d)%s", name, len(value), secretShapeHint(value))
	return ""
}

// secretShapeHint explains a rejected secret from its shape alone. An empty
// value almost always means the variable never reached the process; a trailing
// carriage return means the file was saved with Windows line endings.
func secretShapeHint(value string) string {
	switch {
	case value == "":
		return "; the variable is empty or was not passed to the process: check that .env sits next to docker-compose.yml, that the name is spelled exactly, and recreate the container after editing it"
	case strings.ContainsRune(value, '\r'):
		return "; the value ends with a carriage return: the file was saved with Windows line endings, convert it to LF"
	case strings.TrimSpace(value) != value:
		return "; the value has surrounding whitespace: drop the quotes and any spaces around the = sign"
	default:
		return "; generate one with `openssl rand -hex 32`"
	}
}

func getenv(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", key)
	}
	return value, nil
}

func envInt(key string, fallback, min, max int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be between %d and %d", key, min, max)
	}
	return value, nil
}

func looksLikePlaceholderSecret(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	for _, marker := range []string{"change-me", "change-this", "replace-with", "development-", "example"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS proxies (id INTEGER PRIMARY KEY AUTOINCREMENT, uri TEXT UNIQUE NOT NULL, scheme TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL, username TEXT, encrypted_password TEXT, enabled INTEGER NOT NULL DEFAULT 1, health_status TEXT NOT NULL DEFAULT 'unknown', failure_count INTEGER NOT NULL DEFAULT 0, cooldown_until TEXT, expires_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS models (id INTEGER PRIMARY KEY AUTOINCREMENT, model_id TEXT UNIQUE NOT NULL, display_name TEXT, is_free INTEGER NOT NULL, free_reason TEXT, pricing_metadata TEXT, raw_metadata TEXT, refreshed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS usage_requests (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, request_kind TEXT NOT NULL DEFAULT 'chat', model TEXT NOT NULL, resolved_model TEXT, client_key_id INTEGER, client_key_name TEXT, client_name TEXT, client_user_agent TEXT, stream INTEGER, proxy_id INTEGER, proxy_uri TEXT, status TEXT NOT NULL, status_code INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0, first_token_latency_ms INTEGER, retry_count INTEGER NOT NULL DEFAULT 0, prompt_tokens INTEGER, completion_tokens INTEGER, total_tokens INTEGER, error_message TEXT, error_origin TEXT NOT NULL DEFAULT '', route_engine TEXT NOT NULL DEFAULT 'builtin', attempt_summary TEXT, FOREIGN KEY(proxy_id) REFERENCES proxies(id));
CREATE TABLE IF NOT EXISTS session_proxy_routes (session_key TEXT PRIMARY KEY, proxy_id INTEGER NOT NULL, request_count INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, FOREIGN KEY(proxy_id) REFERENCES proxies(id));
CREATE TABLE IF NOT EXISTS resin_session_routes (session_key TEXT PRIMARY KEY, generation INTEGER NOT NULL DEFAULT 0, request_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS client_keys (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, key_hash TEXT NOT NULL UNIQUE, key_hint TEXT NOT NULL DEFAULT '', encrypted_key TEXT, enabled INTEGER NOT NULL DEFAULT 1, maintenance_allowed INTEGER NOT NULL DEFAULT 0, expires_at TEXT, rpm_limit INTEGER NOT NULL DEFAULT 0, tpm_limit INTEGER NOT NULL DEFAULT 0, last_used_at TEXT, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS model_policies (model_id TEXT PRIMARY KEY, enabled INTEGER NOT NULL DEFAULT 1, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS force_free_models (model_id TEXT PRIMARY KEY, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS model_aliases (id INTEGER PRIMARY KEY AUTOINCREMENT, alias TEXT NOT NULL UNIQUE, target_model_id TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS alert_events (id INTEGER PRIMARY KEY AUTOINCREMENT, dedupe_key TEXT NOT NULL, event_type TEXT NOT NULL, severity TEXT NOT NULL, payload TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL, created_at TEXT NOT NULL, delivered_at TEXT, last_error TEXT);
CREATE TABLE IF NOT EXISTS proxy_probe_jobs (id TEXT PRIMARY KEY, status TEXT NOT NULL, mode TEXT NOT NULL, total INTEGER NOT NULL, completed INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, expires_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS proxy_probe_results (job_id TEXT NOT NULL, proxy_id INTEGER NOT NULL, uri TEXT NOT NULL, exit_ok INTEGER NOT NULL DEFAULT 0, exit_ip TEXT, exit_latency_ms INTEGER, upstream_status TEXT, error TEXT, verdict TEXT, PRIMARY KEY(job_id,proxy_id), FOREIGN KEY(job_id) REFERENCES proxy_probe_jobs(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS proxy_model_bans (proxy_id INTEGER NOT NULL, model_id TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'suspect', consecutive_failures INTEGER NOT NULL DEFAULT 0, banned_until TEXT, updated_at TEXT NOT NULL, PRIMARY KEY(proxy_id,model_id));
CREATE TABLE IF NOT EXISTS compression_sessions (session_key TEXT PRIMARY KEY, model TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', folded_digest TEXT NOT NULL DEFAULT '', folded_messages INTEGER NOT NULL DEFAULT 0, folded_tokens INTEGER NOT NULL DEFAULT 0, compressions INTEGER NOT NULL DEFAULT 0, tokens_before INTEGER NOT NULL DEFAULT 0, tokens_after INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS compression_events (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, session_key TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', api_style TEXT NOT NULL DEFAULT '', summary_model TEXT NOT NULL DEFAULT '', folded_messages INTEGER NOT NULL DEFAULT 0, tokens_before INTEGER NOT NULL DEFAULT 0, tokens_after INTEGER NOT NULL DEFAULT 0, summary_tokens INTEGER NOT NULL DEFAULT 0, latency_ms INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL DEFAULT 'success', error_message TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS log_events (id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, created_at TEXT NOT NULL, level TEXT NOT NULL, source TEXT NOT NULL, event TEXT NOT NULL, request_id TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0, duration_ms INTEGER NOT NULL DEFAULT 0, client_ip TEXT NOT NULL DEFAULT '', user_agent TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '', route_engine TEXT NOT NULL DEFAULT '', client_key_id INTEGER, message TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS log_notes (id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT NOT NULL, author TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT 'note', severity TEXT NOT NULL DEFAULT 'info', title TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '', event_id INTEGER, tags TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_requests(created_at); CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_requests(model);`)
	if err != nil {
		return err
	}
	// Existing databases predate proxy expiry; SQLite reports a duplicate-column
	// error for upgraded databases, which is intentionally ignored here.
	for _, statement := range []string{
		"ALTER TABLE proxies ADD COLUMN expires_at TEXT",
		"ALTER TABLE usage_requests ADD COLUMN proxy_id INTEGER",
		"ALTER TABLE usage_requests ADD COLUMN proxy_uri TEXT",
		"ALTER TABLE usage_requests ADD COLUMN request_kind TEXT NOT NULL DEFAULT 'chat'",
		"ALTER TABLE usage_requests ADD COLUMN first_token_latency_ms INTEGER",
		"ALTER TABLE usage_requests ADD COLUMN error_origin TEXT NOT NULL DEFAULT ''",
		"ALTER TABLE usage_requests ADD COLUMN route_engine TEXT NOT NULL DEFAULT 'builtin'",
		"ALTER TABLE usage_requests ADD COLUMN resolved_model TEXT",
		"ALTER TABLE usage_requests ADD COLUMN client_key_id INTEGER",
		"ALTER TABLE usage_requests ADD COLUMN client_key_name TEXT",
		"ALTER TABLE usage_requests ADD COLUMN client_name TEXT",
		"ALTER TABLE usage_requests ADD COLUMN client_user_agent TEXT",
		"ALTER TABLE usage_requests ADD COLUMN stream INTEGER",
		"ALTER TABLE usage_requests ADD COLUMN attempt_summary TEXT",
		"ALTER TABLE client_keys ADD COLUMN maintenance_allowed INTEGER NOT NULL DEFAULT 0",
		"ALTER TABLE client_keys ADD COLUMN encrypted_key TEXT",
		"ALTER TABLE proxies ADD COLUMN last_probe_at TEXT",
		"ALTER TABLE proxies ADD COLUMN last_probe_latency_ms INTEGER",
		"ALTER TABLE proxies ADD COLUMN last_exit_ip TEXT",
		"ALTER TABLE proxies ADD COLUMN last_probe_error TEXT",
		"ALTER TABLE proxies ADD COLUMN upstream_probe_at TEXT",
		"ALTER TABLE proxies ADD COLUMN upstream_probe_status TEXT",
		"ALTER TABLE proxy_probe_results ADD COLUMN verdict TEXT",
	} {
		if _, alterErr := db.Exec(statement); alterErr != nil && !strings.Contains(strings.ToLower(alterErr.Error()), "duplicate column") {
			return alterErr
		}
	}
	for _, statement := range []string{
		"CREATE INDEX IF NOT EXISTS idx_proxies_expires_at ON proxies(expires_at)",
		"CREATE INDEX IF NOT EXISTS idx_session_proxy_routes_updated_at ON session_proxy_routes(updated_at)",
		"CREATE INDEX IF NOT EXISTS idx_usage_status ON usage_requests(status)",
		"CREATE INDEX IF NOT EXISTS idx_usage_error_origin ON usage_requests(error_origin)",
		"CREATE INDEX IF NOT EXISTS idx_resin_session_routes_updated_at ON resin_session_routes(updated_at)",
		"CREATE INDEX IF NOT EXISTS idx_usage_client_key_created ON usage_requests(client_key_id,created_at)",
		"CREATE INDEX IF NOT EXISTS idx_usage_kind_created ON usage_requests(request_kind,created_at)",
		"CREATE INDEX IF NOT EXISTS idx_usage_kind_status_created ON usage_requests(request_kind,status,created_at)",
		"CREATE INDEX IF NOT EXISTS idx_usage_model_created ON usage_requests(model,created_at)",
		"CREATE INDEX IF NOT EXISTS idx_client_keys_hash ON client_keys(key_hash)",
		"CREATE INDEX IF NOT EXISTS idx_alert_events_due ON alert_events(status,next_attempt_at)",
		"CREATE INDEX IF NOT EXISTS idx_proxy_probe_jobs_expires ON proxy_probe_jobs(expires_at)",
		"CREATE INDEX IF NOT EXISTS idx_proxy_model_bans_until ON proxy_model_bans(banned_until)",
		"CREATE INDEX IF NOT EXISTS idx_compression_sessions_updated ON compression_sessions(updated_at)",
		"CREATE INDEX IF NOT EXISTS idx_compression_events_created ON compression_events(created_at)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_ts ON log_events(ts)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_level_ts ON log_events(level,ts)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_source_ts ON log_events(source,ts)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_request ON log_events(request_id)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_path_ts ON log_events(path,ts)",
		"CREATE INDEX IF NOT EXISTS idx_log_events_event_ts ON log_events(event,ts)",
		"CREATE INDEX IF NOT EXISTS idx_log_notes_created ON log_notes(created_at)",
	} {
		if _, indexErr := db.Exec(statement); indexErr != nil {
			return indexErr
		}
	}
	if err := backfillUsageErrorOrigins(db); err != nil {
		return err
	}
	_, err = db.Exec("UPDATE usage_requests SET route_engine=CASE WHEN COALESCE(proxy_uri,'')='' AND proxy_id IS NULL THEN 'direct' ELSE 'builtin' END WHERE COALESCE(route_engine,'')='' OR route_engine='builtin'")
	if err != nil {
		return err
	}
	// Probe execution is process-local. Keep completed and interrupted reports
	// queryable for their normal retention period after a restart.
	_, err = db.Exec("UPDATE proxy_probe_jobs SET status='interrupted' WHERE status='running'")
	return err
}

func backfillUsageErrorOrigins(db *sql.DB) error {
	rows, err := db.Query("SELECT id,COALESCE(request_kind,'chat'),status,status_code,COALESCE(error_message,'') FROM usage_requests WHERE COALESCE(error_origin,'')=''")
	if err != nil {
		return err
	}
	type usageOriginUpdate struct {
		id     int64
		origin string
	}
	updates := []usageOriginUpdate{}
	for rows.Next() {
		var id int64
		var kind, status, message string
		var code int
		if err := rows.Scan(&id, &kind, &status, &code, &message); err != nil {
			_ = rows.Close()
			return err
		}
		updates = append(updates, usageOriginUpdate{id: id, origin: usageErrorOrigin(kind, status, code, message)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare("UPDATE usage_requests SET error_origin=? WHERE id=?")
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, update := range updates {
		if _, err := statement.Exec(update.origin, update.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) ensureAdmin() error {
	var hash string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		if err := validateAdminPassword(a.admin); err != nil {
			return fmt.Errorf("ADMIN_PASSWORD is required for first startup: %w", err)
		}
		if subtle.ConstantTimeCompare([]byte(a.admin), []byte("admin")) == 1 || looksLikePlaceholderSecret(a.admin) {
			return errors.New("ADMIN_PASSWORD must not use a default or placeholder value")
		}
		h, e := bcrypt.GenerateFromPassword([]byte(a.admin), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		_, e = a.db.Exec("INSERT INTO settings(key,value) VALUES('admin_hash',?)", string(h))
		return e
	}
	if err == nil && hash == "" {
		return errors.New("stored admin_hash is empty")
	}
	return err
}

func (a *App) ensureClientKey() error {
	var enc string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key='client_key'").Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		key, e := randomKey()
		if e != nil {
			return e
		}
		encrypted, e := a.encrypt(key)
		if e != nil {
			return e
		}
		now := time.Now().UTC().Format(time.RFC3339)
		tx, e := a.db.Begin()
		if e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES('client_key',?)", hashToken(key)); e != nil {
			_ = tx.Rollback()
			return e
		}
		if _, e = tx.Exec("INSERT INTO client_keys(name,key_hash,key_hint,encrypted_key,enabled,created_at,updated_at) VALUES('Legacy',?,?,?,?,?,?)", hashToken(key), clientKeyHint(key), encrypted, 1, now, now); e != nil {
			_ = tx.Rollback()
			return e
		}
		return tx.Commit()
	}
	return err
}

func randomKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// Keep the client credential in the conservative token alphabet accepted by
	// clients that reject underscores in API keys.
	return "ocp-" + strings.ReplaceAll(base64.RawURLEncoding.EncodeToString(b), "_", "-"), nil
}
func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func validateAdminPassword(password string) error {
	length := len([]byte(password))
	if length < 8 {
		return errors.New("password must be at least 8 bytes")
	}
	if length > 72 {
		return errors.New("password must not exceed 72 bytes")
	}
	return nil
}

func (a *App) adminSessionKey(token string) string {
	mac := hmac.New(sha256.New, []byte(a.session))
	_, _ = mac.Write([]byte(token))
	return adminSessionPrefix + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) deleteExpiredAdminSessions() error {
	_, err := a.db.Exec(
		"DELETE FROM settings WHERE key GLOB 'session_ocp-*' OR (key GLOB 'admin_session_*' AND value <= ?)",
		time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *App) loginRetryAfter(ip string) time.Duration {
	const window = 15 * time.Minute
	now := time.Now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	if len(a.loginAttempts) > 1024 {
		for key, attempt := range a.loginAttempts {
			if now.Sub(attempt.WindowStart) > window && !attempt.BlockedUntil.After(now) {
				delete(a.loginAttempts, key)
			}
		}
	}
	attempt, ok := a.loginAttempts[ip]
	if !ok {
		return 0
	}
	if attempt.BlockedUntil.After(now) {
		return time.Until(attempt.BlockedUntil)
	}
	if now.Sub(attempt.WindowStart) > window {
		delete(a.loginAttempts, ip)
	}
	return 0
}

func (a *App) recordLoginFailure(ip string) {
	const (
		window      = 15 * time.Minute
		blockFor    = 15 * time.Minute
		maxFailures = 5
	)
	now := time.Now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	attempt := a.loginAttempts[ip]
	if attempt.WindowStart.IsZero() || now.Sub(attempt.WindowStart) > window {
		attempt = loginAttempt{WindowStart: now}
	}
	attempt.Failures++
	if attempt.Failures >= maxFailures {
		attempt.BlockedUntil = now.Add(blockFor)
	}
	a.loginAttempts[ip] = attempt
}

func (a *App) clearLoginFailures(ip string) {
	a.loginMu.Lock()
	delete(a.loginAttempts, ip)
	a.loginMu.Unlock()
}

func (a *App) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     "ocp_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := a.db.PingContext(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/chat/completions", a.gatewayChat)
	mux.HandleFunc("GET /v1/models", a.gatewayModels)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/auth/me", a.requireAdmin(a.me))
	mux.HandleFunc("POST /api/auth/password", a.requireAdmin(a.changePassword))
	mux.HandleFunc("GET /api/settings/upstream", a.requireAdmin(a.getUpstream))
	mux.HandleFunc("PUT /api/settings/upstream", a.requireAdmin(a.putUpstream))
	mux.HandleFunc("PUT /api/settings/routing", a.requireAdmin(a.putRouting))
	mux.HandleFunc("GET /api/settings/proxy-engine", a.requireAdmin(a.getProxyEngine))
	mux.HandleFunc("PUT /api/settings/proxy-engine", a.requireAdmin(a.putProxyEngine))
	mux.HandleFunc("POST /api/settings/proxy-engine/resin/test", a.requireAdmin(a.testResin))
	mux.HandleFunc("POST /api/settings/proxy-engine/resin/recover", a.requireAdmin(a.recoverResin))
	mux.HandleFunc("GET /api/settings/usage-retention", a.requireAdmin(a.getUsageRetention))
	mux.HandleFunc("PUT /api/settings/usage-retention", a.requireAdmin(a.putUsageRetention))
	mux.HandleFunc("GET /api/settings/maintenance", a.requireAdmin(a.getMaintenanceSettings))
	mux.HandleFunc("PUT /api/settings/maintenance", a.requireAdmin(a.putMaintenanceSettings))
	mux.HandleFunc("POST /api/settings/upstream/test", a.requireAdmin(a.testUpstream))
	mux.HandleFunc("POST /api/settings/client-key/rotate", a.requireAdmin(a.retiredClientKeyRotation))
	mux.HandleFunc("GET /api/client-keys", a.requireAdmin(a.listClientKeys))
	mux.HandleFunc("POST /api/client-keys", a.requireAdmin(a.createClientKey))
	mux.HandleFunc("GET /api/client-keys/{id}/secret", a.requireAdmin(a.getClientKeySecret))
	mux.HandleFunc("PATCH /api/client-keys/{id}", a.requireAdmin(a.patchClientKey))
	mux.HandleFunc("DELETE /api/client-keys/{id}", a.requireAdmin(a.deleteClientKey))
	mux.HandleFunc("POST /api/client-keys/{id}/rotate", a.requireAdmin(a.rotateNamedClientKey))
	mux.HandleFunc("POST /api/settings/models/refresh", a.requireAdmin(a.refreshModels))
	mux.HandleFunc("GET /api/settings/model-refresh", a.requireAdmin(a.getModelRefreshSettings))
	mux.HandleFunc("PUT /api/settings/model-refresh", a.requireAdmin(a.putModelRefreshSettings))
	mux.HandleFunc("GET /api/models", a.requireAdmin(a.listModels))
	mux.HandleFunc("GET /api/models/free", a.requireAdmin(a.listFreeModels))
	mux.HandleFunc("PATCH /api/models/{id}/policy", a.requireAdmin(a.patchModelPolicy))
	mux.HandleFunc("GET /api/model-overrides/free", a.requireAdmin(a.listForceFreeModels))
	mux.HandleFunc("POST /api/model-overrides/free", a.requireAdmin(a.createForceFreeModel))
	mux.HandleFunc("DELETE /api/model-overrides/free", a.requireAdmin(a.deleteForceFreeModel))
	mux.HandleFunc("GET /api/model-aliases", a.requireAdmin(a.listModelAliases))
	mux.HandleFunc("POST /api/model-aliases", a.requireAdmin(a.createModelAlias))
	mux.HandleFunc("PATCH /api/model-aliases/{id}", a.requireAdmin(a.patchModelAlias))
	mux.HandleFunc("DELETE /api/model-aliases/{id}", a.requireAdmin(a.deleteModelAlias))
	mux.HandleFunc("GET /api/proxies", a.requireAdmin(a.listProxies))
	mux.HandleFunc("POST /api/proxies", a.requireAdmin(a.addProxy))
	mux.HandleFunc("POST /api/proxies/import", a.requireAdmin(a.importProxies))
	mux.HandleFunc("POST /api/proxies/bulk-delete", a.requireAdmin(a.bulkDeleteProxies))
	mux.HandleFunc("PATCH /api/proxies/{id}", a.requireAdmin(a.patchProxy))
	mux.HandleFunc("DELETE /api/proxies/{id}", a.requireAdmin(a.deleteProxy))
	mux.HandleFunc("POST /api/proxies/{id}/test", a.requireAdmin(a.testProxy))
	mux.HandleFunc("POST /api/proxy-probes", a.requireAdmin(a.createProxyProbeJob))
	mux.HandleFunc("GET /api/proxy-probes/{id}", a.requireAdmin(a.getProxyProbeJob))
	mux.HandleFunc("GET /api/stats/summary", a.requireAdmin(a.statsSummary))
	mux.HandleFunc("GET /api/stats/timeseries", a.requireAdmin(a.statsTimeseries))
	mux.HandleFunc("GET /api/stats/models", a.requireAdmin(a.statsModels))
	mux.HandleFunc("GET /api/stats/proxies", a.requireAdmin(a.statsProxies))
	mux.HandleFunc("GET /api/public/status", a.publicStatus)
	mux.HandleFunc("GET /api/usage/requests", a.requireAdmin(a.usageList))
	mux.HandleFunc("GET /api/usage/rates", a.requireAdmin(a.usageRates))
	mux.HandleFunc("GET /api/settings/probes", a.requireAdmin(a.getProbeSettings))
	mux.HandleFunc("PUT /api/settings/probes", a.requireAdmin(a.putProbeSettings))
	mux.HandleFunc("GET /api/settings/alerts", a.requireAdmin(a.getAlertSettings))
	mux.HandleFunc("PUT /api/settings/alerts", a.requireAdmin(a.putAlertSettings))
	mux.HandleFunc("GET /api/settings/compression", a.requireAdmin(a.getCompressionSettings))
	mux.HandleFunc("PUT /api/settings/compression", a.requireAdmin(a.putCompressionSettings))
	mux.HandleFunc("POST /api/settings/compression/test", a.requireAdmin(a.testCompressionSettings))
	mux.HandleFunc("GET /api/compression/stats", a.requireAdmin(a.compressionStats))
	mux.HandleFunc("GET /api/compression/sessions", a.requireAdmin(a.listCompressionSessions))
	mux.HandleFunc("POST /api/compression/sessions/reset", a.requireAdmin(a.resetCompressionSessions))
	mux.HandleFunc("DELETE /api/compression/sessions/{key}", a.requireAdmin(a.deleteCompressionSession))
	mux.HandleFunc("POST /api/compression/preview", a.requireAdmin(a.previewCompression))
	a.logRoutes(mux)
	webDir := getenv("WEB_DIR", "./web/dist")
	mux.HandleFunc("/", serveSPA(webDir))
}

// withMiddleware composes the outer layers in the order that keeps the log
// complete: the request is traced first, a panic is turned into a logged 500
// next, and the security headers apply to whatever finally answers.
func (a *App) withMiddleware(next http.Handler) http.Handler {
	return a.requestLog(a.recoverPanic(a.securityHeaders(next)))
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; form-action 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; connect-src 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			maintenance, err := a.maintenanceModeEnabled()
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gateway temporarily unavailable", "code": "gateway_unavailable"})
				return
			}
			if maintenance {
				key, authErr := a.authenticateClient(r)
				if authErr == nil && !key.MaintenanceAllowed {
					writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "service temporarily unavailable for maintenance", "code": "maintenance_mode"})
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func serveSPA(dir string) http.HandlerFunc {
	fs := http.FileServer(http.Dir(dir))
	return func(w http.ResponseWriter, r *http.Request) {
		if isAPINamespace(r.URL.Path) {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		p := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if r.URL.Path != "/" {
			if _, err := os.Stat(p); err == nil {
				fs.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	}
}

func isAPINamespace(path string) bool {
	return path == "/api" || strings.HasPrefix(path, "/api/") || path == "/v1" || strings.HasPrefix(path, "/v1/")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func readJSON(r *http.Request, v any) error {
	body, err := readLimitedBody(r.Body, 2<<20)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func readLimitedBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("request body exceeds %d bytes", limit)
	}
	return body, nil
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	ip := requestIP(r)
	if retryAfter := a.loginRetryAfter(ip); retryAfter > 0 {
		seconds := int(retryAfter.Round(time.Second).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts"})
		return
	}
	var hash string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash); err != nil {
		writeJSON(w, 500, map[string]string{"error": "authentication service unavailable"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		a.recordLoginFailure(ip)
		writeJSON(w, 401, map[string]string{"error": "invalid password"})
		return
	}
	token, err := randomKey()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	if _, err = a.db.Exec(
		"INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)",
		a.adminSessionKey(token),
		time.Now().Add(12*time.Hour).UTC().Format(time.RFC3339),
	); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not create session"})
		return
	}
	a.clearLoginFailures(ip)
	a.setSessionCookie(w, token, 43200)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
		return
	}
	if cookie, err := r.Cookie("ocp_session"); err == nil && cookie.Value != "" {
		if _, err := a.db.Exec("DELETE FROM settings WHERE key=?", a.adminSessionKey(cookie.Value)); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not revoke session"})
			return
		}
	}
	a.setSessionCookie(w, "", -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) requireAdmin(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginMutation(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
			return
		}
		if !a.isAdmin(r) {
			writeJSON(w, 401, map[string]string{"error": "authentication required"})
			return
		}
		fn(w, r)
	}
}

func sameOriginMutation(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Host != "" && strings.EqualFold(parsed.Host, r.Host)
}
func (a *App) isAdmin(r *http.Request) bool {
	c, err := r.Cookie("ocp_session")
	if err != nil || c.Value == "" {
		return false
	}
	var exp string
	key := a.adminSessionKey(c.Value)
	if a.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&exp) != nil {
		return false
	}
	t, e := time.Parse(time.RFC3339, exp)
	if e != nil || !t.After(time.Now()) {
		_, _ = a.db.Exec("DELETE FROM settings WHERE key=?", key)
		return false
	}
	return true
}
func (a *App) me(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"authenticated": true})
}
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request"})
		return
	}
	if err := validateAdminPassword(in.New); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	var hash string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not verify current password"})
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Current)) != nil {
		writeJSON(w, 400, map[string]string{"error": "current password is invalid"})
		return
	}
	h, err := bcrypt.GenerateFromPassword([]byte(in.New), bcrypt.DefaultCost)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "password could not be processed"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update password"})
		return
	}
	if _, err = tx.Exec("UPDATE settings SET value=? WHERE key='admin_hash'", string(h)); err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": "could not update password"})
		return
	}
	if _, err = tx.Exec("DELETE FROM settings WHERE key GLOB 'admin_session_*' OR key GLOB 'session_ocp-*'"); err != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": "could not revoke existing sessions"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update password"})
		return
	}
	a.setSessionCookie(w, "", -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) getUpstream(w http.ResponseWriter, _ *http.Request) {
	cfg, err := a.loadUpstream()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	var models int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM models WHERE is_free=1").Scan(&models); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not count models"})
		return
	}
	// The upstream is not configurable: the lane endpoint, credential and
	// disguise headers are constants (upstream_lane.go). The response still
	// reports them so the UI can show what the gateway talks to.
	writeJSON(w, 200, map[string]any{"upstream_lane": "zen-anonymous", "upstream_endpoint": upstreamEndpoint(laneBaseURL(), "/chat/completions"), "upstream_models_endpoint": upstreamEndpoint(laneBaseURL(), "/models"), "upstream_authorization": "Bearer " + zenAnonymousKey, "vision_base_url": cfg.VisionBaseURL, "has_vision_api_key": cfg.VisionAPIKey != "", "vision_model": cfg.VisionModel, "vision_use_proxy": cfg.VisionUseProxy, "vision_configured": cfg.VisionBaseURL != "" && cfg.VisionModel != "", "last_model_refresh_at": cfg.LastRefresh, "last_model_refresh_error": cfg.LastError, "free_model_count": models, "gateway_base_url": "/v1", "client_key_configured": a.clientKeyConfigured(), "session_proxy_request_limit": a.sessionProxyRequestLimit()})
}

func (a *App) putRouting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SessionProxyRequestLimit int `json:"session_proxy_request_limit"`
	}
	if readJSON(r, &in) != nil || in.SessionProxyRequestLimit < 0 || in.SessionProxyRequestLimit > 100000 {
		writeJSON(w, 400, map[string]string{"error": "session_proxy_request_limit must be between 0 and 100000"})
		return
	}
	_, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('session_proxy_request_limit',?)", strconv.Itoa(in.SessionProxyRequestLimit))
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not save routing configuration"})
		return
	}
	a.invalidateRoutingLimit()
	writeJSON(w, 200, map[string]any{"ok": true, "session_proxy_request_limit": in.SessionProxyRequestLimit})
}

func (a *App) sessionProxyRequestLimit() int {
	return a.cachedRoutingLimit()
}

func (a *App) usageRetentionDays() int {
	var raw string
	if a.db.QueryRow("SELECT value FROM settings WHERE key=?", usageRetentionSettingKey).Scan(&raw) != nil {
		return defaultUsageRetentionDays
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return defaultUsageRetentionDays
	}
	if _, ok := usageRetentionOptions[days]; !ok {
		return defaultUsageRetentionDays
	}
	return days
}

func usageRetentionCutoff(days int, now time.Time) string {
	return now.UTC().AddDate(0, 0, -days).Format(time.RFC3339)
}

func (a *App) deleteExpiredUsage() error {
	_, err := a.db.Exec(
		"DELETE FROM usage_requests WHERE created_at < ?",
		usageRetentionCutoff(a.usageRetentionDays(), time.Now()),
	)
	return err
}

func (a *App) getUsageRetention(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"usage_retention_days": a.usageRetentionDays()})
}

func (a *App) putUsageRetention(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Days int `json:"usage_retention_days"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if _, ok := usageRetentionOptions[in.Days]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "usage_retention_days must be 7, 30, 90, or 180"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save usage retention"})
		return
	}
	if _, err = tx.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", usageRetentionSettingKey, strconv.Itoa(in.Days)); err == nil {
		_, err = tx.Exec("DELETE FROM usage_requests WHERE created_at < ?", usageRetentionCutoff(in.Days, time.Now()))
	}
	if err != nil {
		_ = tx.Rollback()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save usage retention"})
		return
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save usage retention"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "usage_retention_days": in.Days})
}

func (a *App) clientKeyConfigured() bool {
	var hash string
	return a.db.QueryRow("SELECT value FROM settings WHERE key='client_key'").Scan(&hash) == nil && hash != ""
}

// putUpstream stores the optional vision bridge. The Zen lane itself is fixed
// (see upstream_lane.go): an operator cannot point this gateway at another
// upstream, so the only mutable part of the form is the vision supplier.
func (a *App) putUpstream(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VisionBaseURL  string `json:"vision_base_url"`
		VisionAPIKey   string `json:"vision_api_key"`
		VisionModel    string `json:"vision_model"`
		VisionUseProxy *bool  `json:"vision_use_proxy"`
	}
	// Lenient on purpose: older clients still send base_url/api_key/
	// custom_headers. Those fields no longer exist, so they are dropped instead
	// of rejected.
	body, bodyErr := readLimitedBody(r.Body, 2<<20)
	if bodyErr != nil || json.Unmarshal(body, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid request body"})
		return
	}
	old, loadErr := a.loadUpstream()
	if loadErr != nil {
		writeJSON(w, 500, map[string]string{"error": loadErr.Error()})
		return
	}
	in.VisionBaseURL = strings.TrimSpace(in.VisionBaseURL)
	if in.VisionBaseURL != "" {
		normalized, e := validateUpstreamBaseURL(in.VisionBaseURL)
		if e != nil {
			writeJSON(w, 400, map[string]string{"error": "vision " + e.Error()})
			return
		}
		in.VisionBaseURL = normalized
	}
	in.VisionAPIKey = normalizeAPIKey(in.VisionAPIKey)
	if in.VisionAPIKey == "" {
		in.VisionAPIKey = old.VisionAPIKey
	}
	in.VisionModel = strings.TrimSpace(in.VisionModel)
	if in.VisionBaseURL == "" || in.VisionModel == "" {
		in.VisionBaseURL = ""
		in.VisionModel = ""
	}
	visionUseProxy := old.VisionUseProxy
	if in.VisionUseProxy != nil {
		visionUseProxy = *in.VisionUseProxy
	}
	visionEnc, e := a.encrypt(in.VisionAPIKey)
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": "vision encryption failed"})
		return
	}
	_, e = a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('upstream_vision_base_url',?),('upstream_vision_api_key',?),('upstream_vision_model',?),('upstream_vision_use_proxy',?)", in.VisionBaseURL, visionEnc, in.VisionModel, strconv.FormatBool(visionUseProxy))
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": e.Error()})
		return
	}
	a.invalidateUpstreamCache()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) loadUpstream() (upstreamConfig, error) {
	a.runtimeCaches.upstream.mu.RLock()
	if a.runtimeCaches.upstream.loaded {
		cfg := cloneUpstreamConfig(a.runtimeCaches.upstream.cfg)
		a.runtimeCaches.upstream.mu.RUnlock()
		return cfg, nil
	}
	a.runtimeCaches.upstream.mu.RUnlock()

	a.runtimeCaches.upstream.mu.Lock()
	defer a.runtimeCaches.upstream.mu.Unlock()
	if a.runtimeCaches.upstream.loaded {
		return cloneUpstreamConfig(a.runtimeCaches.upstream.cfg), nil
	}
	cfg, err := a.loadUpstreamFromDB()
	if err != nil {
		return cfg, err
	}
	a.runtimeCaches.upstream.cfg = cloneUpstreamConfig(cfg)
	a.runtimeCaches.upstream.loaded = true
	return cloneUpstreamConfig(cfg), nil
}

func (a *App) loadUpstreamFromDB() (upstreamConfig, error) {
	// The lane endpoint and credential are constants; only the optional vision
	// supplier and the refresh bookkeeping come from the database.
	cfg := upstreamConfig{BaseURL: laneBaseURL(), APIKey: zenAnonymousKey, VisionUseProxy: true}
	var visionBaseURL, visionModel, encryptedVisionKey string
	if visionErr := a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_vision_base_url'").Scan(&visionBaseURL); visionErr != nil && !errors.Is(visionErr, sql.ErrNoRows) {
		return cfg, visionErr
	}
	if visionErr := a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_vision_api_key'").Scan(&encryptedVisionKey); visionErr != nil && !errors.Is(visionErr, sql.ErrNoRows) {
		return cfg, visionErr
	}
	visionKey := ""
	if encryptedVisionKey != "" {
		var decryptErr error
		visionKey, decryptErr = a.decrypt(encryptedVisionKey)
		if decryptErr != nil {
			return cfg, fmt.Errorf("could not decrypt vision API key")
		}
		visionKey = normalizeAPIKey(visionKey)
	}
	if visionErr := a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_vision_model'").Scan(&visionModel); visionErr != nil && !errors.Is(visionErr, sql.ErrNoRows) {
		return cfg, visionErr
	}
	var visionUseProxyRaw string
	if visionErr := a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_vision_use_proxy'").Scan(&visionUseProxyRaw); visionErr != nil && !errors.Is(visionErr, sql.ErrNoRows) {
		return cfg, visionErr
	}
	if visionUseProxyRaw != "" {
		if visionUseProxy, parseErr := strconv.ParseBool(visionUseProxyRaw); parseErr == nil {
			cfg.VisionUseProxy = visionUseProxy
		}
	}
	var lr, le string
	if refreshErr := a.db.QueryRow("SELECT value FROM settings WHERE key='last_model_refresh_at'").Scan(&lr); refreshErr != nil && !errors.Is(refreshErr, sql.ErrNoRows) {
		return cfg, refreshErr
	}
	if refreshErr := a.db.QueryRow("SELECT value FROM settings WHERE key='last_model_refresh_error'").Scan(&le); refreshErr != nil && !errors.Is(refreshErr, sql.ErrNoRows) {
		return cfg, refreshErr
	}
	var t *time.Time
	if parsed, x := time.Parse(time.RFC3339, lr); x == nil {
		t = &parsed
	}
	cfg.VisionBaseURL = strings.TrimRight(strings.TrimSpace(visionBaseURL), "/")
	cfg.VisionAPIKey = visionKey
	cfg.VisionModel = strings.TrimSpace(visionModel)
	cfg.LastRefresh = t
	cfg.LastError = le
	return cfg, nil
}

func normalizeAPIKey(raw string) string {
	key := strings.TrimSpace(raw)
	if len(key) >= 2 && ((key[0] == '"' && key[len(key)-1] == '"') || (key[0] == '\'' && key[len(key)-1] == '\'')) {
		key = strings.TrimSpace(key[1 : len(key)-1])
	}
	if len(key) >= 7 && strings.EqualFold(key[:7], "bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	return key
}

func validateUpstreamBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return "", errors.New("invalid base_url")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("base_url must use http or https")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("base_url must not contain credentials, query parameters, or a fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func (a *App) refreshModels(w http.ResponseWriter, r *http.Request) {
	cfg, e := a.loadUpstream()
	if e != nil || cfg.BaseURL == "" {
		writeJSON(w, 400, map[string]string{"error": "configure upstream first"})
		return
	}
	req, requestErr := http.NewRequestWithContext(r.Context(), "GET", upstreamEndpoint(cfg.BaseURL, "/models"), nil)
	if requestErr != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid upstream endpoint"})
		return
	}
	applyLaneHeaders(req, a.catalogSession())
	p, engine, proxyErr := a.controlPlaneProxy()
	if proxyErr != nil {
		a.saveRefreshError(proxyErr.Error())
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": proxyErr.Error()})
		return
	}
	client, clientErr := a.httpClient(p)
	if clientErr != nil {
		a.saveRefreshError(clientErr.Error())
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": clientErr.Error()})
		return
	}
	resp, e := client.Do(req)
	if e != nil {
		if engine == proxyEngineResin {
			a.resinFailure(e)
		}
		a.saveRefreshError(e.Error())
		writeJSON(w, 502, map[string]string{"error": e.Error()})
		return
	}
	if engine == proxyEngineResin {
		a.resinSuccess()
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		a.saveRefreshError(string(b))
		writeJSON(w, 502, map[string]string{"error": "upstream returned " + resp.Status})
		return
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if e = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&payload); e != nil {
		a.saveRefreshError(e.Error())
		writeJSON(w, 502, map[string]string{"error": "invalid models response"})
		return
	}
	now := time.Now().UTC()
	tx, e := a.db.Begin()
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": e.Error()})
		return
	}
	forceFreeModels, e := a.forceFreeModelSet(tx)
	if e != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": "could not load model overrides"})
		return
	}
	free := 0
	seenModels := []string{}
	for _, m := range payload.Data {
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		seenModels = append(seenModels, id)
		raw, _ := json.Marshal(m)
		display := id
		if v, ok := m["name"].(string); ok && v != "" {
			display = v
		}
		pricing, _ := json.Marshal(m["pricing"])
		isFree, reason := classifyFree(id, m)
		if _, forced := forceFreeModels[id]; forced {
			isFree, reason = true, "manual_override"
		}
		if isFree {
			free++
		}
		if _, e = tx.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,pricing_metadata,raw_metadata,refreshed_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(model_id) DO UPDATE SET display_name=excluded.display_name,is_free=excluded.is_free,free_reason=excluded.free_reason,pricing_metadata=excluded.pricing_metadata,raw_metadata=excluded.raw_metadata,refreshed_at=excluded.refreshed_at", id, display, boolInt(isFree), reason, string(pricing), string(raw), now.Format(time.RFC3339)); e != nil {
			_ = tx.Rollback()
			writeJSON(w, 500, map[string]string{"error": e.Error()})
			return
		}
	}
	if free == 0 {
		_ = tx.Rollback()
		a.saveRefreshError("no free models detected")
		writeJSON(w, 200, map[string]any{"ok": true, "free_model_count": 0, "warning": "no free models detected; previous cache was kept"})
		return
	}
	placeholders := make([]string, len(seenModels))
	args := make([]any, len(seenModels))
	for i, modelID := range seenModels {
		placeholders[i] = "?"
		args[i] = modelID
	}
	if _, e = tx.Exec("DELETE FROM models WHERE model_id NOT IN ("+strings.Join(placeholders, ",")+")", args...); e != nil {
		_ = tx.Rollback()
		writeJSON(w, 500, map[string]string{"error": "could not replace model cache"})
		return
	}
	if e = tx.Commit(); e != nil {
		writeJSON(w, 500, map[string]string{"error": e.Error()})
		return
	}
	if _, e = a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('last_model_refresh_at',?),('last_model_refresh_error','')", now.Format(time.RFC3339)); e != nil {
		writeJSON(w, 500, map[string]string{"error": "models were refreshed but refresh metadata could not be saved"})
		return
	}
	a.invalidateUpstreamCache()
	a.invalidateModelRuntimeCache()
	writeJSON(w, 200, map[string]any{"ok": true, "free_model_count": free})
}

// catalogSession is the session id the gateway presents when it reads the
// upstream catalog or probes the lane. Catalog traffic is not a conversation,
// so it gets one stable id rather than one per request.
func (a *App) catalogSession() string {
	return a.laneSessionID("gateway\x00catalog")
}

func (a *App) testUpstream(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.loadUpstream()
	if err != nil || cfg.BaseURL == "" {
		writeJSON(w, 400, map[string]string{"error": "configure upstream first"})
		return
	}
	var model string
	if err := a.db.QueryRow("SELECT model_id FROM models WHERE is_free=1 ORDER BY model_id LIMIT 1").Scan(&model); err != nil || model == "" {
		writeJSON(w, 400, map[string]string{"error": "refresh a free model first"})
		return
	}
	// The lane only admits an agent-shaped streaming request, so the probe has to
	// look like one: a bare non-streaming ping is refused with FreeTierError even
	// from a perfectly good exit.
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     true,
	})
	body = ensureFreeLaneShape(body)
	result := map[string]any{"model": model}
	result["direct"] = a.testUpstreamRequest(r.Context(), cfg, body, nil)
	p, engine, proxyErr := a.controlPlaneProxy()
	if proxyErr != nil {
		result["proxy"] = map[string]any{"status": "not_available", "message": proxyErr.Error()}
		writeJSON(w, http.StatusOK, result)
		return
	}
	proxyResult := a.testUpstreamRequest(r.Context(), cfg, body, &p)
	proxyResult["engine"] = engine
	result["proxy"] = proxyResult
	writeJSON(w, 200, result)
}

func (a *App) testUpstreamRequest(ctx context.Context, cfg upstreamConfig, body []byte, p *ProxyRecord) map[string]any {
	var resp *http.Response
	var err error
	if p == nil {
		req, requestErr := http.NewRequestWithContext(ctx, "POST", upstreamEndpoint(cfg.BaseURL, "/chat/completions"), strings.NewReader(string(body)))
		if requestErr != nil {
			err = requestErr
		} else {
			applyLaneHeaders(req, a.catalogSession())
			resp, err = (&http.Client{Timeout: 90 * time.Second}).Do(req)
		}
	} else {
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://relaydesk.invalid", nil)
		resp, err = a.forward(req, body, cfg, *p, a.catalogSession())
	}
	result := map[string]any{}
	if p != nil {
		result["proxy_uri"] = p.URI
	}
	if err != nil {
		result["status"] = "network_error"
		result["message"] = truncateError(err.Error())
		return result
	}
	defer resp.Body.Close()
	captured, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	result["status_code"] = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		result["status"] = "ok"
		return result
	}
	result["status"] = "rejected"
	if summary := upstreamErrorSummary(captured); summary != "" {
		result["message"] = summary
	}
	return result
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func classifyFree(id string, m map[string]any) (bool, string) {
	if modelDeprecated(m) {
		// A retired model is never a usable target, however it is priced.
		return false, ""
	}
	lowerID := strings.ToLower(id)
	suffix := strings.HasSuffix(lowerID, ":free") || strings.HasSuffix(lowerID, "-free")
	pricingZero := false
	if p, ok := m["pricing"].(map[string]any); ok {
		in := priceZero(p["prompt"])
		if !in {
			in = priceZero(p["input"])
		}
		out := priceZero(p["completion"])
		if !out {
			out = priceZero(p["output"])
		}
		pricingZero = in && out
	}
	if suffix && pricingZero {
		return true, "id_suffix_and_pricing_zero"
	}
	if suffix {
		return true, "id_suffix"
	}
	if pricingZero {
		return true, "pricing_zero"
	}
	return false, ""
}

// modelDeprecated reports whether the upstream marks the model as retired. A
// deprecated model keeps its zero pricing, so without this check it would be
// advertised as free and then fail every request routed to it.
func modelDeprecated(m map[string]any) bool {
	if value, ok := m["deprecated"]; ok {
		switch typed := value.(type) {
		case bool:
			return typed
		case string:
			return strings.EqualFold(strings.TrimSpace(typed), "true")
		}
	}
	for _, key := range []string{"status", "state", "lifecycle"} {
		text, ok := m[key].(string)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "deprecated", "retired", "sunset", "disabled", "removed":
			return true
		}
	}
	return false
}

func priceZero(v any) bool {
	switch x := v.(type) {
	case float64:
		return x == 0
	case int:
		return x == 0
	case string:
		s := strings.TrimSpace(strings.TrimPrefix(x, "$"))
		f, e := strconv.ParseFloat(s, 64)
		return e == nil && f == 0
	}
	return false
}
func (a *App) saveRefreshError(s string) {
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('last_model_refresh_error',?)", s); err != nil {
		log.Printf("save model refresh error failed: %v", err)
		return
	}
	a.invalidateUpstreamCache()
	if strings.TrimSpace(s) != "" {
		a.emitAlert("model_refresh_failed", "error", "Model refresh failed", map[string]any{"error": truncateError(s)})
	}
}
func (a *App) listModels(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query("SELECT m.id,m.model_id,m.display_name,m.is_free,m.free_reason,m.pricing_metadata,m.raw_metadata,COALESCE(p.enabled,1),m.refreshed_at FROM models m LEFT JOIN model_policies p ON p.model_id=m.model_id ORDER BY m.model_id")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query models"})
		return
	}
	defer rows.Close()
	models, err := scanModels(rows)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read models"})
		return
	}
	writeJSON(w, 200, models)
}
func (a *App) listFreeModels(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query("SELECT m.id,m.model_id,m.display_name,m.is_free,m.free_reason,m.pricing_metadata,m.raw_metadata,COALESCE(p.enabled,1),m.refreshed_at FROM models m LEFT JOIN model_policies p ON p.model_id=m.model_id WHERE m.is_free=1 ORDER BY m.model_id")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query models"})
		return
	}
	defer rows.Close()
	models, err := scanModels(rows)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read models"})
		return
	}
	writeJSON(w, 200, models)
}
func scanModels(rows *sql.Rows) ([]ModelRecord, error) {
	out := []ModelRecord{}
	for rows.Next() {
		var x ModelRecord
		var free, enabled int
		var p, raw, ts string
		if err := rows.Scan(&x.ID, &x.ModelID, &x.DisplayName, &free, &x.FreeReason, &p, &raw, &enabled, &ts); err != nil {
			return nil, err
		}
		x.IsFree = free == 1
		x.AdminEnabled = enabled == 1
		x.Pricing = json.RawMessage(p)
		x.Raw = json.RawMessage(raw)
		x.RefreshedAt, _ = time.Parse(time.RFC3339, ts)
		out = append(out, x)
	}
	return out, rows.Err()
}

func (a *App) gatewayModels(w http.ResponseWriter, r *http.Request) {
	if !a.validClient(r) {
		writeJSON(w, 401, map[string]string{"error": "invalid client key"})
		return
	}
	models, aliases, err := a.loadModelRuntime()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query models"})
		return
	}
	data := []map[string]any{}
	for _, runtimeModel := range modelRuntimeList(models) {
		model := runtimeModel.Record
		item := map[string]any{"id": model.ModelID, "object": "model", "owned_by": "opencode-proxy", "name": model.DisplayName}
		var metadata map[string]any
		if json.Unmarshal(model.Raw, &metadata) == nil {
			for _, field := range []string{"architecture", "input_modalities", "output_modalities", "supported_parameters"} {
				if value, ok := metadata[field]; ok {
					item[field] = value
				}
			}
		}
		data = append(data, item)
	}
	aliasNames := make([]string, 0, len(aliases))
	for alias := range aliases {
		aliasNames = append(aliasNames, alias)
	}
	sort.Strings(aliasNames)
	for _, alias := range aliasNames {
		target := aliases[alias]
		if model, ok := models[target]; ok && model.Record.AdminEnabled {
			data = append(data, map[string]any{"id": alias, "object": "model", "owned_by": "relaydesk-alias", "name": model.Record.DisplayName + " -> " + target})
		}
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func upstreamEndpoint(base, endpoint string) string {
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return ""
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	parsed.Path = path + "/" + strings.TrimLeft(endpoint, "/")
	return parsed.String()
}
func clientCredential(r *http.Request) string {
	supplied := ""
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		supplied = auth[7:]
	}
	if supplied == "" {
		// OpenAI-compatible clients are not fully consistent: some use
		// x-api-key/api-key instead of Authorization Bearer.
		for _, name := range []string{"X-API-Key", "API-Key"} {
			if value := r.Header.Get(name); value != "" {
				supplied = value
				break
			}
		}
	}
	supplied = strings.TrimSpace(supplied)
	if len(supplied) >= 2 && ((supplied[0] == '"' && supplied[len(supplied)-1] == '"') || (supplied[0] == '\'' && supplied[len(supplied)-1] == '\'')) {
		supplied = strings.TrimSpace(supplied[1 : len(supplied)-1])
	}
	return supplied
}

func usageClientUserAgent(r *http.Request) string {
	return strings.TrimSpace(r.UserAgent())
}

// sessionKey derives the rotation affinity of a request. An explicit session
// header wins, then the OpenAI user field, and finally the opening user message:
// without that last fallback a client that sends neither gets no stickiness at
// all, so every request lands on a different exit and the upstream prompt cache
// has to be rebuilt each time.
func sessionKey(r *http.Request, user json.RawMessage, messages []any) string {
	sessionID := ""
	for _, header := range []string{"X-Relay-Session-ID", "X-OpenCode-Session-ID", "X-Session-ID"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			sessionID = value
			break
		}
	}
	if sessionID == "" && len(user) > 0 {
		_ = json.Unmarshal(user, &sessionID)
		sessionID = strings.TrimSpace(sessionID)
	}
	if sessionID == "" {
		sessionID = firstUserMessageFingerprint(messages)
	}
	if sessionID == "" {
		return ""
	}
	clientID := hashToken(clientCredential(r))
	return hashToken(clientID + "\x00" + sessionID)
}

// firstUserMessageFingerprint identifies a conversation by its opening user
// message. That message stays the same on every later turn, so it works as a
// session key when the client sends no identifier of its own. Only a conversation
// that already has a second user turn qualifies: a one-shot prompt would
// otherwise create one session route row per request and never reuse it.
func firstUserMessageFingerprint(messages []any) string {
	userTurns := 0
	opening := ""
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		userTurns++
		if opening == "" {
			opening = messageText(message["content"])
		}
	}
	if userTurns < 2 || opening == "" {
		return ""
	}
	if len(opening) > 512 {
		opening = opening[:512]
	}
	return "message:" + hashToken(opening)
}

// messageText flattens message content to its text parts. Image parts are
// skipped so a conversation that opens with a picture still keys on its words.
func messageText(content any) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		parts := make([]string, 0, len(value))
		for _, part := range value {
			item, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if kind, _ := item["type"].(string); kind != "" && kind != "text" {
				continue
			}
			if text, ok := item["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

func (a *App) gatewayChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	clientKey, authErr := a.authenticateClient(r)
	if authErr != nil {
		traceSet(r, "stage", "client_auth")
		traceSet(r, "error", clientKeyError(authErr))
		writeJSON(w, 401, map[string]string{"error": clientKeyError(authErr)})
		return
	}
	traceSet(r, "client_key_id", clientKey.ID)
	traceSet(r, "client_key_name", clientKey.Name)
	if !a.enforceClientLimit(w, clientKey) {
		traceSet(r, "stage", "client_limit")
		traceSet(r, "error", "client key rate or token limit reached")
		return
	}
	a.touchClientKey(clientKey)
	select {
	case a.gatewaySem <- struct{}{}:
		defer func() { <-a.gatewaySem }()
	default:
		traceSet(r, "stage", "gateway_capacity")
		traceSet(r, "error", "gateway is at capacity")
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "gateway is at capacity"})
		return
	}
	body, err := readLimitedBody(r.Body, 16<<20)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "invalid or oversized body"})
		return
	}
	parsed, requestPayload, parseErr := parseChatEnvelope(body)
	if parseErr != nil || parsed.Model == "" {
		traceSet(r, "stage", "parse")
		traceSet(r, "error", "model is required")
		writeJSON(w, 400, map[string]string{"error": "model is required"})
		return
	}
	clientUserAgent := usageClientUserAgent(r)
	suppressReasoning := requestDisablesThinking(requestPayload)
	requestedModel := parsed.Model
	resolvedModel, resolveErr := a.resolveModel(requestedModel)
	if resolveErr != nil {
		status := http.StatusBadRequest
		if !strings.Contains(resolveErr.Error(), "not an available") && !strings.Contains(resolveErr.Error(), "not currently") {
			status = http.StatusInternalServerError
		}
		traceSet(r, "stage", "resolve_model")
		traceSet(r, "model", requestedModel)
		traceSet(r, "error", resolveErr.Error())
		writeJSON(w, status, map[string]string{"error": resolveErr.Error()})
		return
	}
	traceSet(r, "model", requestedModel)
	traceSet(r, "resolved_model", resolvedModel)
	traceSet(r, "stream", parsed.Stream)
	forwardBody := body
	if resolvedModel != requestedModel {
		requestPayload["model"] = resolvedModel
		var rewriteErr error
		forwardBody, rewriteErr = json.Marshal(requestPayload)
		if rewriteErr != nil {
			writeJSON(w, 400, map[string]string{"error": "could not rewrite model request"})
			return
		}
	}
	// The free lane's agent-shape gate wants the CLI's own tools in the payload;
	// the client's body is extended only when they are missing.
	forwardBody = ensureFreeLaneShape(forwardBody)
	cfg, cfgErr := a.loadUpstream()
	if cfgErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": cfgErr.Error()})
		return
	}
	if cfg.BaseURL == "" {
		writeJSON(w, 503, map[string]string{"error": "upstream is not configured"})
		return
	}
	hasImage := containsImageContentInMessages(parsed.Messages)
	visionEnabled := hasImage && cfg.VisionBaseURL != "" && cfg.VisionModel != ""
	if hasImage && !visionEnabled {
		if known, supportsImage := a.cachedModelSupportsImage(resolvedModel); known && !supportsImage {
			message := fmt.Sprintf("model %q only supports text input; choose a Free model with image support or remove image_url", requestedModel)
			a.recordGatewayUsageWithStream(clientKey, requestedModel, resolvedModel, nil, "", "direct", "error", http.StatusBadRequest, time.Since(start), nil, 0, nil, nil, clientUserAgent, parsed.Stream, errors.New(message))
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": message,
				"code":  "unsupported_input_modality",
			})
			return
		}
	}
	engineConfig, err := a.loadProxyEngine()
	if err != nil {
		traceSet(r, "stage", "proxy_engine")
		traceSet(r, "error", "could not load proxy engine")
		writeJSON(w, 500, map[string]string{"error": "could not load proxy engine"})
		return
	}
	routeEngine := engineConfig.Engine
	traceSet(r, "route_engine", routeEngine)
	resinMode := routeEngine == proxyEngineResin
	proxies := []ProxyRecord{}
	if resinMode {
		if _, err := validateResinGatewayURL(engineConfig.ResinGatewayURL); err != nil {
			writeJSON(w, 503, map[string]string{"error": "Resin gateway is not configured"})
			return
		}
		if _, err := validateResinPlatform(engineConfig.ResinPlatform); err != nil {
			writeJSON(w, 503, map[string]string{"error": "Resin platform is not configured"})
			return
		}
	} else {
		proxies, err = a.availableProxies()
		if err != nil {
			a.recordGatewayUsageWithOriginAndStream(clientKey, requestedModel, resolvedModel, nil, "", "builtin", "error", http.StatusInternalServerError, time.Since(start), nil, 0, nil, nil, clientUserAgent, parsed.Stream, errors.New("could not load proxies"), "internal")
			writeJSON(w, 500, map[string]string{"error": "could not load proxies"})
			return
		}
		if len(proxies) == 0 {
			a.recordGatewayUsageWithOriginAndStream(clientKey, requestedModel, resolvedModel, nil, "", "builtin", "error", http.StatusServiceUnavailable, time.Since(start), nil, 0, nil, nil, clientUserAgent, parsed.Stream, errors.New("no healthy proxies available"), "internal")
			traceSet(r, "stage", "proxy_pool")
			traceSet(r, "error", "no healthy proxies available")
			writeJSON(w, 503, map[string]string{"error": "no healthy proxies available"})
			return
		}
	}
	traceSet(r, "proxy_count", len(proxies))
	requestSessionKey := sessionKey(r, parsed.User, parsed.Messages)
	// The upstream session id is a separate identity from the rotation affinity:
	// it must always exist (the free lane rejects a request without a canonical
	// ses_ id) and it must survive the whole conversation so upstream keeps its
	// prompt cache and per-session quota accounting.
	laneSession := a.laneSessionID(laneSessionSignal(r, parsed.User, parsed.Messages, hashToken(clientCredential(r))))
	// The compression gateway folds already-consumed history into one summary
	// message. It runs on the parsed payload so the rewritten body keeps the
	// model rewrite, and the free-lane shape is re-applied afterwards.
	compression := a.applyCompression(r, requestSessionKey, resolvedModel, requestPayload)
	if compression.Applied {
		if rewritten, rewriteErr := json.Marshal(requestPayload); rewriteErr == nil {
			forwardBody = ensureFreeLaneShape(rewritten)
			if messages, ok := requestPayload["messages"].([]any); ok {
				parsed.Messages = messages
			}
		}
	}
	setCompressionHeaders(w, compression)
	traceSet(r, "compression_applied", compression.Applied)
	traceSet(r, "compression_blocking", compression.Blocking)
	// The lane rejects a non-streaming request, so the upstream always sees
	// stream:true. A client that asked for one JSON object gets one folded back
	// out of the stream below, so the requirement never reaches the client.
	forwardBody, clientStreaming := forceLaneStream(forwardBody)
	if compression.Blocking {
		detail := "compression gateway failed"
		if compression.Err != nil {
			detail += ": " + truncateError(compression.Err.Error())
		}
		a.recordGatewayUsageWithStream(clientKey, requestedModel, resolvedModel, nil, "", "direct", "error", http.StatusBadGateway, time.Since(start), nil, 0, nil, nil, clientUserAgent, parsed.Stream, errors.New(detail))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": detail, "code": "compression_failed"})
		return
	}
	var resinRoute *resinRequestRoute
	if resinMode {
		resinRoute, err = newResinRequestRoute(requestSessionKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not initialize Resin account routing"})
			return
		}
	}
	attempts := 3
	if resinMode {
		attempts = resinMaxAttempts
	} else if len(proxies) < attempts {
		attempts = len(proxies)
	}
	var lastErr error
	var downstreamErr error
	lastProxyURI := ""
	retryCount := 0
	used := map[int64]struct{}{}
	// A ban is a refusal of one exit and model pairing, not of the exit, so the
	// banned set is loaded per request and only used to order candidates.
	banned := a.bannedProxyIDs(resolvedModel)
	attemptSummary := []map[string]any{}
	// One request may keep drawing fresh exits while the pool answers 429, but
	// only for a bounded budget: past it the caller is told to come back later.
	// The breaker covers the other failure mode — a pool so saturated that
	// retrying would lengthen the limit instead of escaping it.
	resinBudget := gatewayRetryBudget
	if resinMode {
		resinBudget = a.resinRetryBudget()
		if a.resinBreakerOpenNow() {
			// Answer at once with the same 429 the budget would eventually
			// produce, so clients back off instead of holding a connection and
			// feeding the storm with retries of their own.
			a.recordGatewayUsageWithStream(clientKey, requestedModel, resolvedModel, nil, "", routeEngine, "error", http.StatusTooManyRequests, time.Since(start), nil, 0, nil, attemptSummary, clientUserAgent, parsed.Stream, errors.New("resin breaker open: pool saturated with 429"))
			traceSet(r, "stage", "resin_breaker_open")
			traceSet(r, "status_code", http.StatusTooManyRequests)
			traceSet(r, "error_origin", "upstream")
			waitSeconds := int(a.resinBreakerRemaining().Round(time.Second) / time.Second)
			if waitSeconds < 1 {
				waitSeconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(waitSeconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "upstream rate limited", "detail": "the exit pool is saturated; retry shortly"})
			return
		}
	}
	for i := 0; i < attempts; i++ {
		if requestErr := r.Context().Err(); requestErr != nil {
			downstreamErr = requestErr
			lastErr = requestErr
			break
		}
		var p ProxyRecord
		if resinMode {
			p, err = resinRoute.proxy(a, engineConfig)
			if err != nil {
				lastErr = err
				break
			}
		} else {
			var ok bool
			p, ok, err = a.pickSessionProxy(proxyPick{proxies: proxies, sessionKey: requestSessionKey, model: resolvedModel, used: used, banned: banned})
			if err != nil {
				lastErr = err
				break
			}
			if !ok {
				break
			}
			used[p.ID] = struct{}{}
		}
		retryCount = i
		lastProxyURI = p.URI
		traceNote(r, "attempt %d/%d engine=%s proxy=%s", i+1, attempts, routeEngine, redactProxyURI(p.URI))
		bodyToForward := forwardBody
		if visionEnabled {
			helperStarted := time.Now()
			helperProxy := p
			helperRouteEngine := routeEngine
			var helperProxyID *int64
			if p.ID > 0 {
				helperProxyID = &p.ID
			}
			helperProxyURI := p.URI
			if !cfg.VisionUseProxy {
				helperProxy = ProxyRecord{}
				helperProxyID = nil
				helperProxyURI = ""
				helperRouteEngine = "direct"
			}
			helpBody, buildErr := buildVisionRequestFromMessages(parsed.Messages, cfg.VisionModel)
			if buildErr != nil {
				lastErr = buildErr
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", 0, time.Since(helperStarted), nil, i, nil, buildErr)
				break
			}
			visionCfg := upstreamConfig{VisionBaseURL: cfg.VisionBaseURL, VisionAPIKey: cfg.VisionAPIKey}
			helperCtx, helperCancel := visionRequestContext(r)
			helperRequest := r.Clone(helperCtx)
			helpResp, helpErr := a.forwardVision(helperRequest, helpBody, visionCfg, helperProxy)
			if helpErr != nil {
				helperCancel()
				lastErr = fmt.Errorf("vision helper request failed: %w", helpErr)
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", 0, time.Since(helperStarted), nil, i, nil, lastErr)
				if cfg.VisionUseProxy {
					if resinMode {
						a.resinFailure(helpErr)
						// The helper failed on the account, not on the model, so
						// rotate it and let the remaining attempts try a fresh
						// exit instead of failing the whole request here.
						if advanceErr := resinRoute.advance(a); advanceErr != nil {
							log.Printf("advance Resin account failed: %v", advanceErr)
						}
						if i+1 < attempts && time.Since(start) < gatewayRetryBudget {
							continue
						}
						break
					}
					if isProxyTransportError(helpErr) {
						a.markProxyFailure(p.ID)
					}
					a.clearSessionProxy(requestSessionKey, p.ID)
					continue
				}
				break
			}
			var helpFirstToken *time.Duration
			helpReader := io.Reader(io.LimitReader(helpResp.Body, 4<<20))
			if helpResp.StatusCode < 300 {
				helpReader = &firstByteReader{reader: helpReader, onFirstByte: func() {
					latency := time.Since(helperStarted)
					helpFirstToken = &latency
				}}
			}
			helpCaptured, readErr := io.ReadAll(helpReader)
			helpTokens := parseUsageBytes(helpCaptured)
			helpStatus := helpResp.StatusCode
			_ = helpResp.Body.Close()
			helperCancel()
			if readErr != nil {
				lastErr = fmt.Errorf("vision helper response failed: %w", readErr)
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", helpStatus, time.Since(helperStarted), nil, i, helpTokens, lastErr)
				if cfg.VisionUseProxy {
					if !resinMode && isProxyTransportError(readErr) {
						a.markProxyFailure(p.ID)
						a.clearSessionProxy(requestSessionKey, p.ID)
					}
					continue
				}
				break
			}
			if helpStatus >= 300 {
				detail := upstreamErrorSummary(helpCaptured)
				if detail == "" {
					detail = fmt.Sprintf("upstream returned HTTP %d", helpStatus)
				}
				lastErr = fmt.Errorf("vision helper failed: %s", detail)
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", helpStatus, time.Since(helperStarted), nil, i, helpTokens, lastErr)
				if cfg.VisionUseProxy && i+1 < attempts && (helpStatus == http.StatusTooManyRequests || helpStatus == http.StatusUnauthorized || helpStatus == http.StatusForbidden) {
					if resinMode {
						if helpStatus == http.StatusTooManyRequests {
							a.coolResinAccount(resinRoute.lastAccount, resinAccountCooldownFor(helpResp))
							a.noteResinLimited()
						}
						_ = resinRoute.advance(a)
					} else {
						if classifyUpstreamOutcome(helpStatus) == upstreamOutcomeRefused {
							a.markModelRefusal(p.ID, resolvedModel)
							banned = a.bannedProxyIDs(resolvedModel)
						} else {
							a.markProxyLimited(p.ID, retryAfterDuration(helpResp))
						}
						a.clearSessionProxy(requestSessionKey, p.ID)
					}
					continue
				}
				break
			}
			description, extractErr := extractVisionDescription(helpCaptured)
			if extractErr != nil {
				lastErr = extractErr
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", helpStatus, time.Since(helperStarted), nil, i, helpTokens, lastErr)
				break
			}
			workingPayload, cloneErr := cloneJSONValue(requestPayload).(map[string]any)
			if !cloneErr {
				buildErr = errors.New("could not clone image request")
			} else {
				bodyToForward, buildErr = replaceImageContentPayload(workingPayload, description)
			}
			if buildErr != nil {
				lastErr = buildErr
				a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "error", helpStatus, time.Since(helperStarted), nil, i, helpTokens, lastErr)
				break
			}
			a.recordUsageKindWithEngine("vision_helper", cfg.VisionModel, helperProxyID, helperProxyURI, helperRouteEngine, "success", helpStatus, time.Since(helperStarted), helpFirstToken, i, helpTokens, nil)
		}
		resp, e := a.forward(r, bodyToForward, cfg, p, laneSession)
		if e != nil {
			lastErr = e
			traceNote(r, "attempt %d transport error: %s", i+1, truncateError(e.Error()))
			attemptSummary = append(attemptSummary, map[string]any{"engine": routeEngine, "proxy": p.URI, "error": truncateError(e.Error()), "latency_ms": time.Since(start).Milliseconds()})
			if requestErr := r.Context().Err(); requestErr != nil {
				downstreamErr = requestErr
				lastErr = requestErr
				break
			}
			if resinMode {
				a.resinFailure(e)
				// The account that lost the link must also stop being sticky:
				// rotate it, pace the loop and let the remaining attempts use a
				// fresh exit. Breaking here turned a transient blip on one
				// account into a client-visible 502 while most of the attempt
				// budget was still unspent.
				if advanceErr := resinRoute.advance(a); advanceErr != nil {
					log.Printf("advance Resin account failed: %v", advanceErr)
				}
				if i+1 >= attempts || time.Since(start) >= gatewayRetryBudget {
					break
				}
				if !waitForRetry(r.Context(), retryAfterDelay(nil, i)) {
					downstreamErr = r.Context().Err()
					lastErr = downstreamErr
					break
				}
				continue
			}
			if isProxyTransportError(e) {
				a.markProxyFailure(p.ID)
			}
			a.clearSessionProxy(requestSessionKey, p.ID)
			continue
		}
		if resinMode {
			a.resinSuccess()
			if resp.StatusCode < http.StatusBadRequest {
				// The exit answered: whatever it was parked for is over, and one
				// success proves the pool is moving again.
				a.releaseResinAccount(resinRoute.lastAccount)
				a.noteResinHealthy()
			}
		} else {
			switch classifyUpstreamOutcome(resp.StatusCode) {
			case upstreamOutcomeSuccess:
				a.markProxySuccess(p.ID)
			case upstreamOutcomeLimited:
				// The per-IP quota is spent, so the whole exit cools — honouring
				// the wait the upstream asked for when it is longer than ours.
				a.markProxyLimited(p.ID, retryAfterDuration(resp))
				a.clearSessionProxy(requestSessionKey, p.ID)
			case upstreamOutcomeRefused:
				// 401/403 refuse this exit for this model only. The link itself
				// is fine, so the exit stays in rotation for every other model.
				a.markModelRefusal(p.ID, resolvedModel)
				banned = a.bannedProxyIDs(resolvedModel)
				a.clearSessionProxy(requestSessionKey, p.ID)
			case upstreamOutcomeProxyAuth:
				a.markProxyFailure(p.ID)
				a.clearSessionProxy(requestSessionKey, p.ID)
			case upstreamOutcomeServerError:
				// A 5xx is usually the model failing behind a healthy exit. Leave
				// the exit healthy, but drop the sticky route so the retry may
				// land on a different exit.
				a.clearSessionProxy(requestSessionKey, p.ID)
			}
		}
		traceNote(r, "attempt %d upstream status %d verdict=%s", i+1, resp.StatusCode, upstreamOutcomeName(classifyUpstreamOutcome(resp.StatusCode)))
		if resp.StatusCode == http.StatusProxyAuthRequired && !resinMode {
			attemptSummary = append(attemptSummary, map[string]any{"engine": routeEngine, "proxy": p.URI, "status_code": resp.StatusCode, "error": "proxy authentication failed", "latency_ms": time.Since(start).Milliseconds()})
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			lastErr = errors.New("proxy authentication failed")
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests && resinMode {
			// Park the exit that answered 429 before rotating: the next draw must
			// not land on it again, and neither must the next request.
			a.coolResinAccount(resinRoute.lastAccount, resinAccountCooldownFor(resp))
			a.noteResinLimited()
			if advanceErr := resinRoute.advance(a); advanceErr != nil {
				log.Printf("advance Resin account failed: %v", advanceErr)
			}
			// Rotating an account is cheap; keeping a caller waiting through ten
			// back-to-back 429s is not. The request keeps trying until its own
			// budget runs out — the intermediate 429s are ours to absorb — and
			// only then relays the upstream's 429 so the caller can back off.
			if i+1 < attempts && time.Since(start) < resinBudget {
				attemptSummary = append(attemptSummary, map[string]any{"engine": routeEngine, "proxy": p.URI, "status_code": resp.StatusCode, "latency_ms": time.Since(start).Milliseconds()})
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				// A 429 request never waited at all before, so ten rotations ran
				// back-to-back and hammered the exit the gateway had just left.
				if !waitForRetry(r.Context(), retryAfterDelay(resp, i)) {
					downstreamErr = r.Context().Err()
					lastErr = downstreamErr
					break
				}
				continue
			}
		}
		if shouldRotateUpstream(resp.StatusCode) && i+1 < attempts && !resinMode {
			attemptSummary = append(attemptSummary, map[string]any{"engine": routeEngine, "proxy": p.URI, "status_code": resp.StatusCode, "latency_ms": time.Since(start).Milliseconds()})
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if !waitForRetry(r.Context(), retryAfterDelay(resp, i)) {
				downstreamErr = r.Context().Err()
				lastErr = downstreamErr
				break
			}
			continue
		}
		tokens, upstreamError, firstTokenLatency, copyErr := a.relayUpstreamResponse(w, resp, start, suppressReasoning, !clientStreaming)
		if copyErr != nil && !resinMode && r.Context().Err() == nil && isProxyTransportError(copyErr) {
			// The stream broke mid-flight. It is never replayed — the client has
			// already received bytes — but the exit earns a transport strike so
			// the next request prefers a different one.
			a.markProxyFailure(p.ID)
			a.clearSessionProxy(requestSessionKey, p.ID)
		}
		var proxyID *int64
		if p.ID > 0 {
			proxyID = &p.ID
		}
		status := "success"
		if resp.StatusCode >= 400 || copyErr != nil {
			status = "error"
			firstTokenLatency = nil
		}
		var requestError error
		if upstreamError != "" {
			requestError = errors.New(upstreamError)
		}
		if copyErr != nil {
			requestError = errors.Join(requestError, copyErr)
		}
		attemptSummary = append(attemptSummary, map[string]any{"engine": routeEngine, "proxy": p.URI, "status_code": resp.StatusCode, "latency_ms": time.Since(start).Milliseconds()})
		a.recordGatewayUsageWithStream(clientKey, requestedModel, resolvedModel, proxyID, p.URI, routeEngine, status, resp.StatusCode, time.Since(start), firstTokenLatency, i, tokens, attemptSummary, clientUserAgent, parsed.Stream, requestError)
		traceSet(r, "status_code", resp.StatusCode)
		traceSet(r, "attempts", i+1)
		if firstTokenLatency != nil {
			traceSet(r, "first_token_ms", firstTokenLatency.Milliseconds())
		}
		if requestError != nil {
			traceSet(r, "error", truncateError(requestError.Error()))
		}
		return
	}
	lastErr, finalStatus, origin := terminalGatewayFailure(downstreamErr, lastErr)
	a.recordGatewayUsageWithOriginAndStream(clientKey, requestedModel, resolvedModel, nil, lastProxyURI, routeEngine, "error", finalStatus, time.Since(start), nil, retryCount, nil, attemptSummary, clientUserAgent, parsed.Stream, lastErr, origin)
	traceSet(r, "stage", "all_proxies_failed")
	traceSet(r, "attempts", retryCount+1)
	traceSet(r, "error_origin", origin)
	traceSet(r, "error", lastErrString(lastErr))
	writeJSON(w, finalStatus, map[string]string{"error": "all proxies failed", "detail": lastErrString(lastErr)})
}

func requestDisablesThinking(payload map[string]any) bool {
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		if mode, ok := thinking["type"].(string); ok && strings.EqualFold(strings.TrimSpace(mode), "disabled") {
			return true
		}
	}
	if reasoning, ok := payload["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "none") {
			return true
		}
	}
	return false
}

func terminalGatewayFailure(requestErr, routeErr error) (error, int, string) {
	if requestErr != nil {
		status := statusClientClosedRequest
		if errors.Is(requestErr, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		return requestErr, status, "external"
	}
	return routeErr, http.StatusBadGateway, "internal"
}

func lastErrString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
func (a *App) forward(r *http.Request, body []byte, cfg upstreamConfig, p ProxyRecord, session string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), "POST", upstreamEndpoint(cfg.BaseURL, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyLaneHeaders(req, session)
	client, err := a.httpClient(p)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

// forwardVision sends the image-description request to the operator's own
// multimodal supplier. It is a different upstream with its own credential, so it
// gets neither the Zen endpoint nor the CLI disguise headers.
func (a *App) forwardVision(r *http.Request, body []byte, cfg upstreamConfig, p ProxyRecord) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), "POST", upstreamEndpoint(cfg.VisionBaseURL, "/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.VisionAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.VisionAPIKey)
	}
	client, err := a.httpClient(p)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func visionRequestContext(r *http.Request) (context.Context, context.CancelFunc) {
	// The client can cancel a streamed chat request while the helper is still
	// reading an image. Keep the helper alive briefly, with its own hard limit.
	return context.WithTimeout(context.WithoutCancel(r.Context()), upstreamRequestTimeout)
}

func buildVisionRequest(body []byte, visionModel string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("could not prepare image request: %w", err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("image input requires chat messages")
	}
	return buildVisionRequestFromMessages(messages, visionModel)
}

func buildVisionRequestFromMessages(messages []any, visionModel string) ([]byte, error) {
	if len(messages) == 0 {
		return nil, errors.New("image input requires chat messages")
	}
	helperMessages := make([]any, 0, len(messages)+1)
	helperMessages = append(helperMessages, map[string]any{
		"role":    "system",
		"content": "Describe every image in the conversation in concise, factual text. Include visible text, objects, layout, and relevant details. Return only the description so another text-only model can use it.",
	})
	helperMessages = append(helperMessages, messages...)
	helperPayload := map[string]any{
		"model":      visionModel,
		"messages":   helperMessages,
		"max_tokens": 768,
		"stream":     false,
	}
	return json.Marshal(helperPayload)
}

func cloneJSONValue(value any) any {
	switch item := value.(type) {
	case map[string]any:
		copyMap := make(map[string]any, len(item))
		for key, nested := range item {
			copyMap[key] = cloneJSONValue(nested)
		}
		return copyMap
	case []any:
		copySlice := make([]any, len(item))
		for i, nested := range item {
			copySlice[i] = cloneJSONValue(nested)
		}
		return copySlice
	default:
		return value
	}
}

func extractVisionDescription(body []byte) (string, error) {
	var payload struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
			} `json:"message"`
			Text json.RawMessage `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", errors.New("vision helper returned invalid JSON")
	}
	if len(payload.Choices) == 0 {
		return "", errors.New("vision helper returned no choices")
	}
	choice := payload.Choices[0]
	description := extractTextContent(choice.Message.Content)
	if strings.TrimSpace(description) == "" {
		description = extractTextContent(choice.Message.ReasoningContent)
	}
	if strings.TrimSpace(description) == "" {
		description = extractTextContent(choice.Text)
	}
	if strings.TrimSpace(description) == "" {
		return "", errors.New("vision helper returned an empty description")
	}
	return strings.TrimSpace(description), nil
}

func extractTextContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if value, ok := part["text"].(string); ok && strings.TrimSpace(value) != "" {
			texts = append(texts, value)
		}
	}
	return strings.Join(texts, "\n")
}

func replaceImageContent(body []byte, description string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("could not rewrite image content: %w", err)
	}
	return replaceImageContentPayload(payload, description)
}

func replaceImageContentPayload(payload map[string]any, description string) ([]byte, error) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil, errors.New("image input requires chat messages")
	}
	replaced := false
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"]
		if !ok {
			continue
		}
		rewritten, found := replaceImageContentValue(content, description)
		if !found {
			continue
		}
		message["content"] = rewritten
		replaced = true
	}
	if !replaced {
		return nil, errors.New("could not locate image content in chat messages")
	}
	return json.Marshal(payload)
}

func replaceImageContentValue(content any, description string) (any, bool) {
	if isImageContentPart(content) {
		return imageDescriptionContent(description), true
	}
	parts, ok := content.([]any)
	if !ok {
		return content, false
	}
	rewritten := make([]any, 0, len(parts))
	replaced := false
	for _, part := range parts {
		if isImageContentPart(part) {
			rewritten = append(rewritten, imageDescriptionContent(description))
			replaced = true
			continue
		}
		rewritten = append(rewritten, part)
	}
	return rewritten, replaced
}

func imageDescriptionContent(description string) map[string]any {
	return map[string]any{"type": "text", "text": "[\u56fe\u7247\u5185\u5bb9]\n" + description}
}

func addTokenUsage(first, second *tokenUsage) *tokenUsage {
	if first == nil {
		return second
	}
	if second == nil {
		return first
	}
	return &tokenUsage{
		Prompt:     addTokenValue(first.Prompt, second.Prompt),
		Completion: addTokenValue(first.Completion, second.Completion),
		Total:      addTokenValue(first.Total, second.Total),
	}
}

func addTokenValue(first, second *int64) *int64 {
	if first == nil || second == nil {
		return nil
	}
	total := *first + *second
	return &total
}

func (a *App) httpClient(p ProxyRecord) (*http.Client, error) {
	a.initializeRuntimeServices()
	return a.proxyRuntime.clientFor(a, p)
}

func (a *App) buildHTTPClient(p ProxyRecord) (*http.Client, *http.Transport, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = 256
	tr.MaxIdleConnsPerHost = 32
	tr.IdleConnTimeout = 90 * time.Second
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.ResponseHeaderTimeout = upstreamRequestTimeout
	if strings.TrimSpace(p.URI) == "" {
		tr.Proxy = nil
		return &http.Client{Transport: tr}, tr, nil
	}
	u, err := url.Parse(p.URI)
	if err != nil {
		return nil, nil, err
	}
	if u.Scheme == "socks5" || u.Scheme == "socks5h" {
		var auth *proxy.Auth
		if p.Username != "" {
			auth = &proxy.Auth{User: p.Username, Password: p.Password}
		}
		d, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
		if err != nil {
			return nil, nil, err
		}
		tr.Proxy = nil
		tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if contextDialer, ok := d.(proxy.ContextDialer); ok {
				return contextDialer.DialContext(ctx, network, address)
			}
			return d.Dial(network, address)
		}
	} else {
		if p.Username != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &http.Client{Transport: tr}, tr, nil
}

// copyResponse relays an upstream response unchanged, stream for stream.
func (a *App) copyResponse(w http.ResponseWriter, resp *http.Response, startedAt time.Time, suppressReasoning bool) (*tokenUsage, string, *time.Duration, error) {
	return a.relayUpstreamResponse(w, resp, startedAt, suppressReasoning, false)
}

// relayUpstreamResponse copies an upstream response downstream. When aggregate
// is set and the upstream answers with an event stream, the stream is folded
// into one JSON chat.completion instead of being relayed: the free lane only
// accepts streaming requests, but a non-streaming client must still receive the
// single object it asked for.
func (a *App) relayUpstreamResponse(w http.ResponseWriter, resp *http.Response, startedAt time.Time, suppressReasoning, aggregate bool) (*tokenUsage, string, *time.Duration, error) {
	defer resp.Body.Close()
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	streaming := strings.Contains(contentType, "text/event-stream")
	aggregating := aggregate && streaming
	blocked := make(map[string]struct{}, len(blockedDownstreamHeaders)+4)
	for name := range blockedDownstreamHeaders {
		blocked[name] = struct{}{}
	}
	if aggregating {
		// The framing headers describe the stream, not the folded body.
		blocked["content-type"] = struct{}{}
		blocked["content-length"] = struct{}{}
		blocked["transfer-encoding"] = struct{}{}
	}
	for _, value := range resp.Header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
				blocked[name] = struct{}{}
			}
		}
	}
	for k, v := range resp.Header {
		if _, skip := blocked[strings.ToLower(k)]; skip {
			continue
		}
		for _, x := range v {
			w.Header().Add(k, x)
		}
	}
	if aggregating {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	captured := &limitedCapture{limit: 2 << 20}
	var firstTokenLatency *time.Duration
	markFirstToken := func() {
		if firstTokenLatency != nil || resp.StatusCode >= 400 {
			return
		}
		latency := time.Since(startedAt)
		firstTokenLatency = &latency
	}
	var copyErr error
	if aggregating {
		// The client asked for one object: drain the stream, fold it, and send it.
		// First-token latency cannot be observed inside a single read, so it is
		// reported as the time the whole stream took.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_, _ = captured.Write(body)
		if readErr != nil {
			copyErr = readErr
		} else {
			if resp.StatusCode < 400 && len(body) > 0 {
				markFirstToken()
			}
			if resp.StatusCode < 400 {
				if folded, foldErr := aggregateSSEStream(body); foldErr == nil {
					if suppressReasoning {
						folded = stripReasoningFromChatCompletion(folded)
					}
					body = folded
				}
			}
			_, copyErr = w.Write(body)
		}
	} else if streaming {
		buf := make([]byte, 32*1024)
		flusher, _ := w.(http.Flusher)
		detector := &sseContentDetector{}
		filter := &sseReasoningFilter{enabled: suppressReasoning}
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = captured.Write(buf[:n])
				filtered := filter.Filter(buf[:n])
				if detector.Observe(filtered) {
					markFirstToken()
				}
				if _, writeErr := w.Write(filtered); writeErr != nil {
					copyErr = writeErr
					break
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				copyErr = err
				break
			}
		}
		if copyErr == nil {
			filtered := filter.Flush()
			if detector.Observe(filtered) {
				markFirstToken()
			}
			if len(filtered) > 0 {
				if _, writeErr := w.Write(filtered); writeErr != nil {
					copyErr = writeErr
				}
			}
		}
		if detector.Flush() {
			markFirstToken()
		}
	} else {
		if !suppressReasoning {
			reader := io.Reader(resp.Body)
			if resp.StatusCode < 400 {
				reader = &firstByteReader{reader: reader, onFirstByte: markFirstToken}
			}
			_, copyErr = io.Copy(io.MultiWriter(w, captured), reader)
		} else {
			body, readErr := io.ReadAll(resp.Body)
			_, _ = captured.Write(body)
			if resp.StatusCode < 400 && len(body) > 0 {
				markFirstToken()
			}
			if readErr != nil {
				copyErr = readErr
			} else {
				body = stripReasoningFromChatCompletion(body)
				_, copyErr = w.Write(body)
			}
		}
	}
	var summary string
	if resp.StatusCode >= 400 {
		summary = upstreamErrorSummary(captured.Bytes())
	}
	return parseUsageBytes(captured.Bytes()), summary, firstTokenLatency, copyErr
}

// stripReasoningFromChatCompletion removes upstream thinking traces only for
// clients that explicitly disabled thinking in their request.
func stripReasoningFromChatCompletion(body []byte) []byte {
	var payload any
	if json.Unmarshal(body, &payload) != nil || !stripReasoningFields(payload) {
		return body
	}
	sanitized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return sanitized
}

func stripReasoningFields(value any) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"reasoning_content", "reasoning_details"} {
			if _, found := typed[key]; found {
				delete(typed, key)
				changed = true
			}
		}
		for _, nested := range typed {
			if stripReasoningFields(nested) {
				changed = true
			}
		}
	case []any:
		for _, nested := range typed {
			if stripReasoningFields(nested) {
				changed = true
			}
		}
	}
	return changed
}

type sseReasoningFilter struct {
	pending []byte
	enabled bool
}

func (f *sseReasoningFilter) Filter(chunk []byte) []byte {
	if !f.enabled {
		return chunk
	}
	f.pending = append(f.pending, chunk...)
	var filtered []byte
	for {
		newline := bytes.IndexByte(f.pending, '\n')
		if newline < 0 {
			break
		}
		filtered = append(filtered, filterSSEReasoningLine(f.pending[:newline+1])...)
		f.pending = f.pending[newline+1:]
	}
	return filtered
}

func (f *sseReasoningFilter) Flush() []byte {
	if !f.enabled {
		return nil
	}
	if len(f.pending) == 0 {
		return nil
	}
	filtered := filterSSEReasoningLine(f.pending)
	f.pending = nil
	return filtered
}

func filterSSEReasoningLine(line []byte) []byte {
	ending := []byte{}
	body := line
	if bytes.HasSuffix(body, []byte("\r\n")) {
		ending = []byte("\r\n")
		body = body[:len(body)-2]
	} else if bytes.HasSuffix(body, []byte("\n")) {
		ending = []byte("\n")
		body = body[:len(body)-1]
	}
	trimmed := bytes.TrimSpace(body)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	var event any
	if json.Unmarshal(payload, &event) != nil || !stripReasoningFields(event) {
		return line
	}
	sanitized, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return append(append([]byte("data: "), sanitized...), ending...)
}

type firstByteReader struct {
	reader      io.Reader
	onFirstByte func()
	seen        bool
}

func (r *firstByteReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 && !r.seen {
		r.seen = true
		r.onFirstByte()
	}
	return n, err
}

type sseContentDetector struct {
	buffer []byte
	found  bool
}

func (d *sseContentDetector) Observe(chunk []byte) bool {
	if d.found {
		return false
	}
	d.buffer = append(d.buffer, chunk...)
	for {
		newline := bytes.IndexByte(d.buffer, '\n')
		if newline < 0 {
			break
		}
		line := d.buffer[:newline]
		d.buffer = d.buffer[newline+1:]
		if d.observeLine(line) {
			return true
		}
	}
	if len(d.buffer) > 1<<20 {
		d.buffer = d.buffer[:0]
	}
	return false
}

func (d *sseContentDetector) Flush() bool {
	if d.found || len(d.buffer) == 0 {
		return false
	}
	line := d.buffer
	d.buffer = nil
	return d.observeLine(line)
}

func (d *sseContentDetector) observeLine(line []byte) bool {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return false
	}
	var event struct {
		Choices []struct {
			Delta map[string]json.RawMessage `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return false
	}
	for _, choice := range event.Choices {
		if content, ok := choice.Delta["content"]; ok && contentValuePresent(content) {
			d.found = true
			return true
		}
	}
	return false
}

func contentValuePresent(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return textContentPresent(value)
}

func textContentPresent(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed != ""
	case []any:
		for _, item := range typed {
			if textContentPresent(item) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"text", "content", "output_text"} {
			if item, ok := typed[key]; ok && textContentPresent(item) {
				return true
			}
		}
	}
	return false
}

type limitedCapture struct {
	buf   bytes.Buffer
	limit int
}

func (w *limitedCapture) Write(p []byte) (int, error) {
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = w.buf.Write(p[:remaining])
	}
	return len(p), nil
}

func (w *limitedCapture) Bytes() []byte {
	return w.buf.Bytes()
}

func requestHasImageInput(body []byte) bool {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		return false
	}
	return containsImageContentInMessages(messages)
}

func containsImageContentInMessages(messages []any) bool {
	for _, rawMessage := range messages {
		message, ok := rawMessage.(map[string]any)
		if !ok {
			continue
		}
		if content, ok := message["content"]; ok && containsImageContent(content) {
			return true
		}
	}
	return false
}

func containsImageContent(content any) bool {
	if isImageContentPart(content) {
		return true
	}
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		if isImageContentPart(part) {
			return true
		}
	}
	return false
}

func isImageContentPart(value any) bool {
	part, ok := value.(map[string]any)
	if !ok {
		return false
	}
	if kind, ok := part["type"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "image_url", "input_image", "image":
			return true
		}
	}
	_, hasImageURL := part["image_url"]
	return hasImageURL
}

func (a *App) cachedModelSupportsImage(model string) (known, supported bool) {
	models, _, err := a.loadModelRuntime()
	if err != nil {
		return false, false
	}
	entry, ok := models[model]
	if !ok {
		return false, false
	}
	return entry.ImageKnown, entry.SupportsImages
}

func modelImageSupport(raw []byte) (known, supported bool) {
	var metadata struct {
		InputModalities []string `json:"input_modalities"`
		Modalities      []string `json:"modalities"`
		Modality        string   `json:"modality"`
		Architecture    struct {
			InputModalities []string `json:"input_modalities"`
			Modality        string   `json:"modality"`
		} `json:"architecture"`
	}
	if json.Unmarshal(raw, &metadata) != nil {
		return false, false
	}
	for _, modalities := range [][]string{metadata.InputModalities, metadata.Modalities, metadata.Architecture.InputModalities} {
		if modalities == nil {
			continue
		}
		known = true
		for _, modality := range modalities {
			if isImageModality(modality) {
				supported = true
			}
		}
	}
	for _, modality := range []string{metadata.Modality, metadata.Architecture.Modality} {
		if strings.TrimSpace(modality) == "" {
			continue
		}
		known = true
		if isImageModality(modality) {
			supported = true
		}
	}
	return known, supported
}

func isImageModality(modality string) bool {
	modality = strings.ToLower(strings.TrimSpace(modality))
	return modality == "image" || modality == "image_url" || strings.Contains(modality, "vision") || strings.Contains(modality, "image")
}

func upstreamErrorSummary(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && len(payload.Error) > 0 {
		var detail struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if json.Unmarshal(payload.Error, &detail) == nil {
			if detail.Message != "" {
				return truncateError(detail.Message)
			}
			if detail.Type != "" {
				return truncateError(detail.Type)
			}
		}
	}
	return truncateError(trimmed)
}

func truncateError(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 500 {
		return s[:500]
	}
	return s
}

func parseUsageBytes(body []byte) *tokenUsage {
	var found map[string]any
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(line), &obj) == nil {
			if u, ok := obj["usage"].(map[string]any); ok {
				found = u
			}
		}
	}
	if found == nil {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			found, _ = obj["usage"].(map[string]any)
		}
	}
	if found == nil {
		return nil
	}
	return &tokenUsage{Prompt: usageNumber(found["prompt_tokens"]), Completion: usageNumber(found["completion_tokens"]), Total: usageNumber(found["total_tokens"])}
}

func usageNumber(v any) *int64 {
	var n int64
	switch x := v.(type) {
	case float64:
		n = int64(x)
	case json.Number:
		n, _ = x.Int64()
	case int64:
		n = x
	default:
		return nil
	}
	return &n
}

func (a *App) availableProxies() ([]ProxyRecord, error) {
	a.initializeRuntimeServices()
	return a.proxyRuntime.available(a)
}

func (a *App) loadAvailableProxiesFromDB() ([]ProxyRecord, error) {
	a.deleteExpiredProxies()
	a.deleteStaleSessionRoutes()
	rows, err := a.db.Query("SELECT id,uri,scheme,host,port,COALESCE(username,''),COALESCE(encrypted_password,''),enabled,health_status,failure_count,COALESCE(cooldown_until,''),COALESCE(expires_at,''),created_at FROM proxies WHERE enabled=1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyRecord{}
	for rows.Next() {
		var p ProxyRecord
		var en int
		var cool, expires, created, encrypted string
		if err := rows.Scan(&p.ID, &p.URI, &p.Scheme, &p.Host, &p.Port, &p.Username, &encrypted, &en, &p.HealthStatus, &p.FailureCount, &cool, &expires, &created); err != nil {
			return nil, err
		}
		p.Password, err = a.decrypt(encrypted)
		if err != nil {
			return nil, fmt.Errorf("could not decrypt proxy %d credentials: %w", p.ID, err)
		}
		p.Enabled = en == 1
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		// Cooling and expired exits stay in the snapshot. The pool filters them
		// by time on every pick, so an exit whose cooldown ends returns to
		// rotation immediately instead of waiting for an unrelated cache flush.
		p.CooldownUntil = parseStoredTime(cool)
		p.ExpiresAt = parseStoredTime(expires)
		out = append(out, p)
	}
	return out, rows.Err()
}

// loadProxiesForProbe deliberately includes cooling proxies. A successful
// scheduled or manual exit probe is the signal that clears their cooldown.
func (a *App) loadProxiesForProbeFromDB() ([]ProxyRecord, error) {
	a.deleteExpiredProxies()
	rows, err := a.db.Query("SELECT id,uri,scheme,host,port,COALESCE(username,''),COALESCE(encrypted_password,''),enabled,health_status,failure_count,COALESCE(cooldown_until,''),COALESCE(expires_at,''),created_at FROM proxies WHERE enabled=1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProxyRecord{}
	for rows.Next() {
		var p ProxyRecord
		var enabled int
		var cooldown, expires, created, encrypted string
		if err := rows.Scan(&p.ID, &p.URI, &p.Scheme, &p.Host, &p.Port, &p.Username, &encrypted, &enabled, &p.HealthStatus, &p.FailureCount, &cooldown, &expires, &created); err != nil {
			return nil, err
		}
		password, decryptErr := a.decrypt(encrypted)
		if decryptErr != nil {
			return nil, fmt.Errorf("could not decrypt proxy %d credentials: %w", p.ID, decryptErr)
		}
		p.Password = password
		p.Enabled = enabled == 1
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		p.CooldownUntil = parseStoredTime(cooldown)
		p.ExpiresAt = parseStoredTime(expires)
		if p.ExpiresAt != nil && !p.ExpiresAt.After(time.Now().UTC()) {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (a *App) deleteExpiredProxies() {
	result, err := a.db.Exec("DELETE FROM proxies WHERE expires_at IS NOT NULL AND expires_at <> '' AND expires_at <= ?", time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		log.Printf("delete expired proxies failed: %v", err)
		return
	}
	if deleted, _ := result.RowsAffected(); deleted > 0 {
		a.initializeRuntimeServices()
		a.proxyRuntime.invalidate()
	}
}

func (a *App) deleteStaleSessionRoutes() {
	cutoff := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	if _, err := a.db.Exec("DELETE FROM session_proxy_routes WHERE updated_at < ? OR proxy_id NOT IN (SELECT id FROM proxies)", cutoff); err != nil {
		log.Printf("delete stale session routes failed: %v", err)
	}
	a.clearStaleResinSessionRoutes()
}

func (a *App) expiredProxyJanitor() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		a.deleteExpiredProxies()
		a.deleteStaleSessionRoutes()
		if err := a.deleteExpiredUsage(); err != nil {
			log.Printf("delete expired usage records failed: %v", err)
		}
		if err := a.deleteExpiredAdminSessions(); err != nil {
			log.Printf("delete expired admin sessions failed: %v", err)
		}
		if _, err := a.db.Exec("DELETE FROM alert_events WHERE status IN ('delivered','failed') AND created_at<?", time.Now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339)); err != nil {
			log.Printf("delete expired alert events failed: %v", err)
		}
		if _, err := a.db.Exec("DELETE FROM proxy_probe_jobs WHERE expires_at<=?", time.Now().UTC().Format(time.RFC3339)); err != nil {
			log.Printf("delete expired probe jobs failed: %v", err)
		}
		if _, err := a.db.Exec("DELETE FROM proxy_probe_results WHERE job_id NOT IN (SELECT id FROM proxy_probe_jobs)"); err != nil {
			log.Printf("delete expired probe results failed: %v", err)
		}
		// A pairing ban expires on its own, but the row has to go too or the
		// table grows without bound and a stale suspect could resurrect.
		if _, err := a.db.Exec("DELETE FROM proxy_model_bans WHERE (banned_until IS NOT NULL AND banned_until<=?) OR updated_at<=?", time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Add(-24*time.Hour).Format(time.RFC3339)); err != nil {
			log.Printf("delete expired model bans failed: %v", err)
		}
	}
}

func (a *App) alertEvaluationJanitor() {
	a.evaluateAlertConditions()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		a.evaluateAlertConditions()
	}
}

func (a *App) evaluateAlertConditions() {
	now := time.Now().UTC()
	engine, err := a.proxyEngineStatus()
	if err != nil {
		return
	}
	var active int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE enabled=1 AND (cooldown_until IS NULL OR cooldown_until='' OR cooldown_until<=?) AND (expires_at IS NULL OR expires_at='' OR expires_at>?)", now.Format(time.RFC3339), now.Format(time.RFC3339)).Scan(&active); err != nil {
		return
	}
	a.evaluateGatewayAlerts(now, engine, active)
}

// proxyPick is one rotation decision: the candidate exits, which session is
// asking, which model it asks for, which exits this request already spent, and
// which exit and model pairings are banned.
type proxyPick struct {
	proxies    []ProxyRecord
	sessionKey string
	model      string
	used       map[int64]struct{}
	banned     map[int64]struct{}
}

func (p proxyPick) eligible(proxy ProxyRecord, skipBanned bool) bool {
	if _, skipped := p.used[proxy.ID]; skipped {
		return false
	}
	if skipBanned {
		if _, banned := p.banned[proxy.ID]; banned {
			return false
		}
	}
	return true
}

func (a *App) pickSessionProxy(pick proxyPick) (ProxyRecord, bool, error) {
	var routeLock *sync.Mutex
	if pick.sessionKey != "" {
		routeLock = &a.routingLocks[sessionRouteLockIndex(pick.sessionKey)]
		routeLock.Lock()
		defer routeLock.Unlock()
	}
	if pick.sessionKey != "" {
		limit := a.sessionProxyRequestLimit()
		var currentID int64
		var requestCount int
		err := a.db.QueryRow("SELECT proxy_id,request_count FROM session_proxy_routes WHERE session_key=?", pick.sessionKey).Scan(&currentID, &requestCount)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return ProxyRecord{}, false, err
		}
		for _, candidate := range pick.proxies {
			if candidate.ID != currentID {
				continue
			}
			if pick.eligible(candidate, true) && (limit == 0 || requestCount < limit) {
				if _, err := a.db.Exec("UPDATE session_proxy_routes SET request_count=request_count+1,updated_at=? WHERE session_key=? AND proxy_id=?", time.Now().UTC().Format(time.RFC3339), pick.sessionKey, candidate.ID); err != nil {
					return ProxyRecord{}, false, err
				}
				return candidate, true, nil
			}
			break
		}
	}
	if len(pick.proxies) == 0 {
		return ProxyRecord{}, false, nil
	}
	// The cursor advances once per decision and is taken modulo the whole pool,
	// so excluding spent or banned exits can never skew round-robin fairness.
	start := int(a.rr.Add(1) - 1)
	choose := func(skipBanned bool) (ProxyRecord, bool) {
		for offset := 0; offset < len(pick.proxies); offset++ {
			candidate := pick.proxies[(start+offset)%len(pick.proxies)]
			if !pick.eligible(candidate, skipBanned) {
				continue
			}
			return candidate, true
		}
		return ProxyRecord{}, false
	}
	selected, ok := choose(true)
	if !ok {
		// Every remaining exit is banned for this model. One attempt on a banned
		// pairing beats failing the request without trying at all, and the ban
		// itself expires, so this cannot loop forever.
		selected, ok = choose(false)
	}
	if !ok {
		return ProxyRecord{}, false, nil
	}
	if pick.sessionKey != "" {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := a.db.Exec(`INSERT INTO session_proxy_routes(session_key,proxy_id,request_count,created_at,updated_at) VALUES(?,?,1,?,?) ON CONFLICT(session_key) DO UPDATE SET proxy_id=excluded.proxy_id,request_count=1,updated_at=excluded.updated_at`, pick.sessionKey, selected.ID, now, now); err != nil {
			return ProxyRecord{}, false, err
		}
	}
	return selected, true, nil
}

func (a *App) clearSessionProxy(key string, proxyID int64) {
	if key == "" {
		return
	}
	lock := &a.routingLocks[sessionRouteLockIndex(key)]
	lock.Lock()
	defer lock.Unlock()
	if _, err := a.db.Exec("DELETE FROM session_proxy_routes WHERE session_key=? AND proxy_id=?", key, proxyID); err != nil {
		log.Printf("clear session proxy failed: %v", err)
	}
}

// proxyFailureUpdateSQL escalates an exit cooldown in a single statement. The
// previous read-modify-write lost concurrent failures, so a burst of 429s on one
// exit could fail to escalate and keep re-picking it.
const proxyFailureUpdateSQL = `UPDATE proxies
SET failure_count = failure_count + 1,
    health_status = 'cooldown',
    cooldown_until = strftime('%Y-%m-%dT%H:%M:%SZ','now',CASE
        WHEN failure_count + 1 >= 6 THEN '+30 minutes'
        WHEN failure_count + 1 = 5 THEN '+16 minutes'
        WHEN failure_count + 1 = 4 THEN '+8 minutes'
        WHEN failure_count + 1 = 3 THEN '+4 minutes'
        WHEN failure_count + 1 = 2 THEN '+2 minutes'
        ELSE '+1 minutes' END),
    updated_at = ?
WHERE id = ?`

func (a *App) markProxyFailure(id int64) {
	a.coolProxy(id, 0)
}

// markProxyLimited cools the exit whose per-IP quota is exhausted. The wait the
// upstream asked for wins over the local backoff when it is longer.
func (a *App) markProxyLimited(id int64, retryAfter time.Duration) {
	a.coolProxy(id, retryAfter)
}

func (a *App) coolProxy(id int64, minCooldown time.Duration) {
	now := time.Now().UTC()
	if _, err := a.db.Exec(proxyFailureUpdateSQL, now.Format(time.RFC3339), id); err != nil {
		log.Printf("mark proxy failure failed: %v", err)
		return
	}
	var failures int
	var stored string
	if err := a.db.QueryRow("SELECT failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", id).Scan(&failures, &stored); err != nil {
		log.Printf("load proxy failure state failed: %v", err)
		return
	}
	cooldown := parseStoredTime(stored)
	if minCooldown > maxProxyCooldown {
		minCooldown = maxProxyCooldown
	}
	if cooldown != nil && minCooldown > 0 && cooldown.Sub(now) < minCooldown {
		extended := now.Add(minCooldown)
		if _, err := a.db.Exec("UPDATE proxies SET cooldown_until=?,updated_at=? WHERE id=?", extended.Format(time.RFC3339), now.Format(time.RFC3339), id); err != nil {
			log.Printf("extend proxy cooldown failed: %v", err)
			return
		}
		cooldown = &extended
	}
	a.initializeRuntimeServices()
	a.proxyRuntime.replaceHealth(id, "cooldown", failures, cooldown)
}

// markModelRefusal records a 401 or 403 against one exit and model pairing. The
// first refusal is only suspect; a second one bans the pairing for a short
// window. The exit is never cooled here: the link is healthy, and cooling it
// would remove a working exit from rotation for every other model.
func (a *App) markModelRefusal(proxyID int64, model string) {
	model = strings.TrimSpace(model)
	if proxyID <= 0 || model == "" {
		return
	}
	now := time.Now().UTC()
	var failures int
	if err := a.db.QueryRow("SELECT consecutive_failures FROM proxy_model_bans WHERE proxy_id=? AND model_id=?", proxyID, model).Scan(&failures); err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("load model ban state failed: %v", err)
		return
	}
	failures++
	state := "suspect"
	var bannedUntil any
	if failures >= modelBanConfirmations {
		state = "banned"
		bannedUntil = now.Add(modelBanWindow).Format(time.RFC3339)
	}
	if _, err := a.db.Exec(`INSERT INTO proxy_model_bans(proxy_id,model_id,state,consecutive_failures,banned_until,updated_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(proxy_id,model_id) DO UPDATE SET state=excluded.state,consecutive_failures=excluded.consecutive_failures,banned_until=excluded.banned_until,updated_at=excluded.updated_at`,
		proxyID, model, state, failures, bannedUntil, now.Format(time.RFC3339)); err != nil {
		log.Printf("record model refusal failed: %v", err)
	}
}

// bannedProxyIDs lists the exits currently banned for one model. A read failure
// yields no bans: rotation has to keep working even when bookkeeping fails.
func (a *App) bannedProxyIDs(model string) map[int64]struct{} {
	out := map[int64]struct{}{}
	model = strings.TrimSpace(model)
	if model == "" {
		return out
	}
	rows, err := a.db.Query("SELECT proxy_id FROM proxy_model_bans WHERE model_id=? AND banned_until IS NOT NULL AND banned_until>?", model, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		log.Printf("load model bans failed: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return out
		}
		out[id] = struct{}{}
	}
	return out
}

func (a *App) markProxySuccess(id int64) {
	if _, err := a.db.Exec("UPDATE proxies SET health_status='healthy',failure_count=0,cooldown_until=NULL,updated_at=? WHERE id=?", time.Now().UTC().Format(time.RFC3339), id); err != nil {
		log.Printf("mark proxy success failed: %v", err)
		return
	}
	a.initializeRuntimeServices()
	a.proxyRuntime.replaceHealth(id, "healthy", 0, nil)
}

func (a *App) listProxies(w http.ResponseWriter, r *http.Request) {
	a.deleteExpiredProxies()
	a.deleteStaleSessionRoutes()
	query := r.URL.Query()
	state := strings.TrimSpace(query.Get("state"))
	if state == "" {
		state = "all"
	}
	now := time.Now().UTC()
	where, args, ok := proxyFilterClause(state, now)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "state must be all, unused, in_use, or cooldown"})
		return
	}

	// Keep the original array response for existing API consumers. The console
	// explicitly requests pagination so large proxy pools are never fetched or
	// rendered in one response.
	pageText, hasPage := query["page"]
	pageSizeText, hasPageSize := query["page_size"]
	if !hasPage && !hasPageSize {
		rows, err := a.db.Query(proxySelect+where+" ORDER BY p.id DESC", args...)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not query proxies"})
			return
		}
		defer rows.Close()
		proxies, err := scanProxies(rows)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read proxies"})
			return
		}
		if err := a.annotateProxyUsageStates(proxies, now); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read proxy usage state"})
			return
		}
		writeJSON(w, 200, proxies)
		return
	}

	page, err := parseProxyPageParam(pageText, 1, 1, 1_000_000)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "page must be an integer between 1 and 1000000"})
		return
	}
	pageSize, err := parseProxyPageParam(pageSizeText, 50, 1, 200)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "page_size must be an integer between 1 and 200"})
		return
	}

	var total int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM proxies p"+where, args...).Scan(&total); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not count proxies"})
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	pageArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query(proxySelect+where+" ORDER BY p.id DESC LIMIT ? OFFSET ?", pageArgs...)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query proxies"})
		return
	}
	defer rows.Close()
	proxies, err := scanProxies(rows)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read proxies"})
		return
	}
	if err := a.annotateProxyUsageStates(proxies, now); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read proxy usage state"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"items":       proxies,
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	})
}

const proxySelect = "SELECT p.id,p.uri,p.scheme,p.host,p.port,COALESCE(p.username,''),COALESCE(p.encrypted_password,''),p.enabled,p.health_status,p.failure_count,COALESCE(p.cooldown_until,''),COALESCE(p.expires_at,''),COALESCE(p.last_probe_at,''),p.last_probe_latency_ms,COALESCE(p.last_exit_ip,''),COALESCE(p.last_probe_error,''),COALESCE(p.upstream_probe_at,''),COALESCE(p.upstream_probe_status,''),p.created_at FROM proxies p"

func proxyFilterClause(state string, now time.Time) (string, []any, bool) {
	nowText := now.Format(time.RFC3339)
	activeCutoff := now.Add(-24 * time.Hour).Format(time.RFC3339)
	cooldownReady := "(p.cooldown_until IS NULL OR p.cooldown_until='' OR p.cooldown_until<=?)"
	activeRoute := "p.id IN (SELECT proxy_id FROM session_proxy_routes WHERE updated_at>=?)"
	switch state {
	case "all":
		return "", nil, true
	case "in_use":
		return " WHERE p.enabled=1 AND " + cooldownReady + " AND " + activeRoute, []any{nowText, activeCutoff}, true
	case "cooldown":
		return " WHERE p.cooldown_until IS NOT NULL AND p.cooldown_until<>'' AND p.cooldown_until>?", []any{nowText}, true
	case "unused":
		return " WHERE NOT " + activeRoute + " AND " + cooldownReady, []any{activeCutoff, nowText}, true
	default:
		return "", nil, false
	}
}

func (a *App) annotateProxyUsageStates(proxies []ProxyRecord, now time.Time) error {
	if len(proxies) == 0 {
		return nil
	}
	rows, err := a.db.Query("SELECT DISTINCT proxy_id FROM session_proxy_routes WHERE updated_at>=?", now.Add(-24*time.Hour).Format(time.RFC3339))
	if err != nil {
		return err
	}
	defer rows.Close()
	active := map[int64]struct{}{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		active[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range proxies {
		p := &proxies[i]
		if p.CooldownUntil != nil && p.CooldownUntil.After(now) {
			p.UsageState = "cooldown"
		} else if p.Enabled {
			if _, ok := active[p.ID]; ok {
				p.UsageState = "in_use"
				continue
			}
			p.UsageState = "unused"
		} else {
			p.UsageState = "unused"
		}
	}
	return nil
}

func parseProxyPageParam(values []string, defaultValue, min, max int) (int, error) {
	if len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("invalid page parameter")
	}
	return value, nil
}
func scanProxies(rows *sql.Rows) ([]ProxyRecord, error) {
	out := []ProxyRecord{}
	for rows.Next() {
		var p ProxyRecord
		var en int
		var cool, expires, lastProbe, lastUpstream, created, encrypted string
		var lastLatency sql.NullInt64
		if err := rows.Scan(&p.ID, &p.URI, &p.Scheme, &p.Host, &p.Port, &p.Username, &encrypted, &en, &p.HealthStatus, &p.FailureCount, &cool, &expires, &lastProbe, &lastLatency, &p.LastExitIP, &p.LastProbeError, &lastUpstream, &p.UpstreamProbeStatus, &created); err != nil {
			return nil, err
		}
		p.Enabled = en == 1
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if cool != "" {
			t, _ := time.Parse(time.RFC3339, cool)
			p.CooldownUntil = &t
		}
		if expires != "" {
			t, _ := time.Parse(time.RFC3339, expires)
			p.ExpiresAt = &t
		}
		p.LastProbeAt = parseStoredTime(lastProbe)
		p.UpstreamProbeAt = parseStoredTime(lastUpstream)
		if lastLatency.Valid {
			latency := lastLatency.Int64
			p.LastProbeMS = &latency
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
func parseProxy(raw string) (ProxyRecord, error) {
	u, e := url.Parse(strings.TrimSpace(raw))
	if e != nil || u.Hostname() == "" {
		return ProxyRecord{}, fmt.Errorf("invalid proxy")
	}
	sch := strings.ToLower(u.Scheme)
	if sch != "http" && sch != "https" && sch != "socks5" && sch != "socks5h" {
		return ProxyRecord{}, fmt.Errorf("unsupported proxy scheme")
	}
	port := u.Port()
	if port == "" {
		return ProxyRecord{}, fmt.Errorf("proxy port is required")
	}
	n, er := strconv.Atoi(port)
	if er != nil || n < 1 || n > 65535 {
		return ProxyRecord{}, fmt.Errorf("invalid proxy port")
	}
	safe := *u
	p := ProxyRecord{Scheme: sch, Host: u.Hostname(), Port: n, Enabled: true, HealthStatus: "unknown", CreatedAt: time.Now().UTC()}
	if u.User != nil {
		p.Username = u.User.Username()
		if password, ok := u.User.Password(); ok {
			p.Password = password
		}
		safe.User = url.User(p.Username)
	}
	p.URI = safe.String()
	return p, nil
}

func redactProxyInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
		return parsed.String()
	}
	if strings.Contains(raw, "@") {
		return "<redacted proxy URI>"
	}
	if len(raw) > 256 {
		return raw[:256] + "..."
	}
	return raw
}

func parseProxyExpiry(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("invalid expires_at; use RFC3339")
	}
	t = t.UTC()
	if !t.After(time.Now().UTC()) {
		return nil, fmt.Errorf("expires_at must be in the future")
	}
	return &t, nil
}

func resolveProxyExpiry(raw string, days int) (*time.Time, error) {
	if days < 0 {
		return nil, fmt.Errorf("expires_in_days must be zero or greater")
	}
	if days > 0 && strings.TrimSpace(raw) != "" {
		return nil, fmt.Errorf("use either expires_at or expires_in_days")
	}
	if days > 0 {
		t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
		return &t, nil
	}
	return parseProxyExpiry(raw)
}

func (a *App) addProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URI           string `json:"uri"`
		ExpiresAt     string `json:"expires_at"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "uri is required"})
		return
	}
	p, e := parseProxy(in.URI)
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	p.ExpiresAt, e = resolveProxyExpiry(in.ExpiresAt, in.ExpiresInDays)
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": e.Error()})
		return
	}
	id, e := a.insertProxy(p)
	if e != nil {
		if strings.Contains(strings.ToLower(e.Error()), "unique constraint") {
			writeJSON(w, 409, map[string]string{"error": "proxy already exists"})
		} else {
			writeJSON(w, 500, map[string]string{"error": "could not save proxy"})
		}
		return
	}
	p.ID = id
	writeJSON(w, 201, p)
}
func (a *App) importProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text          string `json:"text"`
		ExpiresAt     string `json:"expires_at"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if readJSON(r, &in) != nil {
		writeJSON(w, 400, map[string]string{"error": "text is required"})
		return
	}
	expiresAt, err := resolveProxyExpiry(in.ExpiresAt, in.ExpiresInDays)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	type result struct {
		Line   int    `json:"line"`
		URI    string `json:"uri"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}
	results := []result{}
	for i, line := range strings.Split(in.Text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		p, e := parseProxy(line)
		if e != nil {
			results = append(results, result{i + 1, redactProxyInput(line), "invalid", e.Error()})
			continue
		}
		p.ExpiresAt = expiresAt
		if _, e = a.insertProxy(p); e != nil {
			status := "error"
			message := "could not save proxy"
			if strings.Contains(strings.ToLower(e.Error()), "unique constraint") {
				status = "duplicate"
				message = "proxy already exists"
			}
			results = append(results, result{i + 1, p.URI, status, message})
			continue
		}
		results = append(results, result{i + 1, p.URI, "imported", ""})
	}
	writeJSON(w, 200, map[string]any{"results": results})
}
func (a *App) insertProxy(p ProxyRecord) (int64, error) {
	encrypted, e := a.encrypt(p.Password)
	if e != nil {
		return 0, e
	}
	var expiresAt any
	if p.ExpiresAt != nil {
		expiresAt = p.ExpiresAt.UTC().Format(time.RFC3339)
	}
	res, e := a.db.Exec("INSERT INTO proxies(uri,scheme,host,port,username,encrypted_password,enabled,health_status,failure_count,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?, ?,0,?,?,?)", p.URI, p.Scheme, p.Host, p.Port, p.Username, encrypted, 1, "unknown", expiresAt, p.CreatedAt.Format(time.RFC3339), p.CreatedAt.Format(time.RFC3339))
	if e != nil {
		return 0, e
	}
	id, err := res.LastInsertId()
	if err == nil {
		a.initializeRuntimeServices()
		a.proxyRuntime.invalidate()
	}
	return id, err
}
func (a *App) proxyID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}
func (a *App) patchProxy(w http.ResponseWriter, r *http.Request) {
	id, e := a.proxyID(r)
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if readJSON(r, &in) != nil || in.Enabled == nil {
		writeJSON(w, 400, map[string]string{"error": "enabled is required"})
		return
	}
	res, e := a.db.Exec("UPDATE proxies SET enabled=?,updated_at=? WHERE id=?", boolInt(*in.Enabled), time.Now().UTC().Format(time.RFC3339), id)
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": "could not update proxy"})
		return
	}
	updated, err := res.RowsAffected()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not confirm proxy update"})
		return
	}
	if updated == 0 {
		writeJSON(w, 404, map[string]string{"error": "proxy not found"})
		return
	}
	a.initializeRuntimeServices()
	a.proxyRuntime.invalidate()
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) deleteProxy(w http.ResponseWriter, r *http.Request) {
	id, e := a.proxyID(r)
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	res, e := a.db.Exec("DELETE FROM proxies WHERE id=?", id)
	if e != nil {
		writeJSON(w, 500, map[string]string{"error": e.Error()})
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeJSON(w, 404, map[string]string{"error": "proxy not found"})
		return
	}
	a.initializeRuntimeServices()
	a.proxyRuntime.invalidate()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) bulkDeleteProxies(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs []int64 `json:"ids"`
	}
	if readJSON(r, &in) != nil || len(in.IDs) == 0 {
		writeJSON(w, 400, map[string]string{"error": "ids are required"})
		return
	}
	if len(in.IDs) > 1000 {
		writeJSON(w, 400, map[string]string{"error": "too many proxy ids"})
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete proxies"})
		return
	}
	seen := make(map[int64]struct{}, len(in.IDs))
	deleted := int64(0)
	for _, id := range in.IDs {
		if id <= 0 {
			_ = tx.Rollback()
			writeJSON(w, 400, map[string]string{"error": "invalid proxy id"})
			return
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		res, deleteErr := tx.Exec("DELETE FROM proxies WHERE id=?", id)
		if deleteErr != nil {
			_ = tx.Rollback()
			writeJSON(w, 500, map[string]string{"error": "could not delete proxies"})
			return
		}
		count, _ := res.RowsAffected()
		deleted += count
	}
	if err = tx.Commit(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not delete proxies"})
		return
	}
	a.initializeRuntimeServices()
	a.proxyRuntime.invalidate()
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": deleted})
}

func (a *App) testProxy(w http.ResponseWriter, r *http.Request) {
	id, e := a.proxyID(r)
	if e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid id"})
		return
	}
	var p ProxyRecord
	var en int
	var encrypted, expires, created string
	queryErr := a.db.QueryRow("SELECT id,uri,scheme,host,port,COALESCE(username,''),COALESCE(encrypted_password,''),enabled,health_status,failure_count,COALESCE(expires_at,''),created_at FROM proxies WHERE id=?", id).Scan(&p.ID, &p.URI, &p.Scheme, &p.Host, &p.Port, &p.Username, &encrypted, &en, &p.HealthStatus, &p.FailureCount, &expires, &created)
	if errors.Is(queryErr, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "proxy not found"})
		return
	}
	if queryErr != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load proxy"})
		return
	}
	if expires != "" {
		t, parseErr := time.Parse(time.RFC3339, expires)
		if parseErr != nil || !t.After(time.Now().UTC()) {
			if _, err := a.db.Exec("DELETE FROM proxies WHERE id=?", id); err != nil {
				writeJSON(w, 500, map[string]string{"error": "could not delete expired proxy"})
				return
			}
			writeJSON(w, 404, map[string]string{"error": "proxy has expired"})
			return
		}
		p.ExpiresAt = &t
	}
	p.Enabled = en == 1
	p.CreatedAt, _ = time.Parse(time.RFC3339, created)
	p.Password, _ = a.decrypt(encrypted)
	client, e := a.httpClient(p)
	var resp *http.Response
	if e == nil {
		req, _ := http.NewRequestWithContext(r.Context(), "GET", "https://api.ipify.org?format=json", nil)
		resp, e = client.Do(req)
	}
	if resp != nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		if e == nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
			e = fmt.Errorf("proxy test returned HTTP %d", resp.StatusCode)
		}
	}
	if e != nil {
		a.markProxyFailure(id)
		writeJSON(w, 502, map[string]any{"ok": false, "error": truncateError(e.Error())})
		return
	}
	a.markProxySuccess(id)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) recordUsage(model string, proxyID *int64, proxyURI, status string, code int, lat time.Duration, firstTokenLatency *time.Duration, retries int, tokens any, e error) {
	a.recordUsageWithEngine(model, proxyID, proxyURI, inferRouteEngine(proxyURI), status, code, lat, firstTokenLatency, retries, tokens, e)
}

func (a *App) recordUsageKind(kind, model string, proxyID *int64, proxyURI, status string, code int, lat time.Duration, firstTokenLatency *time.Duration, retries int, tokens any, e error) {
	a.recordUsageKindWithEngine(kind, model, proxyID, proxyURI, inferRouteEngine(proxyURI), status, code, lat, firstTokenLatency, retries, tokens, e)
}

func inferRouteEngine(proxyURI string) string {
	if proxyURI == "" {
		return "direct"
	}
	return proxyEngineBuiltin
}

func (a *App) recordUsageWithEngine(model string, proxyID *int64, proxyURI, engine, status string, code int, lat time.Duration, firstTokenLatency *time.Duration, retries int, tokens any, e error) {
	a.recordUsageKindWithEngine("chat", model, proxyID, proxyURI, engine, status, code, lat, firstTokenLatency, retries, tokens, e)
}

func (a *App) recordUsageKindWithEngine(kind, model string, proxyID *int64, proxyURI, engine, status string, code int, lat time.Duration, firstTokenLatency *time.Duration, retries int, tokens any, e error) {
	var p, c, t *int64
	if v, ok := tokens.(*tokenUsage); ok && v != nil {
		p, c, t = v.Prompt, v.Completion, v.Total
	}
	var firstTokenMS any
	if firstTokenLatency != nil {
		firstTokenMS = firstTokenLatency.Milliseconds()
	}
	var id any
	if proxyID != nil {
		id = *proxyID
	}
	if kind == "" {
		kind = "chat"
	}
	if engine == "" {
		engine = inferRouteEngine(proxyURI)
	}
	errorMessage := lastErrString(e)
	origin := usageErrorOrigin(kind, status, code, errorMessage)
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,proxy_id,proxy_uri,status,status_code,latency_ms,first_token_latency_ms,retry_count,prompt_tokens,completion_tokens,total_tokens,error_message,error_origin,route_engine) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339), kind, model, id, proxyURI, status, code, lat.Milliseconds(), firstTokenMS, retries, p, c, t, errorMessage, origin, engine); err != nil {
		log.Printf("record usage failed: %v", err)
	}
}

func usageErrorOrigin(kind, status string, code int, message string) string {
	if kind != "chat" {
		return "internal"
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	if code == 0 || code == statusClientClosedRequest || code >= http.StatusInternalServerError || code == http.StatusUnavailableForLegalReasons {
		return "external"
	}
	for _, marker := range []string{
		"context canceled",
		"context deadline exceeded",
		"unexpected eof",
		"proxyconnect",
		"not enough bandwidth",
		"could not locate image content",
		"vision helper",
		"error from provider",
		"upstream request failed",
		"all proxies failed",
	} {
		if strings.Contains(lower, marker) {
			return "external"
		}
	}
	if status == "success" {
		return "none"
	}
	return "user"
}

const usageOriginSQL = "COALESCE(NULLIF(u.error_origin,''),'user')"
const usageSelect = "SELECT u.id,u.created_at,COALESCE(u.request_kind,'chat'),u.model,COALESCE(u.resolved_model,''),u.client_key_id,COALESCE(u.client_key_name,''),COALESCE(u.client_name,''),COALESCE(u.client_user_agent,''),u.stream,u.proxy_id,COALESCE(NULLIF(u.proxy_uri,''),p.uri,''),u.status,u.status_code,u.latency_ms,u.first_token_latency_ms,u.retry_count,u.prompt_tokens,u.completion_tokens,u.total_tokens,COALESCE(u.error_message,'')," + usageOriginSQL + ",COALESCE(NULLIF(u.route_engine,''),'builtin'),COALESCE(u.attempt_summary,'') FROM usage_requests u LEFT JOIN proxies p ON p.id=u.proxy_id"

func (a *App) usageList(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	paged := false
	for _, key := range []string{"page", "page_size", "from", "to", "model", "status"} {
		if _, present := query[key]; present {
			paged = true
			break
		}
	}
	if !paged {
		limit := 50
		if v, _ := strconv.Atoi(query.Get("limit")); v > 0 && v < 200 {
			limit = v
		}
		out, err := a.queryUsageRequests(usageSelect+" ORDER BY u.id DESC LIMIT ?", limit)
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not query usage records"})
			return
		}
		writeJSON(w, 200, out)
		return
	}

	page, err := parseProxyPageParam(query["page"], 1, 1, 1_000_000)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "page must be an integer between 1 and 1000000"})
		return
	}
	pageSize, err := parseProxyPageParam(query["page_size"], 25, 1, 200)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "page_size must be an integer between 1 and 200"})
		return
	}
	status := strings.TrimSpace(query.Get("status"))
	if status != "" && status != "success" && status != "error" && status != "external" {
		writeJSON(w, 400, map[string]string{"error": "status must be success, error, or external"})
		return
	}
	fromTime, err := parseUsageTime(query.Get("from"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "from must be an RFC3339 timestamp"})
		return
	}
	toTime, err := parseUsageTime(query.Get("to"))
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "to must be an RFC3339 timestamp"})
		return
	}
	if fromTime != nil && toTime != nil && fromTime.After(*toTime) {
		writeJSON(w, 400, map[string]string{"error": "from must not be after to"})
		return
	}

	clauses := []string{}
	args := []any{}
	if fromTime != nil {
		clauses = append(clauses, "u.created_at>=?")
		args = append(args, fromTime.UTC().Format(time.RFC3339))
	}
	if toTime != nil {
		clauses = append(clauses, "u.created_at<=?")
		args = append(args, toTime.UTC().Format(time.RFC3339))
	}
	if model := strings.TrimSpace(query.Get("model")); model != "" {
		clauses = append(clauses, "u.model=?")
		args = append(args, model)
	}
	switch status {
	case "success":
		clauses = append(clauses, "u.status='success'")
	case "error":
		clauses = append(clauses, "u.status='error'", usageOriginSQL+"='user'")
	case "external":
		clauses = append(clauses, "u.status='error'", usageOriginSQL+"='external'")
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}

	var total int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM usage_requests u"+where, args...).Scan(&total); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not count usage records"})
		return
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	pageArgs := append(append([]any{}, args...), pageSize, offset)
	items, err := a.queryUsageRequests(usageSelect+where+" ORDER BY u.id DESC LIMIT ? OFFSET ?", pageArgs...)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query usage records"})
		return
	}
	models, err := a.usageModels()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query usage models"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"items":       items,
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
		"models":      models,
	})
}

func parseUsageTime(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func (a *App) queryUsageRequests(query string, args ...any) ([]usageRequest, error) {
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageRequest{}
	for rows.Next() {
		var x usageRequest
		var ts, attempts string
		var stream sql.NullBool
		if err := rows.Scan(&x.ID, &ts, &x.RequestKind, &x.Model, &x.ResolvedModel, &x.ClientKeyID, &x.ClientKeyName, &x.ClientName, &x.ClientUserAgent, &stream, &x.ProxyID, &x.ProxyURI, &x.Status, &x.StatusCode, &x.LatencyMS, &x.FirstTokenLatencyMS, &x.RetryCount, &x.PromptTokens, &x.CompletionTokens, &x.TotalTokens, &x.ErrorMessage, &x.ErrorOrigin, &x.RouteEngine, &attempts); err != nil {
			return nil, err
		}
		x.CreatedAt, _ = time.Parse(time.RFC3339, ts)
		if stream.Valid {
			value := stream.Bool
			x.Stream = &value
		}
		if attempts != "" {
			x.AttemptSummary = json.RawMessage(attempts)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (a *App) usageModels() ([]string, error) {
	rows, err := a.db.Query("SELECT DISTINCT model FROM usage_requests WHERE model<>'' ORDER BY model")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []string{}
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

func (a *App) usageRates(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	cutoff := now.Add(-time.Minute).Format(time.RFC3339)
	var rpm, tpm int64
	if err := a.db.QueryRow("SELECT COUNT(*),COALESCE(SUM(total_tokens),0) FROM usage_requests WHERE request_kind='chat' AND created_at>=? AND created_at<=?", cutoff, now.Format(time.RFC3339)).Scan(&rpm, &tpm); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not calculate usage rates"})
		return
	}
	writeJSON(w, 200, map[string]any{
		"window_seconds": 60,
		"rpm":            rpm,
		"tpm":            tpm,
		"measured_at":    now.Format(time.RFC3339),
	})
}
func (a *App) statsSummary(w http.ResponseWriter, _ *http.Request) {
	a.deleteExpiredProxies()
	now := time.Now().UTC()
	dayStart := chinaDayStart(now).Format(time.RFC3339)
	var total, counted, external, success, pt, ct, tt, free, active int64
	queries := []struct {
		query string
		args  []any
		dest  []any
	}{
		{"SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat'", nil, []any{&total}},
		{"SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat' AND status='success'", nil, []any{&success}},
		{"SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat' AND COALESCE(NULLIF(error_origin,''),'user')<>'external'", nil, []any{&counted}},
		{"SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat' AND COALESCE(NULLIF(error_origin,''),'user')='external'", nil, []any{&external}},
		{"SELECT COALESCE(SUM(prompt_tokens),0),COALESCE(SUM(completion_tokens),0),COALESCE(SUM(total_tokens),0) FROM usage_requests WHERE request_kind='chat' AND created_at>=? AND created_at<=?", []any{dayStart, now.Format(time.RFC3339)}, []any{&pt, &ct, &tt}},
		{"SELECT COUNT(*) FROM models m LEFT JOIN model_policies p ON p.model_id=m.model_id WHERE m.is_free=1 AND COALESCE(p.enabled,1)=1", nil, []any{&free}},
		{"SELECT COUNT(*) FROM proxies WHERE enabled=1 AND (cooldown_until IS NULL OR cooldown_until='' OR cooldown_until<=?) AND (expires_at IS NULL OR expires_at='' OR expires_at>?)", []any{now.Format(time.RFC3339), now.Format(time.RFC3339)}, []any{&active}},
	}
	for _, query := range queries {
		if err := a.db.QueryRow(query.query, query.args...).Scan(query.dest...); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not calculate summary"})
			return
		}
	}
	engine, err := a.proxyEngineStatus()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not load proxy engine status"})
		return
	}
	a.evaluateGatewayAlerts(now, engine, active)
	writeJSON(w, 200, map[string]any{"requests": total, "counted_requests": counted, "external_requests": external, "success": success, "success_rate": rate(success, counted), "prompt_tokens": pt, "completion_tokens": ct, "total_tokens": tt, "free_models": free, "active_proxies": active, "proxy_engine": engine.Engine, "effective_proxy_engine": engine.EffectiveEngine, "resin_fallback_active": engine.ResinFallbackActive, "resin_fallback_since": engine.ResinFallbackSince, "resin_fallback_reason": engine.ResinFallbackReason})
}

func (a *App) evaluateGatewayAlerts(now time.Time, engine proxyEngineStatus, active int64) {
	if engine.Engine == proxyEngineBuiltin {
		if active == 0 {
			a.emitAlert("proxy_pool_empty", "error", "No routable proxies are available", nil)
		} else if settings, _, err := a.loadAlertSettings(); err == nil && active < int64(settings.LowProxyThreshold) {
			a.emitAlert("proxy_availability_low", "warning", "Proxy availability is below the configured threshold", map[string]any{"available_proxies": active})
		}
	}
	var recentCount, recentSuccess int64
	fiveMinutesAgo := now.Add(-5 * time.Minute).Format(time.RFC3339)
	if err := a.db.QueryRow("SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat' AND created_at>=? AND COALESCE(NULLIF(error_origin,''),'user')<>'external'", fiveMinutesAgo).Scan(&recentCount); err != nil {
		return
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM usage_requests WHERE request_kind='chat' AND created_at>=? AND status='success'", fiveMinutesAgo).Scan(&recentSuccess); err != nil {
		return
	}
	if recentCount >= 20 {
		if settings, _, err := a.loadAlertSettings(); err == nil && rate(recentSuccess, recentCount)*100 < float64(settings.SuccessRatePercent) {
			a.emitAlert("success_rate_low", "warning", "Gateway success rate is below the configured threshold", map[string]any{"success_rate": rate(recentSuccess, recentCount) * 100, "requests": recentCount, "window": "5m"})
		}
	}
}

func chinaDayStart(now time.Time) time.Time {
	local := now.In(chinaLocation)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, chinaLocation).UTC()
}

func rate(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
func (a *App) statsTimeseries(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query("SELECT date(created_at,'+8 hours') day,COUNT(*),COALESCE(SUM(total_tokens),0) FROM usage_requests WHERE request_kind='chat' GROUP BY day ORDER BY day DESC LIMIT 30")
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not query timeseries"})
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var day string
		var n, t int64
		if err := rows.Scan(&day, &n, &t); err != nil {
			writeJSON(w, 500, map[string]string{"error": "could not read timeseries"})
			return
		}
		out = append(out, map[string]any{"day": day, "requests": n, "tokens": t})
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, 500, map[string]string{"error": "could not read timeseries"})
		return
	}
	writeJSON(w, 200, out)
}
func (a *App) statsModels(w http.ResponseWriter, r *http.Request) {
	a.statsByDimension(w, r, "model")
}

func (a *App) encrypt(s string) (string, error) {
	return encryptWithKey(a.key, s)
}

func encryptWithKey(key []byte, value string) (string, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	nonce := make([]byte, g.NonceSize())
	if _, e = rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.RawStdEncoding.EncodeToString(g.Seal(nonce, nonce, []byte(value), nil)), nil
}
func (a *App) decrypt(s string) (string, error) {
	return decryptWithKey(a.key, s)
}

func decryptWithKey(key []byte, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	b, e := base64.RawStdEncoding.DecodeString(value)
	if e != nil {
		return "", e
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return "", e
	}
	n := g.NonceSize()
	if len(b) < n {
		return "", errors.New("invalid ciphertext")
	}
	out, e := g.Open(nil, b[:n], b[n:], nil)
	return string(out), e
}

func (a *App) rotateEncryptionKey(previousKey []byte) (int, error) {
	if subtle.ConstantTimeCompare(a.key, previousKey) == 1 {
		return 0, nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	rollback := func(err error) (int, error) {
		_ = tx.Rollback()
		return 0, err
	}

	type settingValue struct {
		key   string
		value string
	}
	settings := []settingValue{}
	rows, err := tx.Query("SELECT key,value FROM settings WHERE key IN ('upstream_api_key','upstream_vision_api_key','upstream_custom_headers','resin_proxy_token')")
	if err != nil {
		return rollback(err)
	}
	for rows.Next() {
		var item settingValue
		if err = rows.Scan(&item.key, &item.value); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		settings = append(settings, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	_ = rows.Close()

	type proxyValue struct {
		id    int64
		value string
	}
	proxies := []proxyValue{}
	rows, err = tx.Query("SELECT id,COALESCE(encrypted_password,'') FROM proxies")
	if err != nil {
		return rollback(err)
	}
	for rows.Next() {
		var item proxyValue
		if err = rows.Scan(&item.id, &item.value); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		proxies = append(proxies, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	_ = rows.Close()

	migrated := 0
	rotate := func(value string) (string, bool, error) {
		if value == "" {
			return value, false, nil
		}
		if _, currentErr := decryptWithKey(a.key, value); currentErr == nil {
			return value, false, nil
		}
		plain, previousErr := decryptWithKey(previousKey, value)
		if previousErr != nil {
			return "", false, errors.New("encrypted value cannot be decrypted with the current or previous key")
		}
		encrypted, encryptErr := encryptWithKey(a.key, plain)
		return encrypted, true, encryptErr
	}
	for _, item := range settings {
		value, changed, rotateErr := rotate(item.value)
		if rotateErr != nil {
			return rollback(fmt.Errorf("setting %s: %w", item.key, rotateErr))
		}
		if changed {
			if _, err = tx.Exec("UPDATE settings SET value=? WHERE key=?", value, item.key); err != nil {
				return rollback(err)
			}
			migrated++
		}
	}
	for _, item := range proxies {
		value, changed, rotateErr := rotate(item.value)
		if rotateErr != nil {
			return rollback(fmt.Errorf("proxy %d: %w", item.id, rotateErr))
		}
		if changed {
			if _, err = tx.Exec("UPDATE proxies SET encrypted_password=? WHERE id=?", value, item.id); err != nil {
				return rollback(err)
			}
			migrated++
		}
	}
	type clientKeyValue struct {
		id    int64
		value string
	}
	clientKeys := []clientKeyValue{}
	rows, err = tx.Query("SELECT id,COALESCE(encrypted_key,'') FROM client_keys WHERE COALESCE(encrypted_key,'')<>''")
	if err != nil {
		return rollback(err)
	}
	for rows.Next() {
		var item clientKeyValue
		if err = rows.Scan(&item.id, &item.value); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		clientKeys = append(clientKeys, item)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	_ = rows.Close()
	for _, item := range clientKeys {
		value, changed, rotateErr := rotate(item.value)
		if rotateErr != nil {
			return rollback(fmt.Errorf("client key %d: %w", item.id, rotateErr))
		}
		if changed {
			if _, err = tx.Exec("UPDATE client_keys SET encrypted_key=? WHERE id=?", value, item.id); err != nil {
				return rollback(err)
			}
			migrated++
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return migrated, nil
}
