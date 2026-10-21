package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMostlyLimitedClassifiesChains(t *testing.T) {
	cases := []struct {
		name     string
		limited  int
		attempts int
		want     bool
	}{
		{"no attempts", 0, 0, false},
		{"all limited", 10, 10, true},
		{"exactly half", 5, 10, true},
		{"one short of half", 4, 9, false},
		{"single limited attempt", 1, 1, true},
		{"limited minority", 1, 4, false},
	}
	for _, tc := range cases {
		if got := mostlyLimited(tc.limited, tc.attempts); got != tc.want {
			t.Fatalf("%s: mostlyLimited(%d, %d) = %v, want %v", tc.name, tc.limited, tc.attempts, got, tc.want)
		}
	}
}

func TestTerminalResinAnswerUpgradesALimitedChain(t *testing.T) {
	status, origin, waitSeconds, ok := terminalResinAnswer(6, 7, 0)
	if !ok || status != http.StatusTooManyRequests || origin != "upstream" {
		t.Fatalf("limited chain answered status=%d origin=%q ok=%v, want 429 upstream", status, origin, ok)
	}
	if waitSeconds < 5 {
		t.Fatalf("Retry-After = %ds, want at least the 5s floor", waitSeconds)
	}

	// The breaker's remaining cooldown wins when it is longer than the floor.
	if _, _, waitSeconds, _ := terminalResinAnswer(6, 7, 20*time.Second); waitSeconds != 20 {
		t.Fatalf("Retry-After = %ds, want the breaker's 20s", waitSeconds)
	}

	// A chain that broke on links, not on limits, keeps the transport answer.
	if _, _, _, ok := terminalResinAnswer(1, 7, 20*time.Second); ok {
		t.Fatal("a mostly-broken chain was reported as rate limited")
	}
}

// TestForwardHeaderDeadlineCutsAStalledHeaderPhase covers the attempt that never
// answers at all: without a deadline it could hold the request for the transport
// timeout (120s) long after the retry budget was spent.
func TestForwardHeaderDeadlineCutsAStalledHeaderPhase(t *testing.T) {
	a := testApp(t)
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer stalled.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	started := time.Now()
	_, err := a.forwardWithHeaderDeadline(request, []byte(`{}`), upstreamConfig{BaseURL: stalled.URL}, ProxyRecord{}, "", 100*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a stalled header phase was not cut")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled header error = %v, want a deadline", err)
	}
	if elapsed > 350*time.Millisecond {
		t.Fatalf("the attempt ran %s, want it cut near the 100ms deadline", elapsed)
	}
}

// TestForwardHeaderDeadlineLeavesAStartedStreamIntact is the property that makes
// a context deadline wrong here: once headers arrive the deadline must be
// disarmed, because a healthy answer can stream for longer than the whole retry
// budget and truncating it would corrupt a successful response.
func TestForwardHeaderDeadlineLeavesAStartedStreamIntact(t *testing.T) {
	a := testApp(t)
	streaming := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			if _, err := io.WriteString(w, "data: chunk\n\n"); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(60 * time.Millisecond)
		}
	}))
	defer streaming.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := a.forwardWithHeaderDeadline(request, []byte(`{}`), upstreamConfig{BaseURL: streaming.URL}, ProxyRecord{}, "", 80*time.Millisecond)
	if err != nil {
		t.Fatalf("a prompt upstream was rejected: %v", err)
	}
	defer resp.Body.Close()
	captured, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.Fatalf("the stream was cut after its headers arrived: %v", readErr)
	}
	if got := strings.Count(string(captured), "chunk"); got != 5 {
		t.Fatalf("read %d of 5 chunks: the header deadline truncated the body", got)
	}
}

// TestGatewayAnswers429WhenTheChainWasMostlyLimited replays the shape every
// production failure had: several 429s and then a transport error on the last
// attempt. The caller was told 502, so it looked for a broken gateway while the
// pool was merely out of quota.
func TestGatewayAnswers429WhenTheChainWasMostlyLimited(t *testing.T) {
	a := testApp(t)
	var mu sync.Mutex
	seen := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		mu.Lock()
		seen++
		attempt := seen
		mu.Unlock()
		if attempt < resinMaxAttempts {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		// The last exit loses the tunnel before the upstream answers.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer proxy.Close()

	pointLaneAt(t, "http://upstream.example.test")
	upstream := httptest.NewRecorder()
	a.putUpstream(upstream, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{"base_url":"http://upstream.example.test"}`)))
	if upstream.Code != http.StatusOK {
		t.Fatalf("configure upstream failed: %d %s", upstream.Code, upstream.Body.String())
	}
	engine := httptest.NewRecorder()
	a.putProxyEngine(engine, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(`{"engine":"resin","resin_gateway_url":"`+proxy.URL+`","resin_platform":"Default","resin_proxy_token":"proxy-token"}`)))
	if engine.Code != http.StatusOK {
		t.Fatalf("configure Resin failed: %d %s", engine.Code, engine.Body.String())
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "limited:free", "Limited", 1, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE settings SET value=? WHERE key='client_key'", hashToken("limited-test-client")); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateLegacyClientKey(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"limited:free","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer limited-test-client")
	response := httptest.NewRecorder()
	a.gatewayChat(response, request)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("a mostly-limited chain answered %d, want 429: %s", response.Code, response.Body.String())
	}
	if retryAfter := response.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatal("a 429 answer carried no Retry-After")
	}
	if !strings.Contains(response.Body.String(), "rate limited") {
		t.Fatalf("body did not explain the limit: %s", response.Body.String())
	}

	var statusCode int
	var origin string
	if err := a.db.QueryRow("SELECT status_code,error_origin FROM usage_requests WHERE request_kind='chat' ORDER BY id DESC LIMIT 1").Scan(&statusCode, &origin); err != nil {
		t.Fatal(err)
	}
	if statusCode != http.StatusTooManyRequests || origin != "upstream" {
		t.Fatalf("usage row recorded code=%d origin=%q, want 429 upstream", statusCode, origin)
	}
}

// TestResinHeaderDeadlineNeverOutlivesTheBudget pins the two bounds on the header
// phase. A 45s budget must not let an attempt run past the budget, and a budget
// larger than one attempt could usefully take must not let a single stalled exit
// hold the caller for the transport's 120s header timeout.
func TestResinHeaderDeadlineNeverOutlivesTheBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		remaining time.Duration
		want      time.Duration
	}{
		{"fresh budget equals the ceiling", 45 * time.Second, 45 * time.Second},
		{"late attempt gets only what is left", 15 * time.Second, 15 * time.Second},
		{"a large budget is capped per attempt", 180 * time.Second, resinAttemptCeiling},
		{"the last sliver of budget is respected", 4 * time.Second, 4 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resinHeaderDeadline(tc.remaining); got != tc.want {
				t.Fatalf("resinHeaderDeadline(%s) = %s, want %s", tc.remaining, got, tc.want)
			}
		})
	}
}
