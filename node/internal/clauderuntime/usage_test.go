package clauderuntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCleanPlanUsage(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(`{
	  "subscription_type": "max",
	  "rate_limits_available": true,
	  "rate_limits": {
	    "five_hour": {"utilization": 12.5, "resets_at": "2026-11-13T19:50:00.193543+00:00", "used_dollars": null},
	    "seven_day": {"utilization": 4, "resets_at": "2026-11-18T05:00:00.193569+00:00"},
	    "seven_day_opus": null,
	    "seven_day_sonnet": {"utilization": null, "resets_at": null},
	    "model_scoped": [
	      {"display_name": "Fable", "utilization": 0, "resets_at": "2026-11-18T05:00:00Z"},
	      {"display_name": "bad\nname", "utilization": 3, "resets_at": null},
	      {"display_name": "Huge", "utilization": "7"}
	    ],
	    "extra_usage": {"is_enabled": false, "monthly_limit": null, "used_credits": null, "utilization": null, "currency": "USD"},
	    "spend": {"disclaimer": "dropped"}
	  },
	  "session": {"total_cost_usd": 3}
	}`), &raw); err != nil {
		t.Fatal(err)
	}
	usage := CleanPlanUsage(raw)
	if usage.SubscriptionType != "max" || !usage.Available {
		t.Fatalf("plan: %+v", usage)
	}
	if usage.FiveHour == nil || *usage.FiveHour.Utilization != 12.5 || *usage.FiveHour.ResetsAt != 1794599400193 {
		t.Fatalf("five hour: %+v", usage.FiveHour)
	}
	if usage.SevenDay == nil || *usage.SevenDay.Utilization != 4 {
		t.Fatalf("seven day: %+v", usage.SevenDay)
	}
	if usage.SevenDayOpus != nil || usage.SevenDaySonnet != nil {
		t.Fatalf("empty windows must be omitted: %+v %+v", usage.SevenDayOpus, usage.SevenDaySonnet)
	}
	if len(usage.Models) != 1 || usage.Models[0].Name != "Fable" || *usage.Models[0].Utilization != 0 {
		t.Fatalf("models: %+v", usage.Models)
	}
	if usage.Extra == nil || usage.Extra.Enabled || usage.Extra.Currency != "USD" || usage.Extra.MonthlyLimit != nil {
		t.Fatalf("extra: %+v", usage.Extra)
	}
	encoded, _ := json.Marshal(usage)
	var view map[string]any
	_ = json.Unmarshal(encoded, &view)
	if _, leaked := view["session"]; leaked {
		t.Fatalf("session usage leaked: %s", encoded)
	}

	unavailable := CleanPlanUsage(map[string]any{"subscription_type": nil, "rate_limits_available": false, "rate_limits": nil})
	if unavailable.Available || unavailable.SevenDay != nil || unavailable.SubscriptionType != "" {
		t.Fatalf("unavailable: %+v", unavailable)
	}
}

func TestReadProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	read := func(content string) (*int64, string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return readProfile(path)
	}
	if at, key := readProfile(path); at != nil || key != "" {
		t.Fatal("missing profile")
	}
	for _, content := range []string{`{`, `{}`, `{"oauthAccount":null}`, `{"oauthAccount":{"subscriptionCreatedAt":null,"accountUuid":null}}`, `{"oauthAccount":{"subscriptionCreatedAt":5,"accountUuid":7,"organizationUuid":{}}}`, `{"oauthAccount":{"subscriptionCreatedAt":"soon","accountUuid":"person"}}`} {
		if at, key := read(content); at != nil || key != "" {
			t.Fatalf("%s: %v %q", content, at, key)
		}
	}
	const account, organization = "AAAAAAAA-0000-4000-8000-000000000001", "bbbbbbbb-0000-4000-8000-000000000002"
	profile := func(account, organization string) string {
		return `{"projects":{},"oauthAccount":{"emailAddress":"x@example.test","accountUuid":"` + account + `","organizationUuid":"` + organization + `","subscriptionCreatedAt":"2026-11-08T11:16:40.478986Z"}}`
	}
	at, key := read(profile(account, organization))
	if at == nil || *at != 1794136600478 || len(key) != 32 || strings.Contains(key, "aaaaaaaa") {
		t.Fatalf("profile: %v %q", at, key)
	}
	if _, same := read(profile(strings.ToLower(account), organization)); same != key {
		t.Fatal("the same account must have one key")
	}
	if _, other := read(profile(account, account)); other == key {
		t.Fatal("another organization must have another key")
	}
}

func TestUsageStartsTheStoppedRuntime(t *testing.T) {
	m := New(t.TempDir())
	defer m.Close()
	dir := filepath.Join(m.root, assetDigest())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// A prepared SDK needs no download, only the Node.js version check.
	fake := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		t.Setenv("MIRA_NODE_CLAUDE_NODE", fake)
	}
	if got, err := m.usageRuntime(10 * time.Second); err != nil || got != dir {
		t.Fatalf("stopped runtime: %q %v", got, err)
	}
	m.mu.Lock()
	m.state, m.lastError, m.prepareAt = "error", "npm failed", time.Now()
	m.mu.Unlock()
	if _, err := m.usageRuntime(time.Second); err == nil || !strings.Contains(err.Error(), "npm failed") {
		t.Fatalf("recent failure must not retry: %v", err)
	}
	m.mu.Lock()
	m.prepareAt = time.Now().Add(-usagePrepareRetry)
	m.mu.Unlock()
	if got, err := m.usageRuntime(10 * time.Second); err != nil || got != dir {
		t.Fatalf("retry: %q %v", got, err)
	}
}
