package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errCodexAccountBusy = errors.New("账号仍有任务或其他凭据操作，请稍后再试")

type accountPendingRequest struct {
	Method   string
	ThreadID string
}

func (manager *appServerManager) reserveAccountRequest(sessionID string, payload []byte) error {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			ThreadID string `json:"threadId"`
		} `json:"params"`
	}
	if json.Unmarshal(payload, &message) != nil {
		return fmt.Errorf("invalid App Server request")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
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
	case "turn/start", "turn/steer", "thread/start", "thread/resume", "thread/fork":
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

func (manager *appServerManager) observeAccountResponse(sessionID string, payload []byte) {
	var response struct {
		ID     json.RawMessage `json:"id"`
		Error  json.RawMessage `json:"error"`
		Method string          `json:"method"`
		Result struct {
			Type string `json:"type"`
		} `json:"result"`
	}
	if json.Unmarshal(payload, &response) != nil {
		return
	}
	if response.Method == "account/login/completed" {
		manager.mu.Lock()
		manager.loginPending = false
		manager.mu.Unlock()
	}
	if len(response.ID) > 0 {
		manager.mu.Lock()
		key := sessionID + ":" + string(response.ID)
		pending, ok := manager.pendingTurns[key]
		delete(manager.pendingTurns, key)
		failed := len(response.Error) > 0 && string(response.Error) != "null"
		if ok && ((pending.Method == "account/login/start" && (failed || response.Result.Type == "apiKey")) || (pending.Method == "account/login/cancel" && !failed)) {
			manager.loginPending = false
		}
		if ok && pending.Method == "turn/start" && pending.ThreadID != "" && (len(response.Error) == 0 || string(response.Error) == "null") {
			if manager.activeThreads == nil {
				manager.activeThreads = map[string]bool{}
			}
			manager.activeThreads[pending.ThreadID] = true
		}
		manager.mu.Unlock()
	}
	manager.observeAccountThread(payload)
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
	if manager.managementSession != "" || len(manager.activeThreads) > 0 || len(manager.pendingTurns) > 0 {
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
