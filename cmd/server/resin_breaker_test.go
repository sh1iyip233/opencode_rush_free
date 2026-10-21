package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// distinctAccounts returns the account names one breaker test walks through.
func distinctAccounts(n int) []string {
	accounts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		accounts = append(accounts, fmt.Sprintf("acct-%02d", i))
	}
	return accounts
}

func TestResinBreakerOpensAfterDistinctExitsAreLimited(t *testing.T) {
	a := testApp(t)
	threshold := a.resinBreakerThreshold()
	if threshold != resinBreakerDefault {
		t.Fatalf("threshold = %d, want default %d", threshold, resinBreakerDefault)
	}
	accounts := distinctAccounts(threshold)
	for _, account := range accounts[:threshold-1] {
		a.noteResinLimited(account)
	}
	if a.resinBreakerOpenNow() {
		t.Fatal("breaker opened before the threshold was reached")
	}
	a.noteResinLimited(accounts[threshold-1])
	if !a.resinBreakerOpenNow() {
		t.Fatal("breaker did not open at the threshold")
	}
	// Expire the window directly instead of waiting out the real cooldown.
	a.resinBreakerMu.Lock()
	a.resinBreakerOpenUntil = time.Now().Add(-time.Second)
	a.resinBreakerMu.Unlock()
	if a.resinBreakerOpenNow() {
		t.Fatal("breaker stayed open past its cooldown")
	}
}

// One exit answering 429 over and over is a sick exit, not a sick pool: the
// breaker must not black out every other account because of it.
func TestResinBreakerIgnoresRepeatedLimitsOnOneExit(t *testing.T) {
	a := testApp(t)
	threshold := a.resinBreakerThreshold()
	for i := 0; i < threshold*3; i++ {
		a.noteResinLimited("acct-stuck")
	}
	if a.resinBreakerOpenNow() {
		t.Fatalf("breaker opened after %d limits on a single exit", threshold*3)
	}
}

func TestResinBreakerResetsOnHealthyAnswer(t *testing.T) {
	a := testApp(t)
	threshold := a.resinBreakerThreshold()
	accounts := distinctAccounts(threshold)
	for _, account := range accounts[:threshold-1] {
		a.noteResinLimited(account)
	}
	a.noteResinHealthy()
	for _, account := range accounts[:threshold-1] {
		a.noteResinLimited(account)
	}
	if a.resinBreakerOpenNow() {
		t.Fatal("breaker opened although a success had reset the streak")
	}
}

func TestResinBreakerIgnoresExpiredStreak(t *testing.T) {
	a := testApp(t)
	threshold := a.resinBreakerThreshold()
	accounts := distinctAccounts(threshold)
	for _, account := range accounts[:threshold-1] {
		a.noteResinLimited(account)
	}
	// Age the streak past the window: old 429s must not count toward the threshold.
	a.resinBreakerMu.Lock()
	a.resinBreakerSince = time.Now().Add(-2 * resinBreakerWindow)
	a.resinBreakerMu.Unlock()
	for _, account := range accounts[:threshold-1] {
		a.noteResinLimited(account)
	}
	if a.resinBreakerOpenNow() {
		t.Fatal("breaker counted 429s from an expired window")
	}
}

func TestResinSettingIntClampsAndFallsBack(t *testing.T) {
	a := testApp(t)
	cases := []struct {
		raw  string
		want int
	}{
		{"20", 20},
		{"1", resinRetryBudgetMin},
		{"9999", resinRetryBudgetMax},
		{"not-a-number", resinRetryBudgetDefault},
	}
	for _, tc := range cases {
		if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", resinRetryBudgetSettingKey, tc.raw); err != nil {
			t.Fatalf("seed setting %q: %v", tc.raw, err)
		}
		if got := a.settingInt(resinRetryBudgetSettingKey, resinRetryBudgetDefault, resinRetryBudgetMin, resinRetryBudgetMax); got != tc.want {
			t.Fatalf("settingInt(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
	if _, err := a.db.Exec("DELETE FROM settings WHERE key=?", resinRetryBudgetSettingKey); err != nil {
		t.Fatal(err)
	}
	if got := a.settingInt(resinRetryBudgetSettingKey, resinRetryBudgetDefault, resinRetryBudgetMin, resinRetryBudgetMax); got != resinRetryBudgetDefault {
		t.Fatalf("missing setting = %d, want default %d", got, resinRetryBudgetDefault)
	}
}

func TestResinRetryBudgetAndBreakerSettings(t *testing.T) {
	a := testApp(t)
	if got := a.resinRetryBudget(); got != 45*time.Second {
		t.Fatalf("retry budget = %s, want 45s", got)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", resinRetryBudgetSettingKey, "90"); err != nil {
		t.Fatal(err)
	}
	if got := a.resinRetryBudget(); got != 90*time.Second {
		t.Fatalf("retry budget = %s, want 90s", got)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", resinBreakerCooldownSettingKey, "120"); err != nil {
		t.Fatal(err)
	}
	if got := a.resinBreakerCooldown(); got != 120*time.Second {
		t.Fatalf("breaker cooldown = %s, want 120s", got)
	}
	if _, err := a.db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", resinBreakerThresholdSettingKey, "40"); err != nil {
		t.Fatal(err)
	}
	if got := a.resinBreakerThreshold(); got != 40 {
		t.Fatalf("breaker threshold = %d, want 40", got)
	}
}

func TestProxyEngineSettingsAcceptRetryKnobs(t *testing.T) {
	a := testApp(t)
	body := `{"engine":"resin","resin_gateway_url":"http://resin.example.test:2260","resin_platform":"Default","resin_proxy_token":"proxy-secret","resin_retry_budget_seconds":60,"resin_breaker_threshold":20,"resin_breaker_cooldown_seconds":45}`
	put := httptest.NewRecorder()
	a.putProxyEngine(put, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(body)))
	if put.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", put.Code, put.Body.String())
	}
	if got := a.resinRetryBudget(); got != 60*time.Second {
		t.Fatalf("retry budget = %s, want 60s", got)
	}
	if got := a.resinBreakerThreshold(); got != 20 {
		t.Fatalf("breaker threshold = %d, want 20", got)
	}
	if got := a.resinBreakerCooldown(); got != 45*time.Second {
		t.Fatalf("breaker cooldown = %s, want 45s", got)
	}
	status, err := a.proxyEngineStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ResinRetryBudgetSeconds != 60 || status.ResinBreakerThreshold != 20 || status.ResinBreakerCooldownSeconds != 45 {
		t.Fatalf("status knobs = budget %d threshold %d cooldown %d", status.ResinRetryBudgetSeconds, status.ResinBreakerThreshold, status.ResinBreakerCooldownSeconds)
	}

	bad := httptest.NewRecorder()
	a.putProxyEngine(bad, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(`{"resin_retry_budget_seconds":5}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range budget status = %d, want 400", bad.Code)
	}
	if got := a.resinRetryBudget(); got != 60*time.Second {
		t.Fatalf("rejected input changed the budget to %s", got)
	}
}
