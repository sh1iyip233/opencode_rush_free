package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// compressionStub stands in for the operator's own summarisation API and records
// every request it receives so tests can assert the wire shape.
type compressionStub struct {
	mu       sync.Mutex
	paths    []string
	requests []map[string]any
	headers  []http.Header
	status   int
	reply    string
}

func newCompressionStub(t *testing.T, reply string) (*compressionStub, *httptest.Server) {
	t.Helper()
	stub := &compressionStub{status: http.StatusOK, reply: reply}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		payload := map[string]any{}
		_ = json.Unmarshal(raw, &payload)
		stub.mu.Lock()
		stub.paths = append(stub.paths, r.URL.Path)
		stub.requests = append(stub.requests, payload)
		stub.headers = append(stub.headers, r.Header.Clone())
		status, reply := stub.status, stub.reply
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(server.Close)
	return stub, server
}

func (s *compressionStub) setResponse(status int, reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.reply = reply
}

func (s *compressionStub) request(t *testing.T, index int) (map[string]any, http.Header) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if index >= len(s.requests) {
		t.Fatalf("summariser received %d requests, want at least %d", len(s.requests), index+1)
	}
	return s.requests[index], s.headers[index]
}

func (s *compressionStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func compressionTestConfig(baseURL string) compressionConfig {
	cfg := defaultCompressionConfig()
	cfg.Enabled = true
	cfg.BaseURL = baseURL
	cfg.APIKey = "sk-compress-test"
	cfg.Model = "summarizer-model"
	cfg.UseProxy = false
	cfg.TriggerRatio = 0.3
	cfg.ContextWindow = 2000
	cfg.KeepRecentMessages = 2
	cfg.MinFoldMessages = 2
	cfg.MaxSummaryTokens = 256
	cfg.TimeoutSeconds = 10
	return cfg
}

func saveCompressionConfig(t *testing.T, a *App, cfg compressionConfig) {
	t.Helper()
	normalized, err := normalizeCompressionConfig(cfg)
	if err != nil {
		t.Fatalf("compression config rejected: %v", err)
	}
	if err := a.saveCompression(normalized); err != nil {
		t.Fatalf("save compression config: %v", err)
	}
}

// compressionConversation builds a system pin plus alternating long turns and a
// trailing user question.
func compressionConversation(turns int) []any {
	messages := []any{map[string]any{"role": "system", "content": "你是运行在 Relay Desk 内的编码代理。"}}
	for i := 0; i < turns; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": "第" + strconv.Itoa(i) + "轮：请修改 cmd/server/main.go 的 gatewayChat，并保留回滚副本。" + strings.Repeat("细节", 60)},
			map[string]any{"role": "assistant", "content": "已定位 main.go:1721，准备先补测试再改代码。" + strings.Repeat("answer ", 80)},
		)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "最后一个问题：现在开始改。"})
	return messages
}

func compressionRequest(t *testing.T, header map[string]string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	return req
}

func TestEstimateTextTokensCountsWideRunesPerCharacter(t *testing.T) {
	if got := estimateTextTokens(""); got != 0 {
		t.Fatalf("empty text estimated %d tokens", got)
	}
	if got := estimateTextTokens("abcd"); got != 1 {
		t.Fatalf("four ASCII characters estimated %d tokens, want 1", got)
	}
	if got := estimateTextTokens("测试"); got != 2 {
		t.Fatalf("two CJK characters estimated %d tokens, want 2", got)
	}
	if got := estimateTextTokens("ab测试"); got != 3 {
		t.Fatalf("mixed text estimated %d tokens, want 3", got)
	}
}

