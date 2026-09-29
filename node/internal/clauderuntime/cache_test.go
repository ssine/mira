package clauderuntime

import (
	"os/exec"
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
