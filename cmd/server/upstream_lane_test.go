package main

import (
	"encoding/json"
	"math/big"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCanonicalSessionIDShape(t *testing.T) {
	now := time.UnixMilli(1790000000000)
	first := canonicalSessionID("conversation-a", now)
	if !canonicalSessionPattern(first) {
		t.Fatalf("session id %q does not match the ses_ shape", first)
	}
	if again := canonicalSessionID("conversation-a", now); again != first {
		t.Fatalf("same signal minted two ids: %q vs %q", first, again)
	}
	if other := canonicalSessionID("conversation-b", now); other == first {
		t.Fatalf("distinct signals collided on %q", first)
	}
	// A client that already speaks the canonical shape is passed through.
	if passthrough := canonicalSessionID(first, now); passthrough != first {
		t.Fatalf("canonical id was re-minted: %q", passthrough)
	}

	// The clock half is a real inverted clock truncated to 48 bits (ids.ts
	// invertedTimestampHex). Upstream reads the low 12 bits back as a
	// per-millisecond counter and accepts only [0, 63]; the high bits are not
	// checked, so counter 1 inverting to ...ffe is the structural tell.
	hexPart := first[len("ses_") : len("ses_")+12]
	if !strings.HasSuffix(hexPart, "ffe") {
		t.Fatalf("counter-1 time segment %q does not end in ffe", hexPart)
	}
	decoded := new(big.Int)
	if _, ok := decoded.SetString(hexPart, 16); !ok {
		t.Fatalf("clock segment is not hex: %q", hexPart)
	}
	decoded.Xor(decoded, big.NewInt(0xffffffffffff))
	if counter := new(big.Int).And(decoded, big.NewInt(0xfff)).Int64(); counter != 1 {
		t.Fatalf("counter bits = %d, want 1", counter)
	}
	// Descending: a later clock sorts earlier as a plain string.
	later := canonicalSessionID("conversation-a", now.Add(time.Second))
	if later[len("ses_"):len("ses_")+12] >= hexPart {
		t.Fatalf("later clock %q did not sort before %q", later, hexPart)
	}
}

func TestLaneSessionIDIsStablePerConversation(t *testing.T) {
	a := testApp(t)
	first := a.laneSessionID("tenant\x00message\x00hello")
	if !canonicalSessionPattern(first) {
		t.Fatalf("session id %q is not canonical", first)
	}
	if again := a.laneSessionID("tenant\x00message\x00hello"); again != first {
		t.Fatalf("retry minted a new session id: %q vs %q", again, first)
	}
	if other := a.laneSessionID("tenant\x00message\x00goodbye"); other == first {
		t.Fatal("different conversations shared one session id")
	}
	// A signal-less request still needs a canonical id, and must not collide.
	signalLess := a.laneSessionID("")
	if !canonicalSessionPattern(signalLess) {
		t.Fatalf("fallback id %q is not canonical", signalLess)
	}
	if a.laneSessionID("") == signalLess {
		t.Fatal("two signal-less requests reused one session id")
	}

	// The memo is bounded so a long-lived gateway cannot leak memory.
	store := a.laneSessionStore()
	for i := 0; i < laneSessionMemoLimit+32; i++ {
		store.remember("filler-"+stringID(int64(i)), "ses_value")
	}
	store.mu.Lock()
	size := len(store.entries)
	store.mu.Unlock()
	if size > laneSessionMemoLimit {
		t.Fatalf("memo grew to %d entries, limit is %d", size, laneSessionMemoLimit)
	}
}

func TestLaneSessionSignalPrefersExplicitThenFirstUserMessage(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "be brief"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "what is a monad"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.test/a.png"}},
		}},
	}

	explicit := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	explicit.Header.Set("X-Relay-Session-ID", "chat-42")
	if got := laneSessionSignal(explicit, nil, messages, "tenant-a"); !strings.Contains(got, "chat-42") {
		t.Fatalf("explicit header was ignored: %q", got)
	}
	// Header spellings from real OpenCode clients are accepted too.
	for _, header := range []string{"X-OpenCode-Session-ID", "X-Session-ID"} {
		req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		req.Header.Set(header, "chat-42")
		if got := laneSessionSignal(req, nil, messages, "tenant-a"); !strings.Contains(got, "chat-42") {
			t.Fatalf("%s was ignored: %q", header, got)
		}
	}

	userField := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	if got := laneSessionSignal(userField, json.RawMessage(`"user-7"`), messages, "tenant-a"); !strings.Contains(got, "user-7") {
		t.Fatalf("user field was ignored: %q", got)
	}

	plain := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	derived := laneSessionSignal(plain, nil, messages, "tenant-a")
	if !strings.Contains(derived, "monad") {
		t.Fatalf("first user message was not used as the seed: %q", derived)
	}
	if strings.Contains(derived, "be brief") {
		t.Fatalf("system message leaked into the seed: %q", derived)
	}
	// The same prompt from another tenant must not share the upstream session.
	if otherTenant := laneSessionSignal(plain, nil, messages, "tenant-b"); otherTenant == derived {
		t.Fatal("two tenants shared one upstream session")
	}
	// Later turns reuse the opening message, so the seed is stable.
	later := []any{messages[0], messages[1], map[string]any{"role": "assistant", "content": "an abstraction"}, map[string]any{"role": "user", "content": "and a functor?"}}
	if again := laneSessionSignal(plain, nil, later, "tenant-a"); again != derived {
		t.Fatalf("second turn changed the seed: %q vs %q", again, derived)
	}
	if empty := laneSessionSignal(plain, nil, []any{map[string]any{"role": "system", "content": "x"}}, "tenant-a"); empty != "" {
		t.Fatalf("a prompt without a user message produced a seed: %q", empty)
	}
}

