package clauderuntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Mirrors DEFAULT_CACHE_BYTES and the per-transcript reserve in session-cache.mjs.
const (
	defaultCacheBytes = 4 << 30
	cacheReserve      = 4096
)

var hex64 = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Cache maintenance uses only Node builtins and does not install or start Claude.
// Calls are serialized by Manager.mu; only this path may reap dead writer locks.
func (m *Manager) cacheCall(action string, maxBytes any) (any, error) {
	source, _ := assets.ReadFile("assets/session-cache.mjs")
	script := string(source) + `
let input = "";
for await (const chunk of process.stdin) input += chunk;
try { process.stdout.write(JSON.stringify(await cacheMaintenance(JSON.parse(input)))); }
catch { process.stderr.write("Claude cache settings or storage are unavailable"); process.exitCode = 1; }
`
	body, err := json.Marshal(map[string]any{"root": m.cacheRoot, "config": m.cacheConfig, "action": action, "maxBytes": maxBytes})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeBinary(), "--input-type=module", "-e", script)
	configureCommand(cmd)
	cmd.Cancel = func() error { killCommand(cmd); return nil }
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(string(body))
	output := &boundedOutput{limit: 4096}
	cmd.Stdout = output
	if err = cmd.Run(); err != nil {
		return nil, errors.New("Claude cache settings or storage are unavailable; Node.js 22 or newer is required")
	}
	var result any
	err = json.Unmarshal(output.Bytes(), &result)
	return result, err
}

func (m *Manager) cachePaths(identityDir string) {
	m.cacheRoot = filepath.Join(identityDir, "cache", "claude-sessions")
	m.cacheConfig = filepath.Join(identityDir, "claude-cache.json")
}

// sessionCache reports the cached transcript the next load would read for
// sessionId/subpath at the current Server endpoint. It reads only metadata
// and file sizes, so it neither waits for Manager.mu nor takes the writer
// lock; the worker still verifies the digest and Server prefix when loading.
func (m *Manager) sessionCache(params map[string]any) (any, error) {
	endpoint, _ := params["endpoint"].(string)
	sessionID, _ := params["sessionId"].(string)
	subpath, _ := params["subpath"].(string)
	origin, err := urlOrigin(endpoint)
	if err != nil || sessionID == "" {
		return nil, errors.New("sessionId and a Mira endpoint are required")
	}
	limit := int64(defaultCacheBytes)
	if raw, err := os.ReadFile(m.cacheConfig); err == nil {
		var config struct {
			MaxBytes *int64 `json:"maxBytes"`
		}
		if json.Unmarshal(raw, &config) != nil || config.MaxBytes == nil || *config.MaxBytes < 0 {
			return nil, errors.New("Claude cache settings are unavailable")
		}
		limit = *config.MaxBytes
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("Claude cache settings are unavailable")
	}
	result := map[string]any{"state": "absent", "maxBytes": limit}
	if limit == 0 {
		result["state"] = "disabled"
		return result, nil
	}
	// Same key as session-cache.mjs: sha256(JSON.stringify([origin, sessionId, subpath])).
	var key bytes.Buffer
	encoder := json.NewEncoder(&key)
	encoder.SetEscapeHTML(false)
	if err = encoder.Encode([]string{origin, sessionID, subpath}); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(bytes.TrimSuffix(key.Bytes(), []byte("\n")))
	name := filepath.Join(m.cacheRoot, hex.EncodeToString(digest[:]))
	raw, err := os.ReadFile(name + ".json")
	if err != nil {
		return result, nil
	}
	var meta struct {
		Version int    `json:"version"`
		Bytes   int64  `json:"bytes"`
		Cursor  int64  `json:"cursor"`
		Prefix  string `json:"prefix"`
		Digest  string `json:"digest"`
	}
	if json.Unmarshal(raw, &meta) != nil || meta.Version != 1 || meta.Cursor < 1 || meta.Bytes <= 0 ||
		meta.Bytes+cacheReserve > limit || !hex64.MatchString(meta.Prefix) || !hex64.MatchString(meta.Digest) {
		return result, nil
	}
	if info, err := os.Stat(name + ".jsonl"); err != nil || !info.Mode().IsRegular() || info.Size() < meta.Bytes {
		return result, nil
	}
	result["state"] = "cached"
	result["cursor"] = meta.Cursor
	result["prefix"] = meta.Prefix
	result["bytes"] = meta.Bytes
	return result, nil
}

// urlOrigin matches WHATWG URL.origin for the HTTP(S) endpoints Mira uses.
func urlOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("invalid endpoint")
	}
	scheme, host, port := strings.ToLower(parsed.Scheme), strings.ToLower(parsed.Hostname()), parsed.Port()
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" && !(scheme == "http" && port == "80" || scheme == "https" && port == "443") {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}
