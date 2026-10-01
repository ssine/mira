package clauderuntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheSettingsSurviveManagerRestart(t *testing.T) {
	if _, err := exec.LookPath(nodeBinary()); err != nil {
		t.Skip("Node.js unavailable")
	}
	root := t.TempDir()
	m := New(root)
	value, err := m.Call(map[string]any{"action": "cache-status"})
	if err != nil || value.(map[string]any)["maxBytes"] != float64(4*1024*1024*1024) {
		t.Fatalf("default: %v %v", value, err)
	}
	_, err = m.Call(map[string]any{"action": "cache-configure", "maxBytes": 0})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	next := New(root)
	defer next.Close()
	value, err = next.Call(map[string]any{"action": "cache-status"})
	if err != nil || value.(map[string]any)["maxBytes"] != float64(0) {
		t.Fatalf("persisted: %v %v", value, err)
	}
}

func TestSessionCacheMatchesWorkerKey(t *testing.T) {
	if _, err := exec.LookPath(nodeBinary()); err != nil {
		t.Skip("Node.js unavailable")
	}
	m := New(t.TempDir())
	defer m.Close()
	session, subpath := "0b8f3f9e-4a5b-4c6d-8e7f-0123456789ab", "subagents/agent-a<b>&.jsonl"
	for _, endpoint := range []string{"https://Mira.Example:443/base/", "http://[::1]:8080", "http://localhost:80"} {
		// Same derivation as session-cache.mjs loadTranscript.
		script := `const { createHash } = await import("node:crypto");
const [endpoint, sessionId, subpath] = process.argv.slice(1);
process.stdout.write(createHash("sha256").update(JSON.stringify([new URL(endpoint).origin, sessionId, subpath])).digest("hex"));`
		out, err := exec.Command(nodeBinary(), "--input-type=module", "-e", script, endpoint, session, subpath).Output()
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(m.cacheRoot, string(out))
		params := map[string]any{"endpoint": endpoint, "sessionId": session, "subpath": subpath}
		if value, err := m.sessionCache(params); err != nil || value.(map[string]any)["state"] != "absent" {
			t.Fatalf("empty cache: %v %v", value, err)
		}
		if err = os.MkdirAll(m.cacheRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		digest := strings.Repeat("a", 64)
		meta := `{"version":1,"bytes":3,"cursor":1,"prefix":"` + digest + `","digest":"` + digest + `"}`
		if err = os.WriteFile(name+".json", []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(name+".jsonl", []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if value, _ := m.sessionCache(params); value.(map[string]any)["state"] != "absent" {
			t.Fatalf("truncated transcript reported as cached for %s: %v", endpoint, value)
		}
		if err = os.WriteFile(name+".jsonl", []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		value, err := m.sessionCache(params)
		if result := value.(map[string]any); err != nil || result["state"] != "cached" || result["cursor"] != int64(1) || result["prefix"] != digest {
			t.Fatalf("cached %s: %v %v", endpoint, value, err)
		}
	}
	if err := os.WriteFile(m.cacheConfig, []byte(`{"maxBytes":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, _ := m.sessionCache(map[string]any{"endpoint": "https://mira.example", "sessionId": session}); value.(map[string]any)["state"] != "disabled" {
		t.Fatalf("disabled cache: %v", value)
	}
}
