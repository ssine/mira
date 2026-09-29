package clauderuntime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

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
