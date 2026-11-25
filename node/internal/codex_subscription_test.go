package node

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeCodexAuth(t *testing.T, home, mode string, claims map[string]any, modified time.Time) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"email": "secret@example.test", "https://api.openai.com/auth": claims})
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	data, _ := json.Marshal(map[string]any{"auth_mode": mode, "tokens": map[string]any{"id_token": token, "access_token": "secret-access"}})
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestCodexSubscriptionReportsOnlyTheBillingPeriod(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	claims := map[string]any{"chatgpt_plan_type": "pro", "chatgpt_user_id": "secret-user",
		"chatgpt_subscription_active_start": "2026-11-09T10:17:53+00:00",
		"chatgpt_subscription_active_until": "2026-12-09T10:17:53+00:00",
		"chatgpt_subscription_last_checked": "2026-11-09T18:28:17.279534+00:00"}
	writeCodexAuth(t, home, "chatgpt", claims, now.Add(-time.Hour))
	manager := newAppServerManager(config{AppServerCodexHome: home})
	manager.accountHome = home
	report, err := json.Marshal(manager.report()["subscription"])
	if err != nil {
		t.Fatal(err)
	}
	if string(report) != `{"checkedAt":1794248897279,"endsAt":1796811473000,"startsAt":1794219473000}` || strings.Contains(string(report), "secret") {
		t.Fatalf("subscription report: %s", report)
	}

	// A renewed token is picked up after the recheck interval, not on every heartbeat.
	var cache codexSubscriptionCache
	if cache.read(home, now)["endsAt"] != int64(1796811473000) {
		t.Fatal("initial period missing")
	}
	claims["chatgpt_subscription_active_until"] = "2027-01-09T10:17:53+00:00"
	writeCodexAuth(t, home, "chatgpt", claims, now)
	if cache.read(home, now.Add(time.Second))["endsAt"] != int64(1796811473000) {
		t.Fatal("heartbeat reread the login file")
	}
	renewed := time.Date(2027, 1, 9, 10, 17, 53, 0, time.UTC).UnixMilli()
	if cache.read(home, now.Add(time.Minute))["endsAt"] != renewed {
		t.Fatal("renewed period was not read")
	}
	if err := os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if cache.read(home, now.Add(2*time.Minute)) != nil {
		t.Fatal("logged-out account kept its period")
	}
}

func TestCodexSubscriptionIgnoresOtherLogins(t *testing.T) {
	period := map[string]any{"chatgpt_plan_type": "pro", "chatgpt_subscription_active_until": "2026-12-09T10:17:53Z"}
	for name, setup := range map[string]struct {
		mode   string
		claims map[string]any
	}{
		"api key":   {"apikey", period},
		"free plan": {"chatgpt", map[string]any{"chatgpt_plan_type": "free", "chatgpt_subscription_active_until": "2026-12-09T10:17:53Z"}},
		"no period": {"chatgpt", map[string]any{"chatgpt_plan_type": "pro", "chatgpt_subscription_active_until": nil}},
		"malformed": {"chatgpt", map[string]any{"chatgpt_plan_type": "pro", "chatgpt_subscription_active_until": "tomorrow"}},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeCodexAuth(t, home, setup.mode, setup.claims, time.Now())
			if view := readCodexSubscription(filepath.Join(home, "auth.json")); view != nil {
				t.Fatalf("unexpected period: %v", view)
			}
		})
	}
	home := t.TempDir()
	writeCodexAuth(t, home, "", period, time.Now())
	if readCodexSubscription(filepath.Join(home, "auth.json")) == nil {
		t.Fatal("older login files without auth_mode must still report")
	}
}
