package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func addTestProxy(t *testing.T, a *App, raw string) int64 {
	t.Helper()
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

// A cooled exit has to come back on its own. The snapshot used to skip rows that
// were cooling when it was built, so the exit vanished from rotation until a
// restart, and the pool shrank until every request failed.
func TestCooledExitReturnsToRotationWithoutRestart(t *testing.T) {
	a := testApp(t)
	cooling := addTestProxy(t, a, "http://127.0.21.1:3128")
	healthy := addTestProxy(t, a, "http://127.0.21.2:3128")

	available, err := a.availableProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 2 {
		t.Fatalf("expected both exits in rotation, got %d", len(available))
	}

	a.markProxyLimited(cooling, 0)
	available, err = a.availableProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != healthy {
		t.Fatalf("cooled exit stayed in rotation: %#v", available)
	}

	// Rebuilding the snapshot while the exit is cooling is the exact case that
	// used to lose the row for good.
	a.proxyRuntime.invalidate()
	available, err = a.availableProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != healthy {
		t.Fatalf("cooled exit stayed in rotation after reload: %#v", available)
	}
	snapshot := a.proxyRuntime.snapshot.Load()
	if snapshot == nil || len(snapshot.proxies) != 2 {
		t.Fatalf("snapshot dropped the cooling exit: %#v", snapshot)
	}

	a.markProxySuccess(cooling)
	available, err = a.availableProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 2 {
		t.Fatalf("recovered exit never returned to rotation: %#v", available)
	}
}

// A cooldown that has already elapsed must not keep an exit out of rotation just
// because the snapshot was built earlier.
func TestExpiredCooldownIsUsableFromStaleSnapshot(t *testing.T) {
	a := testApp(t)
	id := addTestProxy(t, a, "http://127.0.22.1:3128")
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := a.db.Exec("UPDATE proxies SET health_status='cooldown',failure_count=1,cooldown_until=? WHERE id=?", past, id); err != nil {
		t.Fatal(err)
	}
	a.proxyRuntime.invalidate()
	available, err := a.availableProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(available) != 1 || available[0].ID != id {
		t.Fatalf("expired cooldown still excluded the exit: %#v", available)
	}
}

func TestClassifyUpstreamOutcome(t *testing.T) {
	cases := []struct {
		status int
		want   upstreamOutcome
		rotate bool
	}{
		{http.StatusOK, upstreamOutcomeSuccess, false},
		{http.StatusTooManyRequests, upstreamOutcomeLimited, true},
		{http.StatusUnauthorized, upstreamOutcomeRefused, true},
		{http.StatusForbidden, upstreamOutcomeRefused, true},
		{http.StatusProxyAuthRequired, upstreamOutcomeProxyAuth, false},
		{http.StatusInternalServerError, upstreamOutcomeServerError, true},
		{http.StatusBadGateway, upstreamOutcomeServerError, true},
		{http.StatusServiceUnavailable, upstreamOutcomeServerError, true},
		{http.StatusGatewayTimeout, upstreamOutcomeServerError, true},
		{http.StatusRequestTimeout, upstreamOutcomeClientError, true},
		{http.StatusBadRequest, upstreamOutcomeClientError, false},
		{http.StatusNotFound, upstreamOutcomeClientError, false},
	}
	for _, testCase := range cases {
		if got := classifyUpstreamOutcome(testCase.status); got != testCase.want {
			t.Fatalf("status %d classified as %d, want %d", testCase.status, got, testCase.want)
		}
		if got := shouldRotateUpstream(testCase.status); got != testCase.rotate {
			t.Fatalf("status %d rotate=%v, want %v", testCase.status, got, testCase.rotate)
		}
	}
}

// A 429 cools the whole exit, a 401/403 bans only the pairing, and a 2xx heals.
func TestExitCooldownAndModelBanAreSeparate(t *testing.T) {
	a := testApp(t)
	id := addTestProxy(t, a, "http://127.0.23.1:3128")

	a.markProxyLimited(id, 0)
	var status string
	var failures int
	var cooldown string
	if err := a.db.QueryRow("SELECT health_status,failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", id).Scan(&status, &failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	if status != "cooldown" || failures != 1 {
		t.Fatalf("429 did not cool the exit: status=%q failures=%d", status, failures)
	}
	remaining := time.Until(parseStoredTimeOrZero(cooldown))
	if remaining < 50*time.Second || remaining > 70*time.Second {
		t.Fatalf("unexpected first cooldown %s", remaining)
	}

	a.markProxyLimited(id, 0)
	if err := a.db.QueryRow("SELECT failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", id).Scan(&failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	remaining = time.Until(parseStoredTimeOrZero(cooldown))
	if failures != 2 || remaining < 110*time.Second || remaining > 130*time.Second {
		t.Fatalf("cooldown did not escalate: failures=%d remaining=%s", failures, remaining)
	}

	// A longer Retry-After wins over the local backoff.
	a.markProxyLimited(id, 10*time.Minute)
	if err := a.db.QueryRow("SELECT COALESCE(cooldown_until,'') FROM proxies WHERE id=?", id).Scan(&cooldown); err != nil {
		t.Fatal(err)
	}
	if remaining = time.Until(parseStoredTimeOrZero(cooldown)); remaining < 9*time.Minute {
		t.Fatalf("Retry-After was not honoured: %s", remaining)
	}

	// A refusal is scoped to the exit and model pairing, and must not cool the
	// exit that is still healthy for every other model.
	fresh := addTestProxy(t, a, "http://127.0.23.2:3128")
	a.markModelRefusal(fresh, "vendor/model-a")
	if err := a.db.QueryRow("SELECT health_status,failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", fresh).Scan(&status, &failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" || failures != 0 || cooldown != "" {
		t.Fatalf("a refusal cooled the exit: status=%q failures=%d cooldown=%q", status, failures, cooldown)
	}
	if len(a.bannedProxyIDs("vendor/model-a")) != 0 {
		t.Fatal("one refusal already banned the pairing")
	}

	a.markModelRefusal(fresh, "vendor/model-a")
	if _, ok := a.bannedProxyIDs("vendor/model-a")[fresh]; !ok {
		t.Fatal("a repeated refusal did not ban the pairing")
	}
	if len(a.bannedProxyIDs("vendor/model-b")) != 0 {
		t.Fatal("a ban leaked to another model")
	}
	var state string
	var bannedUntil string
	if err := a.db.QueryRow("SELECT state,COALESCE(banned_until,'') FROM proxy_model_bans WHERE proxy_id=? AND model_id=?", fresh, "vendor/model-a").Scan(&state, &bannedUntil); err != nil {
		t.Fatal(err)
	}
	if state != "banned" || bannedUntil == "" {
		t.Fatalf("ban row is not confirmed: state=%q banned_until=%q", state, bannedUntil)
	}

	a.markProxySuccess(fresh)
	if err := a.db.QueryRow("SELECT health_status,failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", fresh).Scan(&status, &failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	if status != "healthy" || failures != 0 || cooldown != "" {
		t.Fatalf("success did not heal the exit: status=%q failures=%d cooldown=%q", status, failures, cooldown)
	}
}

func TestPickSkipsBannedPairingThenDegrades(t *testing.T) {
	a := testApp(t)
	proxies := []ProxyRecord{{ID: 1}, {ID: 2}}

	picked, ok, err := a.pickSessionProxy(proxyPick{proxies: proxies, model: "m", banned: map[int64]struct{}{1: {}}})
	if err != nil || !ok {
		t.Fatalf("pick failed: ok=%v err=%v", ok, err)
	}
	if picked.ID != 2 {
		t.Fatalf("a banned pairing was picked: %d", picked.ID)
	}

	picked, ok, err = a.pickSessionProxy(proxyPick{proxies: proxies, model: "m", banned: map[int64]struct{}{1: {}, 2: {}}})
	if err != nil || !ok {
		t.Fatalf("all-banned pool did not degrade: ok=%v err=%v", ok, err)
	}
	if picked.ID == 0 {
		t.Fatal("degraded pick returned no exit")
	}

	if _, ok, err = a.pickSessionProxy(proxyPick{proxies: proxies, model: "m", used: map[int64]struct{}{1: {}, 2: {}}}); err != nil || ok {
		t.Fatalf("a spent pool must not pick again: ok=%v err=%v", ok, err)
	}
}

func TestIsProxyTransportError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"client hangup", fmt.Errorf("Post \"https://upstream/v1/chat\": %w", context.Canceled), false},
		{"client deadline", fmt.Errorf("Post \"https://upstream/v1/chat\": %w", context.DeadlineExceeded), false},
		{"slow upstream", errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), false},
		{"upstream 500", errors.New("upstream returned HTTP 500"), false},
		{"proxy dial refused", errors.New("proxyconnect tcp: dial tcp 10.0.0.1:1080: connect: connection refused"), true},
		{"proxy dial timeout", errors.New("proxyconnect tcp: dial tcp 10.0.0.1:1080: i/o timeout"), true},
		{"reset mid stream", errors.New("read tcp 127.0.0.1:5123->1.2.3.4:443: connection reset by peer"), true},
		{"truncated body", io.ErrUnexpectedEOF, true},
		{"tls handshake", errors.New("net/http: TLS handshake timeout"), true},
		{"socks dial", errors.New("socks connect tcp 10.0.0.1:1080->1.2.3.4:443: connection refused"), true},
	}
	for _, testCase := range cases {
		if got := isProxyTransportError(testCase.err); got != testCase.want {
			t.Fatalf("%s: got %v, want %v", testCase.name, got, testCase.want)
		}
	}
}

