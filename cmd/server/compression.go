package main

// The compression gateway folds consumed chat history into a self-contained
// summary before a request reaches the upstream free lane. It is modelled on
// billion-context's Active Context Pruning, but stays inside the relay: the
// summariser is a separate, administrator-configured AI API, and the folded
// state is persisted per gateway session so a long conversation keeps its
// summary across turns.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	compressionSettingKey     = "compression_gateway"
	compressionUsageKind      = "compression"
	compressionRequestHeader  = "X-Relay-Compression"
	compressionStyleOpenAI    = "openai"
	compressionStyleAnthropic = "anthropic"

	compressionMaxTranscriptRunes = 60000
	compressionMaxMessageRunes    = 4000
	compressionImageTokens        = 800
	compressionUserAgent          = "RelayDesk-Compression/1.0"
)

// compressionConfig is the persisted and cached shape of the compression
// gateway settings. The API key never leaves the process through JSON: it is
// encrypted into the settings blob and only reported as has_api_key.
type compressionConfig struct {
	Enabled            bool    `json:"enabled"`
	APIStyle           string  `json:"api_style"`
	BaseURL            string  `json:"base_url"`
	APIKey             string  `json:"-"`
	Model              string  `json:"model"`
	UseProxy           bool    `json:"use_proxy"`
	TriggerRatio       float64 `json:"trigger_ratio"`
	ContextWindow      int     `json:"context_window"`
	KeepRecentMessages int     `json:"keep_recent_messages"`
	MinFoldMessages    int     `json:"min_fold_messages"`
	MaxSummaryTokens   int     `json:"max_summary_tokens"`
	Temperature        float64 `json:"temperature"`
	TimeoutSeconds     int     `json:"timeout_seconds"`
	SummaryRole        string  `json:"summary_role"`
	Marker             string  `json:"marker"`
	Prompt             string  `json:"prompt"`
	FailOpen           bool    `json:"fail_open"`
}

// compressionRecord is the on-disk JSON blob stored under compressionSettingKey.
type compressionRecord struct {
	compressionConfig
	APIKeyEnc string `json:"api_key_enc"`
}

func defaultCompressionMarker() string {
	return "📦 [Relay Desk 压缩摘要] 已折叠 {count} 条较早消息（约 {before} → {after} tokens）。"
}

func defaultCompressionPrompt() string {
	return strings.TrimSpace(`你是一个服务于 LLM 网关的上下文压缩器。把给定的对话记录改写成一份自包含的压缩简报。
必须保留：用户的目标与约束、已做的决定及原因、精确技术细节（文件路径、函数签名、错误原文、版本号、配置键、数字阈值）、未解决的问题与 TODO。
删除：寒暄、重复的探索过程、原始工具输出、已被推翻的中间结论。
不要编造事实，不要加入记录中没有的结论。使用对话记录本身的语言输出，只输出简报正文。`)
}

func defaultCompressionConfig() compressionConfig {
	return compressionConfig{
		Enabled:            false,
		APIStyle:           compressionStyleOpenAI,
		UseProxy:           true,
		TriggerRatio:       0.75,
		ContextWindow:      200000,
		KeepRecentMessages: 8,
		MinFoldMessages:    4,
		MaxSummaryTokens:   1200,
		Temperature:        0.2,
		TimeoutSeconds:     90,
		SummaryRole:        "system",
		Marker:             defaultCompressionMarker(),
		Prompt:             defaultCompressionPrompt(),
		FailOpen:           true,
	}
}

func (c compressionConfig) configured() bool {
	return strings.TrimSpace(c.BaseURL) != "" && strings.TrimSpace(c.Model) != ""
}

func (c compressionConfig) isAnthropic() bool {
	return strings.EqualFold(strings.TrimSpace(c.APIStyle), compressionStyleAnthropic)
}

// normalizeCompressionConfig clamps and validates operator input. It is used
// for both the persisted settings and the ad-hoc probe configuration.
func normalizeCompressionConfig(cfg compressionConfig) (compressionConfig, error) {
	style := strings.ToLower(strings.TrimSpace(cfg.APIStyle))
	if style == "" {
		style = compressionStyleOpenAI
	}
	if style != compressionStyleOpenAI && style != compressionStyleAnthropic {
		return cfg, errors.New("api_style must be openai or anthropic")
	}
	cfg.APIStyle = style

	base := strings.TrimSpace(cfg.BaseURL)
	if base != "" {
		normalized, err := validateUpstreamBaseURL(base)
		if err != nil {
			return cfg, err
		}
		base = normalized
	}
	cfg.BaseURL = base
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.APIKey = normalizeAPIKey(cfg.APIKey)

	if cfg.TriggerRatio <= 0 {
		cfg.TriggerRatio = 0.75
	}
	if cfg.TriggerRatio < 0.3 || cfg.TriggerRatio > 0.95 {
		return cfg, errors.New("trigger_ratio must be between 0.3 and 0.95")
	}
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = 200000
	}
	if cfg.ContextWindow < 2000 {
		return cfg, errors.New("context_window must be at least 2000")
	}
	if cfg.ContextWindow > 20000000 {
		return cfg, errors.New("context_window is unreasonably large")
	}
	if cfg.KeepRecentMessages <= 0 {
		cfg.KeepRecentMessages = 8
	}
	if cfg.KeepRecentMessages < 2 || cfg.KeepRecentMessages > 500 {
		return cfg, errors.New("keep_recent_messages must be between 2 and 500")
	}
	if cfg.MinFoldMessages <= 0 {
		cfg.MinFoldMessages = 4
	}
	if cfg.MinFoldMessages < 1 || cfg.MinFoldMessages > 500 {
		return cfg, errors.New("min_fold_messages must be between 1 and 500")
	}
	if cfg.MaxSummaryTokens <= 0 {
		cfg.MaxSummaryTokens = 1200
	}
	if cfg.MaxSummaryTokens < 128 || cfg.MaxSummaryTokens > 16000 {
		return cfg, errors.New("max_summary_tokens must be between 128 and 16000")
	}
	if cfg.Temperature < 0 {
		cfg.Temperature = 0
	}
	if cfg.Temperature > 2 {
		return cfg, errors.New("temperature must be between 0 and 2")
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 90
	}
	if cfg.TimeoutSeconds < 5 || cfg.TimeoutSeconds > 600 {
		return cfg, errors.New("timeout_seconds must be between 5 and 600")
	}
	role := strings.ToLower(strings.TrimSpace(cfg.SummaryRole))
	if role == "" {
		role = "system"
	}
	if role != "system" && role != "user" {
		return cfg, errors.New("summary_role must be system or user")
	}
	cfg.SummaryRole = role
	if strings.TrimSpace(cfg.Marker) == "" {
		cfg.Marker = defaultCompressionMarker()
	}
	if strings.TrimSpace(cfg.Prompt) == "" {
		cfg.Prompt = defaultCompressionPrompt()
	}
	if cfg.Enabled && !cfg.configured() {
		return cfg, errors.New("base_url and model are required before enabling the compression gateway")
	}
	return cfg, nil
}

