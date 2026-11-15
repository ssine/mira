package clauderuntime

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

const keeperAccount = "00000000-0000-4000-8000-00000000c0de"

func keeperFixture(t *testing.T, expiresIn time.Duration, handler http.HandlerFunc) (*Manager, string, *atomic.Int32) {
	t.Helper()
	m := New(t.TempDir())
	home, _ := m.accountHome(keeperAccount)
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	document := map[string]any{
		"claudeAiOauth": map[string]any{"accessToken": "at-old", "refreshToken": "rt-old", "expiresAt": time.Now().Add(expiresIn).UnixMilli(),
			"scopes": []string{"user:inference", "user:profile"}, "subscriptionType": "max", "rateLimitTier": "tier"},
		"mcpOAuth": map[string]any{"server": map[string]any{"accessToken": "keep-me"}},
	}
	data, _ := json.Marshal(document)
	if err := os.WriteFile(filepath.Join(home, ".credentials.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	m.keeper.tokenURL = server.URL + "/v1/oauth/token"
	m.keeper.client = func(func(*http.Request) (*url.URL, error)) *http.Client { return server.Client() }
	t.Cleanup(m.Close)
	return m, home, calls
}

func readStored(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestKeepLoginRenewsAheadOfTheCLI(t *testing.T) {
	var body map[string]any
	m, home, calls := keeperFixture(t, 30*time.Minute, func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":28800,"scope":"user:inference user:profile"}`))
	})
	before := time.Now()
	outcome, err := m.KeepLogin(keeperAccount)
	if err != nil || outcome != "renewed" || calls.Load() != 1 {
		t.Fatalf("renewal: %q %v calls=%d", outcome, err, calls.Load())
	}
	if body["grant_type"] != "refresh_token" || body["refresh_token"] != "rt-old" || body["client_id"] != claudeOAuthClientID || body["scope"] != "user:inference user:profile" {
		t.Fatalf("refresh request: %#v", body)
	}
	stored := readStored(t, home)
	oauth := stored["claudeAiOauth"].(map[string]any)
	expires := time.UnixMilli(int64(oauth["expiresAt"].(float64)))
	if oauth["accessToken"] != "at-new" || oauth["refreshToken"] != "rt-new" || oauth["subscriptionType"] != "max" || oauth["rateLimitTier"] != "tier" ||
		expires.Before(before.Add(8*time.Hour-time.Minute)) {
		t.Fatalf("stored login: %#v", oauth)
	}
	if stored["mcpOAuth"].(map[string]any)["server"].(map[string]any)["accessToken"] != "keep-me" {
		t.Fatalf("other credentials were not preserved: %#v", stored)
	}
	if info, _ := os.Stat(filepath.Join(home, ".credentials.json")); runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
	for _, lock := range []string{filepath.Join(home, ".oauth_refresh.lock"), home + ".lock"} {
		if _, err := os.Stat(lock); !os.IsNotExist(err) {
			t.Fatalf("lock %s left behind: %v", lock, err)
		}
	}
	// Renewed: nothing more to do until the next lead window.
	if outcome, _ := m.KeepLogin(keeperAccount); outcome != "not_needed" || calls.Load() != 1 {
		t.Fatalf("second pass: %q calls=%d", outcome, calls.Load())
	}
}

func TestKeepLoginLeavesFreshAndMissingLoginsAlone(t *testing.T) {
	m, home, calls := keeperFixture(t, 3*time.Hour, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected refresh") })
	if outcome, err := m.KeepLogin(keeperAccount); outcome != "not_needed" || err != nil {
		t.Fatalf("fresh: %q %v", outcome, err)
	}
	_ = os.Remove(filepath.Join(home, ".credentials.json"))
	if outcome, err := m.KeepLogin(keeperAccount); outcome != "no_login" || err != nil || calls.Load() != 0 {
		t.Fatalf("missing: %q %v", outcome, err)
	}
}

func TestKeepLoginKeepsTheFileWhenTheProviderRefuses(t *testing.T) {
	m, home, _ := keeperFixture(t, 10*time.Minute, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	})
	original, _ := os.ReadFile(filepath.Join(home, ".credentials.json"))
	if _, err := m.KeepLogin(keeperAccount); err == nil {
		t.Fatal("a refused refresh must report an error")
	}
	if current, _ := os.ReadFile(filepath.Join(home, ".credentials.json")); string(current) != string(original) {
		t.Fatal("a refused refresh must not touch the stored login")
	}
}

func TestKeepLoginRespectsTheCLIRefreshLock(t *testing.T) {
	previousAttempts, previousWait := lockAttempts, lockWait
	lockAttempts, lockWait = 2, 10*time.Millisecond
	t.Cleanup(func() { lockAttempts, lockWait = previousAttempts, previousWait })
	m, home, calls := keeperFixture(t, 10*time.Minute, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":28800}`))
	})
	lock := filepath.Join(home, ".oauth_refresh.lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	// A live CLI holds the lock: the keeper waits for its next pass.
	if outcome, err := m.KeepLogin(keeperAccount); outcome != "lock_busy" || err != nil || calls.Load() != 0 {
		t.Fatalf("held lock: %q %v", outcome, err)
	}
	// A lock abandoned past the CLI's stale threshold is taken over.
	old := time.Now().Add(-2 * refreshLockStale)
	_ = os.Chtimes(lock, old, old)
	if outcome, err := m.KeepLogin(keeperAccount); outcome != "renewed" || err != nil {
		t.Fatalf("stale lock: %q %v", outcome, err)
	}
}

func TestKeepLoginSeesARenewalUnderTheLock(t *testing.T) {
	m, home, calls := keeperFixture(t, 10*time.Minute, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected refresh") })
	// Another process renews between the keeper's first read and its lock.
	stored := readStored(t, home)
	oauth := stored["claudeAiOauth"].(map[string]any)
	oauth["refreshToken"], oauth["expiresAt"] = "rt-other", time.Now().Add(8*time.Hour).UnixMilli()
	data, _ := json.Marshal(stored)
	first, _ := readOAuthStore(filepath.Join(home, ".credentials.json"))
	_ = os.WriteFile(filepath.Join(home, ".credentials.json"), data, 0600)
	locked, _ := readOAuthStore(filepath.Join(home, ".credentials.json"))
	if first.refreshToken() == locked.refreshToken() || m.keeper.due(locked) {
		t.Fatal("fixture did not simulate a renewal")
	}
	if outcome, err := m.KeepLogin(keeperAccount); outcome != "not_needed" || err != nil || calls.Load() != 0 {
		t.Fatalf("renewed elsewhere: %q %v", outcome, err)
	}
}

func TestDebugLogsAreBounded(t *testing.T) {
	m := New(t.TempDir())
	previous := debugLogKeep["turn"]
	debugLogKeep["turn"] = 3
	t.Cleanup(func() { debugLogKeep["turn"] = previous })
	var dir string
	for index := range 5 {
		env := m.debugLogEnv("turn", keeperAccount, "turn"+string(rune('a'+index)))
		if len(env) != 1 {
			t.Fatalf("env: %v", env)
		}
		path := env[0][len("MIRA_NODE_CLAUDE_DEBUG_FILE="):]
		dir = filepath.Dir(path)
		if err := os.WriteFile(path, []byte("log"), 0600); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(time.Duration(index-10) * time.Minute)
		_ = os.Chtimes(path, at, at)
	}
	old := filepath.Join(dir, "old.log")
	_ = os.WriteFile(old, []byte("log"), 0600)
	stale := time.Now().Add(-4 * 24 * time.Hour)
	_ = os.Chtimes(old, stale, stale)
	m.debugLogEnv("turn", keeperAccount, "next")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("kept %d logs, want the 2 newest before the next one", len(entries))
	}
	if m.debugLogEnv("turn", "../escape", "x") != nil || m.debugLogEnv("turn", keeperAccount, "../x") != nil {
		t.Fatal("unsafe names must not produce a log path")
	}
}
