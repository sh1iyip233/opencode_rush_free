package main

// The log subsystem exists so an external agent (or a human with curl) can
// connect to a running gateway and see what it is doing: which request failed,
// why the router picked that exit, what the upstream answered, how slow each
// endpoint is, and what the process itself is complaining about.
//
// Two rules shape the design:
//
//  1. Logging must never be able to break a request. Emission is in-memory
//     (ring buffer + subscriber fan-out) plus a non-blocking queue; a full
//     queue drops events and counts them instead of blocking the caller.
//  2. Logging must never store a credential. Every free-form string passes
//     through redactSecrets, and the structured fields only carry identifiers
//     (client key id, proxy uri, model id), never key material.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// Log levels are ordered; a sink stores events at or above its configured
// minimum so an operator can dial noise down without losing failures.
const (
	logLevelDebug = "debug"
	logLevelInfo  = "info"
	logLevelWarn  = "warn"
	logLevelError = "error"
)

// Sources group the log by the subsystem that produced the event.
const (
	logSourceRequest = "request"
	logSourceGateway = "gateway"
	logSourceAuth    = "auth"
	logSourceSystem  = "system"
	logSourceStderr  = "stderr"
	logSourcePanic   = "panic"
	logSourceLogs    = "logs"
)

const (
	// logTimeLayout is fixed width on purpose: created_at is stored as text and
	// read back with time.Parse, so it must never vary in length.
	logTimeLayout = "2006-01-02T15:04:05.000Z"

	logRingCapacity     = 1000
	logQueueCapacity    = 8192
	logBatchSize        = 128
	logDrainBatchSize   = 4096
	logDefaultRetention = 7
	logDefaultMaxRows   = 100000
	logMaxMessageBytes  = 2000
	logMaxDetailBytes   = 8000
	logDefaultMinLevel  = logLevelInfo

	// logAPIKeyHeader is the convenience header for agents that already send an
	// Authorization header of their own.
	logAPIKeyHeader = "X-Relay-Log-Key"

	logInsertSQL = `INSERT INTO log_events(ts,created_at,level,source,event,request_id,method,path,status,duration_ms,client_ip,user_agent,model,route_engine,client_key_id,message,detail) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
)

// Settings keys persisted in the settings table.
const (
	logSettingRetentionDays  = "log_retention_days"
	logSettingMaxRows        = "log_max_rows"
	logSettingMinLevel       = "log_min_level"
	logSettingCaptureStderr  = "log_capture_stderr"
	logSettingRecordRequests = "log_record_requests"
)

// logEvent is what callers hand to the sink.
type logEvent struct {
	At          time.Time
	Level       string
	Source      string
	Event       string
	RequestID   string
	Method      string
	Path        string
	Status      int
	DurationMS  int64
	ClientIP    string
	UserAgent   string
	Model       string
	RouteEngine string
	ClientKeyID *int64
	Message     string
	Detail      map[string]any
}

// logRow is the redacted, truncated, ready-to-insert form of an event.
type logRow struct {
	ts          int64
	createdAt   string
	level       string
	source      string
	event       string
	requestID   string
	method      string
	path        string
	status      int
	durationMS  int64
	clientIP    string
	userAgent   string
	model       string
	routeEngine string
	clientKeyID *int64
	message     string
	detail      string
}

// logRecord is one row as returned by the API.
type logRecord struct {
	ID          int64           `json:"id,omitempty"`
	Seq         uint64          `json:"seq,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	Level       string          `json:"level"`
	Source      string          `json:"source"`
	Event       string          `json:"event"`
	RequestID   string          `json:"request_id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Path        string          `json:"path,omitempty"`
	Status      int             `json:"status,omitempty"`
	DurationMS  int64           `json:"duration_ms,omitempty"`
	ClientIP    string          `json:"client_ip,omitempty"`
	UserAgent   string          `json:"user_agent,omitempty"`
	Model       string          `json:"model,omitempty"`
	RouteEngine string          `json:"route_engine,omitempty"`
	ClientKeyID *int64          `json:"client_key_id,omitempty"`
	Message     string          `json:"message,omitempty"`
	Detail      json.RawMessage `json:"detail,omitempty"`
}

// logSettings is the operator-facing configuration of the sink.
type logSettings struct {
	RetentionDays  int    `json:"retention_days"`
	MaxRows        int64  `json:"max_rows"`
	MinLevel       string `json:"min_level"`
	CaptureStderr  bool   `json:"capture_stderr"`
	RecordRequests bool   `json:"record_requests"`
}

type logSinkStatus struct {
	Queued      int         `json:"queued"`
	RingSize    int         `json:"ring_size"`
	Subscribers int         `json:"subscribers"`
	Stored      int64       `json:"stored"`
	Dropped     int64       `json:"dropped"`
	Failed      int64       `json:"failed"`
	LastError   string      `json:"last_error,omitempty"`
	LastPrune   *time.Time  `json:"last_prune,omitempty"`
	Seq         uint64      `json:"seq"`
	Settings    logSettings `json:"settings"`
}

type logSink struct {
	app *App
	db  *sql.DB

	queue   chan logRow
	flushCh chan chan error
	stop    chan struct{}
	done    chan struct{}
	stopOne sync.Once

	ringMu   sync.Mutex
	ring     []logRecord
	ringNext int
	ringLen  int
	seq      atomic.Uint64

	subMu  sync.Mutex
	subs   map[int]chan logRecord
	subSeq int

	minLevel       atomic.Value // string
	retentionDays  atomic.Int64
	maxRows        atomic.Int64
	captureStderr  atomic.Bool
	recordRequests atomic.Bool

	stored    atomic.Int64
	dropped   atomic.Int64
	failed    atomic.Int64
	lastErr   atomic.Value // string
	lastPrune atomic.Int64

	stderrMu  sync.Mutex
	stderrBuf string
}

func logLevelRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case logLevelError:
		return 3
	case logLevelWarn, "warning":
		return 2
	case logLevelDebug:
		return 0
	default:
		return 1
	}
}

func normalizeLogLevel(level string) string {
	switch logLevelRank(level) {
	case 3:
		return logLevelError
	case 2:
		return logLevelWarn
	case 0:
		return logLevelDebug
	default:
		return logLevelInfo
	}
}

func normalizeLogSource(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		return logSourceSystem
	}
	return source
}

// newLogSink builds the sink, loads persisted settings and starts its writer.
func newLogSink(a *App) *logSink {
	s := &logSink{
		app:     a,
		db:      a.db,
		queue:   make(chan logRow, logQueueCapacity),
		flushCh: make(chan chan error, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
		ring:    make([]logRecord, logRingCapacity),
		subs:    map[int]chan logRecord{},
	}
	settings := a.loadLogSettings()
	s.minLevel.Store(settings.MinLevel)
	s.retentionDays.Store(int64(settings.RetentionDays))
	s.maxRows.Store(settings.MaxRows)
	s.captureStderr.Store(settings.CaptureStderr)
	s.recordRequests.Store(settings.RecordRequests)
	s.lastErr.Store("")
	go s.run()
	return s
}

// logSink returns the process-wide sink, creating it on first use. A nil App
// keeps callers in tests honest instead of panicking.
func (a *App) logSink() *logSink {
	if a == nil {
		return nil
	}
	a.logsOnce.Do(func() { a.logs = newLogSink(a) })
	return a.logs
}

// logEvent records one structured event. Safe to call on a nil sink.
func (a *App) logEvent(e logEvent) {
	if a == nil {
		return
	}
	a.logSink().emit(e)
}

func (s *logSink) emit(e logEvent) {
	if s == nil {
		return
	}
	level := normalizeLogLevel(e.Level)
	if logLevelRank(level) < logLevelRank(s.minLevel.Load().(string)) {
		return
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	row := logRow{
		ts:          at.UnixMilli(),
		createdAt:   at.Format(logTimeLayout),
		level:       level,
		source:      normalizeLogSource(e.Source),
		event:       truncateLogText(e.Event, 120),
		requestID:   e.RequestID,
		method:      e.Method,
		path:        e.Path,
		status:      e.Status,
		durationMS:  e.DurationMS,
		clientIP:    e.ClientIP,
		userAgent:   truncateLogText(e.UserAgent, 300),
		model:       e.Model,
		routeEngine: e.RouteEngine,
		clientKeyID: e.ClientKeyID,
		message:     truncateLogText(redactSecrets(e.Message), logMaxMessageBytes),
	}
	if len(e.Detail) > 0 {
		raw, err := json.Marshal(sanitizeDetail(e.Detail))
		if err != nil {
			row.detail = redactSecrets(fmt.Sprintf(`{"marshal_error":%q}`, err.Error()))
		} else {
			row.detail = truncateLogText(redactSecrets(string(raw)), logMaxDetailBytes)
		}
	}
	rec := logRecord{
		Seq:         s.seq.Add(1),
		CreatedAt:   at,
		Level:       row.level,
		Source:      row.source,
		Event:       row.event,
		RequestID:   row.requestID,
		Method:      row.method,
		Path:        row.path,
		Status:      row.status,
		DurationMS:  row.durationMS,
		ClientIP:    row.clientIP,
		UserAgent:   row.userAgent,
		Model:       row.model,
		RouteEngine: row.routeEngine,
		ClientKeyID: row.clientKeyID,
		Message:     row.message,
	}
	if row.detail != "" {
		rec.Detail = json.RawMessage(row.detail)
	}
	s.pushRing(rec)
	s.broadcast(rec)
	select {
	case s.queue <- row:
	default:
		s.dropped.Add(1)
	}
}

// pushRing keeps the newest logRingCapacity records for cheap tailing.
func (s *logSink) pushRing(rec logRecord) {
	s.ringMu.Lock()
	s.ring[s.ringNext] = rec
	s.ringNext = (s.ringNext + 1) % len(s.ring)
	if s.ringLen < len(s.ring) {
		s.ringLen++
	}
	s.ringMu.Unlock()
}

// recent returns up to limit matching records, newest first.
func (s *logSink) recent(limit int, match func(logRecord) bool) []logRecord {
	if s == nil || limit <= 0 {
		return nil
	}
	s.ringMu.Lock()
	defer s.ringMu.Unlock()
	out := make([]logRecord, 0, min(limit, s.ringLen))
	index := s.ringNext - 1
	if index < 0 {
		index = len(s.ring) - 1
	}
	for i := 0; i < s.ringLen && len(out) < limit; i++ {
		rec := s.ring[index]
		if match == nil || match(rec) {
			out = append(out, rec)
		}
		index--
		if index < 0 {
			index = len(s.ring) - 1
		}
	}
	return out
}

func (s *logSink) subscribe() (<-chan logRecord, func()) {
	ch := make(chan logRecord, 128)
	if s == nil {
		close(ch)
		return ch, func() {}
	}
	s.subMu.Lock()
	id := s.subSeq
	s.subSeq++
	s.subs[id] = ch
	s.subMu.Unlock()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			s.subMu.Lock()
			if existing, ok := s.subs[id]; ok {
				delete(s.subs, id)
				close(existing)
			}
			s.subMu.Unlock()
		})
	}
	return ch, cancel
}

func (s *logSink) broadcast(rec logRecord) {
	s.subMu.Lock()
	for _, ch := range s.subs {
		select {
		case ch <- rec:
		default:
			// A slow subscriber must not stall the request path; it simply
			// misses the live event and can re-read the ring or the table.
		}
	}
	s.subMu.Unlock()
}

func (s *logSink) run() {
	defer close(s.done)
	batch := make([]logRow, 0, logBatchSize)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	pruner := time.NewTicker(60 * time.Second)
	defer pruner.Stop()
	for {
		select {
		case <-s.stop:
			s.drainInto(&batch)
			if len(batch) > 0 {
				s.writeBatch(batch)
			}
			return
		case row := <-s.queue:
			batch = append(batch, row)
			if len(batch) >= logBatchSize {
				s.writeBatch(batch)
				batch = batch[:0]
			}
		case <-ticker.C:
			if len(batch) > 0 {
				s.writeBatch(batch)
				batch = batch[:0]
			}
		case reply := <-s.flushCh:
			s.drainInto(&batch)
			err := s.writeBatch(batch)
			batch = batch[:0]
			reply <- err
		case <-pruner.C:
			if _, err := s.prune(context.Background()); err != nil {
				s.noteError(err)
			}
		}
	}
}

func (s *logSink) drainInto(batch *[]logRow) {
	for {
		select {
		case row := <-s.queue:
			*batch = append(*batch, row)
			if len(*batch) >= logDrainBatchSize {
				s.writeBatch(*batch)
				*batch = (*batch)[:0]
			}
		default:
			return
		}
	}
}

func (s *logSink) writeBatch(rows []logRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		s.failed.Add(int64(len(rows)))
		s.noteError(err)
		return err
	}
	stmt, err := tx.Prepare(logInsertSQL)
	if err != nil {
		_ = tx.Rollback()
		s.failed.Add(int64(len(rows)))
		s.noteError(err)
		return err
	}
	for i := range rows {
		row := rows[i]
		if _, err := stmt.Exec(row.ts, row.createdAt, row.level, row.source, row.event, row.requestID, row.method, row.path, row.status, row.durationMS, row.clientIP, row.userAgent, row.model, row.routeEngine, row.clientKeyID, row.message, row.detail); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			s.failed.Add(int64(len(rows)))
			s.noteError(err)
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		s.failed.Add(int64(len(rows)))
		s.noteError(err)
		return err
	}
	if err := tx.Commit(); err != nil {
		s.failed.Add(int64(len(rows)))
		s.noteError(err)
		return err
	}
	s.stored.Add(int64(len(rows)))
	return nil
}

func (s *logSink) noteError(err error) {
	if err == nil {
		return
	}
	s.lastErr.Store(truncateLogText(redactSecrets(err.Error()), 300))
}

// flush writes every queued event and waits for the batch to commit. Tests and
// the export endpoint use it to guarantee the tail they return is complete.
func (s *logSink) flush(ctx context.Context) error {
	if s == nil {
		return nil
	}
	reply := make(chan error, 1)
	select {
	case s.flushCh <- reply:
	case <-s.done:
		return errors.New("log sink is closed")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *logSink) close() {
	if s == nil {
		return
	}
	s.stopOne.Do(func() { close(s.stop) })
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
	}
	s.subMu.Lock()
	for id, ch := range s.subs {
		delete(s.subs, id)
		close(ch)
	}
	s.subMu.Unlock()
}

// prune enforces the retention window and the row cap.
func (s *logSink) prune(ctx context.Context) (int64, error) {
	if s == nil {
		return 0, nil
	}
	var removed int64
	if days := s.retentionDays.Load(); days > 0 {
		cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		result, err := s.db.ExecContext(ctx, "DELETE FROM log_events WHERE ts < ?", cutoff)
		if err != nil {
			return removed, err
		}
		if n, err := result.RowsAffected(); err == nil {
			removed += n
		}
	}
	if maxRows := s.maxRows.Load(); maxRows > 0 {
		var boundary sql.NullInt64
		err := s.db.QueryRowContext(ctx, "SELECT id FROM log_events ORDER BY id DESC LIMIT 1 OFFSET ?", maxRows-1).Scan(&boundary)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = nil
		case err != nil:
			return removed, err
		}
		if boundary.Valid {
			result, err := s.db.ExecContext(ctx, "DELETE FROM log_events WHERE id < ?", boundary.Int64)
			if err != nil {
				return removed, err
			}
			if n, err := result.RowsAffected(); err == nil {
				removed += n
			}
		}
	}
	s.lastPrune.Store(time.Now().UTC().UnixMilli())
	return removed, nil
}

func (s *logSink) status() logSinkStatus {
	if s == nil {
		return logSinkStatus{Settings: defaultLogSettings()}
	}
	status := logSinkStatus{
		Queued:   len(s.queue),
		Stored:   s.stored.Load(),
		Dropped:  s.dropped.Load(),
		Failed:   s.failed.Load(),
		Seq:      s.seq.Load(),
		Settings: s.settings(),
	}
	if err, ok := s.lastErr.Load().(string); ok {
		status.LastError = err
	}
	if ts := s.lastPrune.Load(); ts > 0 {
		pruned := time.UnixMilli(ts).UTC()
		status.LastPrune = &pruned
	}
	s.ringMu.Lock()
	status.RingSize = s.ringLen
	s.ringMu.Unlock()
	s.subMu.Lock()
	status.Subscribers = len(s.subs)
	s.subMu.Unlock()
	return status
}

func (s *logSink) settings() logSettings {
	if s == nil {
		return defaultLogSettings()
	}
	level, _ := s.minLevel.Load().(string)
	return logSettings{
		RetentionDays:  int(s.retentionDays.Load()),
		MaxRows:        s.maxRows.Load(),
		MinLevel:       level,
		CaptureStderr:  s.captureStderr.Load(),
		RecordRequests: s.recordRequests.Load(),
	}
}

func defaultLogSettings() logSettings {
	return logSettings{
		RetentionDays:  logDefaultRetention,
		MaxRows:        logDefaultMaxRows,
		MinLevel:       logDefaultMinLevel,
		CaptureStderr:  true,
		RecordRequests: true,
	}
}

// loadLogSettings reads the persisted configuration, falling back to defaults
// for anything missing or malformed.
func (a *App) loadLogSettings() logSettings {
	settings := defaultLogSettings()
	if a == nil || a.db == nil {
		return settings
	}
	if raw, err := settingValue(a.db, logSettingRetentionDays); err == nil && raw != "" {
		if value, convErr := strconv.Atoi(raw); convErr == nil && value >= 0 && value <= 365 {
			settings.RetentionDays = value
		}
	}
	if raw, err := settingValue(a.db, logSettingMaxRows); err == nil && raw != "" {
		if value, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil && value >= 1000 && value <= 5000000 {
			settings.MaxRows = value
		}
	}
	if raw, err := settingValue(a.db, logSettingMinLevel); err == nil && raw != "" {
		settings.MinLevel = normalizeLogLevel(raw)
	}
	if raw, err := settingValue(a.db, logSettingCaptureStderr); err == nil && raw != "" {
		if value, convErr := strconv.ParseBool(raw); convErr == nil {
			settings.CaptureStderr = value
		}
	}
	if raw, err := settingValue(a.db, logSettingRecordRequests); err == nil && raw != "" {
		if value, convErr := strconv.ParseBool(raw); convErr == nil {
			settings.RecordRequests = value
		}
	}
	return settings
}

// saveLogSettings persists the configuration and applies it to the live sink.
func (a *App) saveLogSettings(settings logSettings) error {
	if a == nil || a.db == nil {
		return errors.New("no database")
	}
	pairs := [][2]string{
		{logSettingRetentionDays, strconv.Itoa(settings.RetentionDays)},
		{logSettingMaxRows, strconv.FormatInt(settings.MaxRows, 10)},
		{logSettingMinLevel, settings.MinLevel},
		{logSettingCaptureStderr, strconv.FormatBool(settings.CaptureStderr)},
		{logSettingRecordRequests, strconv.FormatBool(settings.RecordRequests)},
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	for _, pair := range pairs {
		if _, err := tx.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", pair[0], pair[1]); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	sink := a.logSink()
	sink.minLevel.Store(settings.MinLevel)
	sink.retentionDays.Store(int64(settings.RetentionDays))
	sink.maxRows.Store(settings.MaxRows)
	sink.captureStderr.Store(settings.CaptureStderr)
	sink.recordRequests.Store(settings.RecordRequests)
	return nil
}

// ---------------------------------------------------------------------------
// Redaction and truncation
// ---------------------------------------------------------------------------

var (
	logRedactPair      = regexp.MustCompile(`(?i)(["']?(?:password|passwd|secret|token|api_?key|client_secret|authorization|cookie)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}\)]{3,})`)
	logRedactBearer    = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]{8,}`)
	logRedactClientKey = regexp.MustCompile(`ocp-[A-Za-z0-9_-]{8,}`)
	logRedactOpenAIKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)
	// Proxy URIs carry user:pass in the authority; a log line that keeps them
	// hands out working credentials, so the userinfo is always replaced.
	logRedactUserinfo = regexp.MustCompile(`://[^/@\s]+@`)
)

// upstreamOutcomeName gives the routing verdict a stable word so a log line can
// be filtered by how the gateway classified an upstream status.
func upstreamOutcomeName(outcome upstreamOutcome) string {
	switch outcome {
	case upstreamOutcomeSuccess:
		return "success"
	case upstreamOutcomeLimited:
		return "limited"
	case upstreamOutcomeRefused:
		return "refused"
	case upstreamOutcomeProxyAuth:
		return "proxy_auth"
	case upstreamOutcomeServerError:
		return "server_error"
	case upstreamOutcomeClientError:
		return "client_error"
	default:
		return "unknown"
	}
}

// redactProxyURI renders a proxy endpoint without its credentials.
func redactProxyURI(uri string) string {
	if uri == "" {
		return ""
	}
	return logRedactUserinfo.ReplaceAllString(uri, "://<redacted>@")
}

// redactSecrets removes credential-shaped substrings. It is deliberately
// applied to every free-form field, including detail JSON, so a careless caller
// cannot persist a key by accident.
func redactSecrets(text string) string {
	if text == "" {
		return ""
	}
	text = logRedactPair.ReplaceAllString(text, `${1}"<redacted>"`)
	text = logRedactBearer.ReplaceAllString(text, "Bearer <redacted>")
	text = logRedactClientKey.ReplaceAllString(text, "ocp-<redacted>")
	text = logRedactOpenAIKey.ReplaceAllString(text, "sk-<redacted>")
	text = logRedactUserinfo.ReplaceAllString(text, "://<redacted>@")
	return text
}

func truncateLogText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	// The limit is a byte budget (storage stays bounded even for CJK text), so the
	// cut is pulled back to a rune boundary to keep the stored text valid UTF-8.
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

// sanitizeDetail converts arbitrary values into something json.Marshal accepts
// so a caller can put an error or a duration into detail without a type dance.
func sanitizeDetail(detail map[string]any) map[string]any {
	out := make(map[string]any, len(detail))
	for key, value := range detail {
		switch typed := value.(type) {
		case nil, bool, string, float64, float32, int, int64, int32, uint64, uint32, json.RawMessage, []any, map[string]any, time.Time, *int64:
			out[key] = typed
		case error:
			out[key] = typed.Error()
		case time.Duration:
			out[key] = typed.Milliseconds()
		default:
			out[key] = fmt.Sprintf("%v", typed)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Request tracing
// ---------------------------------------------------------------------------

type traceKey struct{}

// requestTrace is the mutable per-request bag the middleware reads after the
// handler returns. Handlers contribute routing facts without knowing how the
// event is ultimately stored.
type requestTrace struct {
	mu        sync.Mutex
	requestID string
	fields    map[string]any
	notes     []string
}

func newRequestTrace(requestID string) *requestTrace {
	return &requestTrace{requestID: requestID, fields: map[string]any{}}
}

func (t *requestTrace) set(key string, value any) {
	if t == nil || key == "" {
		return
	}
	t.mu.Lock()
	if t.fields == nil {
		t.fields = map[string]any{}
	}
	t.fields[key] = value
	t.mu.Unlock()
}

func (t *requestTrace) note(format string, args ...any) {
	if t == nil {
		return
	}
	line := fmt.Sprintf(format, args...)
	t.mu.Lock()
	if len(t.notes) < 40 {
		t.notes = append(t.notes, truncateLogText(redactSecrets(line), 500))
	}
	t.mu.Unlock()
}

func (t *requestTrace) snapshot() (map[string]any, []string) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	fields := make(map[string]any, len(t.fields))
	for key, value := range t.fields {
		fields[key] = value
	}
	notes := append([]string(nil), t.notes...)
	return fields, notes
}

func traceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	trace, _ := r.Context().Value(traceKey{}).(*requestTrace)
	return trace
}

// traceSet records a structured routing fact for the request's log event.
func traceSet(r *http.Request, key string, value any) {
	traceFrom(r).set(key, value)
}

// traceNote appends a human-readable step to the request's log event.
func traceNote(r *http.Request, format string, args ...any) {
	traceFrom(r).note(format, args...)
}

func requestIDFrom(r *http.Request) string {
	if trace := traceFrom(r); trace != nil {
		return trace.requestID
	}
	return ""
}

func newRequestID() string {
	var raw [9]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return "req-" + hex.EncodeToString(raw[:])
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

// statusRecorder captures the status code and byte count for the log event and
// keeps the optional interfaces a streaming handler needs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status = status
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if !r.wrote {
		r.status = http.StatusOK
		r.wrote = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// requestLog records one summary event per served API request and carries the
// per-request trace into it.
func (a *App) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.shouldRecordRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		requestID := newRequestID()
		trace := newRequestTrace(requestID)
		w.Header().Set("X-Request-Id", requestID)
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), traceKey{}, trace)))
		elapsed := time.Since(start)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		fields, notes := trace.snapshot()
		detail := map[string]any{}
		for key, value := range fields {
			detail[key] = value
		}
		detail["bytes"] = recorder.bytes
		detail["duration_ms"] = elapsed.Milliseconds()
		if len(notes) > 0 {
			detail["notes"] = notes
		}
		level := logLevelInfo
		switch {
		case status >= 500:
			level = logLevelError
		case status >= 400:
			level = logLevelWarn
		}
		if _, hasError := fields["error"]; hasError {
			level = logLevelError
		}
		message := fmt.Sprintf("%s %s -> %d", r.Method, r.URL.Path, status)
		if detailErr, ok := fields["error"].(string); ok && detailErr != "" {
			message += ": " + detailErr
		}
		model, _ := fields["model"].(string)
		routeEngine, _ := fields["route_engine"].(string)
		var clientKeyID *int64
		if id, ok := fields["client_key_id"].(int64); ok && id > 0 {
			clientKeyID = &id
		}
		a.logEvent(logEvent{
			Level:       level,
			Source:      logSourceRequest,
			Event:       "http.request",
			RequestID:   requestID,
			Method:      r.Method,
			Path:        r.URL.Path,
			Status:      status,
			DurationMS:  elapsed.Milliseconds(),
			ClientIP:    requestIP(r),
			UserAgent:   r.UserAgent(),
			Model:       model,
			RouteEngine: routeEngine,
			ClientKeyID: clientKeyID,
			Message:     message,
			Detail:      detail,
		})
	})
}