// ---------------------------------------------------------------------------
// Settings persistence
// ---------------------------------------------------------------------------

func (a *App) loadCompression() (compressionConfig, error) {
	a.runtimeCaches.compression.mu.RLock()
	if a.runtimeCaches.compression.loaded {
		cfg := a.runtimeCaches.compression.cfg
		a.runtimeCaches.compression.mu.RUnlock()
		return cfg, nil
	}
	a.runtimeCaches.compression.mu.RUnlock()

	a.runtimeCaches.compression.mu.Lock()
	defer a.runtimeCaches.compression.mu.Unlock()
	if a.runtimeCaches.compression.loaded {
		return a.runtimeCaches.compression.cfg, nil
	}
	cfg, err := a.loadCompressionFromDB()
	if err != nil {
		return cfg, err
	}
	a.runtimeCaches.compression.cfg = cfg
	a.runtimeCaches.compression.loaded = true
	return cfg, nil
}

func (a *App) loadCompressionFromDB() (compressionConfig, error) {
	record := compressionRecord{compressionConfig: defaultCompressionConfig()}
	var raw string
	err := a.db.QueryRow("SELECT value FROM settings WHERE key=?", compressionSettingKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return record.compressionConfig, err
	}
	if strings.TrimSpace(raw) != "" {
		if unmarshalErr := json.Unmarshal([]byte(raw), &record); unmarshalErr != nil {
			return record.compressionConfig, errors.New("compression settings are corrupt")
		}
	}
	cfg := record.compressionConfig
	if record.APIKeyEnc != "" {
		key, decryptErr := a.decrypt(record.APIKeyEnc)
		if decryptErr != nil {
			return cfg, errors.New("could not decrypt compression API key")
		}
		cfg.APIKey = normalizeAPIKey(key)
	}
	normalized, normalizeErr := normalizeCompressionConfig(cfg)
	if normalizeErr != nil {
		// Stored values from an older build must never break the gateway: fall
		// back to the defaults for the offending fields instead of failing.
		normalized = cfg
	}
	return normalized, nil
}

func (a *App) saveCompression(cfg compressionConfig) error {
	encrypted, err := a.encrypt(cfg.APIKey)
	if err != nil {
		return errors.New("compression API key encryption failed")
	}
	record := compressionRecord{compressionConfig: cfg, APIKeyEnc: encrypted}
	blob, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", compressionSettingKey, string(blob)); err != nil {
		return err
	}
	a.invalidateCompressionCache()
	return nil
}

// ---------------------------------------------------------------------------
// Token estimation and fold planning
// ---------------------------------------------------------------------------

// estimateTextTokens approximates a tokenizer: ASCII text averages four
// characters per token, while CJK and other non-ASCII runes are close to one
// token each.
func estimateTextTokens(text string) int {
	if text == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range text {
		if r < 128 {
			ascii++
		} else {
			wide++
		}
	}
	tokens := ascii/4 + wide
	if tokens < 1 {
		tokens = 1
	}
	return tokens
}

func estimateContentTokens(content any) int {
	switch value := content.(type) {
	case nil:
		return 0
	case string:
		return estimateTextTokens(value)
	case []any:
		total := 0
		for _, part := range value {
			partMap, ok := part.(map[string]any)
			if !ok {
				if text, isString := part.(string); isString {
					total += estimateTextTokens(text)
				}
				continue
			}
			kind, _ := partMap["type"].(string)
			if compressionIsImagePart(kind) {
				total += compressionImageTokens
				continue
			}
			if text, isString := partMap["text"].(string); isString {
				total += estimateTextTokens(text)
			}
		}
		return total
	case map[string]any:
		if encoded, err := json.Marshal(value); err == nil {
			return estimateTextTokens(string(encoded))
		}
	}
	return 0
}

func compressionIsImagePart(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "image_url", "image", "input_image", "image_base64":
		return true
	}
	return false
}

func compressionMessageHasImage(content any) bool {
	parts, ok := content.([]any)
	if !ok {
		return false
	}
	for _, part := range parts {
		partMap, isMap := part.(map[string]any)
		if !isMap {
			continue
		}
		kind, _ := partMap["type"].(string)
		if compressionIsImagePart(kind) {
			return true
		}
	}
	return false
}

func estimateMessageTokens(message any) int {
	messageMap, ok := message.(map[string]any)
	if !ok {
		return 0
	}
	total := 4
	total += estimateContentTokens(messageMap["content"])
	if calls, exists := messageMap["tool_calls"]; exists && calls != nil {
		if encoded, err := json.Marshal(calls); err == nil {
			total += estimateTextTokens(string(encoded))
		}
	}
	if name, isString := messageMap["name"].(string); isString {
		total += estimateTextTokens(name)
	}
	return total
}

func estimateMessagesTokens(messages []any) int {
	total := 0
	for _, message := range messages {
		total += estimateMessageTokens(message)
	}
	return total
}

func compressionMessageRole(message any) string {
	messageMap, ok := message.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := messageMap["role"].(string)
	return strings.ToLower(strings.TrimSpace(role))
}

