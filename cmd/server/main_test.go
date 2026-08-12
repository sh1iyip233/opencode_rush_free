package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func testApp(t testing.TB) *App {
	t.Helper()
	db, err := sql.Open("sqlite", "file:test-"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte("test-encryption-key"))
	a := &App{
		db:            db,
		key:           h[:],
		session:       "test-session-secret-that-is-long-enough",
		admin:         "test-password",
		loginAttempts: make(map[string]loginAttempt),
		gatewaySem:    make(chan struct{}, 10),
	}
	if err := a.ensureAdmin(); err != nil {
		t.Fatal(err)
	}
	if err := a.ensureClientKey(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return a
}

func insertUsageAt(t *testing.T, a *App, createdAt time.Time, model, status string, totalTokens *int64) {
	t.Helper()
	statusCode := http.StatusOK
	if status == "error" {
		statusCode = http.StatusBadGateway
	}
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,retry_count,total_tokens) VALUES(?,?,?,?,?,?,?,?)", createdAt.UTC().Format(time.RFC3339), "chat", model, status, statusCode, 100, 0, totalTokens); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateAddsNullableUsageColumnsToExistingUsageTable(t *testing.T) {
	db, err := sql.Open("sqlite", "file:legacy-usage-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE usage_requests (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		created_at TEXT NOT NULL,
		model TEXT NOT NULL,
		status TEXT NOT NULL,
		status_code INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		retry_count INTEGER NOT NULL DEFAULT 0,
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		total_tokens INTEGER,
		error_message TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("PRAGMA table_info(usage_requests)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundFirstTokenLatency := false
	foundClientName := false
	foundClientUserAgent := false
	foundStream := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "first_token_latency_ms" {
			foundFirstTokenLatency = true
			if notNull != 0 {
				t.Fatal("first_token_latency_ms must remain nullable for historical rows")
			}
		}
		if name == "stream" {
			foundStream = true
			if notNull != 0 {
				t.Fatal("stream must remain nullable for historical rows")
			}
		}
		if name == "client_name" {
			foundClientName = true
			if notNull != 0 {
				t.Fatal("client_name must remain nullable for historical rows")
			}
		}
		if name == "client_user_agent" {
			foundClientUserAgent = true
			if notNull != 0 {
				t.Fatal("client_user_agent must remain nullable for historical rows")
			}
		}
	}
	if !foundFirstTokenLatency {
		t.Fatal("migration did not add first_token_latency_ms")
	}
	if !foundStream {
		t.Fatal("migration did not add stream")
	}
	if !foundClientName {
		t.Fatal("migration did not add client_name")
	}
	if !foundClientUserAgent {
		t.Fatal("migration did not add client_user_agent")
	}
}

func TestParseChatEnvelopeReadsStream(t *testing.T) {
	streaming, _, err := parseChatEnvelope([]byte(`{"model":"model:free","messages":[],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !streaming.Stream {
		t.Fatal("stream=true was not parsed")
	}
	nonStreaming, _, err := parseChatEnvelope([]byte(`{"model":"model:free","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if nonStreaming.Stream {
		t.Fatal("an omitted stream field must be recorded as non-streaming")
	}
}

func TestUsageClientUserAgent(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("User-Agent", "claude-code/2.0.0 (darwin; arm64)")
	if got, want := usageClientUserAgent(r), "claude-code/2.0.0 (darwin; arm64)"; got != want {
		t.Fatalf("usageClientUserAgent() = %q, want %q", got, want)
	}
}

func TestUsageListIncludesClientAndStreamState(t *testing.T) {
	a := testApp(t)
	a.recordGatewayUsageWithStream(&ClientKey{ID: 1, Name: "build-agent"}, "stream:free", "stream:free", nil, "", "direct", "success", http.StatusOK, time.Second, nil, 0, nil, nil, "claude-code/2.0.0 (darwin; arm64)", true, nil)
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,retry_count) VALUES(?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339), "chat", "legacy:free", "success", http.StatusOK, 1, 0); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	a.usageList(recorder, httptest.NewRequest(http.MethodGet, "/api/usage/requests?page=1&page_size=25", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage list failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items []usageRequest `json:"items"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if len(response.Items) != 2 {
		t.Fatalf("usage item count = %d, want 2", len(response.Items))
	}

	var streaming, legacy *usageRequest
	for i := range response.Items {
		switch response.Items[i].Model {
		case "stream:free":
			streaming = &response.Items[i]
		case "legacy:free":
			legacy = &response.Items[i]
		}
	}
	if streaming == nil || streaming.ClientUserAgent != "claude-code/2.0.0 (darwin; arm64)" || streaming.Stream == nil || !*streaming.Stream {
		t.Fatalf("streaming usage was not returned with its client and stream state: %#v", streaming)
	}
	if legacy == nil || legacy.ClientUserAgent != "" || legacy.Stream != nil {
		t.Fatalf("legacy usage must retain an unknown stream state: %#v", legacy)
	}
}

func TestProxyParserSanitizesCredentials(t *testing.T) {
	p, err := parseProxy("socks5://alice:s3cret@127.0.0.1:1080")
	if err != nil {
		t.Fatal(err)
	}
	if p.Password != "s3cret" || p.Username != "alice" {
		t.Fatalf("credentials not parsed: %#v", p)
	}
	if strings.Contains(p.URI, "s3cret") {
		t.Fatalf("password leaked in URI: %s", p.URI)
	}
}

func TestFreeClassificationAndUsageParsing(t *testing.T) {
	if ok, reason := classifyFree("model:free", map[string]any{}); !ok || reason != "id_suffix" {
		t.Fatalf("suffix classification: %v %s", ok, reason)
	}
	if ok, reason := classifyFree("model-free", map[string]any{}); !ok || reason != "id_suffix" {
		t.Fatalf("dash suffix classification: %v %s", ok, reason)
	}
	if ok, reason := classifyFree("model", map[string]any{"pricing": map[string]any{"prompt": "0", "completion": "0"}}); !ok || reason != "pricing_zero" {
		t.Fatalf("pricing classification: %v %s", ok, reason)
	}
	if ok, _ := classifyFree("paid", map[string]any{"pricing": map[string]any{"prompt": "0.1", "completion": "0"}}); ok {
		t.Fatal("paid model classified as free")
	}
	u := parseUsageBytes([]byte(`{"usage":{"prompt_tokens":2,"completion_tokens":5,"total_tokens":7}}`))
	if u == nil || *u.Total != 7 {
		t.Fatalf("usage parsing failed: %#v", u)
	}
}

func TestForceFreeModelOverridesPersistAcrossRefreshes(t *testing.T) {
	a := testApp(t)
	upstreamModels := []byte(`{"data":[{"id":"alpha:free"},{"id":"provider/free-without-suffix"},{"id":"paid","pricing":{"prompt":"0.1","completion":"0.2"}}]}`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("unexpected upstream path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamModels)
	}))
	defer upstream.Close()

	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{"base_url":"`+upstream.URL+`"}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("configure upstream failed: %d: %s", put.Code, put.Body.String())
	}

	firstRefresh := httptest.NewRecorder()
	a.refreshModels(firstRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if firstRefresh.Code != http.StatusOK {
		t.Fatalf("first refresh failed: %d: %s", firstRefresh.Code, firstRefresh.Body.String())
	}
	var detected int
	if err := a.db.QueryRow("SELECT is_free FROM models WHERE model_id=?", "provider/free-without-suffix").Scan(&detected); err != nil || detected != 0 {
		t.Fatalf("model without free metadata must not be auto-detected: free=%d err=%v", detected, err)
	}

	add := httptest.NewRecorder()
	a.createForceFreeModel(add, httptest.NewRequest(http.MethodPost, "/api/model-overrides/free", strings.NewReader(`{"model_id":"provider/free-without-suffix"}`)))
	if add.Code != http.StatusCreated {
		t.Fatalf("creating free override failed: %d: %s", add.Code, add.Body.String())
	}
	var isFree int
	var reason string
	if err := a.db.QueryRow("SELECT is_free,free_reason FROM models WHERE model_id=?", "provider/free-without-suffix").Scan(&isFree, &reason); err != nil || isFree != 1 || reason != "manual_override" {
		t.Fatalf("manual override was not immediately applied: free=%d reason=%q err=%v", isFree, reason, err)
	}

	models, _, err := a.loadModelRuntime()
	if err != nil || models["provider/free-without-suffix"].Record.FreeReason != "manual_override" {
		t.Fatalf("manual override was not exposed to runtime: %#v err=%v", models, err)
	}
	freeList := httptest.NewRecorder()
	a.listFreeModels(freeList, httptest.NewRequest(http.MethodGet, "/api/models/free", nil))
	var listed []ModelRecord
	if freeList.Code != http.StatusOK || json.NewDecoder(freeList.Body).Decode(&listed) != nil || len(listed) != 2 || listed[1].ModelID != "provider/free-without-suffix" {
		t.Fatalf("manual override was not exposed by the free model API: status=%d models=%#v", freeList.Code, listed)
	}
	knownClientKey := "force-free-model-test-client"
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec("INSERT INTO client_keys(name,key_hash,key_hint,enabled,created_at,updated_at) VALUES(?,?,?,?,?,?)", "Force free test", hashToken(knownClientKey), "test", 1, now, now); err != nil {
		t.Fatal(err)
	}
	a.invalidateClientKeyCache()
	gatewayList := httptest.NewRecorder()
	gatewayRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	gatewayRequest.Header.Set("Authorization", "Bearer "+knownClientKey)
	a.gatewayModels(gatewayList, gatewayRequest)
	var gatewayPayload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if gatewayList.Code != http.StatusOK || json.NewDecoder(gatewayList.Body).Decode(&gatewayPayload) != nil || len(gatewayPayload.Data) != 2 || gatewayPayload.Data[1].ID != "provider/free-without-suffix" {
		t.Fatalf("manual override was not exposed by the gateway model API: status=%d payload=%#v", gatewayList.Code, gatewayPayload)
	}

	secondRefresh := httptest.NewRecorder()
	a.refreshModels(secondRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if secondRefresh.Code != http.StatusOK {
		t.Fatalf("second refresh failed: %d: %s", secondRefresh.Code, secondRefresh.Body.String())
	}
	if err := a.db.QueryRow("SELECT is_free,free_reason FROM models WHERE model_id=?", "provider/free-without-suffix").Scan(&isFree, &reason); err != nil || isFree != 1 || reason != "manual_override" {
		t.Fatalf("refresh lost manual override: free=%d reason=%q err=%v", isFree, reason, err)
	}

	upstreamModels = []byte(`{"data":[{"id":"alpha:free"},{"id":"paid","pricing":{"prompt":"0.1","completion":"0.2"}}]}`)
	missingRefresh := httptest.NewRecorder()
	a.refreshModels(missingRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if missingRefresh.Code != http.StatusOK {
		t.Fatalf("refresh with missing forced model failed: %d: %s", missingRefresh.Code, missingRefresh.Body.String())
	}
	var rules int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM force_free_models WHERE model_id=?", "provider/free-without-suffix").Scan(&rules); err != nil || rules != 1 {
		t.Fatalf("missing upstream model must preserve its override: count=%d err=%v", rules, err)
	}

	upstreamModels = []byte(`{"data":[{"id":"alpha:free"},{"id":"provider/free-without-suffix"}]}`)
	reappearedRefresh := httptest.NewRecorder()
	a.refreshModels(reappearedRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if reappearedRefresh.Code != http.StatusOK {
		t.Fatalf("refresh after model reappeared failed: %d: %s", reappearedRefresh.Code, reappearedRefresh.Body.String())
	}
	if err := a.db.QueryRow("SELECT is_free,free_reason FROM models WHERE model_id=?", "provider/free-without-suffix").Scan(&isFree, &reason); err != nil || isFree != 1 || reason != "manual_override" {
		t.Fatalf("reappeared model did not restore override: free=%d reason=%q err=%v", isFree, reason, err)
	}

	deleteOverride := httptest.NewRecorder()
	a.deleteForceFreeModel(deleteOverride, httptest.NewRequest(http.MethodDelete, "/api/model-overrides/free", strings.NewReader(`{"model_id":"provider/free-without-suffix"}`)))
	if deleteOverride.Code != http.StatusOK {
		t.Fatalf("deleting free override failed: %d: %s", deleteOverride.Code, deleteOverride.Body.String())
	}
	if err := a.db.QueryRow("SELECT is_free,free_reason FROM models WHERE model_id=?", "provider/free-without-suffix").Scan(&isFree, &reason); err != nil || isFree != 0 || reason != "" {
		t.Fatalf("deleting override did not restore automatic classification: free=%d reason=%q err=%v", isFree, reason, err)
	}
}

func TestForceFreeModelOverrideInputValidation(t *testing.T) {
	for _, input := range []string{"", "  ", "model id", "model\nname"} {
		if _, err := normalizeForceFreeModelID(input); err == nil {
			t.Fatalf("expected invalid model ID %q to be rejected", input)
		}
	}
	if modelID, err := normalizeForceFreeModelID(" provider/model "); err != nil || modelID != "provider/model" {
		t.Fatalf("valid model ID was not normalized: %q %v", modelID, err)
	}
}

func TestForceFreeModelOverrideRoutesRequireAdmin(t *testing.T) {
	a := testApp(t)
	mux := http.NewServeMux()
	a.routes(mux)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/model-overrides/free", nil),
		httptest.NewRequest(http.MethodPost, "/api/model-overrides/free", strings.NewReader(`{"model_id":"provider/model"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/model-overrides/free", strings.NewReader(`{"model_id":"provider/model"}`)),
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s should require admin, got %d: %s", request.Method, request.URL.Path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestMaintenanceModeAllowsOnlyWhitelistedClientKeys(t *testing.T) {
	a := testApp(t)
	allowedKey := "maintenance-allowed-key"
	blockedKey := "maintenance-blocked-key"
	now := time.Now().UTC().Format(time.RFC3339)
	for _, entry := range []struct {
		name    string
		value   string
		allowed int
	}{
		{name: "Allowed", value: allowedKey, allowed: 1},
		{name: "Blocked", value: blockedKey, allowed: 0},
	} {
		if _, err := a.db.Exec("INSERT INTO client_keys(name,key_hash,key_hint,enabled,maintenance_allowed,created_at,updated_at) VALUES(?,?,?,?,?,?,?)", entry.name, hashToken(entry.value), "test", 1, entry.allowed, now, now); err != nil {
			t.Fatal(err)
		}
	}
	a.invalidateClientKeyCache()
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "maintenance:free", "Maintenance", 1, "test", now); err != nil {
		t.Fatal(err)
	}
	a.invalidateModelRuntimeCache()

	mux := http.NewServeMux()
	a.routes(mux)
	handler := a.withMiddleware(mux)
	request := func(path, key string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	if response := request("/v1/models", blockedKey); response.Code != http.StatusOK {
		t.Fatalf("gateway should be open before maintenance mode: %d %s", response.Code, response.Body.String())
	}

	put := httptest.NewRecorder()
	a.putMaintenanceSettings(put, httptest.NewRequest(http.MethodPut, "/api/settings/maintenance", strings.NewReader(`{"enabled":true}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("enabling maintenance mode failed: %d %s", put.Code, put.Body.String())
	}
	if response := request("/v1/models", blockedKey); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "maintenance_mode") {
		t.Fatalf("blocked key should receive maintenance 503: %d %s", response.Code, response.Body.String())
	}
	if response := request("/v1/models", allowedKey); response.Code != http.StatusOK {
		t.Fatalf("whitelisted key should pass maintenance mode: %d %s", response.Code, response.Body.String())
	}
	if response := request("/v1/models", "invalid-key"); response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid key must keep the regular authentication response: %d", response.Code)
	}
	if response := request("/healthz", ""); response.Code != http.StatusOK {
		t.Fatalf("health check must remain available during maintenance: %d", response.Code)
	}
}

func TestImageInputDetectionAndModelCapabilities(t *testing.T) {
	textOnly := []byte(`{"architecture":{"input_modalities":["text"],"modality":"text->text"}}`)
	if known, supports := modelImageSupport(textOnly); !known || supports {
		t.Fatalf("text-only model capability was not detected: known=%v supports=%v", known, supports)
	}
	vision := []byte(`{"architecture":{"input_modalities":["text","image"],"modality":"text+image->text"}}`)
	if known, supports := modelImageSupport(vision); !known || !supports {
		t.Fatalf("vision model capability was not detected: known=%v supports=%v", known, supports)
	}
	if !requestHasImageInput([]byte(`{"model":"alpha:free","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.test/a.png"}}]}]}`)) {
		t.Fatal("image_url content was not detected")
	}
	if requestHasImageInput([]byte(`{"model":"alpha:free","messages":[{"role":"user","content":"plain text"}]}`)) {
		t.Fatal("plain text was incorrectly detected as image input")
	}
}

func TestVisionBridgeTransformsImageContent(t *testing.T) {
	original := []byte(`{"model":"text-model:free","messages":[{"role":"user","content":[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}],"stream":true}`)
	helperBody, err := buildVisionRequest(original, "vision-provider/model")
	if err != nil {
		t.Fatal(err)
	}
	var helper map[string]any
	if err := json.Unmarshal(helperBody, &helper); err != nil {
		t.Fatal(err)
	}
	if helper["model"] != "vision-provider/model" || helper["stream"] != false {
		t.Fatalf("helper request was not normalized: %#v", helper)
	}
	description, err := extractVisionDescription([]byte(`{"choices":[{"message":{"content":"a red square"}},],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if err == nil {
		// The malformed trailing comma must be rejected rather than silently dropping the helper response.
		t.Fatal("malformed helper response was accepted")
	}
	description, err = extractVisionDescription([]byte(`{"choices":[{"message":{"content":"a red square"}}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	if err != nil || description != "a red square" {
		t.Fatalf("vision description was not extracted: %q %v", description, err)
	}
	description, err = extractVisionDescription([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"a blue circle"}}]}`))
	if err != nil || description != "a blue circle" {
		t.Fatalf("reasoning fallback was not extracted: %q %v", description, err)
	}
	description = "a red square"
	rewritten, err := replaceImageContent(original, description)
	if err != nil {
		t.Fatal(err)
	}
	if requestHasImageInput(rewritten) || !strings.Contains(string(rewritten), "a red square") || !strings.Contains(string(rewritten), "What is this?") {
		t.Fatalf("image content was not safely rewritten: %s", rewritten)
	}
}

func TestVisionBridgeOnlyUsesMessageImageContent(t *testing.T) {
	toolSchemaOnly := []byte(`{"model":"text-model:free","messages":[{"role":"user","content":"describe the file"}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object","properties":{"image_url":{"type":"string"}}}}}]}`)
	if requestHasImageInput(toolSchemaOnly) {
		t.Fatal("an image_url field in a tool schema was treated as image input")
	}

	objectContent := []byte(`{"model":"text-model:free","messages":[{"role":"user","content":{"type":"input_image","image_url":"https://example.test/image.png"}}]}`)
	if !requestHasImageInput(objectContent) {
		t.Fatal("object-form image content was not detected")
	}
	rewritten, err := replaceImageContent(objectContent, "a green triangle")
	if err != nil {
		t.Fatal(err)
	}
	if requestHasImageInput(rewritten) || !strings.Contains(string(rewritten), "a green triangle") {
		t.Fatalf("object-form image content was not safely rewritten: %s", rewritten)
	}
}

func TestVisionRequestContextSurvivesClientCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(parent)
	cancelParent()
	ctx, cancel := visionRequestContext(r)
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("vision helper inherited the canceled client context")
	default:
	}
}

func TestUsageRecordsDistinguishVisionHelper(t *testing.T) {
	a := testApp(t)
	prompt, completion, total := int64(4), int64(6), int64(10)
	firstToken := 42 * time.Millisecond
	a.recordUsageKind("vision_helper", "vision-provider/model", nil, "", "success", http.StatusOK, 120*time.Millisecond, &firstToken, 0, &tokenUsage{Prompt: &prompt, Completion: &completion, Total: &total}, nil)
	list := httptest.NewRecorder()
	a.usageList(list, httptest.NewRequest(http.MethodGet, "/api/usage/requests?limit=10", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("usage list failed: %d: %s", list.Code, list.Body.String())
	}
	var rows []usageRequest
	if err := json.NewDecoder(list.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RequestKind != "vision_helper" || rows[0].Model != "vision-provider/model" || rows[0].PromptTokens == nil || *rows[0].PromptTokens != 4 || rows[0].CompletionTokens == nil || *rows[0].CompletionTokens != 6 || rows[0].TotalTokens == nil || *rows[0].TotalTokens != 10 || rows[0].FirstTokenLatencyMS == nil || *rows[0].FirstTokenLatencyMS != 42 {
		t.Fatalf("vision helper usage was not recorded separately: %#v", rows)
	}
}

func TestUsageErrorOriginsExcludeExternalFailuresFromSuccessRate(t *testing.T) {
	a := testApp(t)
	a.recordUsage("model:free", nil, "", "success", http.StatusOK, time.Millisecond, nil, 0, nil, nil)
	a.recordUsage("model:free", nil, "", "error", http.StatusBadGateway, time.Millisecond, nil, 0, nil, errors.New("Post upstream: context canceled"))
	a.recordUsage("model:free", nil, "", "error", http.StatusBadRequest, time.Millisecond, nil, 0, nil, errors.New("unsupported input modality"))
	a.recordUsageKind("vision_helper", "vision-model", nil, "", "success", http.StatusOK, time.Millisecond, nil, 0, nil, nil)

	recorder := httptest.NewRecorder()
	a.statsSummary(recorder, httptest.NewRequest(http.MethodGet, "/api/stats/summary", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("summary failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var summary struct {
		Requests         int64   `json:"requests"`
		CountedRequests  int64   `json:"counted_requests"`
		ExternalRequests int64   `json:"external_requests"`
		Success          int64   `json:"success"`
		SuccessRate      float64 `json:"success_rate"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 3 || summary.CountedRequests != 2 || summary.ExternalRequests != 1 || summary.Success != 1 || summary.SuccessRate != 0.5 {
		t.Fatalf("external failure affected summary counters: %#v", summary)
	}

	userErrors := httptest.NewRecorder()
	a.usageList(userErrors, httptest.NewRequest(http.MethodGet, "/api/usage/requests?page=1&status=error", nil))
	var userPage struct {
		Items []usageRequest `json:"items"`
	}
	if err := json.NewDecoder(userErrors.Body).Decode(&userPage); err != nil {
		t.Fatal(err)
	}
	if len(userPage.Items) != 1 || userPage.Items[0].ErrorOrigin != "user" {
		t.Fatalf("user error filter returned wrong records: %#v", userPage.Items)
	}

	external := httptest.NewRecorder()
	a.usageList(external, httptest.NewRequest(http.MethodGet, "/api/usage/requests?page=1&status=external", nil))
	var externalPage struct {
		Items []usageRequest `json:"items"`
	}
	if err := json.NewDecoder(external.Body).Decode(&externalPage); err != nil {
		t.Fatal(err)
	}
	if len(externalPage.Items) != 1 || externalPage.Items[0].ErrorOrigin != "external" {
		t.Fatalf("external filter returned wrong records: %#v", externalPage.Items)
	}
}

func TestUsageErrorOriginClassifiesProviderAndInternalFailures(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		status  string
		code    int
		message string
		want    string
	}{
		{name: "successful chat", kind: "chat", status: "success", code: http.StatusOK, want: "none"},
		{name: "provider validation", kind: "chat", status: "error", code: http.StatusBadRequest, message: "Error from provider (Console): Upstream request failed", want: "external"},
		{name: "client cancellation", kind: "chat", status: "error", code: 499, message: "context canceled", want: "external"},
		{name: "gateway failure", kind: "chat", status: "error", code: http.StatusBadGateway, message: "proxyconnect tcp: i/o timeout", want: "external"},
		{name: "local invalid input", kind: "chat", status: "error", code: http.StatusBadRequest, message: "unsupported input modality", want: "user"},
		{name: "image helper", kind: "vision_helper", status: "error", code: http.StatusBadRequest, message: "invalid input", want: "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageErrorOrigin(tc.kind, tc.status, tc.code, tc.message); got != tc.want {
				t.Fatalf("origin=%q want %q", got, tc.want)
			}
		})
	}
}

func TestProxyPoolFailureCountsTowardModelSuccessRate(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "pool:free", "Pool", 1, "test", now); err != nil {
		t.Fatal(err)
	}
	a.recordGatewayUsageWithOrigin(nil, "pool:free", "pool:free", nil, "", "builtin", "error", http.StatusBadGateway, time.Second, nil, 3, nil, nil, errors.New("proxyconnect tcp: i/o timeout"), "internal")
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,retry_count,error_origin) VALUES(?,?,?,?,?,?,?,?)", now, "chat", "pool:free", "success", http.StatusOK, 100, 0, "none"); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	a.statsModels(recorder, httptest.NewRequest(http.MethodGet, "/api/stats/models?window=24h", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stats request failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var stats []dimensionStat
	if err := json.NewDecoder(recorder.Body).Decode(&stats); err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Requests != 2 || stats[0].Success != 1 || stats[0].ExternalErrors != 0 || stats[0].SuccessRate != 0.5 {
		t.Fatalf("proxy pool failure was not counted: %#v", stats)
	}
}

func TestCanceledGatewayRequestIsExternalAndDoesNotPenalizeResin(t *testing.T) {
	a := testApp(t)
	proxyStarted := make(chan struct{}, 1)
	releaseProxy := make(chan struct{})
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case proxyStarted <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-releaseProxy:
		}
	}))
	defer func() {
		close(releaseProxy)
		proxy.Close()
	}()

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
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "cancel:free", "Cancel", 1, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE settings SET value=? WHERE key='client_key'", hashToken("cancel-test-client")); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateLegacyClientKey(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cancel:free","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer cancel-test-client")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		a.gatewayChat(response, request)
		close(done)
	}()

	select {
	case <-proxyStarted:
		cancel()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("gateway request did not reach the Resin proxy")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop after client cancellation")
	}

	var origin, message string
	var statusCode, retryCount int
	if err := a.db.QueryRow("SELECT error_origin,status_code,retry_count,COALESCE(error_message,'') FROM usage_requests WHERE request_kind='chat' ORDER BY id DESC LIMIT 1").Scan(&origin, &statusCode, &retryCount, &message); err != nil {
		t.Fatal(err)
	}
	if origin != "external" || statusCode != statusClientClosedRequest || retryCount != 0 || message != context.Canceled.Error() {
		t.Fatalf("canceled request was misclassified: origin=%q status=%d retries=%d message=%q", origin, statusCode, retryCount, message)
	}
	a.resinMu.Lock()
	resinFailures := a.resinFailureCount
	a.resinMu.Unlock()
	if resinFailures != 0 {
		t.Fatalf("client cancellation penalized Resin: failures=%d", resinFailures)
	}

	statusRecorder := httptest.NewRecorder()
	a.publicStatus(statusRecorder, httptest.NewRequest(http.MethodGet, "/api/public/status", nil))
	if statusRecorder.Code != http.StatusOK {
		t.Fatalf("public status failed: %d %s", statusRecorder.Code, statusRecorder.Body.String())
	}
	var statusPayload struct {
		Models []publicModelStatus `json:"models"`
	}
	if err := json.NewDecoder(statusRecorder.Body).Decode(&statusPayload); err != nil {
		t.Fatal(err)
	}
	if len(statusPayload.Models) != 1 || statusPayload.Models[0].ExternalErrors24h != 1 || statusPayload.Models[0].SuccessRate != nil {
		t.Fatalf("canceled request affected public availability: %#v", statusPayload.Models)
	}
}

func TestTerminalGatewayFailureRequiresObservedDownstreamCancellation(t *testing.T) {
	routeErr := fmt.Errorf("proxy transport: %w", context.Canceled)
	gotErr, status, origin := terminalGatewayFailure(nil, routeErr)
	if !errors.Is(gotErr, context.Canceled) || status != http.StatusBadGateway || origin != "internal" {
		t.Fatalf("proxy cancellation was excluded: err=%v status=%d origin=%q", gotErr, status, origin)
	}
	gotErr, status, origin = terminalGatewayFailure(context.Canceled, routeErr)
	if !errors.Is(gotErr, context.Canceled) || status != statusClientClosedRequest || origin != "external" {
		t.Fatalf("downstream cancellation was counted: err=%v status=%d origin=%q", gotErr, status, origin)
	}
}

func TestBackfillUsageErrorOriginsClassifiesExistingRecords(t *testing.T) {
	a := testApp(t)
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,retry_count,error_message) VALUES(?,?,?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339), "chat", "model:free", "error", http.StatusBadGateway, 1, 0, "Post upstream: context canceled"); err != nil {
		t.Fatal(err)
	}
	if err := backfillUsageErrorOrigins(a.db); err != nil {
		t.Fatal(err)
	}
	var origin string
	if err := a.db.QueryRow("SELECT error_origin FROM usage_requests").Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin != "external" {
		t.Fatalf("existing external failure origin=%q, want external", origin)
	}
}

func TestSSEContentDetectorFindsFirstNonEmptyContentAcrossChunks(t *testing.T) {
	detector := &sseContentDetector{}
	if detector.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n")) {
		t.Fatal("empty role event was treated as first content")
	}
	if detector.Observe([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n")) {
		t.Fatal("reasoning metadata was treated as first content")
	}
	if detector.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":[{\"type\":\"text\",\"te")) {
		t.Fatal("partial SSE event was treated as complete")
	}
	if !detector.Observe([]byte("xt\":\"hello\"}]}}]}\n")) {
		t.Fatal("text content split across reads was not detected")
	}
	if detector.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"again\"}}]}\n")) {
		t.Fatal("detector fired more than once")
	}

	withoutNewline := &sseContentDetector{}
	if withoutNewline.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}")) {
		t.Fatal("unterminated event was processed before flush")
	}
	if !withoutNewline.Flush() {
		t.Fatal("final unterminated event was not detected on flush")
	}
}

func TestUsageListPaginationAndCombinedFilters(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	ten, twenty := int64(10), int64(20)
	insertUsageAt(t, a, now.Add(-2*time.Hour), "model-a", "success", &ten)
	insertUsageAt(t, a, now.Add(-45*time.Minute), "model-a", "success", &ten)
	insertUsageAt(t, a, now.Add(-30*time.Minute), "model-a", "success", &twenty)
	insertUsageAt(t, a, now.Add(-10*time.Minute), "model-a", "error", nil)
	insertUsageAt(t, a, now.Add(-5*time.Minute), "model-b", "success", &twenty)

	params := url.Values{
		"page":      {"2"},
		"page_size": {"1"},
		"from":      {now.Add(-time.Hour).Format(time.RFC3339)},
		"model":     {"model-a"},
		"status":    {"success"},
	}
	recorder := httptest.NewRecorder()
	a.usageList(recorder, httptest.NewRequest(http.MethodGet, "/api/usage/requests?"+params.Encode(), nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("filtered usage page failed: %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Items      []usageRequest `json:"items"`
		Page       int            `json:"page"`
		PageSize   int            `json:"page_size"`
		Total      int            `json:"total"`
		TotalPages int            `json:"total_pages"`
		Models     []string       `json:"models"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Page != 2 || response.PageSize != 1 || response.Total != 2 || response.TotalPages != 2 || len(response.Items) != 1 {
		t.Fatalf("unexpected filtered page: %#v", response)
	}
	if !response.Items[0].CreatedAt.Equal(now.Add(-45*time.Minute)) || response.Items[0].Model != "model-a" {
		t.Fatalf("wrong filtered item: %#v", response.Items[0])
	}
	if len(response.Models) != 2 || response.Models[0] != "model-a" || response.Models[1] != "model-b" {
		t.Fatalf("historical model options were not returned: %#v", response.Models)
	}

	last := httptest.NewRecorder()
	a.usageList(last, httptest.NewRequest(http.MethodGet, "/api/usage/requests?page=99&page_size=2", nil))
	var lastPage struct {
		Page  int            `json:"page"`
		Items []usageRequest `json:"items"`
	}
	if err := json.NewDecoder(last.Body).Decode(&lastPage); err != nil {
		t.Fatal(err)
	}
	if lastPage.Page != 3 || len(lastPage.Items) != 1 {
		t.Fatalf("out-of-range usage page was not clamped: %#v", lastPage)
	}

	legacy := httptest.NewRecorder()
	a.usageList(legacy, httptest.NewRequest(http.MethodGet, "/api/usage/requests?limit=1", nil))
	var legacyRows []usageRequest
	if err := json.NewDecoder(legacy.Body).Decode(&legacyRows); err != nil {
		t.Fatalf("legacy usage response changed shape: %v; body=%s", err, legacy.Body.String())
	}
	if len(legacyRows) != 1 {
		t.Fatalf("legacy usage limit was not preserved: %#v", legacyRows)
	}
}

func TestUsageListRejectsInvalidPaginationAndFilters(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	cases := []string{
		"/api/usage/requests?page=0&page_size=25",
		"/api/usage/requests?page=1&page_size=201",
		"/api/usage/requests?page=1&status=unknown",
		"/api/usage/requests?page=1&from=not-a-time",
		"/api/usage/requests?page=1&from=" + url.QueryEscape(now.Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)),
	}
	for _, path := range cases {
		recorder := httptest.NewRecorder()
		a.usageList(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("expected invalid query to fail: path=%s status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestUsageRatesUseRollingSixtySecondWindow(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Truncate(time.Second)
	ten, fifty := int64(10), int64(50)
	insertUsageAt(t, a, now.Add(-10*time.Second), "model-a", "success", &ten)
	insertUsageAt(t, a, now.Add(-20*time.Second), "vision-model", "error", nil)
	insertUsageAt(t, a, now.Add(-2*time.Minute), "model-b", "success", &fifty)

	recorder := httptest.NewRecorder()
	a.usageRates(recorder, httptest.NewRequest(http.MethodGet, "/api/usage/rates", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("usage rates failed: %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		WindowSeconds int    `json:"window_seconds"`
		RPM           int64  `json:"rpm"`
		TPM           int64  `json:"tpm"`
		MeasuredAt    string `json:"measured_at"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.WindowSeconds != 60 || response.RPM != 2 || response.TPM != 10 || response.MeasuredAt == "" {
		t.Fatalf("unexpected rolling usage rates: %#v", response)
	}
}

func TestCustomUpstreamHeadersReachModelsAndChat(t *testing.T) {
	a := testApp(t)
	var modelHeaders, chatHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			modelHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"alpha:free"}]}`))
		case "/v1/chat/completions":
			chatHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	payload, _ := json.Marshal(map[string]any{
		"base_url":        upstream.URL,
		"api_key":         "up-secret",
		"vision_base_url": upstream.URL,
		"vision_api_key":  "vision-secret",
		"vision_model":    "vision-model",
		"custom_headers": map[string]string{
			"User-Agent":        "opencode/1.18.12 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13",
			"x-opencode-client": "cli",
			"X-Relay-Test":      "enabled",
		},
	})
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", bytes.NewReader(payload)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d body=%s", put.Code, put.Body.String())
	}

	refresh := httptest.NewRecorder()
	a.refreshModels(refresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if refresh.Code != http.StatusOK {
		t.Fatalf("model refresh failed: %d body=%s", refresh.Code, refresh.Body.String())
	}
	if modelHeaders.Get("User-Agent") != "opencode/1.18.12 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13" || modelHeaders.Get("X-Opencode-Client") != "cli" || modelHeaders.Get("X-Relay-Test") != "enabled" {
		t.Fatalf("custom model headers were not forwarded: %#v", modelHeaders)
	}
	if modelHeaders.Get("Authorization") != "Bearer up-secret" {
		t.Fatalf("upstream authorization was not injected: %#v", modelHeaders)
	}

	cfg, err := a.loadUpstream()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionBaseURL != upstream.URL || cfg.VisionAPIKey != "vision-secret" || cfg.VisionModel != "vision-model" {
		t.Fatalf("vision supplier configuration was not isolated: %#v", cfg)
	}
	resp, err := a.forward(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(`{"model":"alpha:free","messages":[]}`), cfg, ProxyRecord{URI: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if chatHeaders.Get("User-Agent") != "opencode/1.18.12 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.13" || chatHeaders.Get("X-Opencode-Client") != "cli" || chatHeaders.Get("X-Relay-Test") != "enabled" {
		t.Fatalf("custom chat headers were not forwarded: %#v", chatHeaders)
	}
	if chatHeaders.Get("Authorization") != "Bearer up-secret" || chatHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("gateway-managed headers were not injected: %#v", chatHeaders)
	}
}

func TestVisionUseProxySettingAndDirectTransport(t *testing.T) {
	a := testApp(t)

	cfg, err := a.loadUpstream()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VisionUseProxy {
		t.Fatal("vision proxy should be enabled when the setting is absent")
	}

	payload := []byte(`{"base_url":"https://upstream.example.test","vision_base_url":"https://vision.example.test","vision_model":"vision-model","vision_use_proxy":false}`)
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", bytes.NewReader(payload)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d body=%s", put.Code, put.Body.String())
	}
	cfg, err = a.loadUpstream()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VisionUseProxy {
		t.Fatal("explicitly disabled vision proxy was not persisted")
	}

	get := httptest.NewRecorder()
	a.getUpstream(get, httptest.NewRequest(http.MethodGet, "/api/settings/upstream", nil))
	var response struct {
		VisionUseProxy bool `json:"vision_use_proxy"`
	}
	if err := json.NewDecoder(get.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.VisionUseProxy {
		t.Fatal("upstream API returned an enabled vision proxy after it was disabled")
	}

	// A legacy client that omits the new field must preserve the saved value.
	legacyPut := httptest.NewRecorder()
	a.putUpstream(legacyPut, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", bytes.NewReader([]byte(`{"base_url":"https://upstream.example.test"}`))))
	if legacyPut.Code != http.StatusOK {
		t.Fatalf("legacy upstream update failed: %d body=%s", legacyPut.Code, legacyPut.Body.String())
	}
	cfg, err = a.loadUpstream()
	if err != nil || cfg.VisionUseProxy {
		t.Fatalf("legacy update changed vision proxy setting: %#v err=%v", cfg, err)
	}

	client, err := a.httpClient(ProxyRecord{})
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("empty proxy record did not configure a direct transport")
	}
}

func TestVisionDirectRequestDoesNotPenalizeChatProxy(t *testing.T) {
	a := testApp(t)
	visionHits := 0
	vision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		visionHits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"vision unavailable"}`))
	}))
	defer vision.Close()

	payload := []byte(`{"base_url":"http://gateway-upstream.example.test","vision_base_url":"` + vision.URL + `","vision_model":"vision-model","vision_use_proxy":false}`)
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", bytes.NewReader(payload)))
	if put.Code != http.StatusOK {
		t.Fatalf("upstream config failed: %d body=%s", put.Code, put.Body.String())
	}
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "text-model:free", "Text model", 1, "test", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	p, err := parseProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	proxyID, err := a.insertProxy(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('client_key',?)", hashToken("test-client")); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"model":"text-model:free","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"https://image.example/test.png"}}]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-client")
	response := httptest.NewRecorder()
	a.gatewayChat(response, req)
	if response.Code != http.StatusBadGateway || visionHits != 1 {
		t.Fatalf("expected one direct helper request and a gateway error, status=%d hits=%d body=%s", response.Code, visionHits, response.Body.String())
	}

	var failureCount int
	if err := a.db.QueryRow("SELECT failure_count FROM proxies WHERE id=?", proxyID).Scan(&failureCount); err != nil {
		t.Fatal(err)
	}
	if failureCount != 0 {
		t.Fatalf("direct vision failure penalized the chat proxy: %d", failureCount)
	}
	var helperProxyID sql.NullInt64
	var helperProxyURI, helperStatus string
	if err := a.db.QueryRow("SELECT proxy_id,proxy_uri,status FROM usage_requests WHERE request_kind='vision_helper' ORDER BY id DESC LIMIT 1").Scan(&helperProxyID, &helperProxyURI, &helperStatus); err != nil {
		t.Fatal(err)
	}
	if helperProxyID.Valid || helperProxyURI != "" || helperStatus != "error" {
		t.Fatalf("direct helper usage was not recorded as direct: id=%#v uri=%q status=%q", helperProxyID, helperProxyURI, helperStatus)
	}
}

func TestCustomHeaderValidation(t *testing.T) {
	if _, err := validateCustomHeaders(map[string]string{"Authorization": "Bearer override"}); err == nil {
		t.Fatal("authorization override was accepted")
	}
	if _, err := validateCustomHeaders(map[string]string{"X-Test\r\nInjected": "value"}); err == nil {
		t.Fatal("invalid header name was accepted")
	}
	if _, err := validateCustomHeaders(map[string]string{"X-Test": "line1\r\nline2"}); err == nil {
		t.Fatal("invalid header value was accepted")
	}
}

func TestProxyExpiryAndBulkDelete(t *testing.T) {
	a := testApp(t)
	p1, err := parseProxy("http://127.0.0.1:3128")
	if err != nil {
		t.Fatal(err)
	}
	p1.ExpiresAt = func() *time.Time { t := time.Now().UTC().Add(24 * time.Hour); return &t }()
	id1, err := a.insertProxy(p1)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := parseProxy("http://127.0.0.2:3128")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := a.insertProxy(p2)
	if err != nil {
		t.Fatal(err)
	}
	expired, err := parseProxy("http://127.0.0.3:3128")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Hour)
	expired.ExpiresAt = &past
	if _, err = a.insertProxy(expired); err != nil {
		t.Fatal(err)
	}

	list := httptest.NewRecorder()
	a.listProxies(list, httptest.NewRequest(http.MethodGet, "/api/proxies", nil))
	var proxies []ProxyRecord
	if err := json.NewDecoder(list.Body).Decode(&proxies); err != nil {
		t.Fatal(err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expired proxy was not removed: %#v", proxies)
	}
	var foundExpiry bool
	for _, proxy := range proxies {
		if proxy.ID == id1 {
			foundExpiry = proxy.ExpiresAt != nil
		}
	}
	if !foundExpiry {
		t.Fatal("proxy expiry was not returned")
	}

	bulk := httptest.NewRecorder()
	a.bulkDeleteProxies(bulk, httptest.NewRequest(http.MethodPost, "/api/proxies/bulk-delete", strings.NewReader(`{"ids":[`+strconv.FormatInt(id1, 10)+`,`+strconv.FormatInt(id2, 10)+`]}`)))
	if bulk.Code != http.StatusOK || !strings.Contains(bulk.Body.String(), `"deleted":2`) {
		t.Fatalf("bulk delete failed: status=%d body=%s", bulk.Code, bulk.Body.String())
	}
}

func TestListProxiesPagination(t *testing.T) {
	a := testApp(t)
	for i := 1; i <= 5; i++ {
		p, err := parseProxy("http://127.0.1." + strconv.Itoa(i) + ":3128")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.insertProxy(p); err != nil {
			t.Fatal(err)
		}
	}

	page := httptest.NewRecorder()
	a.listProxies(page, httptest.NewRequest(http.MethodGet, "/api/proxies?page=2&page_size=2", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("expected paginated list to succeed, got %d: %s", page.Code, page.Body.String())
	}
	var response struct {
		Items      []ProxyRecord `json:"items"`
		Page       int           `json:"page"`
		PageSize   int           `json:"page_size"`
		Total      int           `json:"total"`
		TotalPages int           `json:"total_pages"`
	}
	if err := json.NewDecoder(page.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Page != 2 || response.PageSize != 2 || response.Total != 5 || response.TotalPages != 3 || len(response.Items) != 2 {
		t.Fatalf("unexpected page response: %#v", response)
	}

	last := httptest.NewRecorder()
	a.listProxies(last, httptest.NewRequest(http.MethodGet, "/api/proxies?page=99&page_size=2", nil))
	var lastPage struct {
		Page  int           `json:"page"`
		Items []ProxyRecord `json:"items"`
	}
	if err := json.NewDecoder(last.Body).Decode(&lastPage); err != nil {
		t.Fatal(err)
	}
	if lastPage.Page != 3 || len(lastPage.Items) != 1 {
		t.Fatalf("expected out-of-range page to clamp to last page, got %#v", lastPage)
	}

	invalid := httptest.NewRecorder()
	a.listProxies(invalid, httptest.NewRequest(http.MethodGet, "/api/proxies?page=0&page_size=2", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid page to fail, got %d: %s", invalid.Code, invalid.Body.String())
	}
}

func TestAdminImportRefreshAndGatewayModels(t *testing.T) {
	a := testApp(t)
	mux := http.NewServeMux()
	a.routes(mux)
	server := httptest.NewServer(a.withMiddleware(mux))
	defer server.Close()

	client := &http.Client{}
	loginBody := `{"password":"test-password"}`
	loginReq, _ := http.NewRequest(http.MethodPost, server.URL+"/api/auth/login", strings.NewReader(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp, err := client.Do(loginReq)
	if err != nil || loginResp.StatusCode != 200 {
		t.Fatalf("login failed: %v status=%d", err, loginResp.StatusCode)
	}
	cookie := loginResp.Cookies()[0]
	request := func(method, path, body string) *http.Response {
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		resp, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return resp
	}
	imp := request(http.MethodPost, "/api/proxies/import", `{"text":"http://127.0.0.1:3128\nsocks5://u:p@127.0.0.2:1080\ninvalid"}`)
	if imp.StatusCode != 200 {
		t.Fatalf("import failed: %d", imp.StatusCode)
	}
	var imported struct {
		Results []struct {
			Status string `json:"status"`
		} `json:"results"`
	}
	_ = json.NewDecoder(imp.Body).Decode(&imported)
	if len(imported.Results) != 3 {
		t.Fatalf("unexpected import results: %#v", imported)
	}
	proxyList := request(http.MethodGet, "/api/proxies", "")
	var listed []ProxyRecord
	_ = json.NewDecoder(proxyList.Body).Decode(&listed)
	if len(listed) != 2 {
		t.Fatalf("proxy list did not return imported rows: status=%d rows=%#v", proxyList.StatusCode, listed)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"alpha:free","pricing":{"prompt":"0","completion":"0"}},{"id":"paid","pricing":{"prompt":"0.01","completion":"0.01"}}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer upstream.Close()
	put := request(http.MethodPut, "/api/settings/upstream", `{"base_url":"`+upstream.URL+`","api_key":"up-secret"}`)
	if put.StatusCode != 200 {
		t.Fatalf("upstream config failed: %d", put.StatusCode)
	}
	refresh := request(http.MethodPost, "/api/settings/models/refresh", "{}")
	if refresh.StatusCode != 200 {
		t.Fatalf("refresh failed: %d", refresh.StatusCode)
	}
	free := request(http.MethodGet, "/api/models/free", "")
	var freeModels []ModelRecord
	_ = json.NewDecoder(free.Body).Decode(&freeModels)
	if len(freeModels) != 1 || freeModels[0].ModelID != "alpha:free" {
		t.Fatalf("free filter failed: %#v", freeModels)
	}
	rotate := request(http.MethodPost, "/api/settings/client-key/rotate", "{}")
	var key struct {
		ClientKey string `json:"client_key"`
	}
	_ = json.NewDecoder(rotate.Body).Decode(&key)
	if !strings.HasPrefix(key.ClientKey, "ocp-") || strings.Contains(key.ClientKey, "_") {
		t.Fatalf("invalid client key: %q", key.ClientKey)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key.ClientKey)
	gw, err := client.Do(req)
	if err != nil || gw.StatusCode != 200 {
		t.Fatalf("gateway models failed: %v status=%d", err, gw.StatusCode)
	}
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(gw.Body).Decode(&payload)
	if len(payload.Data) != 1 {
		t.Fatalf("gateway returned non-free models: %#v", payload)
	}
	for _, headerName := range []string{"X-API-Key", "API-Key"} {
		compatReq, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
		compatReq.Header.Set(headerName, key.ClientKey)
		compatResp, compatErr := client.Do(compatReq)
		if compatErr != nil || compatResp.StatusCode != http.StatusOK {
			t.Fatalf("gateway %s authentication failed: %v status=%d", headerName, compatErr, compatResp.StatusCode)
		}
		_ = compatResp.Body.Close()
	}
}

func TestGatewayRoutesBypassSPAFallback(t *testing.T) {
	a := testApp(t)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer proxyServer.Close()

	upstream := httptest.NewRecorder()
	a.putUpstream(upstream, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{"base_url":"http://upstream.example.test"}`)))
	if upstream.Code != http.StatusOK {
		t.Fatalf("configure upstream failed: %d: %s", upstream.Code, upstream.Body.String())
	}
	if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", "alpha:free", "Alpha", 1, "test", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	proxy, err := parseProxy(proxyServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.insertProxy(proxy); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE settings SET value=? WHERE key='client_key'", hashToken("route-test-client")); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateLegacyClientKey(); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	a.routes(mux)
	server := httptest.NewServer(a.withMiddleware(mux))
	defer server.Close()
	client := server.Client()

	modelsReq, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
	modelsReq.Header.Set("Authorization", "Bearer route-test-client")
	modelsResp, err := client.Do(modelsReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = modelsResp.Body.Close()
	if modelsResp.StatusCode != http.StatusOK {
		t.Fatalf("models route was intercepted by SPA fallback: %d", modelsResp.StatusCode)
	}

	chatReq, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"alpha:free","messages":[{"role":"user","content":"hello"}]}`))
	chatReq.Header.Set("Authorization", "Bearer route-test-client")
	chatReq.Header.Set("Content-Type", "application/json")
	chatResp, err := client.Do(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	defer chatResp.Body.Close()
	if chatResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(chatResp.Body)
		t.Fatalf("chat route was intercepted by SPA fallback: %d: %s", chatResp.StatusCode, body)
	}
}

func TestServeSPARejectsUnknownAPIRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>relay desk</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := serveSPA(dir)

	for _, path := range []string{"/v1", "/v1/", "/v1/unknown", "/api", "/api/unknown"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("expected API path to return 404, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
				t.Fatalf("expected JSON response, got %q", contentType)
			}
			if strings.Contains(recorder.Body.String(), "relay desk") {
				t.Fatal("API path returned the SPA index")
			}
		})
	}

	for _, path := range []string{"/", "/settings", "/v10"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "relay desk") {
				t.Fatalf("expected SPA route to return the index, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestNormalizeAPIKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" sk-test ", "sk-test"},
		{"Bearer sk-test", "sk-test"},
		{"bearer   sk-test", "sk-test"},
		{"'sk-test'", "sk-test"},
	} {
		if got := normalizeAPIKey(tc.in); got != tc.want {
			t.Fatalf("normalizeAPIKey(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUpstreamErrorSummary(t *testing.T) {
	got := upstreamErrorSummary([]byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`))
	if got != "Invalid API key." {
		t.Fatalf("unexpected summary: %q", got)
	}
}

func TestSessionProxyAffinityAndQuotaAdvance(t *testing.T) {
	a := testApp(t)
	if _, err := a.db.Exec("INSERT INTO settings(key,value) VALUES('session_proxy_request_limit','2')"); err != nil {
		t.Fatal(err)
	}
	proxies := []ProxyRecord{{ID: 101, URI: "http://127.0.0.1:3128"}, {ID: 202, URI: "http://127.0.0.2:3128"}}
	session := "session-hash"

	first, ok, err := a.pickSessionProxy(session, proxies, nil)
	if err != nil || !ok {
		t.Fatal("expected a proxy")
	}
	same, ok, err := a.pickSessionProxy(session, proxies, nil)
	if err != nil || !ok || same.ID != first.ID {
		t.Fatalf("session did not stay on its proxy: first=%d got=%d", first.ID, same.ID)
	}
	next, ok, err := a.pickSessionProxy(session, proxies, nil)
	if err != nil || !ok || next.ID == first.ID {
		t.Fatalf("quota did not advance the session: first=%d got=%d", first.ID, next.ID)
	}

	a.clearSessionProxy(session, next.ID)
	again, ok, err := a.pickSessionProxy(session, proxies, map[int64]struct{}{first.ID: {}})
	if err != nil || !ok || again.ID != next.ID {
		t.Fatalf("cleared binding did not select the next eligible proxy: want=%d got=%d", next.ID, again.ID)
	}
}

func TestSessionKeyUsesHeaderThenUser(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer client-one")
	r.Header.Set("X-Relay-Session-ID", "conversation-one")
	fromHeader := sessionKey(r, json.RawMessage(`"ignored-user"`))
	if fromHeader != sessionKey(r, json.RawMessage(`"different-user"`)) {
		t.Fatal("session header should take precedence")
	}
	r.Header.Del("X-Relay-Session-ID")
	fromUser := sessionKey(r, json.RawMessage(`"conversation-one"`))
	if fromHeader != fromUser {
		t.Fatal("equivalent header and user session identifiers should match")
	}
	r.Header.Set("Authorization", "Bearer client-two")
	if fromUser == sessionKey(r, json.RawMessage(`"conversation-one"`)) {
		t.Fatal("sessions from different client keys must not share a route")
	}
}

func TestPasswordChangeRejectsBcryptOverflowAndRevokesSessions(t *testing.T) {
	a := testApp(t)
	token := "ocp-test-admin-session"
	if _, err := a.db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", a.adminSessionKey(token), time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}

	overlong := httptest.NewRecorder()
	a.changePassword(overlong, httptest.NewRequest(http.MethodPost, "/api/auth/password", strings.NewReader(`{"current":"test-password","new":"`+strings.Repeat("x", 73)+`"}`)))
	if overlong.Code != http.StatusBadRequest {
		t.Fatalf("expected overlong password rejection, got %d: %s", overlong.Code, overlong.Body.String())
	}
	var hash string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("test-password")); err != nil {
		t.Fatal("overlong password corrupted the existing hash")
	}

	changed := httptest.NewRecorder()
	a.changePassword(changed, httptest.NewRequest(http.MethodPost, "/api/auth/password", strings.NewReader(`{"current":"test-password","new":"replacement-password"}`)))
	if changed.Code != http.StatusOK {
		t.Fatalf("password change failed: %d %s", changed.Code, changed.Body.String())
	}
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key=?", a.adminSessionKey(token)).Scan(&hash); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("existing session was not revoked: %v", err)
	}
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key='admin_hash'").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("replacement-password")); err != nil {
		t.Fatal("new password was not stored")
	}
}

