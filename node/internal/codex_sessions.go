package node

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	maxCodexSessionFiles = 2000
	maxCodexSessionChunk = 8 * 1024 * 1024
	defaultSessionChunk  = 1024 * 1024
)

type codexSessionsParams struct {
	Action    string `json:"action"`
	Path      string `json:"path,omitempty"`
	Cursor    int64  `json:"cursor,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	RolloutID string `json:"rolloutId,omitempty"`
}

type codexSessionSummary struct {
	Path           string    `json:"path"`
	CodexHome      string    `json:"codexHome"`
	ThreadID       string    `json:"threadId"`
	SessionID      string    `json:"sessionId,omitempty"`
	ParentThreadID string    `json:"parentThreadId,omitempty"`
	Title          string    `json:"title,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	Source         any       `json:"source,omitempty"`
	Originator     string    `json:"originator,omitempty"`
	ClientKind     string    `json:"clientKind"`
	HistoryMode    string    `json:"historyMode,omitempty"`
	HistoryBase    any       `json:"historyBase,omitempty"`
	Archived       bool      `json:"archived"`
	ForkedFromID   string    `json:"forkedFromId,omitempty"`
	CodexVersion   string    `json:"codexVersion,omitempty"`
	StartedAt      string    `json:"startedAt,omitempty"`
	ModifiedAt     time.Time `json:"modifiedAt"`
	SizeBytes      int64     `json:"sizeBytes"`
}

func (runtimeValue *capabilityRuntime) codexSessionHomes() ([]string, error) {
	if runtime.GOOS == "android" {
		return nil, fmt.Errorf("Codex sessions are not available on Android")
	}
	candidates := []string{runtimeValue.configuration.AppServerCodexHome, os.Getenv("CODEX_HOME")}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".codex"))
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" || !filepath.IsAbs(candidate) {
			continue
		}
		cleaned := filepath.Clean(candidate)
		resolved, resolveErr := filepath.EvalSymlinks(cleaned)
		if resolveErr != nil {
			if os.IsNotExist(resolveErr) {
				continue
			}
			return nil, resolveErr
		}
		if !seen[resolved] {
			seen[resolved] = true
			result = append(result, resolved)
		}
	}
	return result, nil
}

func textFromContent(value any) string {
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := item["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func compactSessionTitle(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len([]rune(value)) > 160 {
		value = string([]rune(value)[:157]) + "…"
	}
	return value
}

func isSessionTitleCandidate(value string) bool {
	trimmed := strings.TrimSpace(value)
	for _, prefix := range []string{
		"<recommended_plugins>", "<environment_context>", "<app-context>",
		"# AGENTS.md instructions", "<INSTRUCTIONS>",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return trimmed != ""
}

func summarizeCodexSession(pathValue, codexHome string, info fs.FileInfo) (codexSessionSummary, error) {
	file, err := os.Open(pathValue)
	if err != nil {
		return codexSessionSummary{}, err
	}
	defer file.Close()
	buffer := make([]byte, 512*1024)
	count, readErr := file.Read(buffer)
	if readErr != nil && count == 0 {
		return codexSessionSummary{}, readErr
	}
	lines := bytes.Split(buffer[:count], []byte{'\n'})
	result := codexSessionSummary{
		Path: pathValue, CodexHome: codexHome, ModifiedAt: info.ModTime().UTC(), SizeBytes: info.Size(),
	}
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Timestamp string         `json:"timestamp"`
			Type      string         `json:"type"`
			Payload   map[string]any `json:"payload"`
		}
		if json.Unmarshal(line, &record) != nil {
			if index == 0 {
				return codexSessionSummary{}, fmt.Errorf("invalid first JSONL record")
			}
			continue
		}
		if record.Type == "session_meta" && result.ThreadID == "" {
			result.ThreadID, _ = record.Payload["id"].(string)
			result.SessionID, _ = record.Payload["session_id"].(string)
			result.ParentThreadID, _ = record.Payload["parent_thread_id"].(string)
			result.Cwd, _ = record.Payload["cwd"].(string)
			result.Source = record.Payload["source"]
			result.Originator, _ = record.Payload["originator"].(string)
			result.HistoryMode, _ = record.Payload["history_mode"].(string)
			result.HistoryBase = record.Payload["history_base"]
			result.ForkedFromID, _ = record.Payload["forked_from_id"].(string)
			result.ClientKind = "unknown"
			if source, ok := result.Source.(map[string]any); ok && source["subagent"] != nil {
				result.ClientKind = "subagent"
				if subagent, ok := source["subagent"].(map[string]any); ok {
					if spawn, ok := subagent["thread_spawn"].(map[string]any); ok && result.ParentThreadID == "" {
						result.ParentThreadID, _ = spawn["parent_thread_id"].(string)
					}
				}
			} else if strings.EqualFold(result.Originator, "Codex Desktop") {
				result.ClientKind = "desktop"
			} else if source, ok := result.Source.(string); ok {
				switch source {
				case "cli":
					result.ClientKind = "cli"
				case "vscode":
					result.ClientKind = "ide"
				}
			}
			result.CodexVersion, _ = record.Payload["cli_version"].(string)
			result.StartedAt = record.Timestamp
			if result.StartedAt == "" {
				result.StartedAt, _ = record.Payload["timestamp"].(string)
			}
			continue
		}
		if result.Title != "" {
			continue
		}
		if record.Type == "event_msg" && record.Payload["type"] == "user_message" {
			if message, ok := record.Payload["message"].(string); ok && isSessionTitleCandidate(message) {
				result.Title = compactSessionTitle(message)
			}
		}
		if record.Type == "response_item" && record.Payload["type"] == "message" && record.Payload["role"] == "user" {
			message := textFromContent(record.Payload["content"])
			if isSessionTitleCandidate(message) {
				result.Title = compactSessionTitle(message)
			}
		}
		if record.Type == "event_msg" && record.Payload["type"] == "item_completed" {
			if item, ok := record.Payload["item"].(map[string]any); ok && item["type"] == "userMessage" {
				message := textFromContent(item["content"])
				if isSessionTitleCandidate(message) {
					result.Title = compactSessionTitle(message)
				}
			}
		}
	}
	if result.ThreadID == "" {
		return codexSessionSummary{}, fmt.Errorf("session_meta thread id is missing")
	}
	if result.SessionID == "" {
		result.SessionID = result.ThreadID
	}
	return result, nil
}

