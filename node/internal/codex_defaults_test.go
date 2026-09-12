package node

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAppServerSubagentDefaultAndOverrideOrder(t *testing.T) {
	for _, key := range []string{"agents.max_threads", "agents.max_concurrent_threads_per_session"} {
		t.Run(key, func(t *testing.T) {
			manager := newAppServerManager(config{ConfigOverrides: []string{key + "=200"}})
			desired := desiredAppServer{ConfigOverrides: []string{key + "=300"}}
			got := manager.effectiveDesired(desired).ConfigOverrides
			want := []string{"agents.max_threads=1000", key + "=300", key + "=200"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("launcher/server/local precedence: got %v, want %v", got, want)
			}
			if !reflect.DeepEqual(desired.ConfigOverrides, []string{key + "=300"}) {
				t.Fatal("resolving defaults mutated desired state")
			}
		})
	}
	manager := newAppServerManager(config{})
	if got := manager.effectiveDesired(desiredAppServer{}).ConfigOverrides; !reflect.DeepEqual(got, []string{"agents.max_threads=1000"}) {
		t.Fatalf("unconfigured App Server omitted the subagent default: %v", got)
	}
}

func TestSubagentLimitChangeWaitsForActiveAccount(t *testing.T) {
	if !supportsAppServer() {
		t.Skip("App Server is unsupported")
	}
	binary := filepath.Join(t.TempDir(), "codex")
	installation := codexInstallation{Path: binary, AppServerSupported: true}
	current := &appServerInstance{
		codex: installation, done: make(chan struct{}),
		configOverrides: []string{"agents.max_threads=100"},
	}
	manager := newAppServerManager(config{})
	manager.installations = []codexInstallation{installation}
	manager.instance = current
	manager.activeThreads = map[string]bool{"active-thread": true}
	err := manager.reconcile(context.Background(), desiredAppServer{Running: true, CodexPath: binary})
	if err == nil || !strings.Contains(err.Error(), "配置变更等待任务结束") {
		t.Fatalf("changing the subagent limit must wait for active tasks: %v", err)
	}
	if manager.instance != current || channelClosed(current.done) {
		t.Fatal("configuration change replaced the active account process")
	}
}