func TestLogoutRevokesServerSession(t *testing.T) {
	a := testApp(t)
	token := "ocp-test-logout-session"
	key := a.adminSessionKey(token)
	if _, err := a.db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", key, time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: "ocp_session", Value: token})
	recorder := httptest.NewRecorder()
	a.logout(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var value string
	if err := a.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&value); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("server session still exists: %v", err)
	}
}

func TestCopyResponseStreamsFullBodyAndFiltersHeaders(t *testing.T) {
	a := testApp(t)
	payload := bytes.Repeat([]byte("a"), 3<<20)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":   []string{"application/octet-stream"},
			"Content-Length": []string{strconv.Itoa(len(payload))},
			"Set-Cookie":     []string{"ocp_session=attacker"},
			"Connection":     []string{"X-Hop"},
			"X-Hop":          []string{"remove-me"},
			"X-Upstream":     []string{"keep-me"},
		},
		Body: io.NopCloser(bytes.NewReader(payload)),
	}
	recorder := httptest.NewRecorder()
	_, _, firstToken, copyErr := a.copyResponse(recorder, resp, time.Now(), false)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if firstToken == nil {
		t.Fatal("non-streaming response did not record its first body byte")
	}
	if recorder.Body.Len() != len(payload) {
		t.Fatalf("response was truncated: got=%d want=%d", recorder.Body.Len(), len(payload))
	}
	if recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("Content-Length") != "" || recorder.Header().Get("X-Hop") != "" {
		t.Fatalf("unsafe upstream headers were forwarded: %#v", recorder.Header())
	}
	if recorder.Header().Get("X-Upstream") != "keep-me" {
		t.Fatal("safe upstream header was removed")
	}
}

