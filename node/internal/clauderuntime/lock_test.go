//go:build !windows

package clauderuntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slowNode replaces Node.js with a child that stays alive past the Server's
// three-second reconciliation budget, then fails.
func slowNode(t *testing.T) chan struct{} {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "started")
	script := filepath.Join(dir, "node")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\nsleep 4\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIRA_NODE_CLAUDE_NODE", script)
	started := make(chan struct{})
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				close(started)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return started
}

func readyManager(t *testing.T) *Manager {
	m := New(t.TempDir())
	m.state, m.dir = "ready", t.TempDir()
	return m
}

func probeWithin(t *testing.T, m *Manager, turn string, limit time.Duration) map[string]any {
	t.Helper()
	begin := time.Now()
	state := executionProbe(t, m, m.runtimeID, turn)
	if elapsed := time.Since(begin); elapsed > limit {
		t.Fatalf("reconciliation waited %v behind a child process", elapsed)
	}
	return state
}

func TestReconcileDoesNotWaitForModelDescription(t *testing.T) {
	started := slowNode(t)
	m := readyManager(t)
	done := make(chan error, 1)
	go func() { _, err := m.Call(map[string]any{"action": "describe"}); done <- err }()
	<-started
	probeWithin(t, m, executionTestTurn, time.Second)
	if err := <-done; err == nil {
		t.Fatal("failed description succeeded")
	}
}

func TestReconcileFencesAStartDuringItsPreparation(t *testing.T) {
	started := slowNode(t)
	m := readyManager(t)
	done := make(chan error, 1)
	go func() {
		_, err := m.Call(map[string]any{"action": "start", "runtimeId": m.runtimeID, "turnId": executionTestTurn})
		done <- err
	}()
	<-started // cache maintenance runs before the worker exists
	if state := probeWithin(t, m, executionTestTurn, time.Second); state["state"] != "stopped" || state["proof"] != "turn_fenced" {
		t.Fatal(state)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("fenced start: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.processes) != 0 || m.accepted[executionTestTurn] || len(m.starting) != 0 {
		t.Fatalf("fenced start left state: %v %v %v", m.processes, m.accepted, m.starting)
	}
}

func TestAccountChangesWaitForAStartingTurn(t *testing.T) {
	m := New(t.TempDir())
	const account = "22222222-2222-4222-8222-222222222222"
	m.starting[account] = 1
	_, err := m.Call(map[string]any{"action": "account/configure", "nodeAccountId": account, "provider": map[string]any{"id": "anthropic"}})
	if err == nil || !strings.Contains(err.Error(), "active turn") {
		t.Fatalf("configure during start: %v", err)
	}
}