func TestClassifyExitProbe(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		err     error
		exitIP  string
		want    probeVerdict
		touches bool
		healthy bool
	}{
		{"reachable", http.StatusOK, nil, "203.0.113.7", probeVerdictOK, true, true},
		{"no address", http.StatusOK, nil, "", probeVerdictUnreadable, false, false},
		{"echo server error", http.StatusBadGateway, nil, "", probeVerdictEchoUnavailable, false, false},
		{"echo unreachable", 0, errors.New("context deadline exceeded (Client.Timeout exceeded while awaiting headers)"), "", probeVerdictEchoUnavailable, false, false},
		{"echo cancelled", 0, fmt.Errorf("Get \"https://api.ipify.org\": %w", context.Canceled), "", probeVerdictEchoUnavailable, false, false},
		{"dead link", 0, errors.New("proxyconnect tcp: dial tcp 10.0.0.1:1080: connect: connection refused"), "", probeVerdictTransport, true, false},
	}
	for _, testCase := range cases {
		got := classifyExitProbe(testCase.status, testCase.err, testCase.exitIP)
		if got != testCase.want {
			t.Fatalf("%s: verdict %v, want %v", testCase.name, got, testCase.want)
		}
		if got.touchesExitHealth() != testCase.touches || got.healthy() != testCase.healthy {
			t.Fatalf("%s: touches=%v healthy=%v", testCase.name, got.touchesExitHealth(), got.healthy())
		}
	}
}

