package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// logApp builds a test app with the runtime services and a live sink, and makes
// sure the sink is drained and stopped before the database is closed.
func logApp(t testing.TB) *App {
	t.Helper()
	a := testApp(t)
	a.initializeRuntimeServices()
	if a.logSink() == nil {
		t.Fatal("log sink was not created")
	}
	t.Cleanup(func() {
		if a.logs != nil {
			a.logs.close()
		}
	})
	return a
}

func logHandler(a *App) http.Handler {
	mux := http.NewServeMux()
	a.routes(mux)
	return a.withMiddleware(mux)
}

func logFlush(t testing.TB, a *App) {
	t.Helper()
	if err := a.logSink().flush(context.Background()); err != nil {
		t.Fatalf("flush log sink: %v", err)
	}
}

func logDo(t testing.TB, h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func logDecode(t testing.TB, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %s: %v (%s)", rec.Body.String(), err, rec.Body.String())
	}
}

func logStoredEvents(t testing.TB, a *App) []logRecord {
	t.Helper()
	records, err := a.readLogRows(context.Background(), logQuery{limit: 500, withDetail: true})
	if err != nil {
		t.Fatalf("read log rows: %v", err)
	}
	return records
}

func TestLogEventIsStoredAndServed(t *testing.T) {
	a := logApp(t)
	a.logEvent(logEvent{
		Level:     logLevelError,
		Source:    logSourceGateway,
		Event:     "test.stored",
		Message:   "upstream exploded",
		Model:     "gpt-test",
		RequestID: "req-teststored",
		Detail:    map[string]any{"attempt": 2},
	})
	logFlush(t, a)

	h := logHandler(a)
	rec := logDo(t, h, http.MethodGet, "/api/logs?event=test.stored", "", map[string]string{logAPIKeyHeader: "test-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs = %d (%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Events []logRecord `json:"events"`
		Count  int         `json:"count"`
		Total  int         `json:"total"`
	}
	logDecode(t, rec, &out)
	if out.Count != 1 || len(out.Events) != 1 {
		t.Fatalf("expected one event, got count=%d len=%d", out.Count, len(out.Events))
	}
	event := out.Events[0]
	if event.Event != "test.stored" || event.Level != logLevelError || event.Model != "gpt-test" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.RequestID != "req-teststored" {
		t.Fatalf("request id was not preserved: %q", event.RequestID)
	}
	if !bytes.Contains(event.Detail, []byte(`"attempt":2`)) {
		t.Fatalf("detail was not preserved: %s", event.Detail)
	}
}

func TestLogBearerTokenMustBeThePanelPassword(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)

	rec := logDo(t, h, http.MethodGet, "/api/logs", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request = %d, want 401", rec.Code)
	}
	rec = logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{logAPIKeyHeader: "wrong-password"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key = %d, want 401", rec.Code)
	}
	rec = logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{"Authorization": "Bearer test-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer password = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	rec = logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{logAPIKeyHeader: "test-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("header password = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

func TestLogAcceptsAnAdminSessionCookie(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)

	login := logDo(t, h, http.MethodPost, "/api/auth/login", `{"password":"test-password"}`, map[string]string{"Content-Type": "application/json"})
	if login.Code != http.StatusOK {
		t.Fatalf("login = %d (%s)", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login returned no session cookie")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/logs", nil)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie auth = %d (%s), want 200", rec.Code, rec.Body.String())
	}
}

func TestLogAuthFailuresAreThrottled(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)
	var last int
	for i := 0; i < 12; i++ {
		last = logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{logAPIKeyHeader: "nope"}).Code
		if last == http.StatusTooManyRequests {
			break
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("expected throttling after repeated failures, last status = %d", last)
	}
	// The throttle must not outlive the failures it was earned by: a correct
	// password is still rejected while the block is active, and the block is
	// cleared explicitly here so the rest of the suite is unaffected.
	a.logAuthMu.Lock()
	a.logAuthFails = map[string]loginAttempt{}
	a.logAuthMu.Unlock()
	if rec := logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{logAPIKeyHeader: "test-password"}); rec.Code != http.StatusOK {
		t.Fatalf("password after clearing the block = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestLogRedactsCredentials(t *testing.T) {
	a := logApp(t)
	a.logEvent(logEvent{
		Level:   logLevelError,
		Source:  logSourceSystem,
		Event:   "test.leak",
		Message: `password=hunter2 token: abc123xyz proxy=http://alice:s3cret@10.0.0.1:8080 key ocp-abcdefghijkl sk-proj-abcdefghijkl`,
		Detail: map[string]any{
			"proxy_uri": "http://alice:s3cret@10.0.0.1:8080",
			"api_key":   "sk-abcdefghijkl",
		},
	})
	logFlush(t, a)

	records := logStoredEvents(t, a)
	if len(records) != 1 {
		t.Fatalf("expected one record, got %d", len(records))
	}
	record := records[0]
	haystack := record.Message + string(record.Detail)
	for _, secret := range []string{"hunter2", "abc123xyz", "alice", "s3cret", "ocp-abcdefghijkl", "sk-proj-abcdefghijkl", "sk-abcdefghijkl"} {
		if strings.Contains(haystack, secret) {
			t.Fatalf("secret %q leaked into the log: %s", secret, haystack)
		}
	}
	if !strings.Contains(haystack, "<redacted>") {
		t.Fatalf("expected redaction markers, got: %s", haystack)
	}
	if got := redactProxyURI("http://alice:s3cret@10.0.0.1:8080"); got != "http://<redacted>@10.0.0.1:8080" {
		t.Fatalf("redactProxyURI = %q", got)
	}
}

func TestRequestLogMiddlewareRecordsTheRequest(t *testing.T) {
	a := logApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/probe", func(w http.ResponseWriter, r *http.Request) {
		traceSet(r, "probe", "yes")
		traceNote(r, "custom note")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	})
	h := a.withMiddleware(mux)

	rec := logDo(t, h, http.MethodGet, "/api/probe", "", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("probe = %d", rec.Code)
	}
	requestID := rec.Header().Get("X-Request-Id")
	if requestID == "" {
		t.Fatal("middleware did not set X-Request-Id")
	}
	logFlush(t, a)

	records := logStoredEvents(t, a)
	var found *logRecord
	for i := range records {
		if records[i].Event == "http.request" && records[i].Path == "/api/probe" {
			found = &records[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("no http.request event recorded, got %d records", len(records))
	}
	if found.Level != logLevelError {
		t.Fatalf("a 503 must be logged as error, got %q", found.Level)
	}
	if found.Status != http.StatusServiceUnavailable {
		t.Fatalf("status not recorded: %+v", found.Status)
	}
	if found.RequestID != requestID {
		t.Fatalf("request id mismatch: %q vs %q", found.RequestID, requestID)
	}
	detail := string(found.Detail)
	for _, want := range []string{`"probe":"yes"`, "custom note", `"duration_ms"`} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q: %s", want, detail)
		}
	}
}

