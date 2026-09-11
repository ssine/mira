package node

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

var nodeUUIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type desiredCodexAccount struct {
	NodeAccountID string           `json:"nodeAccountId"`
	AccountID     string           `json:"accountId"`
	Name          string           `json:"name"`
	IsDefault     bool             `json:"isDefault"`
	Enabled       bool             `json:"enabled"`
	Revision      int64            `json:"revision"`
	Desired       desiredAppServer `json:"desiredAppServer"`
}

type accountRuntime struct {
	manager *appServerManager
	busy    bool // reconciliation in flight; guarded by controlClient.accountsMu
	ctx     context.Context
	cancel  context.CancelFunc
}

func accountRuntimeLimit() int {
	if raw := os.Getenv("MIRA_NODE_MAX_CODEX_RUNTIMES"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value >= 1 && value <= 128 {
			return value
		}
	}
	return 4
}

func accountProfileHome(identityFile, accountID string) (string, error) {
	if !nodeUUIDPattern.MatchString(accountID) {
		return "", fmt.Errorf("invalid Node account ID")
	}
	if identityFile == "" {
		var err error
		identityFile, err = DefaultIdentityFile()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(identityFile) {
		return "", fmt.Errorf("Node identity path must be absolute")
	}
	return filepath.Join(filepath.Dir(identityFile), "accounts", accountID, "codex"), nil
}

func effectiveAccountHome(value string) (string, error) {
	if value == "" {
		value = os.Getenv("CODEX_HOME")
	}
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		value = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("Codex home must be absolute")
	}
	value = filepath.Clean(value)
	// Resolve existing ancestors as well as the final directory, which may not
	// exist until first login. Two profiles may not share the same auth cache.
	parent := value
	tail := []string{}
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", err
		}
		tail = append(tail, filepath.Base(parent))
		parent = next
	}
}

func (client *controlClient) setDesiredAccounts(accounts []desiredCodexAccount) {
	client.accountsMu.Lock()
	defer client.accountsMu.Unlock()
	client.desiredAccounts = accounts
}

func (client *controlClient) accountManager(id string) (*appServerManager, error) {
	client.accountsMu.Lock()
	defer client.accountsMu.Unlock()
	if id == "" {
		return client.appServer, nil
	}
	for _, account := range client.desiredAccounts {
		if account.NodeAccountID != id {
			continue
		}
		if !account.Enabled {
			return nil, fmt.Errorf("selected account is disabled")
		}
		if account.IsDefault {
			return client.appServer, nil
		}
		if entry := client.accountRuntimes[id]; entry != nil {
			return entry.manager, nil
		}
		return nil, fmt.Errorf("selected account runtime is not ready")
	}
	return nil, fmt.Errorf("selected account does not belong to this Node")
}

func (client *controlClient) accountReports() []map[string]any {
	client.accountsMu.Lock()
	defer client.accountsMu.Unlock()
	result := []map[string]any{}
	for _, account := range client.desiredAccounts {
		manager := client.appServer
		if !account.IsDefault {
			entry := client.accountRuntimes[account.NodeAccountID]
			if entry == nil {
				continue
			}
			manager = entry.manager
		}
		result = append(result, map[string]any{"nodeAccountId": account.NodeAccountID, "reportedAppServer": manager.report()})
	}
	return result
}