func TestCopyResponseTracksStreamingContentAndIgnoresErrorBodies(t *testing.T) {
	a := testApp(t)
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":""}}]}`,
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		`data: {"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"choices":[]}`,
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	recorder := httptest.NewRecorder()
	usage, _, firstToken, copyErr := a.copyResponse(recorder, resp, time.Now().Add(-50*time.Millisecond), false)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if firstToken == nil || *firstToken < 40*time.Millisecond {
		t.Fatalf("streaming first content latency was not captured: %v", firstToken)
	}
	if usage == nil || usage.Prompt == nil || *usage.Prompt != 2 || usage.Completion == nil || *usage.Completion != 1 {
		t.Fatalf("streaming usage was not preserved: %#v", usage)
	}

	errorResp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"upstream failed"}}`)),
	}
	_, _, errorFirstToken, copyErr := a.copyResponse(httptest.NewRecorder(), errorResp, time.Now().Add(-time.Second), false)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if errorFirstToken != nil {
		t.Fatalf("error body recorded a first token latency: %v", *errorFirstToken)
	}
}

func TestCopyResponseStripsReasoningContentFromJSON(t *testing.T) {
	a := testApp(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"visible answer","reasoning_content":"private chain","reasoning_details":[{"text":"private detail"}]}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)),
	}
	recorder := httptest.NewRecorder()
	usage, _, _, copyErr := a.copyResponse(recorder, resp, time.Now(), true)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if strings.Contains(recorder.Body.String(), "private") {
		t.Fatalf("reasoning trace leaked to the client: %s", recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	choice := payload["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "visible answer" {
		t.Fatalf("final answer was changed: %#v", message)
	}
	if _, found := message["reasoning_content"]; found {
		t.Fatalf("reasoning_content was not removed: %#v", message)
	}
	if usage == nil || usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("usage was not retained: %#v", usage)
	}
}

func TestCopyResponseStripsReasoningContentFromSSE(t *testing.T) {
	a := testApp(t)
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","reasoning_content":"private chain"}}]}`,
		`data: {"choices":[{"delta":{"content":"visible answer","reasoning_content":"private continuation"}}]}`,
		`data: {"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3},"choices":[]}`,
		"data: [DONE]",
		"",
	}, "\n")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}
	recorder := httptest.NewRecorder()
	usage, _, _, copyErr := a.copyResponse(recorder, resp, time.Now(), true)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	response := recorder.Body.String()
	if strings.Contains(response, "private") || strings.Contains(response, "reasoning_content") {
		t.Fatalf("reasoning trace leaked to the client: %s", response)
	}
	if !strings.Contains(response, `"content":"visible answer"`) || !strings.Contains(response, "data: [DONE]") {
		t.Fatalf("stream content was not retained: %s", response)
	}
	if usage == nil || usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("usage was not retained: %#v", usage)
	}
}

