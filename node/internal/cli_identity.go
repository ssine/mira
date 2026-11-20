package node

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// cliDefaultIdentityFile resolves the identity for a CLI started without
// MIRA_IDENTITY_FILE. An installed image (<state>/versions/<v>/mira or
// <state>/current/mira) uses the identity its own Node worker uses, recorded
// in <state>/node.json, so the absolute CLI path given to Agents works on
// Nodes whose identity is not at the default location. Otherwise it falls
// back to DefaultIdentityFile.
func cliDefaultIdentityFile() (string, error) {
	if os.Getenv("MIRA_IDENTITY_FILE") == "" {
		if executable, err := os.Executable(); err == nil {
			if path := installedIdentityFile(executable); path != "" {
				return path, nil
			}
		}
	}
	return DefaultIdentityFile()
}

func installedIdentityFile(executable string) string {
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}
	dir := filepath.Dir(executable)
	state := ""
	switch {
	case filepath.Base(filepath.Dir(dir)) == "versions":
		state = filepath.Dir(filepath.Dir(dir))
	case filepath.Base(dir) == "current":
		state = filepath.Dir(dir)
	default:
		return ""
	}
	contents, err := os.ReadFile(filepath.Join(state, "node.json"))
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
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}
