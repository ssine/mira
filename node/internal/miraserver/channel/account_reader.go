package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

type accountResponse struct {
	result any
	err    error
	detail string
	code   string
}

type accountSession struct {
	mu            sync.Mutex
	id            string
	nodeID        string
	accountID     string
	nextID        int64
	pending       map[string]chan accountResponse
	changed       bool
	closed        bool
	closeError    error
	done          chan struct{}
	loginComplete bool
	loginSuccess  bool
	loginID       string
	retiring      bool
}

type AccountReader struct {
	mu       sync.Mutex
	send     func(string, any) bool
	timeout  time.Duration
	sessions map[string]*accountSession
	logins   map[string]*accountSession
	retired  map[string]time.Time
}

type AccountSnapshot struct {
	Account any `json:"account"`
	Limits  any `json:"limits"`
}

func NewAccountReader(send func(string, any) bool, timeout time.Duration) *AccountReader {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &AccountReader{send: send, timeout: timeout, sessions: map[string]*accountSession{}, logins: map[string]*accountSession{}}
}

func (reader *AccountReader) Handle(nodeID string, message map[string]any) bool {
	sessionID, _ := message["sessionId"].(string)
	reader.mu.Lock()
	session := reader.sessions[sessionID]
	reader.mu.Unlock()
	if session == nil || session.nodeID != nodeID {
		return false
	}
	messageType, _ := message["type"].(string)
	if messageType == "appserver.error" || messageType == "appserver.closed" {
		session.mu.Lock()
		retiring := session.retiring
		if message["code"] == "account_busy" {
			session.closeError = errors.New("此账号仍有对话、子任务或凭据操作运行，请等待结束后重试")
		}
		if message["code"] == "thread_handoff_failed" {
			if detail, ok := message["error"].(string); ok && len(detail) <= 2048 {
				session.closeError = errors.New(detail)
			}
		}
		session.mu.Unlock()
		if retiring {
			return true
		}
		reader.closeSession(session)
		return true
	}
	if messageType == "account.result" {
		session.mu.Lock()
		pending := session.pending["1"]
		delete(session.pending, "1")
		session.mu.Unlock()
		if pending != nil {
			if message["error"] != nil {
				pending <- accountResponse{err: errors.New("节点拒绝配置更新；请确认账号已停止、输入合法且配置目录可写")}
			} else {
				pending <- accountResponse{result: message["result"]}
			}
		}
		return true
	}
	if messageType != "appserver.message" {
		return true
	}
	payload, _ := message["payload"].(string)
	value, err := decodeObject([]byte(payload))
	if err != nil {
		reader.closeSession(session)
		return true
	}
	if method, _ := value["method"].(string); method == "account/updated" {
		session.mu.Lock()
		session.changed = true
		session.mu.Unlock()
	}
	if method, _ := value["method"].(string); method == "account/login/completed" {
		params, _ := value["params"].(map[string]any)
		session.mu.Lock()
		session.loginComplete = true
		session.loginSuccess = params["success"] == true
		session.mu.Unlock()
	}
	if _, notification := value["method"]; notification {
		return true
	}
	id := rpcID(value["id"])
	session.mu.Lock()
	pending := session.pending[id]
	if pending != nil {
		delete(session.pending, id)
	}
	session.mu.Unlock()
	if pending != nil {
		if value["error"] != nil {
			rpcError, _ := value["error"].(map[string]any)
			detail, _ := rpcError["message"].(string)
			pending <- accountResponse{err: errors.New("account RPC failed"), detail: detail, code: fmt.Sprint(rpcError["code"])}
		} else {
			pending <- accountResponse{result: value["result"]}
		}
	}
	return true
}

func (reader *AccountReader) Read(ctx context.Context, nodeID string) (AccountSnapshot, error) {
	return reader.ReadAccount(ctx, nodeID, "", "")
}

func (reader *AccountReader) open(ctx context.Context, nodeID, accountID, runtimeID string, management ...bool) (*accountSession, error) {
	return reader.openScoped(ctx, nodeID, accountID, runtimeID, len(management) > 0 && management[0], nil)
}