func TestCopyResponseKeepsReasoningContentWhenThinkingWasNotDisabled(t *testing.T) {
	a := testApp(t)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"answer","reasoning_content":"requested trace"}}]}`)),
	}
	recorder := httptest.NewRecorder()
	_, _, _, copyErr := a.copyResponse(recorder, resp, time.Now(), false)
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if !strings.Contains(recorder.Body.String(), "requested trace") {
		t.Fatalf("reasoning trace was removed without an opt-out request: %s", recorder.Body.String())
	}
}

func TestRequestDisablesThinking(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{name: "DeepSeek disabled", payload: map[string]any{"thinking": map[string]any{"type": "disabled"}}, want: true},
		{name: "DeepSeek case insensitive", payload: map[string]any{"thinking": map[string]any{"type": " DISABLED "}}, want: true},
		{name: "Responses none", payload: map[string]any{"reasoning": map[string]any{"effort": "none"}}, want: true},
		{name: "thinking enabled", payload: map[string]any{"thinking": map[string]any{"type": "enabled"}}, want: false},
		{name: "low reasoning", payload: map[string]any{"reasoning": map[string]any{"effort": "low"}}, want: false},
		{name: "no setting", payload: map[string]any{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestDisablesThinking(tt.payload); got != tt.want {
				t.Fatalf("requestDisablesThinking() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestProxyImportRedactsPassword(t *testing.T) {
	a := testApp(t)
	recorder := httptest.NewRecorder()
	a.importProxies(recorder, httptest.NewRequest(http.MethodPost, "/api/proxies/import", strings.NewReader(`{"text":"http://alice:secret@127.0.0.1:3128"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("import failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("proxy password leaked in import response: %s", recorder.Body.String())
	}
}

