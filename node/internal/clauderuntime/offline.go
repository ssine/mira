package clauderuntime

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/ssine/mira/node/internal/accountegress"
)

// OfflineAccount is non-secret metadata about one Node-local Claude account.
// Display names live in Mira Server; offline listings identify accounts by
// UUID, provider and, for subscriptions, the locally recorded login email.
type OfflineAccount struct {
	NodeAccountID string                    `json:"nodeAccountId"`
	AuthType      string                    `json:"authType"`
	Provider      AccountProvider           `json:"provider"`
	Email         string                    `json:"email,omitempty"`
	Configured    bool                      `json:"configured"`
	ConfigDir     string                    `json:"configDir"`
	Egress        *accountegress.PublicSpec `json:"egress,omitempty"`
	Error         string                    `json:"error,omitempty"`
}

// sdkPlatform maps Go's platform names to the SDK's optional native packages.
func sdkPlatform() string {
	system := runtime.GOOS
	if system == "windows" {
		system = "win32"
	}
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x64"
	}
	return system + "-" + arch
}

// sdkExecutable returns the native Claude Code binary inside one prepared SDK
// directory, or "" when it is absent.
func sdkExecutable(dir string) string {
	if dir == "" {
		return ""
	}
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	packages := []string{
		"claude-agent-sdk-" + sdkPlatform(),
		"claude-agent-sdk-" + sdkPlatform() + "-musl",
		"claude-agent-sdk-" + runtime.GOOS + "-" + runtime.GOARCH,
	}
	for _, pkg := range packages {
		path := filepath.Join(dir, "node_modules", "@anthropic-ai", pkg, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// LocalExecutable finds a Claude Code binary without contacting Mira Server.
// Order: MIRA_NODE_CLAUDE_BINARY, the SDK prepared for this build, any other
// prepared SDK (newest first), then claude on PATH.
func LocalExecutable(identityDir string) (string, error) {
	if binary := os.Getenv("MIRA_NODE_CLAUDE_BINARY"); binary != "" {
		return binary, nil
	}
	root := filepath.Join(identityDir, "runtimes", "claude")
	if path := readySDKExecutable(filepath.Join(root, assetDigest())); path != "" {
		return path, nil
	}
	entries, _ := os.ReadDir(root)
	type candidate struct {
		dir      string
		modified int64
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if info, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			candidates = append(candidates, candidate{dir, info.ModTime().UnixNano()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modified > candidates[j].modified })
	for _, item := range candidates {
		if path := sdkExecutable(item.dir); path != "" {
			return path, nil
		}
	}
	if path, err := exec.LookPath("claude"); err == nil {
		return path, nil
	}
	return "", errors.New("Claude Code executable not found: let the Node prepare its Claude runtime once, install claude on PATH, or set MIRA_NODE_CLAUDE_BINARY")
}

func readySDKExecutable(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "ready")); err != nil {
		return ""
	}
	return sdkExecutable(dir)
}

// ListOfflineAccounts reports every Claude account configured on this Node.
func ListOfflineAccounts(identityDir string) ([]OfflineAccount, error) {
	m := New(identityDir)
	entries, err := os.ReadDir(m.accountsRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result := []OfflineAccount{}
	for _, entry := range entries {
		if !entry.IsDir() || !accountIDPattern.MatchString(entry.Name()) {
			continue
		}
		home, _ := m.accountHome(entry.Name())
		if _, err := os.Stat(filepath.Join(home, "mira-account.json")); err != nil {
			continue
		}
		item := OfflineAccount{NodeAccountID: strings.ToLower(entry.Name()), ConfigDir: home}
		account, err := m.readAccount(entry.Name())
		if err != nil {
			item.Error = err.Error()
			result = append(result, item)
			continue
		}
		item.AuthType, item.Provider = account.AuthType, account.Provider
		item.Configured = m.accountConfigured(entry.Name(), account)
		if account.AuthType == "subscription" {
			item.Email = loginEmail(home)
		}
		if public, err := m.egress.PublicConfig(entry.Name()); err == nil && public != nil {
			item.Egress = public
		} else if account.Egress != nil {
			public := account.Egress.Public()
			item.Egress = &public
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].NodeAccountID < result[j].NodeAccountID })
	return result, nil
}

// loginEmail reads the account email Claude Code recorded after OAuth login.
func loginEmail(home string) string {
	file, err := os.Open(filepath.Join(home, ".claude.json"))
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return ""
	}
	var value struct {
		OAuthAccount struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	return value.OAuthAccount.EmailAddress
}

// Session-scoped variables injected into a parent Claude Code or SDK process.
// They must not leak into an independent interactive session.
var parentSessionEnvironment = map[string]bool{
	"CLAUDECODE": true, "CLAUDE_PID": true, "CLAUDE_EFFORT": true, "CLAUDE_AGENT_SDK_VERSION": true,
	"CLAUDE_CODE_ENTRYPOINT": true, "CLAUDE_CODE_SESSION_ID": true, "CLAUDE_CODE_CHILD_SESSION": true,
	"CLAUDE_CODE_SESSION_ATTENDED": true, "CLAUDE_CODE_MESSAGING_SOCKET": true, "CLAUDE_CODE_MESSAGING_TOKEN": true,
	"CLAUDE_CODE_EXECPATH": true, "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS": true, "AI_AGENT": true,
}

// OfflineSession is a prepared interactive Claude Code invocation.
type OfflineSession struct {
	Environment []string
	// Arguments precede the caller's own arguments.
	Arguments []string
	Release   func()
}

// OfflineSession prepares the same account environment as managed turns,
// including the account's egress gateway, for a foreground Claude Code CLI.
// Callers must invoke Release after the child exits.
func (m *Manager) OfflineSession(id string, userArgs []string) (*OfflineSession, error) {
	account, err := m.readAccount(id)
	if err != nil {
		return nil, err
	}
	values, release, err := m.accountEnvironmentAcquired(id, false)
	if err != nil {
		return nil, err
	}
	environment := make([]string, 0, len(values))
	for _, entry := range values {
		key, _, _ := strings.Cut(entry, "=")
		if !parentSessionEnvironment[key] {
			environment = append(environment, entry)
		}
	}
	arguments := []string{}
	if account.Provider.Effort != "" && !hasFlag(userArgs, "--effort") {
		arguments = append(arguments, "--effort", account.Provider.Effort)
	}
	return &OfflineSession{Environment: environment, Arguments: arguments, Release: release}, nil
}

func hasFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