func (reader *AccountReader) openScoped(ctx context.Context, nodeID, accountID, runtimeID string, management bool, threadIDs []string) (*accountSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.New("account operation stopped")
	}
	sessionID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	session := &accountSession{id: sessionID, nodeID: nodeID, accountID: accountID, pending: map[string]chan accountResponse{}, done: make(chan struct{})}
	reader.mu.Lock()
	if len(reader.sessions) >= 64 {
		reader.mu.Unlock()
		return nil, errors.New("account operation capacity reached")
	}
	reader.sessions[sessionID] = session
	reader.mu.Unlock()
	opened := false
	defer func() {
		if !opened {
			reader.closeSession(session)
		}
	}()
	message := map[string]any{"type": "appserver.open", "sessionId": sessionID}
	if accountID != "" {
		message["nodeAccountId"] = accountID
	}
	if runtimeID != "" {
		message["runtimeId"] = runtimeID
	}
	if management {
		message["accountManagement"] = true
	}
	if len(threadIDs) > 0 {
		message["accountThreads"] = threadIDs
	}
	if !reader.send(nodeID, message) {
		return nil, errors.New("account channel offline")
	}
	_, err = reader.call(ctx, session, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "mira_account_history", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		return nil, err
	}
	if !reader.send(nodeID, map[string]any{"type": "appserver.message", "sessionId": sessionID,
		"payload": `{"method":"initialized"}`}) {
		return nil, errors.New("account channel offline")
	}
	opened = true
	return session, nil
}

func (reader *AccountReader) ReadAccount(ctx context.Context, nodeID, accountID, runtimeID string) (AccountSnapshot, error) {
	reader.mu.Lock()
	changing := false
	for _, login := range reader.logins {
		if login.nodeID == nodeID && login.accountID == accountID {
			changing = true
			break
		}
	}
	reader.mu.Unlock()
	if changing {
		return AccountSnapshot{}, errors.New("account credentials are changing")
	}
	readContext, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	session, err := reader.open(readContext, nodeID, accountID, runtimeID)
	if err != nil {
		return AccountSnapshot{}, err
	}
	defer reader.closeSession(session)
	first, err := reader.call(readContext, session, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return AccountSnapshot{}, err
	}
	firstRecord, _ := first.(map[string]any)
	account := firstRecord["account"]
	session.mu.Lock()
	session.changed = false
	session.mu.Unlock()
	var limits any
	if record, ok := account.(map[string]any); ok && stringValue(record["type"]) == "chatgpt" {
		limits, err = reader.call(readContext, session, "account/rateLimits/read", map[string]any{})
		if err != nil {
			return AccountSnapshot{}, err
		}
	}
	verified, err := reader.call(readContext, session, "account/read", map[string]any{"refreshToken": false})
	if err != nil {
		return AccountSnapshot{}, err
	}
	verifiedRecord, _ := verified.(map[string]any)
	session.mu.Lock()
	changed := session.changed
	session.mu.Unlock()
	firstJSON, _ := canonicalJSON(account)
	verifiedJSON, _ := canonicalJSON(verifiedRecord["account"])
	if changed || firstJSON != verifiedJSON {
		return AccountSnapshot{}, errors.New("account changed during sample")
	}
	return AccountSnapshot{Account: account, Limits: limits}, nil
}

// RPC is used by administrator account management and controlled thread
// handoff. It never invokes a model by itself; callers own method validation.
func (reader *AccountReader) RPC(ctx context.Context, nodeID, accountID, runtimeID, method string, params any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	session, err := reader.open(ctx, nodeID, accountID, runtimeID)
	if err != nil {
		return nil, err
	}
	defer reader.closeSession(session)
	return reader.call(ctx, session, method, params)
}

