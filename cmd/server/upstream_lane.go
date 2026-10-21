package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This gateway has exactly one upstream: OpenCode Zen's anonymous free lane.
// opencode2dsh (docs/design.md §2.4, legacy/internal/gateway/gateway.go:29,345)
// fixes the endpoint, the credential and the disguise header set, and none of
// them is operator-configurable — every value here is part of the handshake the
// free lane inspects, so making it a form field only produced misconfigured
// gateways that failed upstream with an opaque 403.
const (
	zenUpstreamBase   = "https://opencode.ai/zen"
	zenAnonymousKey   = "public"
	openCodeProjectID = "global"
	openCodeVersion   = "1.18.31"
)

// laneBaseOverride is the test seam: unit tests point the lane at an
// httptest server. It is never read from the environment or the database.
var laneBaseOverride string

// laneBaseURL is the upstream root. Chat is POST {base}/v1/chat/completions and
// the catalog is GET {base}/v1/models (gateway.go:403 protocolPath).
func laneBaseURL() string {
	if laneBaseOverride != "" {
		return laneBaseOverride
	}
	return zenUpstreamBase
}

// openCodeUserAgent mirrors ids.go opencodeUserAgent: the lane expects a client
// that claims to be the CLI. The runtime half is decorative.
func openCodeUserAgent() string {
	return "opencode/" + openCodeVersion + " (linux x64; node22.11.0)"
}

// applyLaneHeaders writes the full CLI disguise set (gateway.go:345-380,
// adapter/ids.ts disguiseHeaders). x-opencode-request is deliberately absent:
// upstream refuses any value for it, and x-opencode-project carries the literal
// "global" because a prj_ id is refused too.
func applyLaneHeaders(req *http.Request, session string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", openCodeUserAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("Authorization", "Bearer "+zenAnonymousKey)
	if session == "" {
		return
	}
	// Four spellings of the same affinity id: older and newer Zen deployments
	// each read a different one, and the 2026-10 gate rejects a request whose
	// canonical session id is missing from all of them.
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("X-Session-Id", session)
	req.Header.Set("x-opencode-project", openCodeProjectID)
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// invertedTimestampHex is the 12-hex-character clock segment of a genuine
// OpenCode session id: milliseconds shifted by the per-millisecond counter (we
// always mint counter 1) and inverted, so ids sort newest first as strings
// (packages/schema/src/identifier.ts, ids.ts invertedTimestampHex). A digest
// standing in for the clock is refused: the lane decodes these bits and
// requires the low 12 to be a small counter.
func invertedTimestampHex(now time.Time) string {
	current := big.NewInt(now.UnixMilli())
	current.Lsh(current, 12)
	current.Add(current, big.NewInt(1))
	current.Xor(current, big.NewInt(0xffffffffffff))
	hex := current.Text(16)
	if len(hex) < 12 {
		hex = strings.Repeat("0", 12-len(hex)) + hex
	}
	if len(hex) > 12 {
		hex = hex[len(hex)-12:]
	}
	return hex
}

// base62Fixed renders value in exactly width Base62 characters, zero padded.
func base62Fixed(value *big.Int, width int) string {
	base := big.NewInt(62)
	n := new(big.Int).Set(value)
	out := make([]byte, width)
	quotient, remainder := new(big.Int), new(big.Int)
	for i := width - 1; i >= 0; i-- {
		quotient.QuoRem(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
		n = new(big.Int).Set(quotient)
	}
	return string(out)
}

// canonicalSessionID expands any conversation identity into the "ses_" shape
// upstream accepts: 12 hex clock characters plus 14 Base62 characters derived
// from the signal, so distinct conversations never collide while a conversation
// keeps one id for its whole life (upstream meters the free tier per session and
// keys its prompt cache on it).
func canonicalSessionID(signal string, now time.Time) string {
	if canonicalSessionPattern(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	tail := new(big.Int).SetBytes(sum[6:16])
	return "ses_" + invertedTimestampHex(now) + base62Fixed(tail, 14)
}

func canonicalSessionPattern(value string) bool {
	if len(value) != len("ses_")+12+14 || !strings.HasPrefix(value, "ses_") {
		return false
	}
	for _, r := range value[len("ses_"):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// laneSessionMemo keeps one session id per conversation. The clock half must be
// minted once, not per request: a rotation retry and every later turn of the
// same conversation have to reuse the identical id.
type laneSessionMemo struct {
	mu      sync.Mutex
	entries map[string]string
	order   []string
}

const laneSessionMemoLimit = 1024

func newLaneSessionMemo() *laneSessionMemo {
	return &laneSessionMemo{entries: make(map[string]string, laneSessionMemoLimit)}
}

func (m *laneSessionMemo) id(signal string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.entries[signal]
	return value, ok
}

func (m *laneSessionMemo) remember(signal, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[signal]; exists {
		return
	}
	if len(m.order) >= laneSessionMemoLimit {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.entries, oldest)
	}
	m.entries[signal] = id
	m.order = append(m.order, signal)
}

// laneSessionStore returns the per-process session memo, creating it on first
// use so a zero-value App in tests behaves the same as a served one.
func (a *App) laneSessionStore() *laneSessionMemo {
	a.laneSessionsOnce.Do(func() {
		if a.laneSessions == nil {
			a.laneSessions = newLaneSessionMemo()
		}
	})
	return a.laneSessions
}

// laneSessionID returns the upstream session id for one conversation. A signal
// that already carries a canonical id passes through untouched; anything else is
// expanded once and remembered.
func (a *App) laneSessionID(signal string) string {
	if signal == "" {
		return canonicalSessionID("fallback\x00"+randomHexToken(16), time.Now())
	}
	store := a.laneSessionStore()
	if id, ok := store.id(signal); ok {
		return id
	}
	id := canonicalSessionID(signal, time.Now())
	store.remember(signal, id)
	return id
}

// randomHexToken is the only entropy a signal-less request gets: it makes the
// session id unique without pretending to be a conversation.
func randomHexToken(size int) string {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// laneSessionSignal builds the conversation identity the session id is derived
// from: an explicit client session header or user field when the client sends
// one, otherwise the opening user message (ids.go conversationSeed). The client
// credential is folded in so two tenants with the same prompt never share an
// upstream session.
func laneSessionSignal(r *http.Request, user json.RawMessage, messages []any, clientKeyHash string) string {
	explicit := ""
	for _, header := range []string{"X-Relay-Session-ID", "X-OpenCode-Session-ID", "X-Session-ID"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			explicit = value
			break
		}
	}
	if explicit == "" && len(user) > 0 {
		var value string
		if json.Unmarshal(user, &value) == nil {
			explicit = strings.TrimSpace(value)
		}
	}
	if explicit != "" {
		return clientKeyHash + "\x00session\x00" + explicit
	}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); !strings.EqualFold(strings.TrimSpace(role), "user") {
			continue
		}
		encoded, err := json.Marshal(message["content"])
		if err != nil || len(encoded) == 0 || string(encoded) == "null" {
			continue
		}
		seed := string(encoded)
		if len(seed) > 512 {
			seed = seed[:512]
		}
		return clientKeyHash + "\x00message\x00" + seed
	}
	return ""
}

// freeLaneGateTool is one injected tool definition. The free lane's agent-shape
// gate requires the payload to offer `bash` and `read` with non-empty parameter
// schemas; an empty shell of the same name is refused, so the real schema is
// always written (adapter/messages.ts GATE_TOOL_SCHEMAS).
type freeLaneGateTool struct {
	Name        string
	Description string
	Parameters  map[string]any
}

var freeLaneGateTools = []freeLaneGateTool{
	{
		Name:        "bash",
		Description: "Run a shell command in the project workspace.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "The command to run."},
				"description": map[string]any{"type": "string", "description": "What the command does."},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	},
	{
		Name:        "read",
		Description: "Read a file from the filesystem.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filePath": map[string]any{"type": "string", "description": "Absolute path of the file to read."},
				"offset":   map[string]any{"type": "integer", "description": "Line number to start reading from."},
				"limit":    map[string]any{"type": "integer", "description": "Maximum number of lines to read."},
			},
			"required":             []any{"filePath"},
			"additionalProperties": false,
		},
	},
}

