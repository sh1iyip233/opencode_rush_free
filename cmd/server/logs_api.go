package main

// The log API is the "connect from outside and fix things" surface. It is
// deliberately shaped for an autonomous client rather than for a human eye:
// every response is JSON, every list is paginated, and the diagnostics
// endpoint answers "what looks broken right now" without requiring the caller
// to know the schema.
//
// Authentication is the panel password (see requireLogAccess in logs.go), so
// there is exactly one secret in the system.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// logRoutes registers every read/annotate endpoint. It is registered both on
// the main mux and, when LOG_API_ADDR is set, on a dedicated listener.
func (a *App) logRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/logs", a.requireLogAccess(a.logsList))
	mux.HandleFunc("GET /api/logs/stream", a.requireLogAccess(a.logsStream))
	mux.HandleFunc("GET /api/logs/stats", a.requireLogAccess(a.logsStats))
	mux.HandleFunc("GET /api/logs/diagnostics", a.requireLogAccess(a.logsDiagnostics))
	mux.HandleFunc("GET /api/logs/export", a.requireLogAccess(a.logsExport))
	mux.HandleFunc("GET /api/logs/guide", a.requireLogAccess(a.logsGuide))
	mux.HandleFunc("GET /api/logs/settings", a.requireLogAccess(a.getLogSettings))
	mux.HandleFunc("PUT /api/logs/settings", a.requireLogAccess(a.putLogSettings))
	mux.HandleFunc("POST /api/logs/prune", a.requireLogAccess(a.pruneLogs))
	mux.HandleFunc("GET /api/logs/notes", a.requireLogAccess(a.listLogNotes))
	mux.HandleFunc("POST /api/logs/notes", a.requireLogAccess(a.createLogNote))
	mux.HandleFunc("PATCH /api/logs/notes/{id}", a.requireLogAccess(a.patchLogNote))
	mux.HandleFunc("DELETE /api/logs/notes/{id}", a.requireLogAccess(a.deleteLogNote))
}

// ---------------------------------------------------------------------------
// Query parsing
// ---------------------------------------------------------------------------

type logQuery struct {
	levels     []string
	sources    []string
	event      string
	requestID  string
	path       string
	pathPrefix string
	search     string
	since      time.Time
	until      time.Time
	limit      int
	offset     int
	ascending  bool
	withDetail bool
}

func splitLogList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// parseLogWindow accepts Go durations plus a day suffix, which is the unit an
// operator actually reaches for.
func parseLogWindow(raw string) (time.Duration, bool) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return 0, false
	}
	if strings.HasSuffix(raw, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil && days >= 0 {
			return time.Duration(days) * 24 * time.Hour, true
		}
		return 0, false
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false
	}
	return duration, true
}