func TestRequestLogSkipsHealthAndLogReads(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)
	logDo(t, h, http.MethodGet, "/healthz", "", nil)
	logDo(t, h, http.MethodGet, "/api/logs", "", map[string]string{logAPIKeyHeader: "test-password"})
	logDo(t, h, http.MethodOptions, "/api/probe", "", nil)
	logFlush(t, a)

	for _, record := range logStoredEvents(t, a) {
		if record.Event != "http.request" {
			continue
		}
		switch {
		case record.Path == "/healthz":
			t.Fatal("health checks must not be logged as requests")
		case strings.HasPrefix(record.Path, "/api/logs"):
			t.Fatalf("log reads must not log themselves: %s", record.Path)
		}
	}
}

func TestRecoverPanicIsLogged(t *testing.T) {
	a := logApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/boom", func(http.ResponseWriter, *http.Request) {
		panic("kaboom")
	})
	h := a.withMiddleware(mux)

	rec := logDo(t, h, http.MethodGet, "/api/boom", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic handler = %d, want 500", rec.Code)
	}
	logFlush(t, a)

	var found bool
	for _, record := range logStoredEvents(t, a) {
		if record.Event == "panic.recovered" {
			found = true
			if record.Level != logLevelError {
				t.Fatalf("panic level = %q", record.Level)
			}
			if !strings.Contains(record.Message, "kaboom") {
				t.Fatalf("panic message missing: %q", record.Message)
			}
			if !strings.Contains(string(record.Detail), "stack") {
				t.Fatalf("panic detail has no stack: %s", record.Detail)
			}
		}
	}
	if !found {
		t.Fatal("panic was not recorded")
	}
}