func (runtimeValue *capabilityRuntime) listCodexSessions() (any, error) {
	homes, err := runtimeValue.codexSessionHomes()
	if err != nil {
		return nil, err
	}
	sessions := make([]codexSessionSummary, 0)
	warnings := make([]string, 0)
	warn := func(message string) {
		if len(warnings) < 50 {
			warnings = append(warnings, message)
		}
	}
	for _, codexHome := range homes {
		for _, directory := range []string{"sessions", "archived_sessions"} {
			sessionsRoot := filepath.Join(codexHome, directory)
			walkErr := filepath.WalkDir(sessionsRoot, func(pathValue string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					if !os.IsNotExist(walkErr) {
						warn(walkErr.Error())
					}
					return nil
				}
				if entry.IsDir() {
					return nil
				}
				if len(sessions) >= maxCodexSessionFiles {
					return fs.SkipAll
				}
				if !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
					return nil
				}
				info, infoErr := entry.Info()
				if infoErr != nil || !info.Mode().IsRegular() {
					return nil
				}
				summary, summaryErr := summarizeCodexSession(pathValue, codexHome, info)
				if summaryErr != nil {
					warn(fmt.Sprintf("%s: %v", pathValue, summaryErr))
					return nil
				}
				summary.Archived = directory == "archived_sessions"
				sessions = append(sessions, summary)
				return nil
			})
			if walkErr != nil && !os.IsNotExist(walkErr) {
				warn(walkErr.Error())
			}
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ModifiedAt.After(sessions[j].ModifiedAt) })
	return map[string]any{
		"sessions": sessions, "codexHomes": homes, "warnings": warnings,
		"truncated": len(sessions) >= maxCodexSessionFiles, "maximum": maxCodexSessionFiles,
	}, nil
}

