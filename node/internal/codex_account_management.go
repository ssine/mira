package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errCodexAccountBusy = errors.New("账号仍有任务或其他凭据操作，请稍后再试")

type accountPendingRequest struct {
	Method           string
	ThreadID         string
	ObservedActivity bool
}

func (manager *appServerManager) reserveAccountRequest(instance *appServerInstance, sessionID string, payload []byte) error {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ThreadID  string   `json:"threadId"`
			ThreadIDs []string `json:"threadIds"`
		} `json:"params"`
	}
	if json.Unmarshal(payload, &message) != nil {
		return fmt.Errorf("invalid App Server request")
	}
	if message.Method == "mira/thread/residency" {
		return fmt.Errorf("residency is controlled by the local Mira Node")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.instance != instance || (instance != nil && channelClosed(instance.done)) {
		return fmt.Errorf("account runtime was replaced")
	}
	if manager.transitioning || manager.activityChecking || manager.residencyEvicting {
		switch message.Method {
		case "turn/start", "turn/steer", "thread/start", "thread/resume", "thread/fork", "thread/compact/start", "thread/realtime/start":
			return fmt.Errorf("账号正在核验状态或停止，请稍后重试")
		}
	}
	if owner := manager.threadManagement[message.Params.ThreadID]; owner != "" && owner != sessionID {
		switch message.Method {
		case "thread/read", "thread/turns/list", "thread/items/list":
		default:
			return fmt.Errorf("此对话正在交接账号或处理历史，请稍后重新打开")
		}
	}
	if message.Method == "mira/thread/unload" {
		if len(message.Params.ThreadIDs) == 0 || manager.threadManagement[message.Params.ThreadID] != sessionID {
			return fmt.Errorf("thread unload requires a scoped management session")
		}
		for _, id := range message.Params.ThreadIDs {
			if manager.threadManagement[id] != sessionID {
				return fmt.Errorf("thread unload exceeds the management scope")
			}
		}
	}
	authMutation := strings.HasPrefix(message.Method, "account/login/") || message.Method == "account/logout" || strings.HasPrefix(message.Method, "config/value/") || message.Method == "config/batchWrite"
	if authMutation && manager.managementSession != sessionID {
		return fmt.Errorf("请从账号管理页修改凭据和配置")
	}
	if message.Method == "account/login/start" || message.Method == "account/login/cancel" {
		if manager.pendingTurns == nil {
			manager.pendingTurns = map[string]accountPendingRequest{}
		}
		if message.Method == "account/login/start" {
			manager.loginPending = true
		}
		manager.pendingTurns[sessionID+":"+string(message.ID)] = accountPendingRequest{Method: message.Method}
	}
	switch message.Method {
	case "turn/start", "turn/steer", "thread/start", "thread/resume", "thread/fork", "thread/compact/start", "thread/realtime/start":
		if manager.managementSession != "" {
			return fmt.Errorf("此账号正在更新凭据，请完成或取消登录后再发送")
		}
		if manager.pendingTurns == nil {
			manager.pendingTurns = map[string]accountPendingRequest{}
		}
		if len(manager.pendingTurns) >= 256 || len(manager.activeThreads) >= 1024 {
			return fmt.Errorf("account execution capacity reached")
		}
		if len(message.ID) > 0 {
			manager.pendingTurns[sessionID+":"+string(message.ID)] = accountPendingRequest{Method: message.Method, ThreadID: message.Params.ThreadID}
		}
	}
	return nil
}

func (manager *appServerManager) observeAccountResponse(instance *appServerInstance, sessionID string, payload []byte) {
	var response struct {
		ID     json.RawMessage `json:"id"`
		Error  json.RawMessage `json:"error"`
		Method string          `json:"method"`
		Result struct {
			Type string `json:"type"`
			Turn struct {
				Status string `json:"status"`
			} `json:"turn"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	// A replaced process can still have queued tunnel messages. They must not
	// resurrect activity in the new runtime (or after the old process exited).
	if manager.instance != instance || (instance != nil && channelClosed(instance.done)) {
		return
	}
	if response.Method == "account/login/completed" {
		manager.loginPending = false
	}
	if len(response.ID) > 0 && response.Method == "" {
		key := sessionID + ":" + string(response.ID)
		pending, ok := manager.pendingTurns[key]
		delete(manager.pendingTurns, key)
		failed := len(response.Error) > 0 && string(response.Error) != "null"
		if ok && ((pending.Method == "account/login/start" && (failed || response.Result.Type == "apiKey")) || (pending.Method == "account/login/cancel" && !failed)) {
			manager.loginPending = false
		}
		if ok && pending.Method == "turn/start" && pending.ThreadID != "" && !pending.ObservedActivity && !failed && response.Result.Turn.Status == "inProgress" {
			if manager.activeThreads == nil {
				manager.activeThreads = map[string]bool{}
			}
			manager.activeThreads[pending.ThreadID] = true
		}
	}
	manager.observeAccountThreadLocked(payload)
}

func (manager *appServerManager) hasPendingAccountRequests(instance *appServerInstance, sessionID string) bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.instance != instance || (instance != nil && channelClosed(instance.done)) {
		return false
	}
	for key := range manager.pendingTurns {
		if strings.HasPrefix(key, sessionID+":") {
			return true
		}
	}
	return false
}

func (client *controlClient) configureCodexAccount(id string, params json.RawMessage) (map[string]any, error) {
	manager, err := client.accountManager(id)
	if err != nil {
		return nil, err
	}
	client.accountsMu.Lock()
	var selected *desiredCodexAccount
	for _, account := range client.desiredAccounts {
		if account.NodeAccountID == id {
			copy := account
			selected = &copy
			break
		}
	}
	client.accountsMu.Unlock()
	if selected == nil || selected.IsDefault {
		return nil, fmt.Errorf("create a separate account to manage its local configuration")
	}
	if selected.Desired.Running {
		return nil, fmt.Errorf("请先停止账号运行实例，等待节点确认后再配置")
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(params, &input) != nil {
		return nil, fmt.Errorf("invalid account configuration")
	}
	if _, providerChange := input["provider"]; providerChange {
		owned, err := accountProfileHome(manager.configuration.IdentityFile, id)
		if err != nil {
			return nil, err
		}
		actual, err := effectiveAccountHome(manager.effectiveDesired(selected.Desired).CodexHome)
		if err != nil {
			return nil, err
		}
		owned, err = effectiveAccountHome(owned)
		if err != nil {
			return nil, err
		}
		if actual != owned {
			return nil, fmt.Errorf("接管的 Codex 配置保持由原文件管理；请编辑原文件，或创建 Mira 托管账号")
		}
	}
	return manager.configureAccount(params)
}

func (manager *appServerManager) beginAccountManagement(sessionID string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.transitioning || manager.activityChecking || manager.managementSession != "" || len(manager.threadManagement) > 0 || len(manager.activeThreads) > 0 || len(manager.pendingTurns) > 0 {
		return errCodexAccountBusy
	}
	manager.managementSession = sessionID
	return nil
}

func (manager *appServerManager) endAccountManagement(sessionID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.managementSession == sessionID {
		// An unacknowledged login may still complete after its WebSocket closes.
		// Retiring this idle process cancels that flow before releasing the gate.
		if manager.loginPending {
			_ = manager.stopLocked()
			manager.loginPending = false
		}
		manager.managementSession = ""
	}
}
