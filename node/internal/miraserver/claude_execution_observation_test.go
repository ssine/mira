package miraserver

import (
	"testing"
	"time"
)

func TestClaudeExecutionObservationToleratesOneSlowProbe(t *testing.T) {
	server := &Server{}
	target := claudeExecutionTarget{session: "s", turn: "t", node: "n", revision: 3}
	observed := func() claudeExecutionObservation {
		server.claudeExecutionMu.Lock()
		defer server.claudeExecutionMu.Unlock()
		return server.claudeExecution[target.session]
	}
	server.rememberClaudeExecution(target, "running", "")
	server.rememberClaudeExecution(target, "unknown", "connection")
	if got := observed(); got.state != "running" {
		t.Fatalf("a slow probe replaced a fresh confirmation: %#v", got)
	}
	// Evidence about the execution itself, another turn or a stale confirmation still wins.
	for _, change := range []func(){
		func() { server.rememberClaudeExecution(target, "unknown", "runtime") },
		func() {
			next := target
			next.turn = "u"
			server.rememberClaudeExecution(next, "unknown", "connection")
		},
		func() {
			server.claudeExecutionMu.Lock()
			stale := server.claudeExecution[target.session]
			stale.checked = time.Now().Add(-21 * time.Second)
			server.claudeExecution[target.session] = stale
			server.claudeExecutionMu.Unlock()
			server.rememberClaudeExecution(target, "unknown", "connection")
		},
	} {
		server.rememberClaudeExecution(target, "running", "")
		change()
		if got := observed(); got.state != "unknown" {
			t.Fatalf("observation kept running: %#v", got)
		}
	}
}