func TestPlanCompressionPinsSystemAndKeepsRecentTurns(t *testing.T) {
	cfg := compressionTestConfig("http://summariser.example.test")
	messages := compressionConversation(3)
	plan, ok := planCompression(cfg, messages, true, cfg.ContextWindow)
	if !ok {
		t.Fatal("forced plan was rejected")
	}
	if len(plan.Head) != 1 || plan.Head[0].(map[string]any)["role"] != "system" {
		t.Fatalf("system pin was not preserved: %#v", plan.Head)
	}
	if len(plan.Tail) != cfg.KeepRecentMessages {
		t.Fatalf("tail kept %d messages, want %d", len(plan.Tail), cfg.KeepRecentMessages)
	}
	if len(plan.Folding)+len(plan.Tail)+len(plan.Head) != len(messages) {
		t.Fatalf("plan lost messages: head=%d folding=%d tail=%d total=%d", len(plan.Head), len(plan.Folding), len(plan.Tail), len(messages))
	}
	if plan.Before != estimateMessagesTokens(messages) {
		t.Fatalf("plan token estimate = %d, want %d", plan.Before, estimateMessagesTokens(messages))
	}

	if _, ok := planCompression(cfg, messages, false, 1<<20); ok {
		t.Fatal("plan was produced below the trigger threshold")
	}
}

func TestPlanCompressionStopsAtNonTextTurns(t *testing.T) {
	cfg := compressionTestConfig("http://summariser.example.test")
	cfg.MinFoldMessages = 1
	messages := []any{
		map[string]any{"role": "system", "content": "pin"},
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": "calling a tool", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function"}}},
		map[string]any{"role": "user", "content": "second"},
		map[string]any{"role": "assistant", "content": "third"},
		map[string]any{"role": "user", "content": "fourth"},
	}
	plan, ok := planCompression(cfg, messages, true, cfg.ContextWindow)
	if !ok {
		t.Fatal("forced plan was rejected")
	}
	if len(plan.Folding) != 1 {
		t.Fatalf("folded %d messages, want only the leading text turn", len(plan.Folding))
	}
	if len(plan.Tail) != 4 {
		t.Fatalf("tail = %d, want 4", len(plan.Tail))
	}
}

func TestCompressionSettingsRoundTrip(t *testing.T) {
	a := testApp(t)
	put := httptest.NewRecorder()
	body := `{"enabled":true,"api_style":"openai","base_url":"https://api.example.test/v1","api_key":"sk-secret-value","model":"summarizer","trigger_ratio":0.6,"context_window":128000,"keep_recent_messages":6,"min_fold_messages":3,"max_summary_tokens":800,"temperature":0.3,"timeout_seconds":45,"summary_role":"user","marker":"[{count}] {before}->{after}","prompt":"压缩它","fail_open":false,"use_proxy":false}`
	a.putCompressionSettings(put, httptest.NewRequest(http.MethodPut, "/api/settings/compression", strings.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("put compression settings: %d %s", put.Code, put.Body.String())
	}
	if strings.Contains(put.Body.String(), "sk-secret-value") {
		t.Fatal("api key leaked in the settings response")
	}

	get := httptest.NewRecorder()
	a.getCompressionSettings(get, httptest.NewRequest(http.MethodGet, "/api/settings/compression", nil))
	if get.Code != http.StatusOK {
		t.Fatalf("get compression settings: %d %s", get.Code, get.Body.String())
	}
	var view compressionSettingsView
	if err := json.Unmarshal(get.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Enabled || view.Model != "summarizer" || view.TriggerRatio != 0.6 || view.SummaryRole != "user" {
		t.Fatalf("settings did not round-trip: %#v", view)
	}
	if view.FailOpen || view.UseProxy {
		t.Fatalf("boolean settings did not round-trip: %#v", view)
	}
	if !view.HasAPIKey || !view.Configured {
		t.Fatalf("view did not report the stored key: %#v", view)
	}
	if view.DefaultMarker == "" || view.DefaultPrompt == "" {
		t.Fatal("default marker or prompt missing from the view")
	}

	// The stored key stays encrypted and is decrypted on load.
	loaded, err := a.loadCompression()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "sk-secret-value" {
		t.Fatalf("stored api key did not decrypt: %q", loaded.APIKey)
	}

	clearKey := httptest.NewRecorder()
	a.putCompressionSettings(clearKey, httptest.NewRequest(http.MethodPut, "/api/settings/compression", strings.NewReader(`{"clear_api_key":true}`)))
	if clearKey.Code != http.StatusOK {
		t.Fatalf("clear api key: %d %s", clearKey.Code, clearKey.Body.String())
	}
	loaded, err = a.loadCompression()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIKey != "" {
		t.Fatalf("api key was not cleared: %q", loaded.APIKey)
	}

	invalid := httptest.NewRecorder()
	a.putCompressionSettings(invalid, httptest.NewRequest(http.MethodPut, "/api/settings/compression", strings.NewReader(`{"trigger_ratio":0.99}`)))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range trigger ratio accepted: %d %s", invalid.Code, invalid.Body.String())
	}

	missing := httptest.NewRecorder()
	a.putCompressionSettings(missing, httptest.NewRequest(http.MethodPut, "/api/settings/compression", strings.NewReader(`{"enabled":true,"base_url":"","model":""}`)))
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("enabled gateway without base_url/model accepted: %d %s", missing.Code, missing.Body.String())
	}
}