// Only a dead link may cool an exit from a probe. Echo trouble and upstream
// failures have to leave the health column alone.
func TestProbeVerdictOnlyTouchesHealthWhenTheLinkFailed(t *testing.T) {
	a := testApp(t)
	id := addTestProxy(t, a, "http://127.0.24.1:3128")

	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, Verdict: probeVerdictEchoUnavailable, Error: "echo endpoint returned HTTP 502", exitProbed: true})
	assertProxyHealth(t, a, id, "unknown", 0, "")

	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, Verdict: probeVerdictUnreadable, exitProbed: true})
	assertProxyHealth(t, a, id, "unknown", 0, "")

	// An upstream probe failure never speaks about the exit either.
	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, UpstreamStatus: "HTTP 500", upstreamProbed: true})
	assertProxyHealth(t, a, id, "unknown", 0, "")

	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, Verdict: probeVerdictTransport, Error: "dial timeout", exitProbed: true})
	status, _, cooldown := proxyHealth(t, a, id)
	if status != "cooldown" || cooldown == "" {
		t.Fatalf("a dead link was not cooled: status=%q cooldown=%q", status, cooldown)
	}

	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, ExitOK: true, ExitIP: "203.0.113.9", Verdict: probeVerdictOK, exitProbed: true})
	assertProxyHealth(t, a, id, "healthy", 0, "")

	// Results restored from before the verdict column only carry exit_ok.
	a.persistProbeResult(id, proxyProbeResult{ProxyID: id, ExitOK: true, exitProbed: true})
	assertProxyHealth(t, a, id, "healthy", 0, "")
}