// compressionFoldable reports whether a message may be replaced by the summary.
// Only plain user/assistant text qualifies: tool calls, tool results, images and
// unknown content kinds stay in the tail so the upstream keeps a coherent turn.
func compressionFoldable(message any) bool {
	messageMap, ok := message.(map[string]any)
	if !ok {
		return false
	}
	switch compressionMessageRole(messageMap) {
	case "user", "assistant":
	default:
		return false
	}
	if calls, exists := messageMap["tool_calls"]; exists && calls != nil {
		if list, isList := calls.([]any); !isList || len(list) > 0 {
			return false
		}
	}
	if callID, exists := messageMap["tool_call_id"]; exists && callID != nil && fmt.Sprint(callID) != "" {
		return false
	}
	if functionCall, exists := messageMap["function_call"]; exists && functionCall != nil {
		return false
	}
	content := messageMap["content"]
	if compressionMessageHasImage(content) {
		return false
	}
	switch value := content.(type) {
	case nil, string:
		return true
	case []any:
		for _, part := range value {
			partMap, isMap := part.(map[string]any)
			if !isMap {
				return false
			}
			kind, _ := partMap["type"].(string)
			if kind != "" && !strings.EqualFold(kind, "text") {
				return false
			}
		}
		return true
	}
	return false
}

type compressionPlan struct {
	Head    []any
	Folding []any
	Tail    []any
	Before  int
	Window  int
}

// planCompression decides which leading messages of the conversation body can
// be folded. Pinned system/developer prefixes, the most recent messages and any
// non-text turn are always preserved.
func planCompression(cfg compressionConfig, messages []any, force bool, window int) (compressionPlan, bool) {
	before := estimateMessagesTokens(messages)
	if !force && float64(before) < cfg.TriggerRatio*float64(window) {
		return compressionPlan{}, false
	}
	head := 0
	for head < len(messages) {
		switch compressionMessageRole(messages[head]) {
		case "system", "developer":
			head++
			continue
		}
		break
	}
	body := messages[head:]
	if len(body) <= cfg.KeepRecentMessages {
		return compressionPlan{}, false
	}
	maxFold := len(body) - cfg.KeepRecentMessages
	fold := 0
	for fold < maxFold && compressionFoldable(body[fold]) {
		fold++
	}
	if fold < cfg.MinFoldMessages {
		return compressionPlan{}, false
	}
	return compressionPlan{
		Head:    messages[:head],
		Folding: body[:fold],
		Tail:    body[fold:],
		Before:  before,
		Window:  window,
	}, true
}