// shouldRecordRequest keeps static assets, health probes and the log API's own
// read traffic out of the table so the signal stays readable.
func (a *App) shouldRecordRequest(r *http.Request) bool {
	sink := a.logSink()
	if sink == nil || !sink.recordRequests.Load() {
		return false
	}
	if r.Method == http.MethodOptions || r.Method == http.MethodHead {
		return false
	}
	path := r.URL.Path
	if path == "/healthz" {
		return false
	}
	if strings.HasPrefix(path, "/api/logs") && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
		return false
	}
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/v1/")
}

// recoverPanic turns a handler panic into a logged 500 instead of a dropped
// connection, which is the difference between "the gateway is broken" and
// "the gateway is broken at main.go:1234".
func (a *App) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			stack := truncateLogText(string(debug.Stack()), 6000)
			a.logEvent(logEvent{
				Level:     logLevelError,
				Source:    logSourcePanic,
				Event:     "panic.recovered",
				RequestID: requestIDFrom(r),
				Method:    r.Method,
				Path:      r.URL.Path,
				ClientIP:  requestIP(r),
				Message:   fmt.Sprintf("panic: %v", recovered),
				Detail:    map[string]any{"stack": stack, "user_agent": r.UserAgent()},
			})
			if recorder, ok := w.(*statusRecorder); ok && recorder.wrote {
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal server error", "request_id": requestIDFrom(r)})
		}()
		next.ServeHTTP(w, r)
	})
}

