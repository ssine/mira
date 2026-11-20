package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// cliInstallation is the Mira installation a CLI invocation belongs to. Every
// component uses one rule to find it (defaultSupervisorStateDir): the explicit
// MIRA_STATE_DIR, then the installation containing the running executable,
// then the user's default state directory. Identity and settings come from
// that directory, so no entry point needs a wrapper script.
type cliInstallation struct {
	StateDir       string
	IdentityFile   string
	IdentitySource string // MIRA_IDENTITY_FILE, node.json or default
}

func resolveCLIInstallation() (cliInstallation, error) {
	installation := cliInstallation{}
	if stateDir, err := defaultSupervisorStateDir(); err == nil {
		installation.StateDir = stateDir
	}
	// The environment override stays for compatibility and development.
	if os.Getenv("MIRA_IDENTITY_FILE") != "" {
		path, err := DefaultIdentityFile()
		installation.IdentityFile, installation.IdentitySource = path, "MIRA_IDENTITY_FILE"
		return installation, err
	}
	if path := installationIdentityFile(installation.StateDir); path != "" {
		installation.IdentityFile, installation.IdentitySource = path, "node.json"
		return installation, nil
	}
	path, err := DefaultIdentityFile()
	installation.IdentityFile, installation.IdentitySource = path, "default"
	return installation, err
}

// installationIdentityFile returns the identity recorded by an installation's
// node.json. The file need not exist yet: a Node that has not enrolled still
// belongs to its installation.
func installationIdentityFile(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	contents, err := os.ReadFile(filepath.Join(stateDir, "node.json"))
	if err != nil {
		return ""
	}
	var stored struct {
		IdentityFile    string `json:"identityFile"`
		LegacyStateFile string `json:"stateFile"`
	}
	if json.Unmarshal(contents, &stored) != nil {
		return ""
	}
	path := stored.IdentityFile
	if path == "" {
		path = stored.LegacyStateFile
	}
	if !filepath.IsAbs(path) {
		return ""
	}
	return filepath.Clean(path)
}

// installationSettings are the Node settings in <state>/mira.env that the CLI
// shares with the Node worker. Other entries (PATH, database or provider
// credentials on a Server host) stay with the service and never reach a CLI
// child process.
var installationSettings = map[string]bool{
	"CODEX_BINARY": true, "MIRA_NODE_CODEX_CACHE": true, "MIRA_NODE_SING_BOX": true,
	"MIRA_NODE_CLAUDE_BINARY": true, "MIRA_NODE_CLAUDE_NODE": true, "MIRA_NODE_OPENSSH_DIR": true,
	"MIRA_CODEX_STORE_ID": true,
}

// applyInstallationSettings gives the CLI the worker's view of those settings.
// An explicit environment value wins; a missing or unreadable file changes nothing.
func applyInstallationSettings(stateDir string) {
	if stateDir == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(stateDir, "mira.env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		key = strings.TrimSpace(key)
		if !ok || !installationSettings[key] || os.Getenv(key) != "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		if value != "" {
			_ = os.Setenv(key, value)
		}
	}
}