func truncateCompressionRunes(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// compressionTranscript renders folded messages for the summariser and for the
// incremental digest. The rendering must be stable: the digest is compared
// across turns to decide whether the stored summary can be extended.
func compressionTranscript(messages []any) string {
	var builder strings.Builder
	for index, message := range messages {
		messageMap, ok := message.(map[string]any)
		if !ok {
			continue
		}
		role := compressionMessageRole(messageMap)
		if role == "" {
			role = "user"
		}
		text := strings.TrimSpace(messageText(messageMap["content"]))
		if text == "" {
			text = "(no text content)"
		}
		text = truncateCompressionRunes(text, compressionMaxMessageRunes)
		fmt.Fprintf(&builder, "[%d] %s: %s\n", index+1, role, text)
		if len([]rune(builder.String())) >= compressionMaxTranscriptRunes {
			builder.WriteString("…(transcript truncated for length)\n")
			break
		}
	}
	text := builder.String()
	if runes := []rune(text); len(runes) > compressionMaxTranscriptRunes {
		text = string(runes[:compressionMaxTranscriptRunes]) + "\n…(transcript truncated for length)"
	}
	return text
}

func compressionDigest(messages []any) string {
	sum := sha256.Sum256([]byte(compressionTranscript(messages)))
	return hex.EncodeToString(sum[:])
}

func buildCompressionSummaryMessage(cfg compressionConfig, summary string, count, before, after int) map[string]any {
	marker := strings.NewReplacer(
		"{count}", strconv.Itoa(count),
		"{before}", strconv.Itoa(before),
		"{after}", strconv.Itoa(after),
		"{model}", cfg.Model,
	).Replace(cfg.Marker)
	content := strings.TrimSpace(marker + "\n\n" + summary)
	return map[string]any{"role": cfg.SummaryRole, "content": content}
}

func buildCompressionPromptInput(cfg compressionConfig, prior string, folding []any) string {
	var builder strings.Builder
	if strings.TrimSpace(prior) != "" {
		builder.WriteString("已折叠的既有摘要（保留其中的事实，除非新的对话记录与之冲突）：\n")
		builder.WriteString(strings.TrimSpace(prior))
		builder.WriteString("\n\n")
	}
	builder.WriteString("需要压缩的对话记录：\n")
	builder.WriteString(compressionTranscript(folding))
	if strings.TrimSpace(prior) != "" {
		builder.WriteString("\n请把既有摘要与新增对话合并成一份新的完整摘要，不要只输出增量部分。")
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// Context window discovery
// ---------------------------------------------------------------------------

func firstPositiveNumber(values map[string]any, keys ...string) int {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		switch number := value.(type) {
		case float64:
			if number > 0 {
				return int(number)
			}
		case int:
			if number > 0 {
				return number
			}
		case json.Number:
			if parsed, err := number.Int64(); err == nil && parsed > 0 {
				return int(parsed)
			}
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(number)); err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

func contextWindowFromMetadata(metadata map[string]any) int {
	if limit, ok := metadata["limit"].(map[string]any); ok {
		if window := firstPositiveNumber(limit, "context", "context_length", "context_window", "max_context_tokens", "input"); window > 0 {
			return window
		}
	}
	if window := firstPositiveNumber(metadata, "context_length", "context_window", "max_context_tokens", "max_input_tokens"); window > 0 {
		return window
	}
	if provider, ok := metadata["top_provider"].(map[string]any); ok {
		if window := firstPositiveNumber(provider, "context_length", "context_window"); window > 0 {
			return window
		}
	}
	if architecture, ok := metadata["architecture"].(map[string]any); ok {
		if window := firstPositiveNumber(architecture, "context_length", "context_window"); window > 0 {
			return window
		}
	}
	return 0
}

func (a *App) modelContextWindow(model string) int {
	if strings.TrimSpace(model) == "" {
		return 0
	}
	var raw string
	if err := a.db.QueryRow("SELECT COALESCE(raw_metadata,'') FROM models WHERE model_id=?", model).Scan(&raw); err != nil {
		return 0
	}
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return 0
	}
	return contextWindowFromMetadata(metadata)
}

func (a *App) compressionWindow(model string, cfg compressionConfig) int {
	if window := a.modelContextWindow(model); window > 0 {
		return window
	}
	if cfg.ContextWindow > 0 {
		return cfg.ContextWindow
	}
	return 200000
}

// ---------------------------------------------------------------------------
// Summariser transport
// ---------------------------------------------------------------------------

type compressionSummary struct {
	Text             string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

type compressionUpstreamError struct {
	Status int
	Detail string
}

func (e *compressionUpstreamError) Error() string {
	detail := strings.TrimSpace(e.Detail)
	if detail == "" {
		detail = "no error body"
	}
	return fmt.Sprintf("compression upstream returned HTTP %d: %s", e.Status, truncateError(detail))
}

func (a *App) compressionHTTPClient(cfg compressionConfig) (*http.Client, ProxyRecord, string, error) {
	if cfg.UseProxy {
		proxy, engine, err := a.controlPlaneProxy()
		if err != nil {
			return nil, ProxyRecord{}, "", err
		}
		client, clientErr := a.httpClient(proxy)
		if clientErr != nil {
			return nil, ProxyRecord{}, "", clientErr
		}
		return client, proxy, engine, nil
	}
	client, err := a.httpClient(ProxyRecord{})
	if err != nil {
		return nil, ProxyRecord{}, "", err
	}
	return client, ProxyRecord{}, "direct", nil
}

func compressionEndpoint(cfg compressionConfig) string {
	if cfg.isAnthropic() {
		return upstreamEndpoint(cfg.BaseURL, "messages")
	}
	return upstreamEndpoint(cfg.BaseURL, "chat/completions")
}

func compressionRequestBody(cfg compressionConfig, input string) map[string]any {
	if cfg.isAnthropic() {
		return map[string]any{
			"model":       cfg.Model,
			"max_tokens":  cfg.MaxSummaryTokens,
			"temperature": cfg.Temperature,
			"system":      cfg.Prompt,
			"messages":    []map[string]any{{"role": "user", "content": input}},
		}
	}
	return map[string]any{
		"model":       cfg.Model,
		"max_tokens":  cfg.MaxSummaryTokens,
		"temperature": cfg.Temperature,
		"stream":      false,
		"messages": []map[string]any{
			{"role": "system", "content": cfg.Prompt},
			{"role": "user", "content": input},
		},
	}
}

func extractCompressionText(raw []byte, cfg compressionConfig) (string, error) {
	if cfg.isAnthropic() {
		var payload struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return "", errors.New("compression response was not valid JSON")
		}
		parts := make([]string, 0, len(payload.Content))
		for _, block := range payload.Content {
			if block.Type != "" && !strings.EqualFold(block.Type, "text") {
				continue
			}
			if strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		text := strings.TrimSpace(strings.Join(parts, "\n"))
		if text == "" {
			return "", errors.New("compression model returned an empty summary")
		}
		return text, nil
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errors.New("compression response was not valid JSON")
	}
	if len(payload.Choices) == 0 {
		return "", errors.New("compression response contained no choices")
	}
	choice := payload.Choices[0]
	text := strings.TrimSpace(extractTextContent(choice.Message.Content))
	if text == "" {
		text = strings.TrimSpace(extractTextContent(choice.Message.ReasoningContent))
	}
	if text == "" {
		text = strings.TrimSpace(choice.Text)
	}
	if text == "" {
		return "", errors.New("compression model returned an empty summary")
	}
	return text, nil
}

func extractCompressionUsage(raw []byte, cfg compressionConfig) (int, int, int) {
	var payload struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Usage == nil {
		return 0, 0, 0
	}
	prompt := firstPositiveNumber(payload.Usage, "prompt_tokens", "input_tokens")
	completion := firstPositiveNumber(payload.Usage, "completion_tokens", "output_tokens")
	total := firstPositiveNumber(payload.Usage, "total_tokens")
	if total == 0 && prompt > 0 && completion > 0 {
		total = prompt + completion
	}
	return prompt, completion, total
}

func (s compressionSummary) tokenUsage() *tokenUsage {
	if s.PromptTokens == 0 && s.CompletionTokens == 0 && s.TotalTokens == 0 {
		return nil
	}
	prompt := int64(s.PromptTokens)
	completion := int64(s.CompletionTokens)
	total := int64(s.TotalTokens)
	if total == 0 {
		total = prompt + completion
	}
	return &tokenUsage{Prompt: &prompt, Completion: &completion, Total: &total}
}

// runCompressionSummary performs one summarisation call against the configured
// AI API and returns the summary text with token accounting.
func (a *App) runCompressionSummary(ctx context.Context, cfg compressionConfig, prior string, folding []any) (compressionSummary, error) {
	if !cfg.configured() {
		return compressionSummary{}, errors.New("compression gateway is not configured")
	}
	endpoint := compressionEndpoint(cfg)
	if endpoint == "" {
		return compressionSummary{}, errors.New("compression base_url is invalid")
	}
	input := buildCompressionPromptInput(cfg, prior, folding)
	encoded, err := json.Marshal(compressionRequestBody(cfg, input))
	if err != nil {
		return compressionSummary{}, err
	}
	client, _, _, clientErr := a.compressionHTTPClient(cfg)
	if clientErr != nil {
		return compressionSummary{}, clientErr
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return compressionSummary{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", compressionUserAgent)
	if cfg.APIKey != "" {
		if cfg.isAnthropic() {
			request.Header.Set("x-api-key", cfg.APIKey)
		} else {
			request.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		}
	}
	if cfg.isAnthropic() {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := client.Do(request)
	if err != nil {
		return compressionSummary{}, fmt.Errorf("compression upstream request failed: %w", err)
	}
	defer response.Body.Close()
	raw, readErr := readLimitedBody(response.Body, 4<<20)
	if readErr != nil {
		return compressionSummary{}, fmt.Errorf("compression upstream response failed: %w", readErr)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return compressionSummary{}, &compressionUpstreamError{Status: response.StatusCode, Detail: upstreamErrorSummary(raw)}
	}
	text, extractErr := extractCompressionText(raw, cfg)
	if extractErr != nil {
		return compressionSummary{}, extractErr
	}
	prompt, completion, total := extractCompressionUsage(raw, cfg)
	if total == 0 {
		total = estimateTextTokens(input) + estimateTextTokens(text)
	}
	return compressionSummary{Text: text, PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}, nil
}

// ---------------------------------------------------------------------------
// Per-session state
// ---------------------------------------------------------------------------

type compressionSession struct {
	SessionKey     string
	Model          string
	Summary        string
	FoldedDigest   string
	FoldedMessages int
	FoldedTokens   int
	Compressions   int
	TokensBefore   int
	TokensAfter    int
	CreatedAt      string
	UpdatedAt      string
}

func (a *App) loadCompressionSession(sessionKey string) (compressionSession, bool, error) {
	var session compressionSession
	err := a.db.QueryRow("SELECT session_key,model,summary,folded_digest,folded_messages,folded_tokens,compressions,tokens_before,tokens_after,created_at,updated_at FROM compression_sessions WHERE session_key=?", sessionKey).Scan(
		&session.SessionKey, &session.Model, &session.Summary, &session.FoldedDigest,
		&session.FoldedMessages, &session.FoldedTokens, &session.Compressions,
		&session.TokensBefore, &session.TokensAfter, &session.CreatedAt, &session.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return compressionSession{}, false, nil
	}
	if err != nil {
		return compressionSession{}, false, err
	}
	return session, true, nil
}

func (a *App) saveCompressionSession(session compressionSession) error {
	now := time.Now().UTC().Format(time.RFC3339)
	if session.CreatedAt == "" {
		session.CreatedAt = now
	}
	_, err := a.db.Exec(`INSERT INTO compression_sessions(session_key,model,summary,folded_digest,folded_messages,folded_tokens,compressions,tokens_before,tokens_after,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(session_key) DO UPDATE SET model=excluded.model,summary=excluded.summary,folded_digest=excluded.folded_digest,folded_messages=excluded.folded_messages,folded_tokens=excluded.folded_tokens,compressions=excluded.compressions,tokens_before=excluded.tokens_before,tokens_after=excluded.tokens_after,updated_at=excluded.updated_at`,
		session.SessionKey, session.Model, session.Summary, session.FoldedDigest, session.FoldedMessages,
		session.FoldedTokens, session.Compressions, session.TokensBefore, session.TokensAfter,
		session.CreatedAt, now)
	return err
}

func (a *App) recordCompressionEvent(event compressionEvent) error {
	_, err := a.db.Exec("INSERT INTO compression_events(created_at,session_key,model,api_style,summary_model,folded_messages,tokens_before,tokens_after,summary_tokens,latency_ms,status,error_message) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
		time.Now().UTC().Format(time.RFC3339), event.SessionKey, event.Model, event.APIStyle, event.SummaryModel,
		event.FoldedMessages, event.TokensBefore, event.TokensAfter, event.SummaryTokens, event.LatencyMS,
		event.Status, event.ErrorMessage)
	return err
}

type compressionEvent struct {
	SessionKey     string
	Model          string
	APIStyle       string
	SummaryModel   string
	FoldedMessages int
	TokensBefore   int
	TokensAfter    int
	SummaryTokens  int
	LatencyMS      int64
	Status         string
	ErrorMessage   string
}

// ---------------------------------------------------------------------------
// Gateway hook
// ---------------------------------------------------------------------------

type compressionOutcome struct {
	Status        string
	Reason        string
	Applied       bool
	Blocking      bool
	Messages      int
	TokensBefore  int
	TokensAfter   int
	SummaryTokens int
	Model         string
	Err           error
}

func (o compressionOutcome) skipped(reason string) compressionOutcome {
	o.Status = "skipped"
	o.Reason = reason
	return o
}

func compressionRequestMode(r *http.Request) (force bool, disabled bool) {
	if r == nil {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get(compressionRequestHeader))) {
	case "off", "none", "disabled", "0", "false":
		return false, true
	case "force", "always", "on", "1", "true":
		return true, false
	}
	return false, false
}

// applyCompression folds the request's history in place when the configured
// threshold is crossed. The caller re-serialises the payload afterwards.
func (a *App) applyCompression(r *http.Request, sessionKey, model string, payload map[string]any) compressionOutcome {
	outcome := compressionOutcome{Status: "skipped", Model: model}
	force, disabled := compressionRequestMode(r)
	if disabled {
		outcome.Status = "disabled"
		outcome.Reason = "client_disabled"
		return outcome
	}
	cfg, cfgErr := a.loadCompression()
	if cfgErr != nil {
		log.Printf("compression settings unavailable: %v", cfgErr)
		outcome.Status = "failed"
		outcome.Reason = "settings_unavailable"
		outcome.Err = cfgErr
		return outcome
	}
	if !cfg.Enabled {
		outcome.Status = "disabled"
		outcome.Reason = "gateway_disabled"
		return outcome
	}
	if !cfg.configured() {
		return outcome.skipped("not_configured")
	}
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return outcome.skipped("no_messages")
	}
	window := a.compressionWindow(model, cfg)
	plan, planned := planCompression(cfg, messages, force, window)
	if !planned {
		return outcome.skipped("below_threshold")
	}

	started := time.Now()
	prior := ""
	baseFolded := 0
	previousCompressions := 0
	session, hasSession, sessionErr := a.loadCompressionSession(sessionKey)
	if sessionErr != nil {
		log.Printf("compression session lookup failed: %v", sessionErr)
	}
	if hasSession && session.FoldedMessages > 0 && session.FoldedMessages <= len(plan.Folding) {
		if session.FoldedDigest != "" && session.FoldedDigest == compressionDigest(plan.Folding[:session.FoldedMessages]) {
			prior = session.Summary
			baseFolded = session.FoldedMessages
			previousCompressions = session.Compressions
		}
	}
	delta := plan.Folding[baseFolded:]

	summaryText := prior
	summary := compressionSummary{}
	var summaryErr error
	if len(delta) > 0 || strings.TrimSpace(prior) == "" {
		summary, summaryErr = a.runCompressionSummary(r.Context(), cfg, prior, delta)
		if summaryErr != nil {
			outcome.Status = "failed"
			outcome.Reason = "summarizer_failed"
			outcome.Err = summaryErr
			outcome.Blocking = !cfg.FailOpen
			outcome.Messages = len(plan.Folding)
			outcome.TokensBefore = plan.Before
			status, code := "error", 0
			var upstreamFailure *compressionUpstreamError
			if errors.As(summaryErr, &upstreamFailure) {
				code = upstreamFailure.Status
			}
			proxyID, proxyURI, engine := a.compressionUsageRoute(cfg)
			a.recordUsageKindWithEngine(compressionUsageKind, cfg.Model, proxyID, proxyURI, engine, status, code, time.Since(started), nil, 0, nil, summaryErr)
			_ = a.recordCompressionEvent(compressionEvent{
				SessionKey: sessionKey, Model: model, APIStyle: cfg.APIStyle, SummaryModel: cfg.Model,
				FoldedMessages: len(plan.Folding), TokensBefore: plan.Before, TokensAfter: plan.Before,
				SummaryTokens: 0, LatencyMS: time.Since(started).Milliseconds(), Status: "error",
				ErrorMessage: truncateError(summaryErr.Error()),
			})
			return outcome
		}
		summaryText = summary.Text
	}

	summaryTokens := estimateTextTokens(summaryText)
	summaryMessage := buildCompressionSummaryMessage(cfg, summaryText, len(plan.Folding), plan.Before, 0)
	composed := make([]any, 0, len(plan.Head)+1+len(plan.Tail))
	composed = append(composed, plan.Head...)
	composed = append(composed, summaryMessage)
	composed = append(composed, plan.Tail...)
	after := estimateMessagesTokens(composed)
	// Rebuild the marker with the final estimate so the model sees the real size.
	composed[len(plan.Head)] = buildCompressionSummaryMessage(cfg, summaryText, len(plan.Folding), plan.Before, after)

	payload["messages"] = composed

	outcome.Status = "applied"
	outcome.Applied = true
	outcome.Messages = len(plan.Folding)
	outcome.TokensBefore = plan.Before
	outcome.TokensAfter = after
	outcome.SummaryTokens = summaryTokens

	next := compressionSession{
		SessionKey:     sessionKey,
		Model:          model,
		Summary:        summaryText,
		FoldedDigest:   compressionDigest(plan.Folding),
		FoldedMessages: len(plan.Folding),
		FoldedTokens:   estimateMessagesTokens(plan.Folding),
		Compressions:   previousCompressions + 1,
		TokensBefore:   plan.Before,
		TokensAfter:    after,
		CreatedAt:      session.CreatedAt,
	}
	if err := a.saveCompressionSession(next); err != nil {
		log.Printf("compression session save failed: %v", err)
	}
	proxyID, proxyURI, engine := a.compressionUsageRoute(cfg)
	if len(delta) > 0 {
		a.recordUsageKindWithEngine(compressionUsageKind, cfg.Model, proxyID, proxyURI, engine, "success", 200, time.Since(started), nil, 0, summary.tokenUsage(), nil)
	}
	_ = a.recordCompressionEvent(compressionEvent{
		SessionKey: sessionKey, Model: model, APIStyle: cfg.APIStyle, SummaryModel: cfg.Model,
		FoldedMessages: len(plan.Folding), TokensBefore: plan.Before, TokensAfter: after,
		SummaryTokens: summaryTokens, LatencyMS: time.Since(started).Milliseconds(), Status: "success",
	})
	return outcome
}

// compressionUsageRoute resolves the accounting route used by the summariser so
// the usage record matches the proxy that actually carried the call.
func (a *App) compressionUsageRoute(cfg compressionConfig) (*int64, string, string) {
	if !cfg.UseProxy {
		return nil, "", "direct"
	}
	proxy, engine, err := a.controlPlaneProxy()
	if err != nil || strings.TrimSpace(proxy.URI) == "" {
		return nil, "", "direct"
	}
	id := proxy.ID
	return &id, proxy.URI, engine
}

func setCompressionHeaders(w http.ResponseWriter, outcome compressionOutcome) {
	if w == nil {
		return
	}
	w.Header().Set(compressionRequestHeader, outcome.Status)
	if outcome.Reason != "" {
		w.Header().Set("X-Relay-Compression-Reason", outcome.Reason)
	}
	if !outcome.Applied {
		return
	}
	saved := outcome.TokensBefore - outcome.TokensAfter
	if saved < 0 {
		saved = 0
	}
	w.Header().Set("X-Relay-Compression-Messages", strconv.Itoa(outcome.Messages))
	w.Header().Set("X-Relay-Compression-Before", strconv.Itoa(outcome.TokensBefore))
	w.Header().Set("X-Relay-Compression-After", strconv.Itoa(outcome.TokensAfter))
	w.Header().Set("X-Relay-Compression-Saved-Tokens", strconv.Itoa(saved))
	w.Header().Set("X-Relay-Compression-Model", outcome.Model)
}

// ---------------------------------------------------------------------------
// Administrative API
// ---------------------------------------------------------------------------

type compressionSettingsView struct {
	compressionConfig
	HasAPIKey     bool   `json:"has_api_key"`
	Configured    bool   `json:"configured"`
	DefaultMarker string `json:"default_marker"`
	DefaultPrompt string `json:"default_prompt"`
}

func compressionView(cfg compressionConfig) compressionSettingsView {
	return compressionSettingsView{
		compressionConfig: cfg,
		HasAPIKey:         cfg.APIKey != "",
		Configured:        cfg.configured(),
		DefaultMarker:     defaultCompressionMarker(),
		DefaultPrompt:     defaultCompressionPrompt(),
	}
}

type compressionUpdate struct {
	Enabled            *bool    `json:"enabled"`
	APIStyle           *string  `json:"api_style"`
	BaseURL            *string  `json:"base_url"`
	APIKey             *string  `json:"api_key"`
	ClearAPIKey        bool     `json:"clear_api_key"`
	Model              *string  `json:"model"`
	UseProxy           *bool    `json:"use_proxy"`
	TriggerRatio       *float64 `json:"trigger_ratio"`
	ContextWindow      *int     `json:"context_window"`
	KeepRecentMessages *int     `json:"keep_recent_messages"`
	MinFoldMessages    *int     `json:"min_fold_messages"`
	MaxSummaryTokens   *int     `json:"max_summary_tokens"`
	Temperature        *float64 `json:"temperature"`
	TimeoutSeconds     *int     `json:"timeout_seconds"`
	SummaryRole        *string  `json:"summary_role"`
	Marker             *string  `json:"marker"`
	Prompt             *string  `json:"prompt"`
	FailOpen           *bool    `json:"fail_open"`
}

func (in compressionUpdate) apply(cfg compressionConfig) compressionConfig {
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.APIStyle != nil {
		cfg.APIStyle = *in.APIStyle
	}
	if in.BaseURL != nil {
		cfg.BaseURL = *in.BaseURL
	}
	if in.APIKey != nil {
		cfg.APIKey = *in.APIKey
	}
	if in.ClearAPIKey {
		cfg.APIKey = ""
	}
	if in.Model != nil {
		cfg.Model = *in.Model
	}
	if in.UseProxy != nil {
		cfg.UseProxy = *in.UseProxy
	}
	if in.TriggerRatio != nil {
		cfg.TriggerRatio = *in.TriggerRatio
	}
	if in.ContextWindow != nil {
		cfg.ContextWindow = *in.ContextWindow
	}
	if in.KeepRecentMessages != nil {
		cfg.KeepRecentMessages = *in.KeepRecentMessages
	}
	if in.MinFoldMessages != nil {
		cfg.MinFoldMessages = *in.MinFoldMessages
	}
	if in.MaxSummaryTokens != nil {
		cfg.MaxSummaryTokens = *in.MaxSummaryTokens
	}
	if in.Temperature != nil {
		cfg.Temperature = *in.Temperature
	}
	if in.TimeoutSeconds != nil {
		cfg.TimeoutSeconds = *in.TimeoutSeconds
	}
	if in.SummaryRole != nil {
		cfg.SummaryRole = *in.SummaryRole
	}
	if in.Marker != nil {
		cfg.Marker = *in.Marker
	}
	if in.Prompt != nil {
		cfg.Prompt = *in.Prompt
	}
	if in.FailOpen != nil {
		cfg.FailOpen = *in.FailOpen
	}
	return cfg
}

func (a *App) getCompressionSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.loadCompression()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, compressionView(cfg))
}

func (a *App) putCompressionSettings(w http.ResponseWriter, r *http.Request) {
	var in compressionUpdate
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	current, err := a.loadCompression()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	next, normalizeErr := normalizeCompressionConfig(in.apply(current))
	if normalizeErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": normalizeErr.Error()})
		return
	}
	if err := a.saveCompression(next); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, compressionView(next))
}

type compressionProbeInput struct {
	BaseURL        string  `json:"base_url"`
	APIKey         *string `json:"api_key"`
	Model          string  `json:"model"`
	APIStyle       string  `json:"api_style"`
	UseProxy       *bool   `json:"use_proxy"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}

func compressionProbeTranscript() []any {
	return []any{
		map[string]any{"role": "system", "content": "You are a coding agent working inside D:\\work\\relay."},
		map[string]any{"role": "user", "content": "帮我给 cmd/server/main.go 的 gatewayChat 加一个请求头开关，默认关闭。"},
		map[string]any{"role": "assistant", "content": "我会在 gatewayChat 中读取 X-Relay-Feature 头，默认 auto；并在 withMiddleware 里补 no-store。已确认 handleChat 的调用顺序，准备先改 main.go:1721。"},
		map[string]any{"role": "user", "content": "好，另外把测试补上，go test ./cmd/server 必须通过。"},
	}
}

func (a *App) testCompressionSettings(w http.ResponseWriter, r *http.Request) {
	var in compressionProbeInput
	// An empty body is valid: probe the stored configuration.
	body, bodyErr := readLimitedBody(r.Body, 1<<20)
	if bodyErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(bytes.TrimSpace(body)) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
	}
	cfg, err := a.loadCompression()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.BaseURL) != "" {
		cfg.BaseURL = in.BaseURL
	}
	if strings.TrimSpace(in.Model) != "" {
		cfg.Model = in.Model
	}
	if strings.TrimSpace(in.APIStyle) != "" {
		cfg.APIStyle = in.APIStyle
	}
	if in.APIKey != nil && strings.TrimSpace(*in.APIKey) != "" {
		cfg.APIKey = *in.APIKey
	}
	if in.UseProxy != nil {
		cfg.UseProxy = *in.UseProxy
	}
	if in.TimeoutSeconds > 0 {
		cfg.TimeoutSeconds = in.TimeoutSeconds
	}
	cfg, normalizeErr := normalizeCompressionConfig(cfg)
	if normalizeErr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": normalizeErr.Error()})
		return
	}
	if !cfg.configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base_url and model are required"})
		return
	}
	started := time.Now()
	summary, summaryErr := a.runCompressionSummary(r.Context(), cfg, "", compressionProbeTranscript())
	latency := time.Since(started)
	if summaryErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":         false,
			"error":      truncateError(summaryErr.Error()),
			"latency_ms": latency.Milliseconds(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"summary":           summary.Text,
		"summary_tokens":    estimateTextTokens(summary.Text),
		"prompt_tokens":     summary.PromptTokens,
		"completion_tokens": summary.CompletionTokens,
		"latency_ms":        latency.Milliseconds(),
		"api_style":         cfg.APIStyle,
		"model":             cfg.Model,
		"endpoint":          compressionEndpoint(cfg),
	})
}

type compressionPreviewInput struct {
	Model    string `json:"model"`
	Messages []any  `json:"messages"`
	Force    *bool  `json:"force"`
}

func (a *App) previewCompression(w http.ResponseWriter, r *http.Request) {
	var in compressionPreviewInput
	if err := readJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(in.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "messages are required"})
		return
	}
	cfg, err := a.loadCompression()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !cfg.configured() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "compression gateway is not configured"})
		return
	}
	force := true
	if in.Force != nil {
		force = *in.Force
	}
	window := a.compressionWindow(in.Model, cfg)
	plan, planned := planCompression(cfg, in.Messages, force, window)
	if !planned {
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": false,
			"reason":  "below_threshold",
			"window":  window,
			"tokens":  estimateMessagesTokens(in.Messages),
		})
		return
	}
	started := time.Now()
	summary, summaryErr := a.runCompressionSummary(r.Context(), cfg, "", plan.Folding)
	if summaryErr != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"applied": false, "error": truncateError(summaryErr.Error())})
		return
	}
	summaryMessage := buildCompressionSummaryMessage(cfg, summary.Text, len(plan.Folding), plan.Before, 0)
	composed := make([]any, 0, len(plan.Head)+1+len(plan.Tail))
	composed = append(composed, plan.Head...)
	composed = append(composed, summaryMessage)
	composed = append(composed, plan.Tail...)
	after := estimateMessagesTokens(composed)
	composed[len(plan.Head)] = buildCompressionSummaryMessage(cfg, summary.Text, len(plan.Folding), plan.Before, after)
	writeJSON(w, http.StatusOK, map[string]any{
		"applied":         true,
		"window":          window,
		"folded_messages": len(plan.Folding),
		"kept_messages":   len(plan.Tail),
		"tokens_before":   plan.Before,
		"tokens_after":    after,
		"saved_tokens":    maxInt(0, plan.Before-after),
		"summary":         summary.Text,
		"summary_tokens":  estimateTextTokens(summary.Text),
		"latency_ms":      time.Since(started).Milliseconds(),
		"summary_message": composed[len(plan.Head)],
		"messages":        composed,
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type compressionDailyStat struct {
	Date         string `json:"date"`
	Compressions int64  `json:"compressions"`
	SavedTokens  int64  `json:"saved_tokens"`
	Failures     int64  `json:"failures"`
}

type compressionEventRow struct {
	ID             int64  `json:"id"`
	CreatedAt      string `json:"created_at"`
	SessionKey     string `json:"session_key"`
	Model          string `json:"model"`
	APIStyle       string `json:"api_style"`
	SummaryModel   string `json:"summary_model"`
	FoldedMessages int64  `json:"folded_messages"`
	TokensBefore   int64  `json:"tokens_before"`
	TokensAfter    int64  `json:"tokens_after"`
	SavedTokens    int64  `json:"saved_tokens"`
	SummaryTokens  int64  `json:"summary_tokens"`
	LatencyMS      int64  `json:"latency_ms"`
	Status         string `json:"status"`
	ErrorMessage   string `json:"error_message"`
}

func (a *App) compressionStats(w http.ResponseWriter, r *http.Request) {
	days := 7
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 90 {
			days = parsed
		}
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	response := struct {
		Days             int                    `json:"days"`
		Compressions     int64                  `json:"compressions"`
		Failures         int64                  `json:"failures"`
		TokensBefore     int64                  `json:"tokens_before"`
		TokensAfter      int64                  `json:"tokens_after"`
		SavedTokens      int64                  `json:"saved_tokens"`
		SummaryTokens    int64                  `json:"summary_tokens"`
		AverageLatencyMS int64                  `json:"average_latency_ms"`
		Sessions         int64                  `json:"sessions"`
		Daily            []compressionDailyStat `json:"daily"`
		Recent           []compressionEventRow  `json:"recent"`
	}{Days: days, Daily: []compressionDailyStat{}, Recent: []compressionEventRow{}}

	if err := a.db.QueryRow(`SELECT
COUNT(*),
COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0),
COALESCE(SUM(tokens_before),0),
COALESCE(SUM(tokens_after),0),
COALESCE(SUM(summary_tokens),0),
COALESCE(CAST(AVG(latency_ms) AS INTEGER),0)
FROM compression_events WHERE created_at>=?`, since).Scan(
		&response.Compressions, &response.Failures, &response.TokensBefore, &response.TokensAfter,
		&response.SummaryTokens, &response.AverageLatencyMS,
	); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	response.SavedTokens = response.TokensBefore - response.TokensAfter
	if response.SavedTokens < 0 {
		response.SavedTokens = 0
	}
	if err := a.db.QueryRow("SELECT COUNT(*) FROM compression_sessions").Scan(&response.Sessions); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if rows, err := a.db.Query(`SELECT substr(created_at,1,10) AS day,
COUNT(*),
COALESCE(SUM(tokens_before-tokens_after),0),
COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END),0)
FROM compression_events WHERE created_at>=? GROUP BY day ORDER BY day`, since); err == nil {
		defer rows.Close()
		for rows.Next() {
			var stat compressionDailyStat
			if scanErr := rows.Scan(&stat.Date, &stat.Compressions, &stat.SavedTokens, &stat.Failures); scanErr != nil {
				break
			}
			response.Daily = append(response.Daily, stat)
		}
	}
	if rows, err := a.db.Query(`SELECT id,created_at,session_key,model,api_style,summary_model,folded_messages,tokens_before,tokens_after,tokens_before-tokens_after,summary_tokens,latency_ms,status,error_message