// logLineWriter captures the standard library logger into the sink so panics
// and library errors that bypass the structured path are still visible.
type logLineWriter struct {
	sink *logSink
}

func (w logLineWriter) Write(p []byte) (int, error) {
	sink := w.sink
	if sink == nil {
		return len(p), nil
	}
	sink.stderrMu.Lock()
	buffer := sink.stderrBuf + string(p)
	lines := strings.Split(buffer, "\n")
	sink.stderrBuf = lines[len(lines)-1]
	if len(sink.stderrBuf) > 4096 {
		sink.stderrBuf = sink.stderrBuf[:4096]
	}
	sink.stderrMu.Unlock()
	for _, line := range lines[:len(lines)-1] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sink.emit(logEvent{
			Level:   inferLogLineLevel(line),
			Source:  logSourceStderr,
			Event:   "log.line",
			Message: line,
		})
	}
	return len(p), nil
}

func inferLogLineLevel(line string) string {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "panic"), strings.Contains(lower, "fatal"), strings.Contains(lower, "error"), strings.Contains(lower, "failed"), strings.Contains(lower, "failure"):
		return logLevelError
	case strings.Contains(lower, "warn"), strings.Contains(lower, "deprecated"), strings.Contains(lower, "slow"):
		return logLevelWarn
	default:
		return logLevelInfo
	}
}