func proxyHealth(t *testing.T, a *App, id int64) (string, int, string) {
	t.Helper()
	var status, cooldown string
	var failures int
	if err := a.db.QueryRow("SELECT health_status,failure_count,COALESCE(cooldown_until,'') FROM proxies WHERE id=?", id).Scan(&status, &failures, &cooldown); err != nil {
		t.Fatal(err)
	}
	return status, failures, cooldown
}

func assertProxyHealth(t *testing.T, a *App, id int64, wantStatus string, wantFailures int, wantCooldown string) {
	t.Helper()
	status, failures, cooldown := proxyHealth(t, a, id)
	if status != wantStatus || failures != wantFailures || cooldown != wantCooldown {
		t.Fatalf("health is status=%q failures=%d cooldown=%q, want status=%q failures=%d cooldown=%q", status, failures, cooldown, wantStatus, wantFailures, wantCooldown)
	}
}

func parseStoredTimeOrZero(raw string) time.Time {
	if parsed := parseStoredTime(raw); parsed != nil {
		return *parsed
	}
	return time.Time{}
}

func TestProbeVerdictTextRoundTrip(t *testing.T) {
	for _, verdict := range []probeVerdict{probeVerdictOK, probeVerdictEchoUnavailable, probeVerdictTransport, probeVerdictUnreadable} {
		text, err := verdict.MarshalText()
		if err != nil {
			t.Fatal(err)
		}
		var restored probeVerdict
		if err := restored.UnmarshalText(text); err != nil {
			t.Fatal(err)
		}
		if restored != verdict {
			t.Fatalf("%v did not round trip through %q", verdict, text)
		}
	}
	var empty probeVerdict
	if err := empty.UnmarshalText(nil); err != nil || empty != probeVerdictUnknown {
		t.Fatalf("empty text did not map to unknown: %v %v", empty, err)
	}
	if err := empty.UnmarshalText([]byte(strings.TrimSpace("nonsense"))); err == nil {
		t.Fatal("unknown verdict text was accepted")
	}
}

// A retired model keeps its zero pricing. Without the deprecation check it would
// be advertised as free and fail every request routed to it.
func TestDeprecatedModelIsNotFree(t *testing.T) {
	free := map[string]any{"pricing": map[string]any{"prompt": "0", "completion": "0"}}
	if isFree, _ := classifyFree("vendor/model-a", free); !isFree {
		t.Fatal("a zero priced model is not free")
	}
	retired := map[string]any{"pricing": map[string]any{"prompt": "0", "completion": "0"}, "deprecated": true}
	if isFree, reason := classifyFree("vendor/model-a", retired); isFree {
		t.Fatalf("a deprecated model was advertised as free: %q", reason)
	}
	statusRetired := map[string]any{"pricing": map[string]any{"prompt": "0", "completion": "0"}, "status": "Retired"}
	if isFree, _ := classifyFree("vendor/model-a", statusRetired); isFree {
		t.Fatal("a retired model was advertised as free")
	}
	if isFree, _ := classifyFree("vendor/model-a-free", map[string]any{"deprecated": "true"}); isFree {
		t.Fatal("a deprecated id-suffixed model was advertised as free")
	}
}
