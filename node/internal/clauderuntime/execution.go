package clauderuntime

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

var runtimeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// reconcile runs under the same lock as start. An absent worker is fenced before
// reporting it stopped, so a delayed start cannot race the Server's finalization.
func (m *Manager) reconcile(params map[string]any) (any, error) {
	runtimeID, _ := params["runtimeId"].(string)
	turnID, _ := params["turnId"].(string)
	if !runtimeIDPattern.MatchString(runtimeID) || !accountIDPattern.MatchString(turnID) {
		return nil, errors.New("runtimeId and turnId are required for Claude reconciliation")
	}
	result := map[string]any{"runtimeId": m.runtimeID, "expectedRuntimeId": runtimeID, "turnId": turnID, "state": "unknown"}
	if runtimeID != m.runtimeID {
		// These small local receipts are process-lifecycle evidence, never history.
		// Missing receipts (old releases, crashes, or retention) remain unknown.
		file, err := os.Open(filepath.Join(m.root, "closed-runtimes", runtimeID))
		if err != nil {
			return result, nil
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 9))
		if err == nil && string(data) == "stopped\n" {
			result["state"], result["proof"] = "stopped", "runtime_closed"
		}
		return result, nil
	}
	if m.processes[turnID] != nil {
		result["state"] = "running"
		return result, nil
	}
	if m.unconfirmedExit {
		return result, nil
	}
	if !m.sealed[turnID] && len(m.sealed) >= 4096 {
		return result, nil // Never evict a fence while this runtime can accept starts.
	}
	m.sealed[turnID] = true
	result["state"], result["proof"] = "stopped", "turn_fenced"
	return result, nil
}

func (m *Manager) recordClosedRuntime() error {
	dir := filepath.Join(m.root, "closed-runtimes")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".closing-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString("stopped\n"); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, m.runtimeID)); err != nil {
		return err
	}
	// Bound bookkeeping. An expired receipt causes uncertainty, never an inferred
	// success. No session text, credentials or native transcript is stored here.
	entries, _ := os.ReadDir(dir)
	type receipt struct {
		name     string
		modified int64
	}
	var receipts []receipt
	for _, entry := range entries {
		if !entry.IsDir() && runtimeIDPattern.MatchString(entry.Name()) {
			if info, err := entry.Info(); err == nil {
				receipts = append(receipts, receipt{entry.Name(), info.ModTime().UnixNano()})
			}
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].modified > receipts[j].modified })
	for _, item := range receipts[min(128, len(receipts)):] {
		_ = os.Remove(filepath.Join(dir, item.name))
	}
	return nil
}