func TestUsageKeepsProxySnapshotAfterDeletion(t *testing.T) {
	a := testApp(t)
	p, err := parseProxy("http://127.0.0.9:3128")
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.insertProxy(p)
	if err != nil {
		t.Fatal(err)
	}
	a.recordUsage("model:free", &id, p.URI, "success", 200, time.Millisecond, nil, 0, nil, nil)
	if _, err := a.db.Exec("DELETE FROM proxies WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	a.usageList(recorder, httptest.NewRequest(http.MethodGet, "/api/usage/requests", nil))
	var rows []usageRequest
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ProxyURI != p.URI {
		t.Fatalf("proxy snapshot was lost: %#v", rows)
	}
}

func TestPatchMissingProxyReturnsNotFound(t *testing.T) {
	a := testApp(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/proxies/999", strings.NewReader(`{"enabled":false}`))
	req.SetPathValue("id", "999")
	recorder := httptest.NewRecorder()
	a.patchProxy(recorder, req)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestRequestsWithoutSessionIDRoundRobin(t *testing.T) {
	a := testApp(t)
	proxies := []ProxyRecord{{ID: 1}, {ID: 2}}
	first, ok, err := a.pickSessionProxy("", proxies, nil)
	if err != nil || !ok {
		t.Fatalf("first selection failed: %v", err)
	}
	second, ok, err := a.pickSessionProxy("", proxies, nil)
	if err != nil || !ok {
		t.Fatalf("second selection failed: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("requests without a session ID shared one route: %d", first.ID)
	}
}

func TestEncryptionKeyRotationIsRepeatable(t *testing.T) {
	a := testApp(t)
	previousKey := append([]byte(nil), a.key...)
	p, err := parseProxy("http://alice:secret@127.0.0.10:3128")
	if err != nil {
		t.Fatal(err)
	}
	id, err := a.insertProxy(p)
	if err != nil {
		t.Fatal(err)
	}
	apiKey, err := a.encrypt("upstream-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('upstream_api_key',?)", apiKey); err != nil {
		t.Fatal(err)
	}
	newHash := sha256.Sum256([]byte("replacement-encryption-key-that-is-long-enough"))
	a.key = newHash[:]
	migrated, err := a.rotateEncryptionKey(previousKey)
	if err != nil {
		t.Fatal(err)
	}
	if migrated != 2 {
		t.Fatalf("expected two encrypted values to migrate, got %d", migrated)
	}
	var encryptedPassword string
	if err = a.db.QueryRow("SELECT encrypted_password FROM proxies WHERE id=?", id).Scan(&encryptedPassword); err != nil {
		t.Fatal(err)
	}
	if password, err := a.decrypt(encryptedPassword); err != nil || password != "secret" {
		t.Fatalf("rotated proxy password is invalid: password=%q err=%v", password, err)
	}
	var encryptedAPIKey string
	if err = a.db.QueryRow("SELECT value FROM settings WHERE key='upstream_api_key'").Scan(&encryptedAPIKey); err != nil {
		t.Fatal(err)
	}
	if value, err := a.decrypt(encryptedAPIKey); err != nil || value != "upstream-secret" {
		t.Fatalf("rotated API key is invalid: value=%q err=%v", value, err)
	}
	migrated, err = a.rotateEncryptionKey(previousKey)
	if err != nil || migrated != 0 {
		t.Fatalf("repeat rotation should be a no-op: migrated=%d err=%v", migrated, err)
	}
}

func insertTokenUsageAt(t *testing.T, a *App, createdAt time.Time, status string, prompt, completion, total int64) {
	t.Helper()
	statusCode := http.StatusOK
	if status == "error" {
		statusCode = http.StatusBadGateway
	}
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,retry_count,prompt_tokens,completion_tokens,total_tokens) VALUES(?,?,?,?,?,?,?,?,?,?)", createdAt.UTC().Format(time.RFC3339), "chat", "model:free", status, statusCode, 10, 0, prompt, completion, total); err != nil {
		t.Fatal(err)
	}
}

func TestStatsSummaryUsesChinaDayForTokens(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC()
	insertTokenUsageAt(t, a, now, "success", 11, 7, 18)
	insertTokenUsageAt(t, a, chinaDayStart(now).Add(-time.Second), "error", 101, 99, 200)

	recorder := httptest.NewRecorder()
	a.statsSummary(recorder, httptest.NewRequest(http.MethodGet, "/api/stats/summary", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("summary failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var got struct {
		Requests         int64   `json:"requests"`
		Success          int64   `json:"success"`
		SuccessRate      float64 `json:"success_rate"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		TotalTokens      int64   `json:"total_tokens"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Requests != 2 || got.Success != 1 || got.SuccessRate != 0.5 {
		t.Fatalf("historical request counters changed: %#v", got)
	}
	if got.PromptTokens != 11 || got.CompletionTokens != 7 || got.TotalTokens != 18 {
		t.Fatalf("today token summary included the prior China day: %#v", got)
	}
}

func TestStatsTimeseriesGroupsByChinaDate(t *testing.T) {
	a := testApp(t)
	insertTokenUsageAt(t, a, time.Date(2026, time.January, 1, 16, 30, 0, 0, time.UTC), "success", 2, 3, 5)

	recorder := httptest.NewRecorder()
	a.statsTimeseries(recorder, httptest.NewRequest(http.MethodGet, "/api/stats/timeseries", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("timeseries failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var rows []struct {
		Day      string `json:"day"`
		Requests int64  `json:"requests"`
		Tokens   int64  `json:"tokens"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Day != "2026-01-02" || rows[0].Requests != 1 || rows[0].Tokens != 5 {
		t.Fatalf("usage was not grouped by China date: %#v", rows)
	}
}

func TestProxyStateFiltersAndUsageLabels(t *testing.T) {
	a := testApp(t)
	add := func(raw string) int64 {
		p, err := parseProxy(raw)
		if err != nil {
			t.Fatal(err)
		}
		id, err := a.insertProxy(p)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	unusedID := add("http://127.0.10.1:3128")
	inUseID := add("http://127.0.10.2:3128")
	cooldownID := add("http://127.0.10.3:3128")
	now := time.Now().UTC()
	if _, err := a.db.Exec("INSERT INTO session_proxy_routes(session_key,proxy_id,request_count,created_at,updated_at) VALUES(?,?,?,?,?)", "active-route", inUseID, 1, now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("INSERT INTO session_proxy_routes(session_key,proxy_id,request_count,created_at,updated_at) VALUES(?,?,?,?,?)", "cooldown-route", cooldownID, 1, now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec("UPDATE proxies SET cooldown_until=? WHERE id=?", now.Add(time.Hour).Format(time.RFC3339), cooldownID); err != nil {
		t.Fatal(err)
	}

	list := func(state string) []ProxyRecord {
		recorder := httptest.NewRecorder()
		a.listProxies(recorder, httptest.NewRequest(http.MethodGet, "/api/proxies?page=1&page_size=50&state="+state, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("state %s failed: %d %s", state, recorder.Code, recorder.Body.String())
		}
		var response struct {
			Items []ProxyRecord `json:"items"`
		}
		if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response.Items
	}
	if rows := list("unused"); len(rows) != 1 || rows[0].ID != unusedID || rows[0].UsageState != "unused" {
		t.Fatalf("unexpected unused proxies: %#v", rows)
	}
	if rows := list("in_use"); len(rows) != 1 || rows[0].ID != inUseID || rows[0].UsageState != "in_use" {
		t.Fatalf("unexpected in-use proxies: %#v", rows)
	}
	if rows := list("cooldown"); len(rows) != 1 || rows[0].ID != cooldownID || rows[0].UsageState != "cooldown" {
		t.Fatalf("cooldown did not take precedence: %#v", rows)
	}

	invalid := httptest.NewRecorder()
	a.listProxies(invalid, httptest.NewRequest(http.MethodGet, "/api/proxies?page=1&state=wrong", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid proxy state succeeded: %d %s", invalid.Code, invalid.Body.String())
	}
}

func TestUsageRetentionSettingsDeleteExpiredRecords(t *testing.T) {
	a := testApp(t)
	if got := a.usageRetentionDays(); got != defaultUsageRetentionDays {
		t.Fatalf("unexpected default retention: %d", got)
	}
	oldTokens := int64(10)
	freshTokens := int64(20)
	insertUsageAt(t, a, time.Now().AddDate(0, 0, -31), "old", "success", &oldTokens)
	insertUsageAt(t, a, time.Now().AddDate(0, 0, -29), "fresh", "success", &freshTokens)

	recorder := httptest.NewRecorder()
	a.putUsageRetention(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/usage-retention", strings.NewReader(`{"usage_retention_days":30}`)))
	if recorder.Code != http.StatusOK || a.usageRetentionDays() != 30 {
		t.Fatalf("retention update failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var count int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM usage_requests").Scan(&count); err != nil || count != 1 {
		t.Fatalf("expired records were not removed: count=%d err=%v", count, err)
	}

	invalid := httptest.NewRecorder()
	a.putUsageRetention(invalid, httptest.NewRequest(http.MethodPut, "/api/settings/usage-retention", strings.NewReader(`{"usage_retention_days":14}`)))
	if invalid.Code != http.StatusBadRequest || a.usageRetentionDays() != 30 {
		t.Fatalf("invalid retention was accepted: %d %s", invalid.Code, invalid.Body.String())
	}

	get := httptest.NewRecorder()
	a.getUsageRetention(get, httptest.NewRequest(http.MethodGet, "/api/settings/usage-retention", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"usage_retention_days":30`) {
		t.Fatalf("retention read failed: %d %s", get.Code, get.Body.String())
	}
}

func TestResinProxyEngineStoresTokenAndDerivesSessionAccounts(t *testing.T) {
	a := testApp(t)
	payload := `{"engine":"resin","resin_gateway_url":"http://resin.example.test:2260","resin_platform":"Default","resin_proxy_token":"proxy-secret"}`
	recorder := httptest.NewRecorder()
	a.putProxyEngine(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(payload)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save Resin engine failed: %d %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "proxy-secret") {
		t.Fatalf("Resin proxy token leaked in API response: %s", recorder.Body.String())
	}
	cfg, err := a.loadProxyEngine()
	if err != nil || cfg.Engine != proxyEngineResin || cfg.ResinProxyToken != "proxy-secret" {
		t.Fatalf("stored Resin configuration is invalid: %#v err=%v", cfg, err)
	}
	first, err := a.resinProxyForSession("session-one")
	if err != nil {
		t.Fatal(err)
	}
	same, err := a.resinProxyForSession("session-one")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.resinProxyForSession("session-two")
	if err != nil {
		t.Fatal(err)
	}
	if first.Username != same.Username || first.Username == other.Username || !strings.HasPrefix(first.Username, "Default.") {
		t.Fatalf("Resin accounts are not session-stable and isolated: first=%q same=%q other=%q", first.Username, same.Username, other.Username)
	}
	if err := a.advanceResinAccount("session-one"); err != nil {
		t.Fatal(err)
	}
	rotated, err := a.resinProxyForSession("session-one")
	if err != nil || rotated.Username == first.Username {
		t.Fatalf("Resin account did not rotate: before=%q after=%q err=%v", first.Username, rotated.Username, err)
	}
}

func TestEphemeralResinRequestRouteRotatesWithoutPersistingSession(t *testing.T) {
	a := testApp(t)
	cfg := proxyEngineConfig{
		Engine:          proxyEngineResin,
		ResinGatewayURL: "http://resin.example.test:2260",
		ResinPlatform:   "Default",
		ResinProxyToken: "proxy-secret",
	}
	route, err := newResinRequestRoute("")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]struct{}, resinMaxAttempts)
	for attempt := 0; attempt < resinMaxAttempts; attempt++ {
		proxy, err := route.proxy(a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(proxy.Username, "Default.") {
			t.Fatalf("ephemeral Resin account is malformed: %q", proxy.Username)
		}
		if _, exists := seen[proxy.Username]; exists {
			t.Fatalf("ephemeral Resin account was reused on attempt %d: %q", attempt, proxy.Username)
		}
		seen[proxy.Username] = struct{}{}
		if attempt+1 < resinMaxAttempts {
			if err := route.advance(a); err != nil {
				t.Fatal(err)
			}
		}
	}
	var persisted int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM resin_session_routes").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("ephemeral Resin routing persisted %d session rows", persisted)
	}
}

func TestStickyResinRequestRouteStillPersistsAndRotates(t *testing.T) {
	a := testApp(t)
	cfg := proxyEngineConfig{
		Engine:          proxyEngineResin,
		ResinGatewayURL: "http://resin.example.test:2260",
		ResinPlatform:   "Default",
		ResinProxyToken: "proxy-secret",
	}
	route, err := newResinRequestRoute("sticky-session")
	if err != nil {
		t.Fatal(err)
	}
	first, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := route.advance(a); err != nil {
		t.Fatal(err)
	}
	second, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Username == second.Username {
		t.Fatalf("sticky Resin route did not rotate after 429: %q", first.Username)
	}
	var persisted, generation int
	if err := a.db.QueryRow("SELECT COUNT(*),MAX(generation) FROM resin_session_routes").Scan(&persisted, &generation); err != nil {
		t.Fatal(err)
	}
	if persisted != 1 || generation != 1 {
		t.Fatalf("sticky Resin route persistence changed: rows=%d generation=%d", persisted, generation)
	}
}

func TestResinStaysActiveAfterGatewayFailures(t *testing.T) {
	a := testApp(t)
	recorder := httptest.NewRecorder()
	a.putProxyEngine(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(`{"engine":"resin","resin_gateway_url":"http://resin.example.test:2260","resin_platform":"Default","resin_proxy_token":"proxy-secret"}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save Resin engine failed: %d %s", recorder.Code, recorder.Body.String())
	}
	for range resinFailureThreshold {
		a.resinFailure(errors.New("proxyconnect tcp: Resin unavailable"))
	}
	status, err := a.proxyEngineStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ResinFallbackActive || status.EffectiveEngine != proxyEngineResin {
		t.Fatalf("Resin must remain active after gateway failures: %#v", status)
	}
}

func TestUsageRecordsRouteEngine(t *testing.T) {
	a := testApp(t)
	a.recordUsageWithEngine("model:free", nil, "http://resin.example.test:2260", proxyEngineResin, "success", http.StatusOK, time.Millisecond, nil, 0, nil, nil)
	recorder := httptest.NewRecorder()
	a.usageList(recorder, httptest.NewRequest(http.MethodGet, "/api/usage/requests", nil))
	var rows []usageRequest
	if err := json.NewDecoder(recorder.Body).Decode(&rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].RouteEngine != proxyEngineResin || rows[0].ProxyURI != "http://resin.example.test:2260" {
		t.Fatalf("Resin route usage was not retained: %#v", rows)
	}
}

func TestResinHTTPProxyUsesDynamicBasicCredentials(t *testing.T) {
	a := testApp(t)
	var authorization string
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Proxy-Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxyServer.Close()
	p, err := a.resinProxy(proxyEngineConfig{ResinGatewayURL: proxyServer.URL, ResinPlatform: "Default", ResinProxyToken: "proxy-token"}, "account-hash")
	if err != nil {
		t.Fatal(err)
	}
	client, err := a.httpClient(p)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get("http://target.example.test/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("Default.account-hash:proxy-token"))
	if authorization != want {
		t.Fatalf("unexpected Resin proxy authorization: got=%q want=%q", authorization, want)
	}
}

func TestRecoverResinClearsPersistedFallback(t *testing.T) {
	a := testApp(t)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if r.URL.Path == "/v1/models" {
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{}})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}))
	defer proxyServer.Close()
	upstreamPayload := `{"base_url":"http://upstream.example.test","api_key":"upstream-token"}`
	upstreamRecorder := httptest.NewRecorder()
	a.putUpstream(upstreamRecorder, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(upstreamPayload)))
	if upstreamRecorder.Code != http.StatusOK {
		t.Fatalf("save upstream failed: %d %s", upstreamRecorder.Code, upstreamRecorder.Body.String())
	}
	enginePayload := `{"engine":"resin","resin_gateway_url":"` + proxyServer.URL + `","resin_platform":"Default","resin_proxy_token":"proxy-token"}`
	engineRecorder := httptest.NewRecorder()
	a.putProxyEngine(engineRecorder, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(enginePayload)))
	if engineRecorder.Code != http.StatusOK {
		t.Fatalf("save Resin engine failed: %d %s", engineRecorder.Code, engineRecorder.Body.String())
	}
	for range resinFailureThreshold {
		a.resinFailure(errors.New("proxyconnect tcp: unavailable"))
	}
	recoverRecorder := httptest.NewRecorder()
	a.recoverResin(recoverRecorder, httptest.NewRequest(http.MethodPost, "/api/settings/proxy-engine/resin/recover", nil))
	if recoverRecorder.Code != http.StatusOK {
		t.Fatalf("recover Resin failed: %d %s", recoverRecorder.Code, recoverRecorder.Body.String())
	}
	status, err := a.proxyEngineStatus()
	if err != nil || status.ResinFallbackActive || status.EffectiveEngine != proxyEngineResin {
		t.Fatalf("Resin did not recover: %#v err=%v", status, err)
	}
}
