package clauderuntime

import (
	"bufio"
	"encoding/json"
	"io"
	"testing"
)

// A fake worker answers each steer command on stdout, in its own write chunks.
func steerProcess(t *testing.T, m *Manager, reply func(command map[string]any) string) *process {
	t.Helper()
	reader, writer := io.Pipe()
	p := &process{input: writer, done: make(chan struct{})}
	m.processes["turn"] = p
	// The fake has no OS process for Manager.Close to stop.
	t.Cleanup(func() { _ = writer.Close() })
	go func() {
		lines := bufio.NewScanner(reader)
		for lines.Scan() {
			var command map[string]any
			if json.Unmarshal(lines.Bytes(), &command) != nil {
				continue
			}
			line := reply(command)
			for len(line) > 3 {
				_, _ = p.acks.Write([]byte(line[:3]))
				line = line[3:]
			}
			_, _ = p.acks.Write([]byte(line))
		}
	}()
	return p
}

func TestSteerWaitsForTheWorkerVerdict(t *testing.T) {
	m := New(t.TempDir())
	steerProcess(t, m, func(command map[string]any) string {
		if command["action"] != "steer" || command["text"] != "also check the tests" {
			return "{}\n"
		}
		return "not json\n" + `{"steerId":"other","accepted":true}` + "\n" + `{"steerId":"` + command["steerId"].(string) + `","accepted":true}` + "\n"
	})
	result, err := m.Call(map[string]any{"action": "steer", "turnId": "turn", "steerId": "one", "text": "also check the tests"})
	if err != nil || result.(map[string]any)["accepted"] != true {
		t.Fatalf("steer: %v %v", result, err)
	}
	acks := &m.processes["turn"].acks
	acks.mu.Lock()
	defer acks.mu.Unlock()
	if len(acks.waiters) != 0 {
		t.Fatalf("acknowledged steers must not leave waiters: %v", acks.waiters)
	}
}

func TestSteerRejectsFinishedTurns(t *testing.T) {
	m := New(t.TempDir())
	result, err := m.Call(map[string]any{"action": "steer", "turnId": "missing", "steerId": "one"})
	if err != nil || result.(map[string]any)["accepted"] != false || result.(map[string]any)["reason"] != "turn_finishing" {
		t.Fatalf("missing turn: %v %v", result, err)
	}
	// A worker that exits without answering cannot have queued the message.
	p := steerProcess(t, m, func(map[string]any) string { return "" })
	go func() {
		for {
			p.acks.mu.Lock()
			waiting := len(p.acks.waiters["two"])
			p.acks.mu.Unlock()
			if waiting > 0 {
				close(p.done)
				return
			}
		}
	}()
	result, err = m.Call(map[string]any{"action": "steer", "turnId": "turn", "steerId": "two"})
	if err != nil || result.(map[string]any)["accepted"] != false {
		t.Fatalf("exited worker: %v %v", result, err)
	}
	if _, err = m.Call(map[string]any{"action": "steer", "turnId": "turn"}); err == nil {
		t.Fatal("steerId is required")
	}
}
