package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// seedResinEngine points the proxy engine at a Resin gateway so the routing
// helpers can build records without a database round trip per assertion.
func seedResinEngine(t *testing.T, a *App) {
	t.Helper()
	if _, err := a.db.Exec(
		"INSERT OR REPLACE INTO settings(key,value) VALUES(?,?),(?,?),(?,?)",
		proxyEngineSettingKey, proxyEngineResin,
		resinGatewayURLSettingKey, "http://resin.example.test:2260",
		resinPlatformSettingKey, "Default",
	); err != nil {
		t.Fatalf("seed Resin engine: %v", err)
	}
	a.invalidateProxyEngineCache()
}

func TestResinExitIPCooldownCoversEveryAccountBehindTheSameIP(t *testing.T) {
	a := testApp(t)
	seedResinEngine(t, a)

	a.rememberResinExitIP("acct-a", "203.0.113.7")
	a.rememberResinExitIP("acct-b", "203.0.113.7")
	a.rememberResinExitIP("acct-c", "198.51.100.9")

	a.coolResinExitIP("203.0.113.7", time.Minute)

	if a.resinExitUsable("acct-a") {
		t.Fatal("the limited account is still usable")
	}
	if a.resinExitUsable("acct-b") {
		t.Fatal("a fresh account behind the same limited IP is still usable")
	}
	if !a.resinExitUsable("acct-c") {
		t.Fatal("an account on another IP was parked by association")
	}
	if got := a.cooledResinExitIPs(); got != 1 {
		t.Fatalf("cooled exit IPs = %d, want 1", got)
	}
	if got := a.knownResinExitIPs(); got != 3 {
		t.Fatalf("known exit IPs = %d, want 3", got)
	}
}

func TestResinExitIPMappingExpires(t *testing.T) {
	a := testApp(t)
	seedResinEngine(t, a)

	a.rememberResinExitIP("acct-a", "203.0.113.7")
	a.resinExitMu.Lock()
	a.resinExitIPs["acct-a"] = resinExitIPRecord{IP: "203.0.113.7", LearnedAt: time.Now().Add(-2 * resinExitIPTTL)}
	a.resinExitMu.Unlock()

	if _, known := a.resinExitIP("acct-a"); known {
		t.Fatal("an expired mapping was still reported")
	}
	if got := a.knownResinExitIPs(); got != 0 {
		t.Fatalf("known exit IPs = %d after expiry, want 0", got)
	}
}

func TestResinExitIPCooldownExpiresAndClamps(t *testing.T) {
	a := testApp(t)

	a.coolResinExitIP("203.0.113.7", time.Millisecond)
	if remaining, cooling := a.resinExitIPCooling("203.0.113.7"); !cooling {
		t.Fatalf("cooldown was not clamped to the floor, remaining = %s", remaining)
	}
	if remaining, _ := a.resinExitIPCooling("203.0.113.7"); remaining < resinAccountCooldownFloor-time.Second {
		t.Fatalf("remaining = %s, want at least %s", remaining, resinAccountCooldownFloor)
	}

	// Expire the entry directly instead of waiting out the floor.
	a.resinCoolMu.Lock()
	a.resinCooledIPs["203.0.113.7"] = time.Now().Add(-time.Second)
	a.resinCoolMu.Unlock()
	if _, cooling := a.resinExitIPCooling("203.0.113.7"); cooling {
		t.Fatal("an expired IP park was still reported")
	}
	if got := a.cooledResinExitIPs(); got != 0 {
		t.Fatalf("cooled exit IPs = %d after expiry, want 0", got)
	}
}

func TestResinExitUsableHonoursAccountPark(t *testing.T) {
	a := testApp(t)
	a.coolResinAccount("acct-a", time.Minute)
	if a.resinExitUsable("acct-a") {
		t.Fatal("a parked account was reported usable")
	}
	if !a.resinExitUsable("acct-b") {
		t.Fatal("an unrelated account was reported unusable")
	}
}

func TestResinExitUsableSkipsUnknownAccounts(t *testing.T) {
	a := testApp(t)
	// No mapping and no park: the draw must not block on missing knowledge.
	if !a.resinExitUsable("acct-never-seen") {
		t.Fatal("an account with no learned IP was reported unusable")
	}
}

// A fresh account that lands on an IP another account was just limited on has
// to be skipped at draw time, which is the whole point of remembering the IP.
func TestResinDrawSkipsAccountWhoseExitIPIsParked(t *testing.T) {
	a := testApp(t)
	seedResinEngine(t, a)
	cfg, err := a.loadProxyEngine()
	if err != nil {
		t.Fatal(err)
	}
	route, err := newResinRequestRoute("")
	if err != nil {
		t.Fatal(err)
	}
	base, err := route.baseAccount(a)
	if err != nil {
		t.Fatal(err)
	}
	a.rememberResinExitIP(base, "203.0.113.7")
	a.coolResinExitIP("203.0.113.7", time.Minute)

	p, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := resinAccountOf(p); got == base {
		t.Fatal("the draw reused the account whose exit IP was parked")
	}
	if route.lastAccount != resinAccountOf(p) {
		t.Fatalf("route.lastAccount = %q, want %q", route.lastAccount, resinAccountOf(p))
	}
}

func TestNormalizeExitIP(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		{"  203.0.113.7\n", "203.0.113.7"},
		{"2001:db8::1", "2001:db8::1"},
		{"", ""},
		{"<html>blocked</html>", ""},
		{"not-an-ip", ""},
		{strings.Repeat("9", 80), ""},
	}
	for _, tc := range cases {
		if got := normalizeExitIP(tc.raw); got != tc.want {
			t.Fatalf("normalizeExitIP(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestProxyEngineSettingsAcceptIPProbeURL(t *testing.T) {
	a := testApp(t)
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		a.putProxyEngine(rec, httptest.NewRequest(http.MethodPut, "/api/settings/proxy-engine", strings.NewReader(body)))
		return rec
	}

	rec := put(`{"engine":"resin","resin_gateway_url":"http://resin.example.test:2260","resin_platform":"Default","resin_proxy_token":"proxy-secret","resin_ip_probe_url":"https://ip.example.test/echo"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var status proxyEngineStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.ResinIPProbeURL != "https://ip.example.test/echo" {
		t.Fatalf("probe URL = %q, want the configured value", status.ResinIPProbeURL)
	}

	// "off" is the documented way to switch exit-IP learning off, and it has to
	// survive a reload instead of falling back to the default service.
	rec = put(`{"resin_ip_probe_url":"off"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cfg, err := a.loadProxyEngine()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResinIPProbeURL != "" {
		t.Fatalf("probe URL after off = %q, want empty", cfg.ResinIPProbeURL)
	}

	// An unset value must come back as the built-in default, not as "off".
	if _, err := a.db.Exec("DELETE FROM settings WHERE key=?", resinIPProbeURLSettingKey); err != nil {
		t.Fatal(err)
	}
	a.invalidateProxyEngineCache()
	if cfg, err = a.loadProxyEngine(); err != nil {
		t.Fatal(err)
	}
	if cfg.ResinIPProbeURL != resinIPProbeDefault {
		t.Fatalf("probe URL default = %q, want %q", cfg.ResinIPProbeURL, resinIPProbeDefault)
	}

	rec = put(`{"resin_ip_probe_url":"ftp://ip.example.test"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid probe URL status = %d, want 400", rec.Code)
	}
}