func (reader *AccountReader) Login(ctx context.Context, nodeID, accountID, runtimeID string, loginParams map[string]any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	reader.mu.Lock()
	for _, login := range reader.logins {
		if login.nodeID == nodeID && login.accountID == accountID {
			reader.mu.Unlock()
			return nil, errors.New("此账号已有登录操作，请等待完成或取消")
		}
	}
	reader.mu.Unlock()
	session, err := reader.open(ctx, nodeID, accountID, runtimeID, true)
	if err != nil {
		return nil, err
	}
	if err = reader.assertIdle(ctx, session); err != nil {
		reader.closeSession(session)
		return nil, err
	}
	reader.mu.Lock()
	reader.logins[session.id] = session
	reader.mu.Unlock()
	value, err := reader.call(ctx, session, "account/login/start", loginParams)
	if err != nil {
		reader.mu.Lock()
		delete(reader.logins, session.id)
		reader.mu.Unlock()
		reader.closeSession(session)
		return nil, err
	}
	result, _ := value.(map[string]any)
	session.mu.Lock()
	session.loginID, _ = result["loginId"].(string)
	session.mu.Unlock()
	if loginParams["type"] == "apiKey" {
		reader.mu.Lock()
		delete(reader.logins, session.id)
		reader.mu.Unlock()
		reader.closeSession(session)
		return map[string]any{"status": "completed"}, nil
	}
	clean := map[string]any{"loginSessionId": session.id}
	for _, key := range []string{"verificationUrl", "userCode", "loginId"} {
		if text, ok := result[key].(string); ok && len(text) <= 4096 {
			clean[key] = text
		}
	}
	time.AfterFunc(10*time.Minute, func() { _ = reader.CancelLogin(context.Background(), nodeID, accountID, session.id) })
	return clean, nil
}

func (reader *AccountReader) LoginStatus(nodeID, accountID, sessionID string) map[string]any {
	reader.mu.Lock()
	session := reader.logins[sessionID]
	reader.mu.Unlock()
	if session == nil || session.nodeID != nodeID || session.accountID != accountID {
		return map[string]any{"status": "expired"}
	}
	session.mu.Lock()
	complete, success, closed := session.loginComplete, session.loginSuccess, session.closed
	session.mu.Unlock()
	status := "pending"
	if complete {
		status = "failed"
		if success {
			status = "completed"
		}
	} else if closed {
		status = "failed"
	}
	if status != "pending" {
		reader.closeSession(session)
		reader.mu.Lock()
		delete(reader.logins, sessionID)
		reader.mu.Unlock()
	}
	return map[string]any{"status": status}
}

func (reader *AccountReader) call(ctx context.Context, session *accountSession, method string, params any) (any, error) {
	session.mu.Lock()
	if session.closed {
		err := session.closeError
		session.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("account channel closed")
	}
	session.nextID++
	requestID := session.nextID
	id := fmt.Sprintf("%d", requestID)
	response := make(chan accountResponse, 1)
	session.pending[id] = response
	session.mu.Unlock()
	defer func() { session.mu.Lock(); delete(session.pending, id); session.mu.Unlock() }()
	payload, err := json.Marshal(map[string]any{"id": requestID, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	if !reader.send(session.nodeID, map[string]any{"type": "appserver.message", "sessionId": session.id, "payload": string(payload)}) {
		session.mu.Lock()
		delete(session.pending, id)
		session.mu.Unlock()
		return nil, errors.New("account channel offline")
	}
	select {
	case value := <-response:
		if method == "mira/thread/unload" && value.err != nil && value.detail != "" && len(value.detail) <= 2048 {
			if value.code == "-32601" {
				return nil, errors.New("请升级旧账号的 Codex 运行包，以支持单独交接对话")
			}
			return nil, fmt.Errorf("无法交接此对话：%s", value.detail)
		}
		return value.result, value.err
	case <-session.done:
		session.mu.Lock()
		err := session.closeError
		session.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("account channel closed")
	case <-ctx.Done():
		return nil, errors.New("account channel closed")
	}
}

func (reader *AccountReader) closeSession(session *accountSession) {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return
	}
	session.closed = true
	close(session.done)
	session.pending = map[string]chan accountResponse{}
	session.mu.Unlock()
	reader.mu.Lock()
	if reader.sessions[session.id] == session {
		delete(reader.sessions, session.id)
	}
	reader.mu.Unlock()
	reader.send(session.nodeID, map[string]any{"type": "appserver.close", "sessionId": session.id})
}

func (reader *AccountReader) CloseNode(nodeID string) {
	reader.mu.Lock()
	var sessions []*accountSession
	for _, session := range reader.sessions {
		if nodeID == "" || session.nodeID == nodeID {
			sessions = append(sessions, session)
		}
	}
	reader.mu.Unlock()
	for _, session := range sessions {
		reader.closeSession(session)
	}
}

func (reader *AccountReader) Close() { reader.CloseNode("") }

func rpcID(value any) string {
	switch typed := value.(type) {
	case json.Number:
		return string(typed)
	case float64:
		return fmt.Sprintf("%g", typed)
	case string:
		return typed
	case nil:
		return "null"
	default:
		return fmt.Sprint(typed)
	}
}