FROM compression_events ORDER BY id DESC LIMIT 20`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var row compressionEventRow
			if scanErr := rows.Scan(&row.ID, &row.CreatedAt, &row.SessionKey, &row.Model, &row.APIStyle, &row.SummaryModel,
				&row.FoldedMessages, &row.TokensBefore, &row.TokensAfter, &row.SavedTokens, &row.SummaryTokens,
				&row.LatencyMS, &row.Status, &row.ErrorMessage); scanErr != nil {
				break
			}
			response.Recent = append(response.Recent, row)
		}
	}
	writeJSON(w, http.StatusOK, response)
}

type compressionSessionRow struct {
	SessionKey     string `json:"session_key"`
	Model          string `json:"model"`
	Preview        string `json:"preview"`
	FoldedMessages int64  `json:"folded_messages"`
	FoldedTokens   int64  `json:"folded_tokens"`
	Compressions   int64  `json:"compressions"`
	TokensBefore   int64  `json:"tokens_before"`
	TokensAfter    int64  `json:"tokens_after"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

func (a *App) listCompressionSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 500 {
			limit = parsed
		}
	}
	rows, err := a.db.Query(`SELECT session_key,model,summary,folded_messages,folded_tokens,compressions,tokens_before,tokens_after,created_at,updated_at
FROM compression_sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()
	sessions := []compressionSessionRow{}
	for rows.Next() {
		var session compressionSessionRow
		var summary string
		if scanErr := rows.Scan(&session.SessionKey, &session.Model, &summary, &session.FoldedMessages,
			&session.FoldedTokens, &session.Compressions, &session.TokensBefore, &session.TokensAfter,
			&session.CreatedAt, &session.UpdatedAt); scanErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": scanErr.Error()})
			return
		}
		session.Preview = truncateCompressionRunes(strings.TrimSpace(summary), 160)
		sessions = append(sessions, session)
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (a *App) deleteCompressionSession(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "session key is required"})
		return
	}
	result, err := a.db.Exec("DELETE FROM compression_sessions WHERE session_key=?", key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	removed, _ := result.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

func (a *App) resetCompressionSessions(w http.ResponseWriter, r *http.Request) {
	result, err := a.db.Exec("DELETE FROM compression_sessions")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	removed, _ := result.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}