// parseLogTime accepts RFC3339, unix milliseconds, or a relative window.
func parseLogTime(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if duration, ok := parseLogWindow(raw); ok {
		return now.Add(-duration), true
	}
	if millis, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if millis > 1e12 {
			return time.UnixMilli(millis).UTC(), true
		}
		return time.Unix(millis, 0).UTC(), true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, logTimeLayout, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

func parseIntQuery(values map[string][]string, key string, fallback, min, max int) int {
	raw := ""
	if list, ok := values[key]; ok && len(list) > 0 {
		raw = strings.TrimSpace(list[0])
	}
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func queryString(values map[string][]string, key string) string {
	if list, ok := values[key]; ok && len(list) > 0 {
		return strings.TrimSpace(list[0])
	}
	return ""
}

func parseLogQuery(r *http.Request) logQuery {
	values := r.URL.Query()
	now := time.Now().UTC()
	query := logQuery{
		levels:     splitLogList(queryString(values, "level")),
		sources:    splitLogList(queryString(values, "source")),
		event:      queryString(values, "event"),
		requestID:  queryString(values, "request_id"),
		path:       queryString(values, "path"),
		pathPrefix: queryString(values, "path_prefix"),
		search:     queryString(values, "q"),
		limit:      parseIntQuery(values, "limit", 100, 1, 1000),
		offset:     parseIntQuery(values, "offset", 0, 0, 1000000),
		ascending:  strings.EqualFold(queryString(values, "order"), "asc"),
		withDetail: !strings.EqualFold(queryString(values, "detail"), "0") && !strings.EqualFold(queryString(values, "detail"), "false"),
	}
	if since, ok := parseLogTime(queryString(values, "since"), now); ok {
		query.since = since
	}
	if until, ok := parseLogTime(queryString(values, "until"), now); ok {
		query.until = until
	}
	if window, ok := parseLogWindow(queryString(values, "window")); ok && window > 0 && query.since.IsZero() {
		query.since = now.Add(-window)
	}
	return query
}

// where renders the filter as SQL plus ordered arguments.
func (q logQuery) where() (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if len(q.levels) > 0 {
		ranks := make([]int, 0, len(q.levels))
		seen := map[int]bool{}
		for _, level := range q.levels {
			rank := logLevelRank(level)
			if !seen[rank] {
				seen[rank] = true
				ranks = append(ranks, rank)
			}
		}
		names := make([]string, 0, 4)
		for _, candidate := range []string{logLevelDebug, logLevelInfo, logLevelWarn, logLevelError} {
			if seen[logLevelRank(candidate)] {
				names = append(names, candidate)
			}
		}
		if len(names) > 0 {
			placeholders := make([]string, len(names))
			for i, name := range names {
				placeholders[i] = "?"
				args = append(args, name)
			}
			clauses = append(clauses, "level IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(q.sources) > 0 {
		placeholders := make([]string, len(q.sources))
		for i, source := range q.sources {
			placeholders[i] = "?"
			args = append(args, strings.ToLower(source))
		}
		clauses = append(clauses, "source IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.event != "" {
		clauses = append(clauses, "event = ?")
		args = append(args, q.event)
	}
	if q.requestID != "" {
		clauses = append(clauses, "request_id = ?")
		args = append(args, q.requestID)
	}
	if q.path != "" {
		clauses = append(clauses, "path = ?")
		args = append(args, q.path)
	}
	if q.pathPrefix != "" {
		clauses = append(clauses, "path LIKE ?")
		args = append(args, escapeLogLike(q.pathPrefix)+"%")
	}
	if q.search != "" {
		pattern := "%" + escapeLogLike(q.search) + "%"
		clauses = append(clauses, "(message LIKE ? ESCAPE '\\' OR detail LIKE ? ESCAPE '\\' OR event LIKE ? ESCAPE '\\' OR path LIKE ? ESCAPE '\\' OR model LIKE ? ESCAPE '\\')")
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if !q.since.IsZero() {
		clauses = append(clauses, "ts >= ?")
		args = append(args, q.since.UnixMilli())
	}
	if !q.until.IsZero() {
		clauses = append(clauses, "ts <= ?")
		args = append(args, q.until.UnixMilli())
	}
	return strings.Join(clauses, " AND "), args
}

func escapeLogLike(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return replacer.Replace(text)
}

// ---------------------------------------------------------------------------
// Listing
// ---------------------------------------------------------------------------

func (a *App) logsList(w http.ResponseWriter, r *http.Request) {
	query := parseLogQuery(r)
	rows, err := a.readLogRows(r.Context(), query)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read log events"})
		return
	}
	total, err := a.countLogRows(r.Context(), query)
	if err != nil {
		total = int64(query.offset + len(rows))
	}
	response := map[string]any{
		"events":   rows,
		"count":    len(rows),
		"total":    total,
		"limit":    query.limit,
		"offset":   query.offset,
		"order":    map[bool]string{true: "asc", false: "desc"}[query.ascending],
		"has_more": int64(query.offset+len(rows)) < total,
		"query":    query.describe(),
	}
	if int64(query.offset+len(rows)) < total {
		response["next_offset"] = query.offset + len(rows)
	}
	if query.since.IsZero() {
		response["default_window"] = "all retained events"
	}
	writeJSON(w, http.StatusOK, response)
}

func (q logQuery) describe() map[string]any {
	describe := map[string]any{"limit": q.limit, "offset": q.offset, "order": map[bool]string{true: "asc", false: "desc"}[q.ascending]}
	if len(q.levels) > 0 {
		describe["level"] = q.levels
	}
	if len(q.sources) > 0 {
		describe["source"] = q.sources
	}
	for key, value := range map[string]string{"event": q.event, "request_id": q.requestID, "path": q.path, "path_prefix": q.pathPrefix, "q": q.search} {
		if value != "" {
			describe[key] = value
		}
	}
	if !q.since.IsZero() {
		describe["since"] = q.since
	}
	if !q.until.IsZero() {
		describe["until"] = q.until
	}
	return describe
}

func (a *App) readLogRows(ctx context.Context, query logQuery) ([]logRecord, error) {
	where, args := query.where()
	order := "DESC"
	if query.ascending {
		order = "ASC"
	}
	statement := "SELECT id,ts,level,source,event,request_id,method,path,status,duration_ms,client_ip,user_agent,model,route_engine,client_key_id,message,detail FROM log_events WHERE " + where + " ORDER BY id " + order + " LIMIT ? OFFSET ?"
	args = append(args, query.limit, query.offset)
	rows, err := a.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]logRecord, 0, query.limit)
	for rows.Next() {
		var (
			record      logRecord
			ts          int64
			clientKeyID sql.NullInt64
			detail      string
		)
		if err := rows.Scan(&record.ID, &ts, &record.Level, &record.Source, &record.Event, &record.RequestID, &record.Method, &record.Path, &record.Status, &record.DurationMS, &record.ClientIP, &record.UserAgent, &record.Model, &record.RouteEngine, &clientKeyID, &record.Message, &detail); err != nil {
			return nil, err
		}
		record.CreatedAt = time.UnixMilli(ts).UTC()
		if clientKeyID.Valid {
			id := clientKeyID.Int64
			record.ClientKeyID = &id
		}
		if detail != "" && query.withDetail {
			record.Detail = json.RawMessage(detail)
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (a *App) countLogRows(ctx context.Context, query logQuery) (int64, error) {
	where, args := query.where()
	var total int64
	err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM log_events WHERE "+where, args...).Scan(&total)
	return total, err
}

// ---------------------------------------------------------------------------
// Live stream
// ---------------------------------------------------------------------------

func (a *App) logsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming unsupported"})
		return
	}
	query := parseLogQuery(r)
	query.limit = 200
	sink := a.logSink()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(event string, payload any) bool {
		raw, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !writeEvent("hello", map[string]any{
		"status":     "connected",
		"server_now": time.Now().UTC(),
		"sink":       sink.status(),
		"filter":     query.describe(),
		"hint":       "send `event: ping` comments are ignored; events are JSON lines under `event: log`",
	}) {
		return
	}

	matches := func(record logRecord) bool {
		if len(query.levels) > 0 {
			found := false
			for _, level := range query.levels {
				if logLevelRank(level) == logLevelRank(record.Level) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		if len(query.sources) > 0 {
			found := false
			for _, source := range query.sources {
				if strings.EqualFold(source, record.Source) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		if query.event != "" && record.Event != query.event {
			return false
		}
		if query.requestID != "" && record.RequestID != query.requestID {
			return false
		}
		if query.path != "" && record.Path != query.path {
			return false
		}
		if query.pathPrefix != "" && !strings.HasPrefix(record.Path, query.pathPrefix) {
			return false
		}
		if !query.since.IsZero() && record.CreatedAt.Before(query.since) {
			return false
		}
		if query.search != "" {
			haystack := strings.ToLower(record.Message + " " + record.Event + " " + record.Path + " " + record.Model + " " + string(record.Detail))
			if !strings.Contains(haystack, strings.ToLower(query.search)) {
				return false
			}
		}
		return true
	}

	if backlog := parseIntQuery(r.URL.Query(), "backlog", 0, 0, 200); backlog > 0 && sink != nil {
		records := sink.recent(backlog, matches)
		// recent() returns newest first; replay oldest first so a client that
		// appends sees a coherent timeline.
		for i := len(records) - 1; i >= 0; i-- {
			if !writeEvent("log", records[i]) {
				return
			}
		}
	}

	events, cancel := sink.subscribe()
	defer cancel()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case record, open := <-events:
			if !open {
				return
			}
			if !matches(record) {
				continue
			}
			if !writeEvent("log", record) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------------------
// Aggregates
// ---------------------------------------------------------------------------

type logPathStat struct {
	Path     string    `json:"path"`
	Count    int       `json:"count"`
	Errors   int       `json:"errors"`
	AvgMS    float64   `json:"avg_ms"`
	P95MS    int64     `json:"p95_ms"`
	MaxMS    int64     `json:"max_ms"`
	LastSeen time.Time `json:"last_seen"`
}

type logCountStat struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

func (a *App) logsStats(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	now := time.Now().UTC()
	window := 24 * time.Hour
	if parsed, ok := parseLogWindow(queryString(values, "window")); ok && parsed > 0 {
		window = parsed
	}
	since := now.Add(-window)
	if explicit, ok := parseLogTime(queryString(values, "since"), now); ok {
		since = explicit
	}

	var total, errors, warnings int64
	if err := a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(CASE WHEN level='error' THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN level='warn' THEN 1 ELSE 0 END),0) FROM log_events WHERE ts >= ?", since.UnixMilli()).Scan(&total, &errors, &warnings); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read log statistics"})
		return
	}

	byLevel := a.logCountBy("level", since)
	bySource := a.logCountBy("source", since)
	byEvent := a.logCountBy("event", since)
	byHour := a.logCountByBucket(since)

	const sampleLimit = 5000
	rows, err := a.db.Query("SELECT path,status,duration_ms,ts FROM log_events WHERE ts >= ? AND event='http.request' ORDER BY id DESC LIMIT ?", since.UnixMilli(), sampleLimit)
	pathStats := []logPathStat{}
	if err == nil {
		type accumulator struct {
			count     int
			errors    int
			totalMS   int64
			maxMS     int64
			lastSeen  int64
			durations []int64
		}
		byPath := map[string]*accumulator{}
		for rows.Next() {
			var path string
			var status int
			var durationMS int64
			var ts int64
			if scanErr := rows.Scan(&path, &status, &durationMS, &ts); scanErr != nil {
				break
			}
			entry := byPath[path]
			if entry == nil {
				entry = &accumulator{}
				byPath[path] = entry
			}
			entry.count++
			if status >= 400 {
				entry.errors++
			}
			entry.totalMS += durationMS
			if durationMS > entry.maxMS {
				entry.maxMS = durationMS
			}
			if ts > entry.lastSeen {
				entry.lastSeen = ts
			}
			if len(entry.durations) < 2000 {
				entry.durations = append(entry.durations, durationMS)
			}
		}
		_ = rows.Close()
		for path, entry := range byPath {
			stat := logPathStat{Path: path, Count: entry.count, Errors: entry.errors, MaxMS: entry.maxMS}
			if entry.count > 0 {
				stat.AvgMS = float64(entry.totalMS) / float64(entry.count)
			}
			stat.P95MS = percentile(entry.durations, 0.95)
			if entry.lastSeen > 0 {
				stat.LastSeen = time.UnixMilli(entry.lastSeen).UTC()
			}
			pathStats = append(pathStats, stat)
		}
		sort.Slice(pathStats, func(i, j int) bool { return pathStats[i].Count > pathStats[j].Count })
		if len(pathStats) > 40 {
			pathStats = pathStats[:40]
		}
	}

	response := map[string]any{
		"window":     window.String(),
		"since":      since,
		"until":      now,
		"total":      total,
		"errors":     errors,
		"warnings":   warnings,
		"by_level":   byLevel,
		"by_source":  bySource,
		"top_events": byEvent,
		"by_hour":    byHour,
		"paths":      pathStats,
		"sink":       a.logSink().status(),
	}
	if total > sampleLimit {
		response["sampled"] = map[string]any{
			"rows": sampleLimit,
			"note": "path latency aggregates use the newest sampled http.request events",
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *App) logCountBy(column string, since time.Time) []logCountStat {
	out := []logCountStat{}
	rows, err := a.db.Query("SELECT "+column+", COUNT(*) AS n FROM log_events WHERE ts >= ? GROUP BY "+column+" ORDER BY n DESC LIMIT 30", since.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var stat logCountStat
		if rows.Scan(&stat.Key, &stat.Count) != nil {
			return out
		}
		out = append(out, stat)
	}
	return out
}

func (a *App) logCountByBucket(since time.Time) []map[string]any {
	out := []map[string]any{}
	rows, err := a.db.Query("SELECT ts/3600000 AS bucket, COUNT(*), COALESCE(SUM(CASE WHEN level='error' THEN 1 ELSE 0 END),0) FROM log_events WHERE ts >= ? GROUP BY bucket ORDER BY bucket ASC LIMIT 200", since.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var bucket, count, errors int64
		if rows.Scan(&bucket, &count, &errors) != nil {
			return out
		}
		out = append(out, map[string]any{
			"hour":   time.UnixMilli(bucket * 3600000).UTC(),
			"count":  count,
			"errors": errors,
		})
	}
	return out
}

// ---------------------------------------------------------------------------
// Diagnostics: the part an agent reads first
// ---------------------------------------------------------------------------

type logCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Metric any    `json:"metric,omitempty"`
}

func (a *App) logsDiagnostics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now().UTC()
	sink := a.logSink()
	checks := []logCheck{}

	addCheck := func(id, status, title, detail string, metric any) {
		checks = append(checks, logCheck{ID: id, Status: status, Title: title, Detail: detail, Metric: metric})
	}

	status := sink.status()
	switch {
	case status.LastError != "":
		addCheck("log_storage", "error", "log writer reported an error", status.LastError, nil)
	case status.Failed > 0:
		addCheck("log_storage", "warn", "log writes failed earlier", fmt.Sprintf("%d events failed to persist", status.Failed), status.Failed)
	default:
		addCheck("log_storage", "ok", "log writer healthy", "every queued event was committed", nil)
	}
	if status.Dropped > 0 {
		addCheck("log_queue", "warn", "events were dropped under load", fmt.Sprintf("%d events dropped because the queue was full", status.Dropped), status.Dropped)
	} else {
		addCheck("log_queue", "ok", "log queue not saturated", fmt.Sprintf("queued=%d ring=%d subscribers=%d", status.Queued, status.RingSize, status.Subscribers), status.Queued)
	}
	if status.Settings.RetentionDays == 0 {
		addCheck("log_retention", "warn", "log retention is unlimited", "log_retention_days is 0, so the table only grows; set a window to bound it", nil)
	} else {
		addCheck("log_retention", "ok", "log retention bounded", fmt.Sprintf("keeping %d days, at most %d rows", status.Settings.RetentionDays, status.Settings.MaxRows), nil)
	}
	if !status.Settings.CaptureStderr {
		addCheck("log_capture", "warn", "standard library output is not captured", "log_capture_stderr is false, so log.Printf and panic traces bypass the log table", nil)
	}
	if !status.Settings.RecordRequests {
		addCheck("log_requests", "warn", "per-request events are disabled", "log_record_requests is false, so only explicitly emitted events are stored", nil)
	}

	// Error and latency shape over the last hour, which is the window a person
	// actually looks at when something broke.
	hourAgo := now.Add(-time.Hour)
	var requests, serverErrors int64
	var avgLatency, maxLatency sql.NullFloat64
	_ = a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(CASE WHEN status >= 500 THEN 1 ELSE 0 END),0), AVG(duration_ms), MAX(duration_ms) FROM log_events WHERE ts >= ? AND event='http.request'", hourAgo.UnixMilli()).Scan(&requests, &serverErrors, &avgLatency, &maxLatency)
	if requests == 0 {
		addCheck("traffic", "info", "no requests in the last hour", "the gateway has been idle; errors may simply not have happened yet", nil)
	} else {
		rate := float64(serverErrors) / float64(requests) * 100
		trafficStatus := "ok"
		switch {
		case rate >= 10:
			trafficStatus = "error"
		case rate > 0:
			trafficStatus = "warn"
		}
		addCheck("traffic", trafficStatus, "server error rate", fmt.Sprintf("%d of %d requests in the last hour returned 5xx (%.1f%%)", serverErrors, requests, rate), map[string]any{"requests": requests, "server_errors": serverErrors, "error_rate_pct": rate})
	}
	if maxLatency.Valid && maxLatency.Float64 > 30000 {
		addCheck("latency", "warn", "slow request observed", fmt.Sprintf("worst request in the last hour took %.0f ms", maxLatency.Float64), maxLatency.Float64)
	}

	var panics int64
	_ = a.db.QueryRow("SELECT COUNT(*) FROM log_events WHERE ts >= ? AND source='panic'", now.Add(-24*time.Hour).UnixMilli()).Scan(&panics)
	if panics > 0 {
		addCheck("panics", "error", "handlers panicked", fmt.Sprintf("%d panics recovered in the last 24 hours", panics), panics)
	}

	// Gateway configuration sanity: these are the failures that look like bugs
	// but are actually an empty pool or a missing key.
	var enabledProxies, clientKeys, modelCount int64
	_ = a.db.QueryRow("SELECT COUNT(*) FROM proxies WHERE enabled=1").Scan(&enabledProxies)
	_ = a.db.QueryRow("SELECT COUNT(*) FROM client_keys WHERE enabled=1").Scan(&clientKeys)
	_ = a.db.QueryRow("SELECT COUNT(*) FROM models").Scan(&modelCount)
	// An empty proxy pool only means "no exit" under the builtin engine: Resin
	// picks its own accounts, so the local pool is unused there and a warning
	// would send a reader chasing a configuration that is correct.
	engine, engineErr := settingValue(a.db, proxyEngineSettingKey)
	engine = strings.TrimSpace(engine)
	if engineErr == nil && engine == proxyEngineResin {
		addCheck("proxy_pool", "ok", "Resin routing", "the Resin engine selects its own accounts; the local proxy pool is not used", enabledProxies)
	} else if enabledProxies == 0 {
		addCheck("proxy_pool", "warn", "no enabled proxies", "the builtin engine has no exit; requests fail with 503 until a proxy is enabled", enabledProxies)
	} else {
		addCheck("proxy_pool", "ok", "proxy pool populated", fmt.Sprintf("%d enabled proxies", enabledProxies), enabledProxies)
	}
	if clientKeys == 0 {
		addCheck("client_keys", "warn", "no enabled client keys", "callers cannot authenticate against /v1", clientKeys)
	}
	if modelCount == 0 {
		addCheck("models", "warn", "model catalogue is empty", "refresh models before serving traffic", modelCount)
	}

	maintenance, maintenanceErr := a.maintenanceModeEnabled()
	if maintenanceErr == nil && maintenance {
		addCheck("maintenance", "warn", "maintenance mode is on", "/v1 requests are refused unless the client key is exempt", nil)
	}

	if a.gatewaySem != nil {
		inflight := len(a.gatewaySem)
		capacity := cap(a.gatewaySem)
		if capacity > 0 && float64(inflight)/float64(capacity) >= 0.9 {
			addCheck("concurrency", "warn", "gateway concurrency nearly exhausted", fmt.Sprintf("%d of %d slots in use", inflight, capacity), map[string]any{"inflight": inflight, "capacity": capacity})
		}
	}

	var visionBase string
	_ = a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_vision_base_url'").Scan(&visionBase)
	if strings.TrimSpace(visionBase) == "" {
		addCheck("vision", "info", "vision helper not configured", "image requests will fail with a configuration error", nil)
	}

	var usageErrors int64
	var usageTotal int64
	_ = a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(CASE WHEN status NOT IN ('success','ok') THEN 1 ELSE 0 END),0) FROM usage_requests WHERE created_at >= ?", hourAgo.Format(time.RFC3339)).Scan(&usageTotal, &usageErrors)
	if usageTotal > 0 && usageErrors > 0 {
		status := "warn"
		if float64(usageErrors)/float64(usageTotal) >= 0.5 {
			status = "error"
		}
		addCheck("upstream_calls", status, "upstream call failures", fmt.Sprintf("%d of %d calls failed in the last hour", usageErrors, usageTotal), map[string]any{"calls": usageTotal, "failed": usageErrors})
	}

	dbBytes := int64(0)
	if err := a.db.QueryRow("SELECT page_count*page_size FROM pragma_page_count(), pragma_page_size()").Scan(&dbBytes); err == nil && dbBytes > 1<<30 {
		addCheck("database", "warn", "database is large", fmt.Sprintf("%.1f MiB on disk", float64(dbBytes)/(1<<20)), dbBytes)
	}

	var logRows, oldest, newest sql.NullInt64
	_ = a.db.QueryRow("SELECT COUNT(*), MIN(ts), MAX(ts) FROM log_events").Scan(&logRows, &oldest, &newest)
	database := map[string]any{
		"bytes":      dbBytes,
		"log_rows":   logRows.Int64,
		"usage_rows": usageTotal,
		"oldest_log": nil,
		"newest_log": nil,
	}
	if oldest.Valid {
		database["oldest_log"] = time.UnixMilli(oldest.Int64).UTC()
	}
	if newest.Valid {
		database["newest_log"] = time.UnixMilli(newest.Int64).UTC()
	}

	recent, _ := a.readLogRows(ctx, logQuery{levels: []string{"warn", "error"}, limit: 20, withDetail: true})

	errorSummary := []logCountStat{}
	rows, err := a.db.Query("SELECT event, COUNT(*) AS n FROM log_events WHERE ts >= ? AND level='error' GROUP BY event ORDER BY n DESC LIMIT 15", now.Add(-24*time.Hour).UnixMilli())
	if err == nil {
		for rows.Next() {
			var stat logCountStat
			if rows.Scan(&stat.Key, &stat.Count) == nil {
				errorSummary = append(errorSummary, stat)
			}
		}
		_ = rows.Close()
	}

	notes := a.readLogNotes(ctx, 10, "")

	response := map[string]any{
		"generated_at": now,
		"process": map[string]any{
			"pid":          os.Getpid(),
			"go_version":   goVersion(),
			"bind":         getenv("BIND_ADDRESS", "127.0.0.1"),
			"port":         getenv("PORT", "8080"),
			"log_api_addr": strings.TrimSpace(os.Getenv("LOG_API_ADDR")),
		},
		"sink":         status,
		"checks":       checks,
		"database":     database,
		"recent":       recent,
		"error_events": errorSummary,
		"notes":        notes,
		"summary": map[string]any{
			"errors":   countChecks(checks, "error"),
			"warnings": countChecks(checks, "warn"),
			"verdict":  diagnosticVerdict(checks),
		},
	}
	writeJSON(w, http.StatusOK, response)
}

func countChecks(checks []logCheck, status string) int {
	count := 0
	for _, check := range checks {
		if check.Status == status {
			count++
		}
	}
	return count
}

func diagnosticVerdict(checks []logCheck) string {
	for _, check := range checks {
		if check.Status == "error" {
			return "broken"
		}
	}
	for _, check := range checks {
		if check.Status == "warn" {
			return "degraded"
		}
	}
	return "healthy"
}

// ---------------------------------------------------------------------------
// Export and guide
// ---------------------------------------------------------------------------

func (a *App) logsExport(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	now := time.Now().UTC()
	window := time.Hour
	if parsed, ok := parseLogWindow(queryString(values, "window")); ok && parsed > 0 {
		window = parsed
	}
	since := now.Add(-window)
	if explicit, ok := parseLogTime(queryString(values, "since"), now); ok {
		since = explicit
	}
	limit := parseIntQuery(values, "limit", 500, 1, 5000)
	query := logQuery{since: since, limit: limit, withDetail: true, levels: splitLogList(queryString(values, "level")), sources: splitLogList(queryString(values, "source"))}
	events, err := a.readLogRows(r.Context(), query)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read log events"})
		return
	}
	sink := a.logSink()
	bundle := map[string]any{
		"exported_at": now,
		"window":      window.String(),
		"since":       since,
		"settings":    sink.settings(),
		"sink":        sink.status(),
		"events":      events,
		"count":       len(events),
		"notes":       a.readLogNotes(r.Context(), 50, ""),
		"truncated":   len(events) >= limit,
	}
	if len(events) >= limit {
		bundle["truncated_note"] = "raise limit (max 5000) or narrow the window to see more"
	}
	if strings.EqualFold(queryString(values, "download"), "1") || strings.EqualFold(queryString(values, "download"), "true") {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="relaydesk-logs-%s.json"`, now.Format("20060102T150405Z")))
	}
	writeJSON(w, http.StatusOK, bundle)
}

// logsGuide returns ready-to-run instructions. An agent that finds the host
// but not the schema can start from this endpoint alone.
func (a *App) logsGuide(w http.ResponseWriter, r *http.Request) {
	base := "http://" + r.Host
	guide := strings.Join([]string{
		"# Relay Desk log API",
		"",
		"Authentication: the panel login password. Send it as a bearer token or in the dedicated header.",
		"",
		"```bash",
		"export RELAY_KEY='<panel password>'",
		"export RELAY_LOG_BASE='" + base + "'",
		"```",
		"",
		"Every request below needs one of:",
		"- `Authorization: Bearer $RELAY_KEY`",
		"- `X-Relay-Log-Key: $RELAY_KEY`",
		"- a valid `ocp_session` cookie from the panel login",
		"",
		"## Start here",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" $RELAY_LOG_BASE/api/logs/diagnostics | jq .",
		"```",
		"`checks[]` is the fastest path to a bug: each entry is `ok`, `info`, `warn` or `error` with a one-line reason.",
		"",
		"## Tail everything live",
		"```bash",
		"curl -N -H \"Authorization: Bearer $RELAY_KEY\" \"$RELAY_LOG_BASE/api/logs/stream?level=warn&backlog=20\"",
		"```",
		"Server-sent events: `event: hello` once, then `event: log` with one JSON record per line.",
		"",
		"## Query history",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" \\",
		"  \"$RELAY_LOG_BASE/api/logs?level=error&window=24h&limit=50\" | jq '.events[] | {created_at,event,path,status,message}'",
		"```",
		"Filters: `level` (comma list, at-or-above semantics), `source`, `event`, `request_id`,",
		"`path`, `path_prefix`, `q` (substring over message/detail/event/path/model),",
		"`since`/`until` (RFC3339 or relative like `90m`), `window`, `limit` (max 1000),",
		"`offset`, `order=asc|desc`, `detail=0` to omit payloads.",
		"",
		"## Follow one request end to end",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" \"$RELAY_LOG_BASE/api/logs?request_id=req-...\" | jq .",
		"```",
		"Every response carries `X-Request-Id`; the same id appears in the request event and in the gateway events for that call.",
		"",
		"## Aggregate before reading",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" \"$RELAY_LOG_BASE/api/logs/stats?window=6h\" | jq '{total,errors,paths}'",
		"```",
		"`paths[]` gives count, error count, average and p95 latency per endpoint - start with the worst p95.",
		"",
		"## Grab a whole incident",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" \"$RELAY_LOG_BASE/api/logs/export?window=2h&limit=2000\" > incident.json",
		"```",
		"",
		"## Leave findings for the next reader",
		"```bash",
		"curl -s -X POST -H \"Authorization: Bearer $RELAY_KEY\" -H 'Content-Type: application/json' \\",
		"  -d '{\"author\":\"agent\",\"kind\":\"finding\",\"severity\":\"warn\",\"title\":\"Retry loop hammers a dead exit\",\"body\":\"...\",\"request_id\":\"req-...\"}' \\",
		"  $RELAY_LOG_BASE/api/logs/notes",
		"```",
		"`kind` is one of `note`, `finding`, `fix`, `todo`. Notes are returned by `/diagnostics` and `/export`.",
		"",
		"## Sources you will see",
		"| source | meaning |",
		"| --- | --- |",
		"| `request` | one event per served `/api` or `/v1` request, with `detail` holding routing facts |",
		"| `gateway` | routing decisions, retries, upstream verdicts, rotation |",
		"| `auth` | client key and panel authentication outcomes |",
		"| `system` | startup, maintenance, janitor work |",
		"| `stderr` | captured `log.Printf` output |",
		"| `panic` | recovered handler panics with stack |",
		"| `logs` | the log API itself (denied credentials, pruning) |",
		"",
		"## Reading a retry chain",
		"Each gateway event carries `detail.notes` with one line per attempt, e.g.",
		"`attempt 3/10 engine=resin proxy=... upstream status 429 verdict=limited`.",
		"`verdict` is `success`, `limited` (429), `refused` (401/403), `proxy_auth` (407) or `server_error` (5xx).",
		"",
		"Rules the gateway follows, so the log can be read without guessing:",
		"- The retry budget (`resin_retry_budget_seconds`, default 45s) bounds the whole chain: the wait for response headers is capped at whatever budget is left, so a request cannot outlive it. Once less than 3s is left no new attempt starts, because that round trip could not have answered in time.",
		"- One attempt never waits more than 45s for headers even if the budget is configured larger. The upstream buffers a whole completion before its first byte, so a healthy attempt can need ~38s; the cap sits above that and only stops a stalled exit from hanging for the transport's 120s timeout.",
		"- A chain that ends on 429s on most of its attempts answers `429` with `Retry-After` and `error_origin=upstream`. It was rate limited, not broken. Only a chain that failed on links answers `502`.",
		"- `stage=resin_breaker_open` means the pool was saturated and the request was rejected immediately instead of being retried.",
		"",
		"## Storage controls",
		"```bash",
		"curl -s -H \"Authorization: Bearer $RELAY_KEY\" $RELAY_LOG_BASE/api/logs/settings | jq .",
		"curl -s -X PUT -H \"Authorization: Bearer $RELAY_KEY\" -H 'Content-Type: application/json' \\",
		"  -d '{\"retention_days\":3,\"max_rows\":50000,\"min_level\":\"info\",\"capture_stderr\":true,\"record_requests\":true}' \\",
		"  $RELAY_LOG_BASE/api/logs/settings",
		"curl -s -X POST -H \"Authorization: Bearer $RELAY_KEY\" -H 'Content-Type: application/json' -d '{}' $RELAY_LOG_BASE/api/logs/prune",
		"```",
		"",
		"Credentials are redacted before storage; `Authorization` headers, `ocp-` client keys and `sk-` upstream keys never reach the table.",
	}, "\n")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(guide))
}

func goVersion() string {
	return runtime.Version()
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

func (a *App) getLogSettings(w http.ResponseWriter, r *http.Request) {
	sink := a.logSink()
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": sink.settings(),
		"sink":     sink.status(),
		"limits": map[string]any{
			"retention_days": map[string]any{"min": 0, "max": 365},
			"max_rows":       map[string]any{"min": 1000, "max": 5000000},
			"min_level":      []string{logLevelDebug, logLevelInfo, logLevelWarn, logLevelError},
		},
		"note": "retention_days=0 keeps events forever; min_level filters at write time",
	})
}

