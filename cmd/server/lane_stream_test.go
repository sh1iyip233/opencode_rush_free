package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// gateToolNames lists the tool names a shaped payload carries.
func gateToolNames(payload map[string]any) []string {
	tools, _ := payload["tools"].([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := gateToolName(tool); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func TestForceLaneStreamForcesStreamingAndReportsClientIntent(t *testing.T) {
	shaped, wanted := forceLaneStream([]byte(`{"model":"beam-free","messages":[]}`))
	if wanted {
		t.Fatal("a body without stream was reported as a streaming request")
	}
	var payload map[string]any
	if err := json.Unmarshal(shaped, &payload); err != nil {
		t.Fatal(err)
	}
	if stream, _ := payload["stream"].(bool); !stream {
		t.Fatalf("stream was not forced: %s", shaped)
	}

	streaming := []byte(`{"model":"beam-free","messages":[],"stream":true}`)
	kept, wanted := forceLaneStream(streaming)
	if !wanted || !bytes.Equal(kept, streaming) {
		t.Fatalf("an already streaming body was rewritten: wanted=%v body=%s", wanted, kept)
	}

	raw := []byte(`not json`)
	unchanged, wanted := forceLaneStream(raw)
	if wanted || !bytes.Equal(unchanged, raw) {
		t.Fatalf("a non-JSON body must pass through untouched: %s", unchanged)
	}
}

func TestAggregateSSEStreamFoldsDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700,"model":"beam-free","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700,"model":"beam-free","choices":[{"index":0,"delta":{"content":"hel"}}]}`,
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700,"model":"beam-free","choices":[{"index":0,"delta":{"content":"lo","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"comm"}}]}}]}`,
		`data: {"id":"chatcmpl-9","object":"chat.completion.chunk","created":1700,"model":"beam-free","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`,
		`data: [DONE]`,
		``,
	}, "\n\n")

	folded, err := aggregateSSEStream([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(folded, &completion); err != nil {
		t.Fatalf("folded body is not JSON: %v body=%s", err, folded)
	}
	if completion.Object != "chat.completion" || completion.ID != "chatcmpl-9" || completion.Model != "beam-free" || completion.Created != 1700 {
		t.Fatalf("envelope was not preserved: %s", folded)
	}
	if len(completion.Choices) != 1 {
		t.Fatalf("expected one choice: %s", folded)
	}
	choice := completion.Choices[0]
	if choice.Message.Role != "assistant" || choice.Message.Content != "hello" {
		t.Fatalf("content was not concatenated: %#v", choice.Message)
	}
	if choice.Message.ReasoningContent != "think " {
		t.Fatalf("reasoning was dropped: %#v", choice.Message)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected one tool call: %s", folded)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_1" || call.Type != "function" || call.Function.Name != "bash" || call.Function.Arguments != `{"command":"ls"}` {
		t.Fatalf("tool call was not merged: %#v", call)
	}
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q", choice.FinishReason)
	}
	if completion.Usage.TotalTokens != 10 {
		t.Fatalf("usage was not carried over: %s", folded)
	}

	if _, err := aggregateSSEStream([]byte("not a stream")); err == nil {
		t.Fatal("a body without chat chunks must be reported as unfolded")
	}
}

// laneProxy answers every proxied request itself, so the gateway's chat request
// lands on a fixture without any real upstream or exit.
func laneProxy(t *testing.T, status int, contentType, body string, capture func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture(r)
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

func laneChatApp(t *testing.T, proxyURL string) *App {
	t.Helper()
	a := testApp(t)
	pointLaneAt(t, "http://upstream.example.test")
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d %s", put.Code, put.Body.String())
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "beam-free", "Beam", 1, "test", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	p, err := parseProxy(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.insertProxy(p); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('client_key',?)", hashToken("lane-client")); err != nil {
		t.Fatal(err)
	}
	return a
}

const laneStreamBody = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"beam-free","choices":[{"index":0,"delta":{"role":"assistant","content":"hel"}}]}` + "\n\n" +
	`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"beam-free","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n" +
	`data: [DONE]` + "\n\n"

func TestNonStreamingClientReceivesOneJSONObject(t *testing.T) {
	var upstreamBody []byte
	var upstreamHeaders http.Header
	proxy := laneProxy(t, http.StatusOK, "text/event-stream", laneStreamBody, func(r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		upstreamHeaders = r.Header.Clone()
	})
	defer proxy.Close()
	a := laneChatApp(t, proxy.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"beam-free","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer lane-client")
	recorder := httptest.NewRecorder()
	a.gatewayChat(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("a non-streaming client got content type %q", got)
	}
	if strings.Contains(recorder.Body.String(), "data:") {
		t.Fatalf("the SSE framing leaked to a non-streaming client: %s", recorder.Body.String())
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &completion); err != nil {
		t.Fatalf("response is not one JSON object: %v body=%s", err, recorder.Body.String())
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 || completion.Choices[0].Message.Content != "hello" || completion.Choices[0].FinishReason != "stop" {
		t.Fatalf("folded response = %s", recorder.Body.String())
	}

	// The upstream must still have received the lane's required shape.
	var sent map[string]any
	if err := json.Unmarshal(upstreamBody, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v body=%s", err, upstreamBody)
	}
	if stream, _ := sent["stream"].(bool); !stream {
		t.Fatalf("the lane requires stream:true, upstream saw %s", upstreamBody)
	}
	if upstreamHeaders.Get("Authorization") != "Bearer public" {
		t.Fatalf("lane credential missing: %#v", upstreamHeaders)
	}
	names := map[string]bool{}
	for _, tool := range gateToolNames(sent) {
		names[tool] = true
	}
	if !names["bash"] || !names["read"] {
		t.Fatalf("gate tools were not injected: %s", upstreamBody)
	}
}

func TestStreamingClientKeepsTheStream(t *testing.T) {
	proxy := laneProxy(t, http.StatusOK, "text/event-stream", laneStreamBody, nil)
	defer proxy.Close()
	a := laneChatApp(t, proxy.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"beam-free","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer lane-client")
	recorder := httptest.NewRecorder()
	a.gatewayChat(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("gateway status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("a streaming client got content type %q", got)
	}
	if !strings.Contains(recorder.Body.String(), "data: ") {
		t.Fatalf("the stream was folded for a streaming client: %s", recorder.Body.String())
	}
}

func TestLaneProbeSendsAgentShape(t *testing.T) {
	var probeBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, laneStreamBody)
	}))
	defer upstream.Close()
	a := testApp(t)
	pointLaneAt(t, upstream.URL)
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d %s", put.Code, put.Body.String())
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "beam-free", "Beam", 1, "test", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	a.testUpstream(recorder, httptest.NewRequest(http.MethodPost, "/api/settings/test-upstream", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("probe failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(probeBody, &sent); err != nil {
		t.Fatalf("probe body is not JSON: %v body=%s", err, probeBody)
	}
	if stream, _ := sent["stream"].(bool); !stream {
		t.Fatalf("the probe must stream, upstream saw %s", probeBody)
	}
	names := map[string]bool{}
	for _, tool := range gateToolNames(sent) {
		names[tool] = true
	}
	if !names["bash"] || !names["read"] {
		t.Fatalf("the probe must carry the gate tools: %s", probeBody)
	}
}
