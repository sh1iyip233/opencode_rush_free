package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func insertPublicStatusUsage(t *testing.T, a *App, createdAt time.Time, model, status, origin string) {
	t.Helper()
	code := http.StatusOK
	if status != "success" {
		code = http.StatusBadGateway
	}
	if _, err := a.db.Exec("INSERT INTO usage_requests(created_at,request_kind,model,status,status_code,latency_ms,first_token_latency_ms,retry_count,error_origin) VALUES(?,?,?,?,?,?,?,?,?)", createdAt.UTC().Format(time.RFC3339Nano), "chat", model, status, code, 120, 60, 0, origin); err != nil {
		t.Fatal(err)
	}
}

func TestPublicStatusAggregatesAllFreeModelsWithoutAdminSession(t *testing.T) {
	a := testApp(t)
	now := time.Now().UTC()
	nowText := now.Format(time.RFC3339)
	for _, model := range []string{"good:free", "bad:free", "idle:free"} {
		if _, err := a.db.Exec("INSERT INTO models(model_id,display_name,is_free,free_reason,refreshed_at) VALUES(?,?,?,?,?)", model, model, 1, "test", nowText); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.db.Exec("INSERT INTO model_policies(model_id,enabled,updated_at) VALUES(?,?,?)", "idle:free", 0, nowText); err != nil {
		t.Fatal(err)
	}
	windowStart := chinaHourStart(now).Add(-23 * time.Hour)
	goodAt := windowStart.Add(2*time.Hour + 10*time.Minute)
	for i := 0; i < 19; i++ {
		insertPublicStatusUsage(t, a, goodAt.Add(time.Duration(i)*time.Second), "good:free", "success", "none")
	}
	insertPublicStatusUsage(t, a, goodAt.Add(30*time.Second), "good:free", "error", "internal")
	badAt := windowStart.Add(4*time.Hour + 10*time.Minute)
	insertPublicStatusUsage(t, a, badAt, "bad:free", "success", "none")
	insertPublicStatusUsage(t, a, badAt.Add(time.Minute), "bad:free", "error", "internal")
	insertPublicStatusUsage(t, a, badAt.Add(2*time.Minute), "bad:free", "error", "external")

	mux := http.NewServeMux()
	a.routes(mux)
	server := httptest.NewServer(a.withMiddleware(mux))
	defer server.Close()
	response, err := server.Client().Get(server.URL + "/api/public/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("public status failed: %d", response.StatusCode)
	}
	if !strings.HasPrefix(response.Header.Get("Cache-Control"), "public") {
		t.Fatalf("public status should be cacheable: %q", response.Header.Get("Cache-Control"))
	}
	var payload struct {
		Summary publicStatusSummary `json:"summary"`
		Models  []publicModelStatus `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Summary.Models != 3 || len(payload.Models) != 3 {
		t.Fatalf("unexpected public model count: %#v", payload)
	}
	byID := map[string]publicModelStatus{}
	for _, model := range payload.Models {
		byID[model.ModelID] = model
		if len(model.Buckets) != publicStatusWindowHours {
			t.Fatalf("model %s has %d buckets", model.ModelID, len(model.Buckets))
		}
	}
	if good := byID["good:free"]; good.Status != "available" || good.SuccessRate == nil || *good.SuccessRate != 0.95 || good.Requests24h != 20 {
		t.Fatalf("95%% threshold was not applied: %#v", good)
	}
	if bad := byID["bad:free"]; bad.Status != "outage" || bad.SuccessRate == nil || *bad.SuccessRate != 0.5 || bad.ExternalErrors24h != 1 {
		t.Fatalf("proxy failure or external error was miscounted: %#v", bad)
	}
	if idle := byID["idle:free"]; idle.AdminEnabled || idle.Status != "no_request" || idle.SuccessRate != nil {
		t.Fatalf("idle disabled model was not represented correctly: %#v", idle)
	}
	if strings.Contains(response.Header.Get("Content-Type"), "text/html") {
		t.Fatal("public status route returned the SPA instead of JSON")
	}
}

func TestPublicStatusReportsMaintenanceMode(t *testing.T) {
	a := testApp(t)
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", maintenanceModeSettingKey, "true"); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	a.publicStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/public/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public status failed: %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		MaintenanceMode bool `json:"maintenance_mode"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.MaintenanceMode {
		t.Fatal("public status did not report maintenance mode")
	}
}
