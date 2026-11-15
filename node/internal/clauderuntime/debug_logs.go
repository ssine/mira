package clauderuntime

import (
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Claude CLI debug logs are local diagnostics for authentication and runtime
// failures that leave no trace in the transcript (the CLI records its OAuth
// refresh and 401 recovery decisions only there). The CLI redacts tokens; the
// logs stay on this Node, are never uploaded, and keep a bounded number of
// recent files per kind for at most three days.
const debugLogMaxAge = 72 * time.Hour

var debugLogKeep = map[string]int{"turn": 40, "usage": 24}

// debugLogEnv returns the environment entry naming a new debug log for one
// CLI process, after pruning older logs of the same kind. It returns nothing
// when the directory cannot be prepared: logging never blocks a turn.
func (m *Manager) debugLogEnv(kind, accountID, name string) []string {
	if !accountIDPattern.MatchString(accountID) || !runtimeIDPattern.MatchString(name) {
		return nil
	}
	dir := filepath.Join(filepath.Dir(filepath.Dir(m.root)), "logs", "claude", accountID, kind)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil
	}
	pruneDebugLogs(dir, debugLogKeep[kind]-1, time.Now())
	return []string{"MIRA_NODE_CLAUDE_DEBUG_FILE=" + filepath.Join(dir, time.Now().UTC().Format("20060102T150405Z")+"-"+name+".log")}
}

func pruneDebugLogs(dir string, keep int, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		path string
		at   time.Time
	}
	files := []file{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		files = append(files, file{filepath.Join(dir, entry.Name()), info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	for index, item := range files {
		if index >= keep || now.Sub(item.at) > debugLogMaxAge {
			_ = os.Remove(item.path)
		}
	}
}