// gateToolSatisfied reports whether a tool already satisfies the gate: same
// name and a parameter object with at least one property.
func gateToolSatisfied(tool any, name string) bool {
	entry, ok := tool.(map[string]any)
	if !ok {
		return false
	}
	function, ok := entry["function"].(map[string]any)
	if !ok {
		return false
	}
	if functionName, _ := function["name"].(string); functionName != name {
		return false
	}
	parameters, ok := function["parameters"].(map[string]any)
	if !ok {
		return false
	}
	properties, ok := parameters["properties"].(map[string]any)
	return ok && len(properties) > 0
}

func gateToolName(tool any) string {
	entry, ok := tool.(map[string]any)
	if !ok {
		return ""
	}
	function, ok := entry["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := function["name"].(string)
	return name
}

// ensureFreeLaneShape rewrites an outgoing chat payload so the free lane accepts
// it as an agent request: the gate tools are appended when missing (a stub with
// the right name but an empty schema is replaced, not trusted), and a client
// that sent no tools at all gets tool_choice "none" so the injected stubs can
// never be called. A payload that already satisfies the gate is returned byte
// for byte.
func ensureFreeLaneShape(body []byte) []byte {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	rawMessages, ok := payload["messages"].([]any)
	if !ok || len(rawMessages) == 0 {
		return body
	}
	rawTools, _ := payload["tools"].([]any)
	missing := make([]freeLaneGateTool, 0, len(freeLaneGateTools))
	for _, gate := range freeLaneGateTools {
		satisfied := false
		for _, tool := range rawTools {
			if gateToolSatisfied(tool, gate.Name) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			missing = append(missing, gate)
		}
	}
	if len(missing) == 0 {
		return body
	}
	kept := make([]any, 0, len(rawTools)+len(missing))
	for _, tool := range rawTools {
		name := gateToolName(tool)
		replace := false
		for _, gate := range missing {
			if name == gate.Name {
				replace = true
				break
			}
		}
		if !replace {
			kept = append(kept, tool)
		}
	}
	for _, gate := range missing {
		kept = append(kept, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        gate.Name,
				"description": gate.Description,
				"parameters":  gate.Parameters,
			},
		})
	}
	payload["tools"] = kept
	if len(rawTools) == 0 {
		if _, sent := payload["tool_choice"]; !sent {
			payload["tool_choice"] = "none"
		}
	}
	shaped, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return shaped
}

// forceLaneStream rewrites a chat-completions body so the upstream sees
// stream:true. The anonymous lane refuses a non-streaming request outright
// (gate probed 2026-10-09: "stream: true — non-stream 403s"), so the shape must
// hold for every caller, not just streaming ones. The reported bool is what the
// client asked for, so the relay can fold the stream back into one JSON object
// for a client that did not want a stream.
func forceLaneStream(body []byte) ([]byte, bool) {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return body, false
	}
	if wanted, _ := payload["stream"].(bool); wanted {
		return body, true
	}
	payload["stream"] = true
	shaped, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return shaped, false
}
