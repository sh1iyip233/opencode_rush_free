package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLogsGuideListsEveryRegisteredEndpoint keeps /api/logs/guide honest. An
// agent that finds the host but not the schema starts from this document alone,
// so a route that exists in logRoutes but is missing here is a dead end: the
// caller has no way to learn the endpoint exists.
func TestLogsGuideListsEveryRegisteredEndpoint(t *testing.T) {
	t.Parallel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://relay.example/api/logs/guide", nil)
	(&App{}).logsGuide(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("guide status = %d, want 200", recorder.Code)
	}
	guide := recorder.Body.String()

	// Keep in step with logRoutes in logs_api.go.
	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/api/logs"},
		{http.MethodGet, "/api/logs/stream"},
		{http.MethodGet, "/api/logs/stats"},
		{http.MethodGet, "/api/logs/diagnostics"},
		{http.MethodGet, "/api/logs/export"},
		{http.MethodGet, "/api/logs/guide"},
		{http.MethodGet, "/api/logs/settings"},
		{http.MethodPut, "/api/logs/settings"},
		{http.MethodPost, "/api/logs/prune"},
		{http.MethodGet, "/api/logs/notes"},
		{http.MethodPost, "/api/logs/notes"},
		{http.MethodPatch, "/api/logs/notes/{id}"},
		{http.MethodDelete, "/api/logs/notes/{id}"},
	}
	for _, endpoint := range endpoints {
		row := "| " + endpoint.method + " | `" + endpoint.path + "` |"
		if !strings.Contains(guide, row) {
			t.Errorf("guide is missing the endpoint index row %q", row)
		}
	}
}