func TestApplyCompressionFoldsHistory(t *testing.T) {
	a := testApp(t)
	stub, summariser := newCompressionStub(t, `{"choices":[{"message":{"content":"压缩摘要：用户要求修改 gatewayChat 并保留回滚副本。"}}],"usage":{"prompt_tokens":120,"completion_tokens":20,"total_tokens":140}}`)
	saveCompressionConfig(t, a, compressionTestConfig(summariser.URL))

	messages := compressionConversation(4)
	payload := map[string]any{"model": "text-model:free", "messages": messages}
	outcome := a.applyCompression(compressionRequest(t, nil), "session-alpha", "text-model:free", payload)
	if outcome.Status != "applied" || !outcome.Applied {
		t.Fatalf("compression did not apply: %#v", outcome)
	}
	if stub.count() != 1 {
		t.Fatalf("summariser called %d times, want 1", stub.count())
	}
	if outcome.TokensAfter >= outcome.TokensBefore {
		t.Fatalf("compression did not shrink the payload: before=%d after=%d", outcome.TokensBefore, outcome.TokensAfter)
	}

	request, headers := stub.request(t, 0)
	if request["model"] != "summarizer-model" {
		t.Fatalf("summariser model = %#v", request["model"])
	}
	if request["stream"] != false {
		t.Fatalf("summariser request must not stream: %#v", request["stream"])
	}
	if got := headers.Get("Authorization"); got != "Bearer sk-compress-test" {
		t.Fatalf("summariser authorization = %q", got)
	}
	promptMessages, ok := request["messages"].([]any)
	if !ok || len(promptMessages) != 2 {
		t.Fatalf("summariser prompt shape = %#v", request["messages"])
	}
	first := promptMessages[0].(map[string]any)
	if first["role"] != "system" || !strings.Contains(first["content"].(string), "压缩器") {
		t.Fatalf("summariser system prompt = %#v", first)
	}
	transcript := promptMessages[1].(map[string]any)["content"].(string)
	if !strings.Contains(transcript, "第0轮") || !strings.Contains(transcript, "需要压缩的对话记录") {
		t.Fatalf("summariser transcript missing folded turns: %q", transcript)
	}

	composed, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("payload messages replaced with %T", payload["messages"])
	}
	if len(composed) != 1+1+2 {
		t.Fatalf("composed conversation = %d messages, want pin + summary + 2 recent", len(composed))
	}
	summary := composed[1].(map[string]any)
	if summary["role"] != "system" {
		t.Fatalf("summary role = %#v", summary["role"])
	}
	summaryText := summary["content"].(string)
	if !strings.Contains(summaryText, "📦") || !strings.Contains(summaryText, "压缩摘要：用户要求修改 gatewayChat") {
		t.Fatalf("summary message missing marker or summary: %q", summaryText)
	}
	if strings.Contains(summaryText, "{count}") || strings.Contains(summaryText, "{before}") {
		t.Fatalf("marker placeholders were not expanded: %q", summaryText)
	}
	if last := composed[len(composed)-1].(map[string]any); last["content"] != "最后一个问题：现在开始改。" {
		t.Fatalf("last user turn was not preserved: %#v", last)
	}

	var requestKind, model, status string
	var totalTokens int64
	if err := a.db.QueryRow("SELECT request_kind,model,status,total_tokens FROM usage_requests WHERE request_kind='compression' ORDER BY id DESC LIMIT 1").Scan(&requestKind, &model, &status, &totalTokens); err != nil {
		t.Fatal(err)
	}
	if model != "summarizer-model" || status != "success" || totalTokens != 140 {
		t.Fatalf("compression usage row = %q/%q/%q/%d", requestKind, model, status, totalTokens)
	}

	var folded int64
	if err := a.db.QueryRow("SELECT folded_messages FROM compression_events WHERE session_key='session-alpha' AND status='success'").Scan(&folded); err != nil {
		t.Fatal(err)
	}
	if folded != int64(outcome.Messages) {
		t.Fatalf("event folded_messages = %d, want %d", folded, outcome.Messages)
	}

	recorder := httptest.NewRecorder()
	setCompressionHeaders(recorder, outcome)
	if got := recorder.Header().Get("X-Relay-Compression"); got != "applied" {
		t.Fatalf("X-Relay-Compression = %q", got)
	}
	if got := recorder.Header().Get("X-Relay-Compression-Messages"); got != strconv.Itoa(outcome.Messages) {
		t.Fatalf("X-Relay-Compression-Messages = %q", got)
	}
}