func TestApplyLaneHeadersWithoutSessionOmitsAffinity(t *testing.T) {
	req := httptest.NewRequest("GET", "/v1/models", nil)
	applyLaneHeaders(req, "")
	if got := req.Header.Get("Authorization"); got != "Bearer "+zenAnonymousKey {
		t.Fatalf("authorization = %q", got)
	}
	if req.Header.Get("x-opencode-session") != "" || req.Header.Get("x-opencode-project") != "" {
		t.Fatalf("catalog request carried conversation headers: %#v", req.Header)
	}
}

func TestEnsureFreeLaneShape(t *testing.T) {
	gateNames := func(t *testing.T, body []byte) map[string]map[string]any {
		t.Helper()
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("shaped body is not JSON: %v", err)
		}
		tools, _ := payload["tools"].([]any)
		out := make(map[string]map[string]any, len(tools))
		for _, tool := range tools {
			entry, _ := tool.(map[string]any)
			function, _ := entry["function"].(map[string]any)
			name, _ := function["name"].(string)
			out[name] = function
		}
		return out
	}

	// A client that sends no tools gets both gate tools and tool_choice none, so
	// the injected stubs can never be invoked by the model.
	bare := []byte(`{"model":"alpha:free","messages":[{"role":"user","content":"hi"}]}`)
	shaped := ensureFreeLaneShape(bare)
	if string(shaped) == string(bare) {
		t.Fatal("a tool-less payload was forwarded unshaped")
	}
	var barePayload map[string]any
	if err := json.Unmarshal(shaped, &barePayload); err != nil {
		t.Fatal(err)
	}
	if barePayload["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %#v, want none", barePayload["tool_choice"])
	}
	tools := gateNames(t, shaped)
	for _, name := range []string{"bash", "read"} {
		parameters, ok := tools[name]["parameters"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no parameters: %#v", name, tools[name])
		}
		properties, ok := parameters["properties"].(map[string]any)
		if !ok || len(properties) == 0 {
			t.Fatalf("%s has an empty schema: %#v", name, parameters)
		}
	}

	// A payload that already satisfies the gate is forwarded byte for byte.
	satisfied := []byte(`{"model":"alpha:free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}}}}},{"type":"function","function":{"name":"read","parameters":{"type":"object","properties":{"filePath":{"type":"string"}}}}}]}`)
	if got := ensureFreeLaneShape(satisfied); string(got) != string(satisfied) {
		t.Fatalf("satisfied payload was rewritten: %s", got)
	}

	// An empty shell with the right name is replaced, unrelated tools survive,
	// and a client-chosen tool_choice is preserved.
	shell := []byte(`{"model":"alpha:free","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto","tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"search","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]}`)
	fixed := ensureFreeLaneShape(shell)
	fixedTools := gateNames(t, fixed)
	if len(fixedTools) != 3 {
		t.Fatalf("expected bash/read/search, got %#v", fixedTools)
	}
	if _, kept := fixedTools["search"]; !kept {
		t.Fatal("client tool was dropped")
	}
	properties, _ := fixedTools["bash"]["parameters"].(map[string]any)["properties"].(map[string]any)
	if len(properties) == 0 {
		t.Fatal("empty bash schema was trusted")
	}
	var fixedPayload map[string]any
	if err := json.Unmarshal(fixed, &fixedPayload); err != nil {
		t.Fatal(err)
	}
	if fixedPayload["tool_choice"] != "auto" {
		t.Fatalf("client tool_choice was overwritten: %#v", fixedPayload["tool_choice"])
	}

	// Non-JSON and message-less bodies are returned untouched.
	for _, body := range [][]byte{[]byte("not json"), []byte(`{"model":"alpha:free"}`), []byte(`{"messages":[]}`)} {
		if got := ensureFreeLaneShape(body); string(got) != string(body) {
			t.Fatalf("body %s was rewritten to %s", body, got)
		}
	}
}