// Reconciliation is asynchronous for every instance, including the legacy
// default. Health checks and runtime downloads must never delay heartbeats.
func (client *controlClient) reconcileAccounts(ctx context.Context) error {
	client.accountsMu.Lock()
	defer client.accountsMu.Unlock()
	if client.accountsClosed {
		return nil
	}
	if client.accountRuntimes == nil {
		client.accountRuntimes = map[string]*accountRuntime{}
	}
	if client.accountRuntimes[""] == nil {
		entryContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
		client.accountRuntimes[""] = &accountRuntime{manager: client.appServer, ctx: entryContext, cancel: cancel}
	}
	accounts := append([]desiredCodexAccount(nil), client.desiredAccounts...)
	hasDefault := false
	for i := range accounts {
		if accounts[i].IsDefault {
			accounts[i].Desired = client.desired
			hasDefault = true
		}
		if !accounts[i].Enabled {
			accounts[i].Desired.Running = false
		}
	}
	if !hasDefault {
		accounts = append(accounts, desiredCodexAccount{IsDefault: true, Enabled: true, Desired: client.desired})
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].IsDefault != accounts[j].IsDefault {
			return accounts[i].IsDefault
		}
		return accounts[i].NodeAccountID < accounts[j].NodeAccountID
	})
	reserved := 0
	for _, entry := range client.accountRuntimes {
		report := entry.manager.report()
		if report["status"] == "running" || report["status"] == "starting" || entry.busy {
			reserved++
		}
	}
	homes := map[string]string{}
	seen := map[string]bool{}
	for _, account := range accounts {
		key := account.NodeAccountID
		if account.IsDefault {
			key = ""
		}
		if key != "" && !nodeUUIDPattern.MatchString(key) {
			continue
		}
		seen[key] = true
		entry := client.accountRuntimes[key]
		if entry == nil {
			manager := client.appServer
			if !account.IsDefault {
				configuration := client.configuration
				configuration.CodexAccountID = key
				configuration.ConfigOverrides = nil
				configuration.AppServerListenURL = "ws://127.0.0.1:0"
				home, err := accountProfileHome(configuration.IdentityFile, key)
				if err != nil {
					return err
				}
				configuration.AppServerCodexHome = home
				manager = newAppServerManager(configuration)
				manager.setNodeCredential(client.token)
			}
			entryContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
			entry = &accountRuntime{manager: manager, ctx: entryContext, cancel: cancel}
			client.accountRuntimes[key] = entry
		}
		entry.manager.mu.Lock()
		entry.manager.bindingID = account.NodeAccountID
		entry.manager.mu.Unlock()
		desired := account.Desired
		effective := entry.manager.effectiveDesired(desired)
		home, homeErr := effectiveAccountHome(effective.CodexHome)
		if old, exists := homes[home]; exists && homeErr == nil && old != key {
			homeErr = fmt.Errorf("another account uses the same Codex home")
		}
		if homeErr == nil {
			homes[home] = key
		}
		if entry.busy {
			continue
		}
		report := entry.manager.report()
		occupied := report["status"] == "running" || report["status"] == "starting"
		if homeErr != nil {
			entry.manager.mu.Lock()
			entry.manager.lastError = homeErr.Error()
			entry.manager.mu.Unlock()
			continue
		}
		if desired.Running && !occupied && reserved >= accountRuntimeLimit() {
			entry.manager.mu.Lock()
			entry.manager.lastError = "账号运行实例已达上限，请先停止空闲实例"
			entry.manager.mu.Unlock()
			continue
		}
		if desired.Running && !occupied {
			reserved++
		}
		entry.busy = true
		go func(entry *accountRuntime, desired desiredAppServer) {
			if err := entry.manager.discover(entry.ctx); err == nil {
				_ = entry.manager.reconcile(entry.ctx, desired)
			}
			client.accountsMu.Lock()
			entry.busy = false
			client.accountsMu.Unlock()
		}(entry, desired)
	}
	for key, entry := range client.accountRuntimes {
		if seen[key] {
			continue
		}
		if entry.cancel != nil {
			entry.cancel()
		}
		entry.manager.close()
		delete(client.accountRuntimes, key)
	}
	return nil
}

func (client *controlClient) closeAccountRuntimes() {
	client.accountsMu.Lock()
	client.accountsClosed = true
	entries := client.accountRuntimes
	for _, entry := range entries {
		if entry.cancel != nil {
			entry.cancel()
		}
	}
	client.accountsMu.Unlock()
	for _, entry := range entries {
		entry.manager.close()
	}
	client.appServer.close()
}

func (manager *appServerManager) observeAccountThread(payload []byte) {
	var message struct {
		Method string `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
			Status   struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"params"`
	}
	if json.Unmarshal(payload, &message) != nil || message.Params.ThreadID == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.activeThreads == nil {
		manager.activeThreads = map[string]bool{}
	}
	switch message.Method {
	case "turn/started":
		manager.activeThreads[message.Params.ThreadID] = true
	case "turn/completed", "thread/closed":
		delete(manager.activeThreads, message.Params.ThreadID)
		for key, pending := range manager.pendingTurns {
			if pending.ThreadID == message.Params.ThreadID {
				delete(manager.pendingTurns, key)
			}
		}
	case "thread/status/changed":
		if message.Params.Status.Type == "active" {
			manager.activeThreads[message.Params.ThreadID] = true
		}
		if message.Params.Status.Type == "idle" || message.Params.Status.Type == "notLoaded" || message.Params.Status.Type == "systemError" {
			delete(manager.activeThreads, message.Params.ThreadID)
		}
	}
}