func TestApplyCompressionExtendsStoredSummary(t *testing.T) {
	a := testApp(t)
	stub, summariser := newCompressionStub(t, `{"choices":[{"message":{"content":"第一版摘要"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	saveCompressionConfig(t, a, compressionTestConfig(summariser.URL))

	first := compressionConversation(3)
	firstPayload := map[string]any{"messages": first}
	if outcome := a.applyCompression(compressionRequest(t, nil), "session-beta", "text-model:free", firstPayload); !outcome.Applied {
		t.Fatalf("first compression did not apply: %#v", outcome)
	}
	firstFolded := len(compressionConversation(3)) - 1 - 2 // pin + keep_recent

	extended := append(compressionConversation(3), map[string]any{"role": "assistant", "content": "已补上测试。"}, map[string]any{"role": "user", "content": "继续。"})
	secondPayload := map[string]any{"messages": extended}
	outcome := a.applyCompression(compressionRequest(t, nil), "session-beta", "text-model:free", secondPayload)
	if !outcome.Applied {
		t.Fatalf("second compression did not apply: %#v", outcome)
	}
	if stub.count() != 2 {
		t.Fatalf("summariser called %d times, want 2", stub.count())
	}
	request, _ := stub.request(t, 1)
	promptMessages := request["messages"].([]any)
	transcript := promptMessages[1].(map[string]any)["content"].(string)
	if !strings.Contains(transcript, "已折叠的既有摘要") || !strings.Contains(transcript, "第一版摘要") {
		t.Fatalf("incremental request did not carry the stored summary: %q", transcript)
	}
	if strings.Contains(transcript, "第0轮") {
		t.Fatalf("incremental request re-sent already folded turns: %q", transcript)
	}
	if !strings.Contains(transcript, "最后一个问题") {
		t.Fatalf("incremental request missed the new delta: %q", transcript)
	}

	var foldedMessages, compressions int64
	var summary string
	if err := a.db.QueryRow("SELECT folded_messages,compressions,summary FROM compression_sessions WHERE session_key='session-beta'").Scan(&foldedMessages, &compressions, &summary); err != nil {
		t.Fatal(err)
	}
	if compressions != 2 {
		t.Fatalf("session compressions = %d, want 2", compressions)
	}
	if foldedMessages <= int64(firstFolded) {
		t.Fatalf("session folded_messages did not grow: %d", foldedMessages)
	}
	if summary != "第一版摘要" {
		t.Fatalf("session summary = %q", summary)
	}

	// A conversation that rewinds the history must not reuse the stored summary.
	rewound := map[string]any{"messages": compressionConversation(2)}
	if outcome := a.applyCompression(compressionRequest(t, nil), "session-beta", "text-model:free", rewound); !outcome.Applied {
		t.Fatalf("rewound compression did not apply: %#v", outcome)
	}
	request, _ = stub.request(t, 2)
	if strings.Contains(request["messages"].([]any)[1].(map[string]any)["content"].(string), "已折叠的既有摘要") {
		t.Fatal("stored summary was reused for a diverged history")
	}
}

func TestApplyCompressionHonoursClientHeader(t *testing.T) {
	a := testApp(t)
	stub, summariser := newCompressionStub(t, `{"choices":[{"message":{"content":"强制压缩摘要"}}]}`)
	saveCompressionConfig(t, a, compressionTestConfig(summariser.URL))

	disabled := map[string]any{"messages": compressionConversation(4)}
	outcome := a.applyCompression(compressionRequest(t, map[string]string{"X-Relay-Compression": "off"}), "session-gamma", "text-model:free", disabled)
	if outcome.Status != "disabled" || outcome.Applied {
		t.Fatalf("off header did not disable compression: %#v", outcome)
	}
	if stub.count() != 0 {
		t.Fatal("disabled request still called the summariser")
	}

	// A short conversation is normally skipped, but force overrides the threshold.
	short := []any{
		map[string]any{"role": "user", "content": "one"},
		map[string]any{"role": "assistant", "content": "two"},
		map[string]any{"role": "user", "content": "three"},
		map[string]any{"role": "assistant", "content": "four"},
		map[string]any{"role": "user", "content": "five"},
	}
	payload := map[string]any{"messages": short}
	if outcome := a.applyCompression(compressionRequest(t, nil), "session-gamma", "text-model:free", payload); outcome.Status != "skipped" || outcome.Reason != "below_threshold" {
		t.Fatalf("short conversation was not skipped: %#v", outcome)
	}
	payload = map[string]any{"messages": short}
	if outcome := a.applyCompression(compressionRequest(t, map[string]string{"X-Relay-Compression": "force"}), "session-gamma", "text-model:free", payload); !outcome.Applied {
		t.Fatalf("force header did not compress: %#v", outcome)
	}
}

func TestApplyCompressionFailOpenAndFailClosed(t *testing.T) {
	a := testApp(t)
	stub, failing := newCompressionStub(t, `{"error":"summariser offline"}`)
	stub.setResponse(http.StatusInternalServerError, `{"error":"summariser offline"}`)
	saveCompressionConfig(t, a, compressionTestConfig(failing.URL))

	payload := map[string]any{"messages": compressionConversation(4)}
	original := len(payload["messages"].([]any))
	outcome := a.applyCompression(compressionRequest(t, nil), "session-delta", "text-model:free", payload)
	if outcome.Status != "failed" || outcome.Applied {
		t.Fatalf("failing summariser was not reported: %#v", outcome)
	}
	if outcome.Blocking {
		t.Fatal("fail_open=true must not block the request")
	}
	if len(payload["messages"].([]any)) != original {
		t.Fatal("fail_open=true mutated the request payload")
	}

	closed := compressionTestConfig(failing.URL)
	closed.FailOpen = false
	saveCompressionConfig(t, a, closed)
	payload = map[string]any{"messages": compressionConversation(4)}
	outcome = a.applyCompression(compressionRequest(t, nil), "session-delta", "text-model:free", payload)
	if outcome.Status != "failed" || !outcome.Blocking {
		t.Fatalf("fail_open=false did not block: %#v", outcome)
	}

	var failures int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM compression_events WHERE session_key='session-delta' AND status='error'").Scan(&failures); err != nil {
		t.Fatal(err)
	}
	if failures != 2 {
		t.Fatalf("recorded %d failed compression events, want 2", failures)
	}
}

func TestCompressionAnthropicRequestShape(t *testing.T) {
	a := testApp(t)
	stub, summariser := newCompressionStub(t, `{"content":[{"type":"text","text":"Anthropic 摘要"}],"usage":{"input_tokens":40,"output_tokens":9}}`)
	cfg := compressionTestConfig(summariser.URL)
	cfg.APIStyle = "anthropic"
	saveCompressionConfig(t, a, cfg)

	payload := map[string]any{"messages": compressionConversation(4)}
	outcome := a.applyCompression(compressionRequest(t, nil), "session-epsilon", "text-model:free", payload)
	if !outcome.Applied {
		t.Fatalf("anthropic compression did not apply: %#v", outcome)
	}
	stub.mu.Lock()
	path := stub.paths[0]
	stub.mu.Unlock()
	if path != "/v1/messages" {
		t.Fatalf("anthropic request path = %q", path)
	}
	request, headers := stub.request(t, 0)
	if got := headers.Get("x-api-key"); got != "sk-compress-test" {
		t.Fatalf("anthropic x-api-key = %q", got)
	}
	if got := headers.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	if headers.Get("Authorization") != "" {
		t.Fatal("anthropic request carried a bearer token")
	}
	if request["system"] != cfg.Prompt {
		t.Fatalf("anthropic system prompt = %#v", request["system"])
	}
	if _, ok := request["messages"].([]any); !ok {
		t.Fatalf("anthropic messages = %#v", request["messages"])
	}

	var totalTokens int64
	if err := a.db.QueryRow("SELECT total_tokens FROM usage_requests WHERE request_kind='compression' ORDER BY id DESC LIMIT 1").Scan(&totalTokens); err != nil {
		t.Fatal(err)
	}
	if totalTokens != 49 {
		t.Fatalf("anthropic usage total = %d, want 49", totalTokens)
	}
}

func TestCompressionAdminEndpoints(t *testing.T) {
	a := testApp(t)
	_, summariser := newCompressionStub(t, `{"choices":[{"message":{"content":"测试摘要"}}],"usage":{"prompt_tokens":30,"completion_tokens":6,"total_tokens":36}}`)
	cfg := compressionTestConfig(summariser.URL)
	saveCompressionConfig(t, a, cfg)

	probe := httptest.NewRecorder()
	a.testCompressionSettings(probe, httptest.NewRequest(http.MethodPost, "/api/settings/compression/test", strings.NewReader(`{}`)))
	if probe.Code != http.StatusOK {
		t.Fatalf("compression test endpoint: %d %s", probe.Code, probe.Body.String())
	}
	var probeBody map[string]any
	if err := json.Unmarshal(probe.Body.Bytes(), &probeBody); err != nil {
		t.Fatal(err)
	}
	if probeBody["ok"] != true || probeBody["summary"] != "测试摘要" || probeBody["api_style"] != "openai" {
		t.Fatalf("probe response = %#v", probeBody)
	}
	if probeBody["endpoint"] != summariser.URL+"/v1/chat/completions" {
		t.Fatalf("probe endpoint = %#v", probeBody["endpoint"])
	}

	badProbe := httptest.NewRecorder()
	a.testCompressionSettings(badProbe, httptest.NewRequest(http.MethodPost, "/api/settings/compression/test", strings.NewReader(`{"base_url":"http://127.0.0.1:1"}`)))
	if badProbe.Code != http.StatusBadGateway {
		t.Fatalf("failing probe returned %d %s", badProbe.Code, badProbe.Body.String())
	}

	preview := httptest.NewRecorder()
	previewBody, err := json.Marshal(map[string]any{"model": "text-model:free", "messages": compressionConversation(4), "force": true})
	if err != nil {
		t.Fatal(err)
	}
	a.previewCompression(preview, httptest.NewRequest(http.MethodPost, "/api/compression/preview", bytes.NewReader(previewBody)))
	if preview.Code != http.StatusOK {
		t.Fatalf("preview endpoint: %d %s", preview.Code, preview.Body.String())
	}
	var previewResult struct {
		Applied       bool  `json:"applied"`
		Folded        int   `json:"folded_messages"`
		Kept          int   `json:"kept_messages"`
		TokensBefore  int   `json:"tokens_before"`
		TokensAfter   int   `json:"tokens_after"`
		SummaryTokens int   `json:"summary_tokens"`
		Messages      []any `json:"messages"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewResult); err != nil {
		t.Fatal(err)
	}
	if !previewResult.Applied || previewResult.Folded == 0 || previewResult.Kept != 2 {
		t.Fatalf("preview result = %#v", previewResult)
	}
	if previewResult.TokensAfter >= previewResult.TokensBefore {
		t.Fatalf("preview did not shrink tokens: %#v", previewResult)
	}
	if len(previewResult.Messages) != 1+1+previewResult.Kept {
		t.Fatalf("preview composed %d messages, want pin + summary + %d kept", len(previewResult.Messages), previewResult.Kept)
	}

	// Apply one compression so stats and sessions have a row to report.
	payload := map[string]any{"messages": compressionConversation(4)}
	if outcome := a.applyCompression(compressionRequest(t, nil), "session-zeta", "text-model:free", payload); !outcome.Applied {
		t.Fatalf("compression did not apply: %#v", outcome)
	}

	stats := httptest.NewRecorder()
	a.compressionStats(stats, httptest.NewRequest(http.MethodGet, "/api/compression/stats?days=7", nil))
	if stats.Code != http.StatusOK {
		t.Fatalf("stats endpoint: %d %s", stats.Code, stats.Body.String())
	}
	var statsBody struct {
		Days         int   `json:"days"`
		Compressions int64 `json:"compressions"`
		Failures     int64 `json:"failures"`
		SavedTokens  int64 `json:"saved_tokens"`
		Sessions     int64 `json:"sessions"`
		Daily        []struct {
			Date         string `json:"date"`
			Compressions int64  `json:"compressions"`
		} `json:"daily"`
		Recent []struct {
			Status       string `json:"status"`
			SessionKey   string `json:"session_key"`
			SavedTokens  int64  `json:"saved_tokens"`
			SummaryModel string `json:"summary_model"`
		} `json:"recent"`
	}
	if err := json.Unmarshal(stats.Body.Bytes(), &statsBody); err != nil {
		t.Fatal(err)
	}
	if statsBody.Days != 7 || statsBody.Compressions < 1 || statsBody.Sessions != 1 {
		t.Fatalf("stats body = %#v", statsBody)
	}
	if len(statsBody.Daily) == 0 || len(statsBody.Recent) == 0 || statsBody.Recent[0].Status != "success" {
		t.Fatalf("stats detail = %#v", statsBody)
	}
	if statsBody.Recent[0].SummaryModel != "summarizer-model" {
		t.Fatalf("stats summary model = %q", statsBody.Recent[0].SummaryModel)
	}

	sessions := httptest.NewRecorder()
	a.listCompressionSessions(sessions, httptest.NewRequest(http.MethodGet, "/api/compression/sessions", nil))
	if sessions.Code != http.StatusOK {
		t.Fatalf("sessions endpoint: %d %s", sessions.Code, sessions.Body.String())
	}
	var sessionList struct {
		Sessions []compressionSessionRow `json:"sessions"`
	}
	if err := json.Unmarshal(sessions.Body.Bytes(), &sessionList); err != nil {
		t.Fatal(err)
	}
	if len(sessionList.Sessions) != 1 || sessionList.Sessions[0].SessionKey != "session-zeta" {
		t.Fatalf("session list = %#v", sessionList.Sessions)
	}
	if !strings.Contains(sessionList.Sessions[0].Preview, "测试摘要") {
		t.Fatalf("session preview = %q", sessionList.Sessions[0].Preview)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/compression/sessions/session-zeta", nil)
	deleteReq.SetPathValue("key", "session-zeta")
	deleted := httptest.NewRecorder()
	a.deleteCompressionSession(deleted, deleteReq)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete session: %d %s", deleted.Code, deleted.Body.String())
	}

	reset := httptest.NewRecorder()
	a.resetCompressionSessions(reset, httptest.NewRequest(http.MethodPost, "/api/compression/sessions/reset", nil))
	if reset.Code != http.StatusOK {
		t.Fatalf("reset sessions: %d %s", reset.Code, reset.Body.String())
	}
	var remaining int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM compression_sessions").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d compression sessions survived the reset", remaining)
	}
}