func (a *App) putLogSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RetentionDays  *int    `json:"retention_days"`
		MaxRows        *int64  `json:"max_rows"`
		MinLevel       *string `json:"min_level"`
		CaptureStderr  *bool   `json:"capture_stderr"`
		RecordRequests *bool   `json:"record_requests"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	settings := a.loadLogSettings()
	if in.RetentionDays != nil {
		if *in.RetentionDays < 0 || *in.RetentionDays > 365 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "retention_days must be between 0 and 365"})
			return
		}
		settings.RetentionDays = *in.RetentionDays
	}
	if in.MaxRows != nil {
		if *in.MaxRows < 1000 || *in.MaxRows > 5000000 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_rows must be between 1000 and 5000000"})
			return
		}
		settings.MaxRows = *in.MaxRows
	}
	if in.MinLevel != nil {
		settings.MinLevel = normalizeLogLevel(*in.MinLevel)
	}
	if in.CaptureStderr != nil {
		settings.CaptureStderr = *in.CaptureStderr
	}
	if in.RecordRequests != nil {
		settings.RecordRequests = *in.RecordRequests
	}
	if err := a.saveLogSettings(settings); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not save log settings"})
		return
	}
	a.logEvent(logEvent{
		Level:    logLevelInfo,
		Source:   logSourceLogs,
		Event:    "log.settings.updated",
		ClientIP: requestIP(r),
		Message:  fmt.Sprintf("log settings updated: retention=%dd max_rows=%d min_level=%s", settings.RetentionDays, settings.MaxRows, settings.MinLevel),
		Detail:   map[string]any{"settings": settings},
	})
	writeJSON(w, http.StatusOK, map[string]any{"settings": settings, "sink": a.logSink().status()})
}

func (a *App) pruneLogs(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Before    string `json:"before"`
		OlderThan string `json:"older_than"`
		All       bool   `json:"all"`
	}
	if r.ContentLength > 0 {
		if err := readJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
	}
	ctx := r.Context()
	if in.All {
		result, err := a.db.ExecContext(ctx, "DELETE FROM log_events")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not clear log events"})
			return
		}
		removed, _ := result.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "mode": "all"})
		return
	}
	if raw := strings.TrimSpace(in.Before); raw != "" {
		before, ok := parseLogTime(raw, time.Now().UTC())
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "before must be RFC3339, unix milliseconds, or a relative window"})
			return
		}
		result, err := a.db.ExecContext(ctx, "DELETE FROM log_events WHERE ts < ?", before.UnixMilli())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not prune log events"})
			return
		}
		removed, _ := result.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "mode": "before", "before": before})
		return
	}
	if raw := strings.TrimSpace(in.OlderThan); raw != "" {
		window, ok := parseLogWindow(raw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "older_than must be a duration such as 24h or 7d"})
			return
		}
		cutoff := time.Now().UTC().Add(-window).UnixMilli()
		result, err := a.db.ExecContext(ctx, "DELETE FROM log_events WHERE ts < ?", cutoff)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not prune log events"})
			return
		}
		removed, _ := result.RowsAffected()
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "mode": "older_than", "older_than": raw})
		return
	}
	removed, err := a.logSink().prune(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not prune log events"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "mode": "retention", "settings": a.logSink().settings()})
}

// ---------------------------------------------------------------------------
// Notes: the write-back channel for whatever connects
// ---------------------------------------------------------------------------

type logNote struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	Author    string    `json:"author"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	RequestID string    `json:"request_id,omitempty"`
	EventID   *int64    `json:"event_id,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
}

func normalizeNoteKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "finding":
		return "finding"
	case "fix":
		return "fix"
	case "todo":
		return "todo"
	default:
		return "note"
	}
}

func normalizeNoteSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "error":
		return "error"
	case "warn", "warning":
		return "warn"
	case "debug":
		return "debug"
	default:
		return "info"
	}
}

func (a *App) readLogNotes(ctx context.Context, limit int, kind string) []logNote {
	out := []logNote{}
	statement := "SELECT id,created_at,author,kind,severity,title,body,request_id,event_id,tags,updated_at FROM log_notes"
	args := []any{}
	if kind != "" {
		statement += " WHERE kind = ?"
		args = append(args, kind)
	}
	statement += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := a.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var (
			note      logNote
			createdAt string
			updatedAt string
			eventID   sql.NullInt64
			tags      string
		)
		if rows.Scan(&note.ID, &createdAt, &note.Author, &note.Kind, &note.Severity, &note.Title, &note.Body, &note.RequestID, &eventID, &tags, &updatedAt) != nil {
			return out
		}
		if parsed, err := time.Parse(logTimeLayout, createdAt); err == nil {
			note.CreatedAt = parsed
		}
		if parsed, err := time.Parse(logTimeLayout, updatedAt); err == nil {
			note.UpdatedAt = parsed
		}
		if eventID.Valid {
			id := eventID.Int64
			note.EventID = &id
		}
		note.Tags = splitLogList(tags)
		out = append(out, note)
	}
	return out
}

func (a *App) listLogNotes(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	limit := parseIntQuery(values, "limit", 50, 1, 500)
	kind := ""
	if raw := queryString(values, "kind"); raw != "" {
		kind = normalizeNoteKind(raw)
	}
	notes := a.readLogNotes(r.Context(), limit, kind)
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes, "count": len(notes)})
}

func (a *App) createLogNote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Author    string   `json:"author"`
		Kind      string   `json:"kind"`
		Severity  string   `json:"severity"`
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		RequestID string   `json:"request_id"`
		EventID   *int64   `json:"event_id"`
		Tags      []string `json:"tags"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	title := strings.TrimSpace(in.Title)
	body := strings.TrimSpace(in.Body)
	if title == "" && body == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title or body is required"})
		return
	}
	author := strings.TrimSpace(in.Author)
	if author == "" {
		author = "agent"
	}
	now := time.Now().UTC()
	storedAuthor := truncateLogText(redactSecrets(author), 80)
	storedTitle := truncateLogText(redactSecrets(title), 300)
	storedBody := truncateLogText(redactSecrets(body), 8000)
	kind := normalizeNoteKind(in.Kind)
	severity := normalizeNoteSeverity(in.Severity)
	result, err := a.db.Exec("INSERT INTO log_notes(created_at,author,kind,severity,title,body,request_id,event_id,tags,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
		now.Format(logTimeLayout), storedAuthor, kind, severity,
		storedTitle, storedBody,
		strings.TrimSpace(in.RequestID), in.EventID, strings.Join(in.Tags, ","), now.Format(logTimeLayout))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not store note"})
		return
	}
	id, _ := result.LastInsertId()
	note := logNote{ID: id, CreatedAt: now, UpdatedAt: now, Author: storedAuthor, Kind: kind, Severity: severity, Title: storedTitle, Body: storedBody, RequestID: in.RequestID, EventID: in.EventID, Tags: in.Tags}
	a.logEvent(logEvent{
		Level:     logLevelInfo,
		Source:    logSourceLogs,
		Event:     "log.note.created",
		RequestID: in.RequestID,
		ClientIP:  requestIP(r),
		Message:   "note recorded: " + note.Title,
		Detail:    map[string]any{"note_id": id, "kind": note.Kind, "severity": note.Severity, "author": note.Author},
	})
	writeJSON(w, http.StatusCreated, map[string]any{"note": note})
}

