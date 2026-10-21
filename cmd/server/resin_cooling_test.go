package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func resinTestConfig() proxyEngineConfig {
	return proxyEngineConfig{
		Engine:          proxyEngineResin,
		ResinGatewayURL: "http://resin.example.test:2260",
		ResinPlatform:   "Default",
		ResinProxyToken: "proxy-token",
	}
}

func TestResinRouteSkipsAccountParkedAfter429(t *testing.T) {
	a := testApp(t)
	cfg := resinTestConfig()
	route, err := newResinRequestRoute("session-1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstAccount := resinAccountOf(first)
	if firstAccount == "" {
		t.Fatalf("Resin account is empty: %+v", first)
	}

	a.coolResinAccount(firstAccount, time.Minute)

	second, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if account := resinAccountOf(second); account == firstAccount {
		t.Fatalf("retry reused the parked account %q", account)
	}
	if _, cooling := a.resinAccountCooling(firstAccount); !cooling {
		t.Fatal("the parked account was released by the probe")
	}
}

func TestResinRouteReturnsToStickyAccountOnceCooldownLapses(t *testing.T) {
	a := testApp(t)
	cfg := resinTestConfig()
	route, err := newResinRequestRoute("session-2")
	if err != nil {
		t.Fatal(err)
	}
	first, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstAccount := resinAccountOf(first)

	a.coolResinAccount(firstAccount, time.Minute)
	// Expire the park directly: coolResinAccount clamps to the floor, so a
	// short cooldown would still be live.
	a.resinCoolMu.Lock()
	a.resinCooledAccounts[firstAccount] = time.Now().Add(-time.Second)
	a.resinCoolMu.Unlock()
	if _, cooling := a.resinAccountCooling(firstAccount); cooling {
		t.Fatal("an expired park still reported cooling")
	}
	// A fresh route proves the sticky identity itself was never mutated.
	again, err := newResinRequestRoute("session-2")
	if err != nil {
		t.Fatal(err)
	}
	picked, err := again.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if account := resinAccountOf(picked); account != firstAccount {
		t.Fatalf("sticky account changed after the cooldown lapsed: %q vs %q", account, firstAccount)
	}
}

func TestResinRouteRerollsTheExitOnAdvance(t *testing.T) {
	a := testApp(t)
	cfg := resinTestConfig()
	route, err := newResinRequestRoute("")
	if err != nil {
		t.Fatal(err)
	}
	first, err := route.proxy(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct{}{resinAccountOf(first): {}}
	for attempt := 0; attempt < 4; attempt++ {
		if err := route.advance(a); err != nil {
			t.Fatal(err)
		}
		next, err := route.proxy(a, cfg)
		if err != nil {
			t.Fatal(err)
		}
		account := resinAccountOf(next)
		if _, repeat := seen[account]; repeat {
			t.Fatalf("advance redrew account %q on attempt %d", account, attempt+1)
		}
		seen[account] = struct{}{}
	}
}

func TestCoolResinAccountClampsTheParkTime(t *testing.T) {
	a := testApp(t)
	a.coolResinAccount("short", 0)
	remaining, cooling := a.resinAccountCooling("short")
	if !cooling || remaining < resinAccountCooldownFloor-2*time.Second {
		t.Fatalf("floor not applied: remaining=%s cooling=%v", remaining, cooling)
	}

	a.coolResinAccount("long", 24*time.Hour)
	remaining, cooling = a.resinAccountCooling("long")
	if !cooling || remaining > resinAccountCooldownCeil {
		t.Fatalf("ceiling not applied: remaining=%s cooling=%v", remaining, cooling)
	}

	a.releaseResinAccount("long")
	if _, cooling := a.resinAccountCooling("long"); cooling {
		t.Fatal("release left the account parked")
	}
}

func TestCooledResinAccountTableStaysBounded(t *testing.T) {
	a := testApp(t)
	for i := 0; i < resinAccountCoolLimit+64; i++ {
		a.coolResinAccount("account-"+strconv.Itoa(i), time.Minute)
	}
	if got := a.cooledResinAccounts(); got > resinAccountCoolLimit {
		t.Fatalf("cooled table grew to %d entries", got)
	}
}

func TestResinAccountCooldownForReadsRetryAfter(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Retry-After", "30")
	if got := resinAccountCooldownFor(resp); got != 30*time.Second {
		t.Fatalf("Retry-After not honoured: %s", got)
	}
	if got := resinAccountCooldownFor(&http.Response{Header: http.Header{}}); got != 0 {
		t.Fatalf("missing Retry-After produced %s, want 0 so the floor applies", got)
	}
}
