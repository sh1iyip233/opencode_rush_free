package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLegacyClientKeyMigrationAndLimits(t *testing.T) {
	a := testApp(t)
	legacy := "legacy-client-key"
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES('client_key',?)", hashToken(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateLegacyClientKey(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+legacy)
	key, err := a.authenticateClient(req)
	if err != nil || key.Name != "Legacy" {
		t.Fatalf("legacy key was not migrated: key=%#v err=%v", key, err)
	}

	limiter := newClientKeyLimiter()
	limited := &ClientKey{ID: 42, RPMLimit: 2}
	now := time.Now()
	for range 2 {
		if ok, _, code := limiter.allow(limited, now); !ok || code != "" {
			t.Fatalf("request under RPM limit was rejected: %q", code)
		}
	}
	if ok, _, code := limiter.allow(limited, now); ok || code != "key_rpm_limit" {
		t.Fatalf("RPM limit was not enforced: ok=%v code=%q", ok, code)
	}
	tokens := int64(12)
	tokenLimited := &ClientKey{ID: 43, TPMLimit: 12}
	limiter.recordTokens(tokenLimited.ID, &tokenUsage{Total: &tokens}, now)
	if ok, _, code := limiter.allow(tokenLimited, now); ok || code != "key_tpm_limit" {
		t.Fatalf("TPM limit was not enforced: ok=%v code=%q", ok, code)
	}
}