// TestGatewayAppliesCompression exercises the real gateway path: a chat request
// crossing the trigger threshold is rewritten before it reaches the lane.
func TestGatewayAppliesCompression(t *testing.T) {
	a := testApp(t)
	_, summariser := newCompressionStub(t, `{"choices":[{"message":{"content":"压缩摘要：请求给 gatewayChat 增加压缩网关。"}}],"usage":{"prompt_tokens":60,"completion_tokens":12,"total_tokens":72}}`)

	var laneBody map[string]any
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		payload := map[string]any{}
		_ = json.Unmarshal(raw, &payload)
		laneBody = payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-compress","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`))
	}))
	defer lane.Close()
	pointLaneAt(t, lane.URL)

	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{"base_url":"`+lane.URL+`"}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d %s", put.Code, put.Body.String())
	}
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)",
		"text-model:free", "Text model", 1, "test", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// The chat lane needs one usable exit; pointing it at the lane stub keeps the
	// test local (an HTTP proxy receives the absolute-form request).
	proxy, err := parseProxy(lane.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.insertProxy(proxy); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('client_key',?)", hashToken("test-client")); err != nil {
		t.Fatal(err)
	}
	saveCompressionConfig(t, a, compressionTestConfig(summariser.URL))

	requestBody, err := json.Marshal(map[string]any{"model": "text-model:free", "messages": compressionConversation(5)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(requestBody))
	req.Header.Set("Authorization", "Bearer test-client")
	response := httptest.NewRecorder()
	a.gatewayChat(response, req)

	if response.Code != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Relay-Compression"); got != "applied" {
		t.Fatalf("X-Relay-Compression = %q", got)
	}
	if got := response.Header().Get("X-Relay-Compression-Model"); got != "summarizer-model" {
		t.Fatalf("X-Relay-Compression-Model = %q", got)
	}
	messages, ok := laneBody["messages"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("lane payload messages = %#v", laneBody["messages"])
	}
	found := false
	for _, message := range messages {
		entry, ok := message.(map[string]any)
		if !ok {
			continue
		}
		content, _ := entry["content"].(string)
		if strings.Contains(content, "📦") && strings.Contains(content, "压缩摘要") {
			found = true
		}
	}
	if !found {
		t.Fatalf("lane payload carried no compressed summary: %#v", messages)
	}
	if len(messages) != 1+1+2 {
		t.Fatalf("lane payload = %d messages, want pin + summary + 2 recent", len(messages))
	}
	var usageRows int64
	if err := a.db.QueryRow("SELECT COUNT(*) FROM usage_requests WHERE request_kind='compression' AND status='success'").Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if usageRows != 1 {
		t.Fatalf("compression usage rows = %d, want 1", usageRows)
	}
}