// ---------------------------------------------------------------------------
// Credential check for the log API
// ---------------------------------------------------------------------------

// logAPIKey extracts the presented credential. The panel password is the key,
// so an operator has one secret to manage and one place to rotate it.
func logAPIKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value := strings.TrimSpace(r.Header.Get(logAPIKeyHeader)); value != "" {
		return value
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) > 7 && strings.EqualFold(authorization[:7], "bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	return ""
}

// requireLogAccess accepts either a valid panel session cookie or the panel
// password presented as a bearer token.
func (a *App) requireLogAccess(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOriginMutation(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "cross-site request rejected"})
			return
		}
		if a.isAdmin(r) {
			fn(w, r)
			return
		}
		ip := requestIP(r)
		if retryAfter := a.logAuthRetryAfter(ip); retryAfter > 0 {
			seconds := int(retryAfter.Round(time.Second).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many credential attempts"})
			return
		}
		key := logAPIKey(r)
		if key == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="relaydesk-logs"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "authentication required",
				"hint":  "send the panel login password as `Authorization: Bearer <password>` or `X-Relay-Log-Key: <password>`; see GET /api/logs/guide",
			})
			return
		}
		ok, err := a.verifyAdminPassword(key)
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "authentication service unavailable"})
			return
		}
		if !ok {
			a.recordLogAuthFailure(ip)
			a.logEvent(logEvent{
				Level:    logLevelWarn,
				Source:   logSourceLogs,
				Event:    "log.api.denied",
				ClientIP: ip,
				Method:   r.Method,
				Path:     r.URL.Path,
				Message:  "log API rejected a credential",
			})
			w.Header().Set("WWW-Authenticate", `Bearer realm="relaydesk-logs"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credential"})
			return
		}
		a.clearLogAuthFailures(ip)
		fn(w, r)
	}
}

// verifyAdminPassword checks a candidate against the stored bcrypt hash. The
// successful digest is cached briefly because a polling agent would otherwise
// pay a bcrypt round on every request; the cache key includes the stored hash,
// so changing the password invalidates every entry automatically.
func (a *App) verifyAdminPassword(password string) (bool, error) {
	var hash string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash); err != nil {
		return false, err
	}
	digest := logCredentialDigest(hash, password)
	if a.logCredentialCached(digest) {
		return true, nil
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return false, nil
	}
	a.cacheLogCredential(digest)
	return true, nil
}

func logCredentialDigest(storedHash, password string) string {
	sum := sha256.Sum256([]byte(storedHash + "\x00" + password))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

const (
	logCredentialTTL   = 10 * time.Minute
	logCredentialLimit = 32
)

func (a *App) logCredentialCached(digest string) bool {
	a.logKeyMu.Lock()
	defer a.logKeyMu.Unlock()
	if a.logKeyCache == nil {
		return false
	}
	expires, ok := a.logKeyCache[digest]
	if !ok {
		return false
	}
	if time.Now().After(expires) {
		delete(a.logKeyCache, digest)
		return false
	}
	return true
}

func (a *App) cacheLogCredential(digest string) {
	now := time.Now()
	a.logKeyMu.Lock()
	defer a.logKeyMu.Unlock()
	if a.logKeyCache == nil {
		a.logKeyCache = map[string]time.Time{}
	}
	if len(a.logKeyCache) >= logCredentialLimit {
		for key, expires := range a.logKeyCache {
			if now.After(expires) {
				delete(a.logKeyCache, key)
			}
		}
		for key := range a.logKeyCache {
			if len(a.logKeyCache) < logCredentialLimit {
				break
			}
			delete(a.logKeyCache, key)
		}
	}
	a.logKeyCache[digest] = now.Add(logCredentialTTL)
}

// logAuthRetryAfter mirrors the panel login lockout for the log API.
func (a *App) logAuthRetryAfter(ip string) time.Duration {
	const window = 15 * time.Minute
	now := time.Now()
	a.logAuthMu.Lock()
	defer a.logAuthMu.Unlock()
	attempt, ok := a.logAuthFails[ip]
	if !ok {
		return 0
	}
	if attempt.BlockedUntil.After(now) {
		return time.Until(attempt.BlockedUntil)
	}
	if now.Sub(attempt.WindowStart) > window {
		delete(a.logAuthFails, ip)
	}
	return 0
}

func (a *App) recordLogAuthFailure(ip string) {
	const (
		window      = 15 * time.Minute
		blockFor    = 15 * time.Minute
		maxFailures = 10
	)
	now := time.Now()
	a.logAuthMu.Lock()
	defer a.logAuthMu.Unlock()
	if a.logAuthFails == nil {
		a.logAuthFails = map[string]loginAttempt{}
	}
	if len(a.logAuthFails) > 1024 {
		for key, attempt := range a.logAuthFails {
			if now.Sub(attempt.WindowStart) > window && !attempt.BlockedUntil.After(now) {
				delete(a.logAuthFails, key)
			}
		}
	}
	attempt := a.logAuthFails[ip]
	if attempt.WindowStart.IsZero() || now.Sub(attempt.WindowStart) > window {
		attempt = loginAttempt{WindowStart: now}
	}
	attempt.Failures++
	if attempt.Failures >= maxFailures {
		attempt.BlockedUntil = now.Add(blockFor)
	}
	a.logAuthFails[ip] = attempt
}

func (a *App) clearLogAuthFailures(ip string) {
	a.logAuthMu.Lock()
	delete(a.logAuthFails, ip)
	a.logAuthMu.Unlock()
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// startLogging brings the sink up, routes the standard logger into it and
// records the boot facts an operator needs after an unexpected restart.
func (a *App) startLogging() {
	sink := a.logSink()
	if sink == nil {
		return
	}
	if sink.captureStderr.Load() {
		log.SetOutput(logLineWriter{sink: sink})
	}
	a.logEvent(logEvent{
		Level:   logLevelInfo,
		Source:  logSourceSystem,
		Event:   "service.start",
		Message: "Relay Desk started",
		Detail: map[string]any{
			"pid":            os.Getpid(),
			"go":             runtime.Version(),
			"bind":           getenv("BIND_ADDRESS", "127.0.0.1"),
			"port":           getenv("PORT", "8080"),
			"web_dir":        getenv("WEB_DIR", "./web/dist"),
			"log_api_addr":   strings.TrimSpace(os.Getenv("LOG_API_ADDR")),
			"log_settings":   sink.settings(),
			"max_concurrent": cap(a.gatewaySem),
		},
	})
}

// stopLogging drains the queue so a shutdown does not lose the reason for it.
func (a *App) stopLogging() {
	sink := a.logSink()
	if sink == nil {
		return
	}
	a.logEvent(logEvent{Level: logLevelInfo, Source: logSourceSystem, Event: "service.stop", Message: "Relay Desk stopping"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sink.flush(ctx); err != nil {
		log.Printf("log flush during shutdown failed: %v", err)
	}
	sink.close()
	log.SetOutput(os.Stderr)
}

// serveLogAPI starts the optional dedicated listener. Keeping the log API off
// the public port is the usual choice when the panel is internet facing.
func (a *App) serveLogAPI(addr string) {
	mux := http.NewServeMux()
	a.logRoutes(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	server := &http.Server{
		Addr:              addr,
		Handler:           a.withMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// A stream connection must stay open, so no write deadline applies.
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}
	a.logEvent(logEvent{
		Level:   logLevelInfo,
		Source:  logSourceSystem,
		Event:   "log.api.listen",
		Message: "log API listening on " + addr,
		Detail:  map[string]any{"addr": addr},
	})
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.logEvent(logEvent{
				Level:   logLevelError,
				Source:  logSourceSystem,
				Event:   "log.api.failed",
				Message: "log API listener stopped: " + err.Error(),
				Detail:  map[string]any{"addr": addr},
			})
			log.Printf("log API listener on %s stopped: %v", addr, err)
		}
	}()
}
