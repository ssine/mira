package node

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The local account index lets `mira codex` select and launch a Codex account
// while Mira Server is unreachable. It is a disposable Node-local cache of the
// last desired account state, never an authority: the Node rewrites it from
// Server desired state after every reconciliation. It is protected like the
// Node identity because local launcher overrides may reference credentials.
const localAccountIndexName = "local-index.json"

type localCodexAccount struct {
	NodeAccountID    string   `json:"nodeAccountId"`
	Name             string   `json:"name,omitempty"`
	Enabled          bool     `json:"enabled"`
	CodexPath        string   `json:"codexPath,omitempty"`
	CodexHome        string   `json:"codexHome"`
	ConfigOverrides  []string `json:"configOverrides,omitempty"`
	EnvironmentFiles []string `json:"environmentFiles,omitempty"`
	InheritEnv       []string `json:"inheritEnv,omitempty"`
	// Legacy marks the Node's original App Server instance, which runs from the
	// Node's own Codex home instead of a per-account directory.
	Legacy bool `json:"legacy,omitempty"`
}

// localClaudeLabel is Server display metadata; provider settings and
// credentials already live in each Claude account's private directory.
type localClaudeLabel struct {
	NodeAccountID string `json:"nodeAccountId"`
	Name          string `json:"name"`
	Enabled       bool   `json:"enabled"`
}

type localAccountIndex struct {
	SchemaVersion int                 `json:"schemaVersion"`
	UpdatedAt     string              `json:"updatedAt"`
	Codex         []localCodexAccount `json:"codex"`
	Claude        []localClaudeLabel  `json:"claude,omitempty"`
}

// setClaudeLabels accepts labels only from a Server that sends them; an older
// Server omits the field and leaves previously cached labels intact.
func (client *controlClient) setClaudeLabels(labels *[]localClaudeLabel) {
	if labels == nil {
		return
	}
	client.accountsMu.Lock()
	defer client.accountsMu.Unlock()
	valid := []localClaudeLabel{}
	for _, label := range *labels {
		if nodeUUIDPattern.MatchString(label.NodeAccountID) && len(label.Name) <= 128 && len(valid) < 256 {
			label.NodeAccountID = strings.ToLower(label.NodeAccountID)
			valid = append(valid, label)
		}
	}
	client.claudeLabels, client.claudeLabelsKnown = valid, true
}

func localAccountIndexPath(identityFile string) (string, error) {
	if identityFile == "" {
		var err error
		if identityFile, err = DefaultIdentityFile(); err != nil {
			return "", err
		}
	}
	return filepath.Join(filepath.Dir(identityFile), "accounts", localAccountIndexName), nil
}

func readLocalAccountIndex(identityFile string) (localAccountIndex, error) {
	index := localAccountIndex{}
	path, err := localAccountIndexPath(identityFile)
	if err != nil {
		return index, err
	}
	data, err := readAccountFile(path)
	if err != nil {
		return index, err
	}
	if json.Unmarshal(data, &index) != nil || index.SchemaVersion != 1 {
		return localAccountIndex{}, os.ErrNotExist
	}
	return index, nil
}

// buildLocalCodexIndex captures what reconcileAccounts would launch. Callers
// hold accountsMu; it does not touch running managers.
func (client *controlClient) buildLocalCodexIndex(accounts []desiredCodexAccount) []localCodexAccount {
	result := []localCodexAccount{}
	for _, account := range accounts {
		if account.NodeAccountID != "" && !nodeUUIDPattern.MatchString(account.NodeAccountID) {
			continue
		}
		configuration := client.configuration
		desired := account.Desired
		if !account.IsDefault {
			home, err := accountProfileHome(configuration.IdentityFile, account.NodeAccountID)
			if err != nil {
				continue
			}
			configuration.AppServerCodexHome = home
			configuration.ConfigOverrides = nil
		}
		manager := &appServerManager{configuration: configuration}
		effective := manager.effectiveDesired(desired)
		home, err := effectiveAccountHome(effective.CodexHome)
		if err != nil {
			continue
		}
		codexPath := desired.CodexPath
		if codexPath == "" && account.IsDefault {
			codexPath = configuration.CodexBinary
		}
		result = append(result, localCodexAccount{
			NodeAccountID: account.NodeAccountID, Name: account.Name, Enabled: account.Enabled,
			CodexPath: codexPath, CodexHome: home, ConfigOverrides: effective.ConfigOverrides,
			EnvironmentFiles: desired.EnvironmentFiles, InheritEnv: desired.InheritEnv, Legacy: account.IsDefault,
		})
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].NodeAccountID < result[j].NodeAccountID })
	return result
}

// recordLocalAccountIndex rewrites the index only when its content changes.
// Failures are deliberately non-fatal: the cache only serves offline launches.
func (client *controlClient) recordLocalAccountIndex(accounts []desiredCodexAccount) {
	path, err := localAccountIndexPath(client.configuration.IdentityFile)
	if err != nil {
		return
	}
	index := localAccountIndex{SchemaVersion: 1, Codex: client.buildLocalCodexIndex(accounts), Claude: client.claudeLabels}
	if !client.claudeLabelsKnown {
		if previous, err := readLocalAccountIndex(client.configuration.IdentityFile); err == nil {
			index.Claude = previous.Claude
		}
	}
	encoded, err := json.Marshal(index)
	if err != nil || bytes.Equal(encoded, client.localIndexDigest) {
		return
	}
	index.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return
	}
	if writeProtectedAccountFile(path, append(data, '\n')) == nil {
		client.localIndexDigest = encoded
	}
}