func (runtimeValue *capabilityRuntime) authorizedCodexSessionPath(input string) (string, error) {
	if input == "" || !filepath.IsAbs(input) || !strings.HasSuffix(strings.ToLower(input), ".jsonl") {
		return "", fmt.Errorf("path must be an absolute Codex JSONL session path")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(input))
	if err != nil {
		return "", err
	}
	homes, err := runtimeValue.codexSessionHomes()
	if err != nil {
		return "", err
	}
	for _, home := range homes {
		for _, directory := range []string{"sessions", "archived_sessions"} {
			root := filepath.Join(home, directory)
			if pathContained(root, resolved) {
				info, statErr := os.Stat(resolved)
				if statErr != nil {
					return "", statErr
				}
				if !info.Mode().IsRegular() {
					return "", fmt.Errorf("Codex session must be a regular file")
				}
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("path is outside detected Codex session directories")
}

func (runtimeValue *capabilityRuntime) readCodexSession(params codexSessionsParams) (any, error) {
	pathValue, err := runtimeValue.authorizedCodexSessionPath(params.Path)
	if err != nil {
		return nil, err
	}
	if params.Cursor < 0 {
		return nil, fmt.Errorf("cursor must be non-negative")
	}
	limit := params.Limit
	if limit == 0 {
		limit = defaultSessionChunk
	}
	if limit < 1 || limit > maxCodexSessionChunk {
		return nil, fmt.Errorf("limit must be between 1 and %d", maxCodexSessionChunk)
	}
	file, err := os.Open(pathValue)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if params.Cursor > info.Size() {
		return nil, fmt.Errorf("cursor is past end of session")
	}
	if _, err := file.Seek(params.Cursor, 0); err != nil {
		return nil, err
	}
	wanted := int64(limit)
	if remaining := info.Size() - params.Cursor; remaining < wanted {
		wanted = remaining
	}
	buffer := make([]byte, int(wanted))
	count, err := file.Read(buffer)
	if err != nil && count == 0 {
		return nil, err
	}
	buffer = buffer[:count]
	latest, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if latest.Size() != info.Size() || !latest.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("Codex session changed while reading")
	}
	next := params.Cursor + int64(count)
	if next < info.Size() && params.Encoding != "base64" {
		lastNewline := bytes.LastIndexByte(buffer, '\n')
		if lastNewline < 0 {
			return nil, fmt.Errorf("a JSONL record exceeds the %d byte chunk limit", limit)
		}
		buffer = buffer[:lastNewline+1]
		next = params.Cursor + int64(lastNewline+1)
	}
	content := string(buffer)
	encoding := "utf8"
	if params.Encoding == "base64" {
		content = base64.StdEncoding.EncodeToString(buffer)
		encoding = "base64"
	}
	return map[string]any{
		"path": pathValue, "cursor": params.Cursor, "nextCursor": next,
		"content": content, "encoding": encoding, "eof": next >= info.Size(), "sizeBytes": info.Size(),
		"modifiedAt": info.ModTime().UTC(),
	}, nil
}

func (runtimeValue *capabilityRuntime) codexSessions(params codexSessionsParams) (any, error) {
	switch params.Action {
	case "list":
		return runtimeValue.listCodexSessions()
	case "read":
		return runtimeValue.readCodexSession(params)
	case "resolve":
		return runtimeValue.resolveCodexRollout(params)
	default:
		return nil, fmt.Errorf("unsupported Codex sessions action: %s", params.Action)
	}
}

// References name immutable rollout IDs (the filename suffix), not necessarily
// the logical thread ID after a revert. Search only the selected source's home;
// do not resolve against another installation or the bounded discovery page.
func (runtimeValue *capabilityRuntime) resolveCodexRollout(params codexSessionsParams) (any, error) {
	if !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(params.RolloutID) {
		return nil, fmt.Errorf("invalid rolloutId")
	}
	source, err := runtimeValue.authorizedCodexSessionPath(params.Path)
	if err != nil {
		return nil, err
	}
	homes, err := runtimeValue.codexSessionHomes()
	if err != nil {
		return nil, err
	}
	var found []codexSessionSummary
	for _, home := range homes {
		if !pathContained(filepath.Join(home, "sessions"), source) && !pathContained(filepath.Join(home, "archived_sessions"), source) {
			continue
		}
		for _, directory := range []string{"sessions", "archived_sessions"} {
			err = filepath.WalkDir(filepath.Join(home, directory), func(candidate string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					if os.IsNotExist(walkErr) {
						return nil
					}
					return walkErr
				}
				if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(strings.ToLower(entry.Name()), "-"+strings.ToLower(params.RolloutID)+".jsonl") {
					return nil
				}
				resolved, resolveErr := runtimeValue.authorizedCodexSessionPath(candidate)
				if resolveErr != nil {
					return resolveErr
				}
				if !pathContained(filepath.Join(home, "sessions"), resolved) && !pathContained(filepath.Join(home, "archived_sessions"), resolved) {
					return fmt.Errorf("referenced rollout symlink leaves the source Codex home")
				}
				info, statErr := os.Stat(resolved)
				if statErr != nil {
					return statErr
				}
				summary, summaryErr := summarizeCodexSession(resolved, home, info)
				if summaryErr != nil {
					return summaryErr
				}
				summary.Archived = directory == "archived_sessions"
				found = append(found, summary)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		break
	}
	if len(found) != 1 {
		return nil, fmt.Errorf("referenced rollout must have exactly one source in this Codex home (found %d)", len(found))
	}
	return found[0], nil
}
