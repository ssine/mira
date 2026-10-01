package clauderuntime

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const executionTestTurn = "11111111-1111-4111-8111-111111111111"

func executionProbe(t *testing.T, m *Manager, runtime, turn string) map[string]any {
	t.Helper()
	value, err := m.Call(map[string]any{"action": "reconcile", "runtimeId": runtime, "turnId": turn})
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)
}

func TestExecutionFencePreventsDelayedStart(t *testing.T) {
	m := New(t.TempDir())
	if state := executionProbe(t, m, m.runtimeID, executionTestTurn); state["state"] != "stopped" || state["proof"] != "turn_fenced" {
		t.Fatal(state)
	}
	_, err := m.Call(map[string]any{"action": "start", "runtimeId": m.runtimeID, "turnId": executionTestTurn})
	if err == nil || !strings.Contains(err.Error(), "fenced") {
		t.Fatalf("delayed start: %v", err)
	}
	// A live worker must never be fenced merely because it has no new output.
	m.processes[executionTestTurn] = &process{}
	delete(m.sealed, executionTestTurn)
	if state := executionProbe(t, m, m.runtimeID, executionTestTurn); state["state"] != "running" || m.sealed[executionTestTurn] {
		t.Fatal(state)
	}
}

func TestExecutionReceiptRequiresConfirmedShutdown(t *testing.T) {
	dir := t.TempDir()
	old := New(dir)
	fresh := New(dir)
	if state := executionProbe(t, fresh, old.runtimeID, executionTestTurn); state["state"] != "unknown" {
		t.Fatal(state)
	}
	old.Close()
	if state := executionProbe(t, fresh, old.runtimeID, executionTestTurn); state["state"] != "stopped" || state["proof"] != "runtime_closed" {
		t.Fatal(state)
	}
	uncertain := New(dir)
	uncertain.unconfirmedExit = true
	uncertain.Close()
	if state := executionProbe(t, fresh, uncertain.runtimeID, executionTestTurn); state["state"] != "unknown" {
		t.Fatal(state)
	}
	for _, runtime := range []string{"../escape", "", strings.Repeat("x", 129)} {
		if _, err := fresh.Call(map[string]any{"action": "reconcile", "runtimeId": runtime, "turnId": executionTestTurn}); err == nil {
			t.Fatal("invalid runtime accepted")
		}
	}
}

func TestExecutionEvidenceBoundsRemainConservative(t *testing.T) {
	m := New(t.TempDir())
	for i := 0; i < 4096; i++ {
		m.sealed[fmt.Sprint(i)] = true
	}
	if state := executionProbe(t, m, m.runtimeID, executionTestTurn); state["state"] != "unknown" {
		t.Fatal(state)
	}
	for i := 0; i < 131; i++ {
		m.runtimeID = fmt.Sprintf("runtime-%03d", i)
		if err := m.recordClosedRuntime(); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.root, "closed-runtimes"))
	if err != nil || len(entries) != 128 {
		t.Fatalf("receipt retention: %d %v", len(entries), err)
	}
}

func TestExecutionShutdownReapsUnresponsiveWorker(t *testing.T) {
	if _, err := exec.LookPath(nodeBinary()); err != nil {
		t.Skip("Node.js unavailable")
	}
	dir := t.TempDir()
	m := New(dir)
	m.dir = t.TempDir()
	m.state = "ready"
	// A worker that reads input but never honors interruption must be killed and
	// reaped before a new manager can use its stopped-runtime receipt.
	if err := os.WriteFile(filepath.Join(m.dir, "worker.mjs"), []byte("process.stdin.resume(); setInterval(()=>{},1000);"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := m.Call(map[string]any{"action": "start", "runtimeId": m.runtimeID, "turnId": executionTestTurn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	if state := executionProbe(t, m, m.runtimeID, executionTestTurn); state["state"] != "running" {
		t.Fatal(state)
	}
	start := time.Now()
	m.Close()
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("shutdown took %v", elapsed)
	}
	if len(m.processes) != 0 {
		t.Fatal("receipt created before reaping worker")
	}
	fresh := New(dir)
	if state := executionProbe(t, fresh, m.runtimeID, executionTestTurn); state["state"] != "stopped" {
		t.Fatal(state)
	}
}

func TestExecutionShutdownDoesNotTreatKillRequestAsReaped(t *testing.T) {
	dir := t.TempDir()
	m := New(dir)
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	var stopped sync.Once
	killed := make(chan struct{})
	p := &process{input: writer, done: make(chan struct{}), stop: func() bool { stopped.Do(func() { close(killed) }); return true }}
	m.processes[executionTestTurn] = p
	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	select {
	case <-killed:
	case <-time.After(7 * time.Second):
		t.Fatal("blocked stdin prevented shutdown")
	}
	if state := executionProbe(t, New(dir), m.runtimeID, executionTestTurn); state["state"] != "unknown" {
		t.Fatal("kill alone produced evidence", state)
	}
	// Simulate a worker whose Wait never acknowledged. Close must finish within
	// its deadline and leave no receipt for the replacement manager.
	select {
	case <-closed:
	case <-time.After(4 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	if state := executionProbe(t, New(dir), m.runtimeID, executionTestTurn); state["state"] != "unknown" {
		t.Fatal("unreaped worker produced evidence", state)
	}
}