func (a *App) patchLogNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid note id"})
		return
	}
	var in struct {
		Kind     *string  `json:"kind"`
		Severity *string  `json:"severity"`
		Title    *string  `json:"title"`
		Body     *string  `json:"body"`
		Tags     []string `json:"tags"`
	}
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	sets := []string{}
	args := []any{}
	if in.Kind != nil {
		sets = append(sets, "kind = ?")
		args = append(args, normalizeNoteKind(*in.Kind))
	}
	if in.Severity != nil {
		sets = append(sets, "severity = ?")
		args = append(args, normalizeNoteSeverity(*in.Severity))
	}
	if in.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, truncateLogText(redactSecrets(strings.TrimSpace(*in.Title)), 300))
	}
	if in.Body != nil {
		sets = append(sets, "body = ?")
		args = append(args, truncateLogText(redactSecrets(strings.TrimSpace(*in.Body)), 8000))
	}
	if in.Tags != nil {
		sets = append(sets, "tags = ?")
		args = append(args, strings.Join(in.Tags, ","))
	}
	if len(sets) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no fields to update"})
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(logTimeLayout))
	args = append(args, id)
	result, err := a.db.Exec("UPDATE log_notes SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not update note"})
		return
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "note not found"})
		return
	} else {
		notes := a.readLogNotes(r.Context(), 1, "")
		writeJSON(w, http.StatusOK, map[string]any{"updated": affected, "note_id": id, "latest": notes})
	}
}

func (a *App) deleteLogNote(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid note id"})
		return
	}
	result, err := a.db.Exec("DELETE FROM log_notes WHERE id = ?", id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete note"})
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "note not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": affected, "note_id": id})
}