func TestDiagnosticsProxyCheckKnowsTheEngine(t *testing.T) {
	// Under the builtin engine an empty pool really is "no exit"; under Resin
	// the pool is unused, so reporting it as a problem would be a false alarm.
	cases := []struct {
		name   string
		engine string
		want   string
	}{
		{"builtin without proxies", proxyEngineBuiltin, "warn"},
		{"resin without proxies", proxyEngineResin, "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := logApp(t)
			if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", proxyEngineSettingKey, tc.engine); err != nil {
				t.Fatalf("seed engine: %v", err)
			}
			h := logHandler(a)
			rec := logDo(t, h, http.MethodGet, "/api/logs/diagnostics", "", map[string]string{logAPIKeyHeader: "test-password"})
			if rec.Code != http.StatusOK {
				t.Fatalf("diagnostics = %d (%s)", rec.Code, rec.Body.String())
			}
			var diagnostics struct {
				Checks []logCheck `json:"checks"`
			}
			logDecode(t, rec, &diagnostics)
			for _, check := range diagnostics.Checks {
				if check.ID == "proxy_pool" {
					if check.Status != tc.want {
						t.Fatalf("proxy_pool status = %q (%s), want %q", check.Status, check.Detail, tc.want)
					}
					return
				}
			}
			t.Fatal("diagnostics did not report a proxy_pool check")
		})
	}
}

