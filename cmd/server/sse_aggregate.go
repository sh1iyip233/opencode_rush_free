package main

import (
	"encoding/json"
	"errors"
	"strings"
)

// The anonymous free lane only answers a streaming request, but OpenAI-compatible
// clients are allowed to ask for one JSON object. aggregateSSEStream folds an
// upstream chat-completions SSE stream into the single non-streaming body such a
// client expects, so the lane's shape requirement never leaks into the API.

type sseToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type sseDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          string        `json:"content,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Reasoning        string        `json:"reasoning,omitempty"`
	ToolCalls        []sseToolCall `json:"tool_calls,omitempty"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseChunk struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	Choices           []sseChoice     `json:"choices"`
	Usage             json.RawMessage `json:"usage,omitempty"`
}

type aggregatedToolCall struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

// aggregateSSEStream returns a single chat.completion object built from the
// deltas in an upstream SSE stream. An error means the bytes were not a
// recognizable stream; the caller then relays them untouched.
func aggregateSSEStream(raw []byte) ([]byte, error) {
	var (
		content   strings.Builder
		reasoning strings.Builder
		toolCalls []*aggregatedToolCall
		byIndex   = map[int]*aggregatedToolCall{}
		finish    string
		chunkID   string
		model     string
		finger    string
		created   int64
		usage     json.RawMessage
		seen      bool
	)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk sseChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		seen = true
		if chunk.ID != "" {
			chunkID = chunk.ID
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if chunk.SystemFingerprint != "" {
			finger = chunk.SystemFingerprint
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			content.WriteString(choice.Delta.Content)
			if choice.Delta.ReasoningContent != "" {
				reasoning.WriteString(choice.Delta.ReasoningContent)
			} else if choice.Delta.Reasoning != "" {
				reasoning.WriteString(choice.Delta.Reasoning)
			}
			for _, call := range choice.Delta.ToolCalls {
				target, ok := byIndex[call.Index]
				if !ok {
					target = &aggregatedToolCall{}
					byIndex[call.Index] = target
					toolCalls = append(toolCalls, target)
				}
				if call.ID != "" {
					target.ID = call.ID
				}
				if call.Type != "" {
					target.Type = call.Type
				}
				if call.Function.Name != "" {
					target.Name = call.Function.Name
				}
				target.Arguments.WriteString(call.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}
	if !seen {
		return nil, errors.New("upstream stream contained no chat chunks")
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		calls := make([]any, 0, len(toolCalls))
		for i, call := range toolCalls {
			kind := call.Type
			if kind == "" {
				kind = "function"
			}
			calls = append(calls, map[string]any{
				"index": i,
				"id":    call.ID,
				"type":  kind,
				"function": map[string]any{
					"name":      call.Name,
					"arguments": call.Arguments.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	if finish == "" {
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	completion := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if finger != "" {
		completion["system_fingerprint"] = finger
	}
	if len(usage) > 0 {
		completion["usage"] = usage
	}
	return json.Marshal(completion)
}
