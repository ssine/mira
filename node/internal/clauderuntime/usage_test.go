package clauderuntime

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestPlanUsageDefersNearExpiryWhileATurnRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".credentials.json")
	now := time.Date(2026, 11, 15, 12, 26, 4, 0, time.UTC)
	write := func(expires time.Time) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{"accessToken": "a", "refreshToken": "r", "expiresAt": expires.UnixMilli()}})
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The incident: the read ran 49 s before expiry and renewed the login.
	write(now.Add(49 * time.Second))
	if !oauthExpiresWithin(path, now, usageRefreshMargin) {
		t.Fatal("a token expiring within the margin must defer the read")
	}
	write(now.Add(2 * time.Hour))
	if oauthExpiresWithin(path, now, usageRefreshMargin) {
		t.Fatal("a fresh token must not defer the read")
	}
	if oauthExpiresWithin(filepath.Join(dir, "missing.json"), now, usageRefreshMargin) {
		t.Fatal("unreadable credentials must not defer")
	}

	m := New(t.TempDir())
	const account = "00000000-0000-4000-8000-0000000000c1"
	if m.accountTurnActive(account) {
		t.Fatal("no turn runs yet")
	}
	m.processes["turn"] = &process{accountID: account}
	if !m.accountTurnActive(account) || m.accountTurnActive("00000000-0000-4000-8000-0000000000c2") {
		t.Fatal("only the running turn's account is active")
	}
	delete(m.processes, "turn")
	m.starting[account] = 1
	if !m.accountTurnActive(account) {
		t.Fatal("a starting turn counts as running")
	}
}