func TestModelAliasRespectsEnabledPolicy(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,pricing_metadata,raw_metadata,refreshed_at) VALUES(?,?,?,?,?,?,?)", "text:free", "Text", 1, "test", "{}", "{}", now)
	if err != nil {
		t.Fatal(err)
	}
	modelID, _ := result.LastInsertId()
	create := httptest.NewRecorder()
	a.createModelAlias(create, httptest.NewRequest(http.MethodPost, "/api/model-aliases", strings.NewReader(`{"alias":"fast","target_model_id":"text:free"}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create alias failed: %d %s", create.Code, create.Body.String())
	}
	if resolved, err := a.resolveModel("fast"); err != nil || resolved != "text:free" {
		t.Fatalf("alias did not resolve: %q %v", resolved, err)
	}
	policyReq := httptest.NewRequest(http.MethodPatch, "/api/models/1/policy", strings.NewReader(`{"enabled":false}`))
	policyReq.SetPathValue("id", stringID(modelID))
	policy := httptest.NewRecorder()
	a.patchModelPolicy(policy, policyReq)
	if policy.Code != http.StatusOK {
		t.Fatalf("disable policy failed: %d %s", policy.Code, policy.Body.String())
	}
	if _, err := a.resolveModel("fast"); err == nil {
		t.Fatal("alias resolved even though its target policy is disabled")
	}
}

func TestModelRefreshPreservesPoliciesAndHidesStaleAliases(t *testing.T) {
	a := testApp(t)
	requestCount := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		requestCount++
		if requestCount == 1 {
			_, _ = w.Write([]byte(`{"data":[{"id":"alpha:free"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"beta:free"}]}`))
	}))
	defer upstream.Close()
	put := httptest.NewRecorder()
	a.putUpstream(put, httptest.NewRequest(http.MethodPut, "/api/settings/upstream", strings.NewReader(`{"base_url":"`+upstream.URL+`"}`)))
	if put.Code != http.StatusOK {
		t.Fatalf("configure upstream failed: %d %s", put.Code, put.Body.String())
	}
	pointLaneAt(t, upstream.URL)
	firstRefresh := httptest.NewRecorder()
	a.refreshModels(firstRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if firstRefresh.Code != http.StatusOK {
		t.Fatalf("first refresh failed: %d %s", firstRefresh.Code, firstRefresh.Body.String())
	}
	var alphaID int64
	if err := a.db.QueryRow("SELECT id FROM models WHERE model_id='alpha:free'").Scan(&alphaID); err != nil {
		t.Fatal(err)
	}
	policy := httptest.NewRequest(http.MethodPatch, "/api/models/1/policy", strings.NewReader(`{"enabled":false}`))
	policy.SetPathValue("id", stringID(alphaID))
	policyResult := httptest.NewRecorder()
	a.patchModelPolicy(policyResult, policy)
	if policyResult.Code != http.StatusOK {
		t.Fatalf("disable alpha failed: %d %s", policyResult.Code, policyResult.Body.String())
	}
	alias := httptest.NewRecorder()
	a.createModelAlias(alias, httptest.NewRequest(http.MethodPost, "/api/model-aliases", strings.NewReader(`{"alias":"fast","target_model_id":"alpha:free"}`)))
	if alias.Code != http.StatusCreated {
		t.Fatalf("create alias failed: %d %s", alias.Code, alias.Body.String())
	}
	secondRefresh := httptest.NewRecorder()
	a.refreshModels(secondRefresh, httptest.NewRequest(http.MethodPost, "/api/settings/models/refresh", nil))
	if secondRefresh.Code != http.StatusOK {
		t.Fatalf("second refresh failed: %d %s", secondRefresh.Code, secondRefresh.Body.String())
	}
	var enabled int
	if err := a.db.QueryRow("SELECT enabled FROM model_policies WHERE model_id='alpha:free'").Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("refresh discarded alpha policy: enabled=%d err=%v", enabled, err)
	}
	if _, err := a.resolveModel("fast"); err == nil {
		t.Fatal("stale alias must not resolve after its target disappears")
	}
}

func TestProxyRuntimeReuseAndAdaptiveCooldown(t *testing.T) {
	a := testApp(t)
	first, err := a.httpClient(ProxyRecord{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.httpClient(ProxyRecord{})
	if err != nil || first != second {
		t.Fatalf("direct client was not reused: first=%p second=%p err=%v", first, second, err)
	}
	a.proxyRuntime.invalidate()
	third, err := a.httpClient(ProxyRecord{})
	if err != nil || third == first {
		t.Fatalf("cache invalidation did not replace the direct client: first=%p third=%p err=%v", first, third, err)
	}

	encrypted, err := a.encrypt("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	result, err := a.db.Exec("INSERT INTO proxies(uri,scheme,host,port,encrypted_password,enabled,health_status,failure_count,created_at,updated_at) VALUES(?,?,?,?,?,1,'unknown',0,?,?)", "http://127.0.0.1:8080", "http", "127.0.0.1", 8080, encrypted, now, now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	a.markProxyFailure(id)
	var failures int
	var firstCooldown string
	if err := a.db.QueryRow("SELECT failure_count,cooldown_until FROM proxies WHERE id=?", id).Scan(&failures, &firstCooldown); err != nil || failures != 1 {
		t.Fatalf("first failure was not persisted: failures=%d err=%v", failures, err)
	}
	firstAt, _ := time.Parse(time.RFC3339, firstCooldown)
	if remaining := time.Until(firstAt); remaining < 55*time.Second || remaining > 65*time.Second {
		t.Fatalf("first cooldown must be about one minute, got %s", remaining)
	}
	a.markProxyFailure(id)
	var secondCooldown string
	if err := a.db.QueryRow("SELECT failure_count,cooldown_until FROM proxies WHERE id=?", id).Scan(&failures, &secondCooldown); err != nil || failures != 2 {
		t.Fatalf("second failure was not persisted: failures=%d err=%v", failures, err)
	}
	secondAt, _ := time.Parse(time.RFC3339, secondCooldown)
	if remaining := time.Until(secondAt); remaining < 115*time.Second || remaining > 125*time.Second {
		t.Fatalf("second cooldown must be about two minutes, got %s", remaining)
	}
	a.markProxySuccess(id)
	if err := a.db.QueryRow("SELECT failure_count FROM proxies WHERE id=?", id).Scan(&failures); err != nil || failures != 0 {
		t.Fatalf("successful probe did not reset cooldown state: failures=%d err=%v", failures, err)
	}
}

func TestProbeJobPersistsAndRestartInterrupts(t *testing.T) {
	a := testApp(t)
	a.initializeRuntimeServices()
	now := time.Now().UTC()
	job := &proxyProbeJob{ID: "probe-job", Status: "running", Total: 2, CreatedAt: now, ExpiresAt: now.Add(proxyProbeJobRetention)}
	if err := a.probeJobs.add(job, "both"); err != nil {
		t.Fatal(err)
	}
	a.probeJobs.appendResult(job.ID, proxyProbeResult{ProxyID: 1, URI: "http://proxy.example:8080", ExitOK: true})
	if err := migrate(a.db); err != nil {
		t.Fatal(err)
	}
	restored, ok := a.probeJobs.snapshot(job.ID)
	if !ok || restored.Status != "interrupted" || restored.Completed != 1 || len(restored.Results) != 1 {
		t.Fatalf("probe job did not survive restart as interrupted: %#v ok=%v", restored, ok)
	}
}

func TestWebhookOutboxSignsPayload(t *testing.T) {
	a := testApp(t)
	a.initializeRuntimeServices()
	type delivery struct{ body, signature string }
	delivered := make(chan delivery, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		delivered <- delivery{body: string(body), signature: r.Header.Get("X-RelayDesk-Signature")}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	settings := `{"enabled":true,"webhook_url":"` + server.URL + `","webhook_secret":"top-secret","events":["resin_unavailable"],"low_proxy_threshold":3,"success_rate_percent":80}`
	recorder := httptest.NewRecorder()
	a.putAlertSettings(recorder, httptest.NewRequest(http.MethodPut, "/api/settings/alerts", strings.NewReader(settings)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("save alert settings failed: %d %s", recorder.Code, recorder.Body.String())
	}
	a.emitAlert("resin_unavailable", "error", "Resin is unavailable", map[string]any{"error": "dial timeout"})
	select {
	case got := <-delivered:
		mac := hmac.New(sha256.New, []byte("top-secret"))
		_, _ = mac.Write([]byte(got.body))
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if got.signature != want {
			t.Fatalf("invalid webhook signature: got %q want %q", got.signature, want)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(got.body), &payload); err != nil || payload["version"] != float64(1) || payload["type"] != "resin_unavailable" {
			t.Fatalf("invalid webhook payload: %#v err=%v", payload, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("webhook was not delivered")
	}
}

func TestDimensionStatsSplitExternalErrors(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, item := range []struct {
		status, origin         string
		latency, first, tokens int
	}{
		{"success", "none", 100, 20, 9},
		{"error", "external", 400, 0, 0},
		{"error", "user", 250, 0, 0},
	} {
		if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,proxy_uri,status,status_code,latency_ms,first_token_latency_ms,retry_count,total_tokens,error_origin,route_engine) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", now, "chat", "text:free", "http://proxy.example:8080", item.status, http.StatusBadGateway, item.latency, item.first, 0, item.tokens, item.origin, "builtin"); err != nil {
			t.Fatal(err)
		}
	}
	recorder := httptest.NewRecorder()
	a.statsModels(recorder, httptest.NewRequest(http.MethodGet, "/api/stats/models?window=24h", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stats request failed: %d %s", recorder.Code, recorder.Body.String())
	}
	var stats []dimensionStat
	if err := json.NewDecoder(recorder.Body).Decode(&stats); err != nil || len(stats) != 1 {
		t.Fatalf("invalid stats response: %#v err=%v", stats, err)
	}
	if stats[0].Requests != 3 || stats[0].ExternalErrors != 1 || stats[0].UserErrors != 1 || stats[0].Tokens != 9 || stats[0].P95LatencyMS != 400 {
		t.Fatalf("dimension stats did not aggregate correctly: %#v", stats[0])
	}
}

func stringID(id int64) string {
	return strconv.FormatInt(id, 10)
}