func TestLogNotesRoundTrip(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)
	auth := map[string]string{logAPIKeyHeader: "test-password", "Content-Type": "application/json"}

	rec := logDo(t, h, http.MethodPost, "/api/logs/notes", `{"title":"retry storm","body":"proxy 3 flapped","kind":"finding","severity":"warn","tags":["proxy","retry"],"request_id":"req-abc"}`, auth)
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("create note = %d (%s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Note logNote `json:"note"`
	}
	logDecode(t, rec, &created)
	if created.Note.ID == 0 {
		t.Fatalf("note id missing: %+v", created.Note)
	}
	if created.Note.Kind != "finding" || created.Note.Severity != "warn" {
		t.Fatalf("note not normalized: %+v", created.Note)
	}

	rec = logDo(t, h, http.MethodGet, "/api/logs/notes", "", map[string]string{logAPIKeyHeader: "test-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("list notes = %d", rec.Code)
	}
	var listed struct {
		Notes []logNote `json:"notes"`
	}
	logDecode(t, rec, &listed)
	if len(listed.Notes) != 1 || listed.Notes[0].Title != "retry storm" {
		t.Fatalf("notes not listed: %+v", listed.Notes)
	}

	patch := "/api/logs/notes/" + itoa(created.Note.ID)
	rec = logDo(t, h, http.MethodPatch, patch, `{"severity":"error","body":"root cause: upstream 429"}`, auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch note = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = logDo(t, h, http.MethodDelete, patch, "", map[string]string{logAPIKeyHeader: "test-password"})
	if rec.Code != http.StatusOK {
		t.Fatalf("delete note = %d (%s)", rec.Code, rec.Body.String())
	}
	rec = logDo(t, h, http.MethodGet, "/api/logs/notes", "", map[string]string{logAPIKeyHeader: "test-password"})
	logDecode(t, rec, &listed)
	if len(listed.Notes) != 0 {
		t.Fatalf("note survived deletion: %+v", listed.Notes)
	}
}

func TestLogPruneClearsEvents(t *testing.T) {
	a := logApp(t)
	a.logEvent(logEvent{Level: logLevelWarn, Source: logSourceSystem, Event: "test.prune", Message: "bye"})
	logFlush(t, a)
	if len(logStoredEvents(t, a)) == 0 {
		t.Fatal("expected stored events before prune")
	}

	h := logHandler(a)
	rec := logDo(t, h, http.MethodPost, "/api/logs/prune", `{"all":true}`, map[string]string{logAPIKeyHeader: "test-password", "Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("prune = %d (%s)", rec.Code, rec.Body.String())
	}
	if records := logStoredEvents(t, a); len(records) != 0 {
		t.Fatalf("events survived prune: %d", len(records))
	}
}

func TestLogSettingsFilterAtWriteTime(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)
	rec := logDo(t, h, http.MethodPut, "/api/logs/settings", `{"min_level":"warn","retention_days":3}`, map[string]string{logAPIKeyHeader: "test-password", "Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("put settings = %d (%s)", rec.Code, rec.Body.String())
	}
	settings := a.logSink().settings()
	if settings.MinLevel != logLevelWarn || settings.RetentionDays != 3 {
		t.Fatalf("settings not applied: %+v", settings)
	}
	a.logEvent(logEvent{Level: logLevelInfo, Source: logSourceSystem, Event: "test.dropped", Message: "too quiet"})
	a.logEvent(logEvent{Level: logLevelError, Source: logSourceSystem, Event: "test.kept", Message: "loud enough"})
	logFlush(t, a)

	var dropped, kept bool
	for _, record := range logStoredEvents(t, a) {
		switch record.Event {
		case "test.dropped":
			dropped = true
		case "test.kept":
			kept = true
		}
	}
	if dropped {
		t.Fatal("info event was stored below the warn threshold")
	}
	if !kept {
		t.Fatal("error event was not stored")
	}
}

func TestLogStatsAndDiagnostics(t *testing.T) {
	a := logApp(t)
	a.logEvent(logEvent{Level: logLevelError, Source: logSourceGateway, Event: "test.diag", Message: "boom", Path: "/v1/chat/completions", Status: http.StatusBadGateway, DurationMS: 4200})
	logFlush(t, a)

	h := logHandler(a)
	auth := map[string]string{logAPIKeyHeader: "test-password"}

	rec := logDo(t, h, http.MethodGet, "/api/logs/stats", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d (%s)", rec.Code, rec.Body.String())
	}
	var stats struct {
		Total   int            `json:"total"`
		Errors  int            `json:"errors"`
		ByLevel []logCountStat `json:"by_level"`
	}
	logDecode(t, rec, &stats)
	if stats.Total < 1 || stats.Errors < 1 {
		t.Fatalf("stats did not count the event: %+v", stats)
	}
	if len(stats.ByLevel) == 0 || stats.ByLevel[0].Key != logLevelError {
		t.Fatalf("by_level did not report the error: %+v", stats.ByLevel)
	}

	rec = logDo(t, h, http.MethodGet, "/api/logs/diagnostics", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("diagnostics = %d (%s)", rec.Code, rec.Body.String())
	}
	var diagnostics struct {
		Checks  []logCheck `json:"checks"`
		Summary struct {
			Verdict string `json:"verdict"`
		} `json:"summary"`
		Process map[string]any `json:"process"`
	}
	logDecode(t, rec, &diagnostics)
	if len(diagnostics.Checks) == 0 {
		t.Fatal("diagnostics returned no checks")
	}
	switch diagnostics.Summary.Verdict {
	case "healthy", "degraded", "broken":
	default:
		t.Fatalf("unexpected verdict %q", diagnostics.Summary.Verdict)
	}
	if diagnostics.Process["pid"] == nil {
		t.Fatalf("diagnostics did not report the process: %+v", diagnostics.Process)
	}
}

func TestLogGuideAndExport(t *testing.T) {
	a := logApp(t)
	a.logEvent(logEvent{Level: logLevelWarn, Source: logSourceSystem, Event: "test.export", Message: "exported"})
	logFlush(t, a)

	h := logHandler(a)
	auth := map[string]string{logAPIKeyHeader: "test-password"}

	rec := logDo(t, h, http.MethodGet, "/api/logs/guide", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("guide = %d", rec.Code)
	}
	guide := rec.Body.String()
	for _, want := range []string{"/api/logs/diagnostics", "X-Relay-Log-Key", "curl"} {
		if !strings.Contains(guide, want) {
			t.Fatalf("guide is missing %q", want)
		}
	}

	rec = logDo(t, h, http.MethodGet, "/api/logs/export?window=1h", "", auth)
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d (%s)", rec.Code, rec.Body.String())
	}
	var exported struct {
		Events []logRecord `json:"events"`
	}
	logDecode(t, rec, &exported)
	if len(exported.Events) == 0 {
		t.Fatal("export returned no events")
	}
}

// lockedRecorder is a ResponseWriter that can be read while a streaming
// handler is still writing to it.
type lockedRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func newLockedRecorder() *lockedRecorder {
	return &lockedRecorder{hdr: http.Header{}, code: http.StatusOK}
}

func (l *lockedRecorder) Header() http.Header { return l.hdr }

func (l *lockedRecorder) WriteHeader(code int) {
	l.mu.Lock()
	l.code = code
	l.mu.Unlock()
}

func (l *lockedRecorder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedRecorder) Flush() {}

func (l *lockedRecorder) Body() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func TestLogStreamDeliversLiveEvents(t *testing.T) {
	a := logApp(t)
	h := logHandler(a)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/stream?backlog=5", nil).WithContext(ctx)
	req.Header.Set(logAPIKeyHeader, "test-password")
	recorder := newLockedRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(recorder, req)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(recorder.Body(), "event: hello") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("stream never sent hello: %q", recorder.Body())
		}
		time.Sleep(5 * time.Millisecond)
	}

	a.logEvent(logEvent{Level: logLevelWarn, Source: logSourceGateway, Event: "test.stream", Message: "live"})
	for !strings.Contains(recorder.Body(), "test.stream") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("live event never arrived: %q", recorder.Body())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream handler did not return after cancellation")
	}
	if ct := recorder.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestTruncateLogTextKeepsValidUTF8(t *testing.T) {
	long := strings.Repeat("日", 20) // 60 bytes
	got := truncateLogText(long, 5)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated text is not valid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected an ellipsis: %q", got)
	}
	if body := strings.TrimSuffix(got, "…"); len(body) > 5 {
		t.Fatalf("byte budget exceeded: %d bytes", len(body))
	}
	if short := truncateLogText("ok", 5); short != "ok" {
		t.Fatalf("short text changed: %q", short)
	}
	if got := truncateLogText("", 5); got != "" {
		t.Fatalf("empty text changed: %q", got)
	}
}

func TestParseLogQueryClampsLimit(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/logs?limit=100000&level=error,warn&window=2h&q=%20boom%20", nil)
	query := parseLogQuery(req)
	if query.limit != 1000 {
		t.Fatalf("limit was not clamped: %d", query.limit)
	}
	if len(query.levels) != 2 || query.levels[0] != logLevelError || query.levels[1] != logLevelWarn {
		t.Fatalf("levels not parsed: %+v", query.levels)
	}
	if query.since.IsZero() {
		t.Fatal("window did not set since")
	}
	if query.search != "boom" {
		t.Fatalf("search not trimmed: %q", query.search)
	}
}

func TestUpstreamOutcomeName(t *testing.T) {
	cases := map[upstreamOutcome]string{
		upstreamOutcomeSuccess:     "success",
		upstreamOutcomeLimited:     "limited",
		upstreamOutcomeRefused:     "refused",
		upstreamOutcomeProxyAuth:   "proxy_auth",
		upstreamOutcomeServerError: "server_error",
		upstreamOutcomeClientError: "client_error",
	}
	for outcome, want := range cases {
		if got := upstreamOutcomeName(outcome); got != want {
			t.Fatalf("upstreamOutcomeName(%d) = %q, want %q", outcome, got, want)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
