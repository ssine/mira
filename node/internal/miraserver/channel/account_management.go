package channel

import (
	"context"
	"errors"
	"time"
)

// Retire proves the old process has exited before a different account can load
// the same thread. Unsubscribing a WebSocket alone does not unload Codex threads.
func (reader *AccountReader) Retire(ctx context.Context, nodeID, accountID, runtimeID string) error {
	key := nodeID + ":" + accountID + ":" + runtimeID
	reader.mu.Lock()
	retiredAt := reader.retired[key]
	reader.mu.Unlock()
	// A parent handoff also stops its children's old process. The next child
	// resume may arrive before the heartbeat reports that exit. Reuse the
	// acknowledged process exit instead of trying to reopen the dead instance.
	if runtimeID != "" && !retiredAt.IsZero() && time.Since(retiredAt) < 5*time.Minute {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	session, err := reader.open(ctx, nodeID, accountID, runtimeID, true)
	if err != nil {
		return err
	}
	defer reader.closeSession(session)
	if err := reader.assertIdle(ctx, session); err != nil {
		return err
	}
	response := make(chan accountResponse, 1)
	session.mu.Lock()
	session.pending["1"] = response
	session.retiring = true
	session.mu.Unlock()
	if !reader.send(nodeID, map[string]any{"type": "account.retire", "sessionId": session.id, "nodeAccountId": accountID, "runtimeId": runtimeID}) {
		return errors.New("account channel offline")
	}
	// Retiring the process closes its WebSocket before the control ack. Keep a
	// separate ack registration so that closure cannot be mistaken for success.
	select {
	case result := <-response:
		if result.err == nil && runtimeID != "" {
			reader.mu.Lock()
			if reader.retired == nil {
				reader.retired = map[string]time.Time{}
			}
			for old, at := range reader.retired {
				if time.Since(at) >= 5*time.Minute {
					delete(reader.retired, old)
				}
			}
			if len(reader.retired) >= 1024 {
				for old := range reader.retired {
					delete(reader.retired, old)
					break
				}
			}
			reader.retired[key] = time.Now()
			reader.mu.Unlock()
		}
		return result.err
	case <-ctx.Done():
		return errors.New("未确认旧账号运行实例已退出，暂不能切换")
	}
}

// Recheck all loaded threads after acquiring the Node-side credential gate.
// Reading only the currently visible browser thread would miss subagents.
func (reader *AccountReader) assertIdle(ctx context.Context, session *accountSession) error {
	cursor := ""
	for page := 0; page < 64; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		value, err := reader.call(ctx, session, "thread/loaded/list", params)
		if err != nil {
			return errors.New("无法确认此账号是否空闲，请停止运行实例后重试")
		}
		result, _ := value.(map[string]any)
		ids, _ := result["data"].([]any)
		for _, raw := range ids {
			id, ok := raw.(string)
			if !ok {
				return errors.New("无法确认运行中的对话")
			}
			value, err := reader.call(ctx, session, "thread/read", map[string]any{"threadId": id, "includeTurns": false})
			if err != nil {
				return errors.New("无法确认运行中的对话")
			}
			result, _ := value.(map[string]any)
			thread, _ := result["thread"].(map[string]any)
			status, _ := thread["status"].(map[string]any)
			// Codex reports a finished failed turn as systemError. Active tasks
			// and pending approvals take precedence over that status upstream.
			if status["type"] != "idle" && status["type"] != "notLoaded" && status["type"] != "systemError" {
				return errors.New("此账号仍有对话或子任务运行，请结束后再修改凭据")
			}
		}
		cursor, _ = result["nextCursor"].(string)
		if cursor == "" {
			return nil
		}
	}
	return errors.New("运行中对话过多，无法确认账号空闲")
}

func (reader *AccountReader) Logout(ctx context.Context, nodeID, accountID, runtimeID string) error {
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	session, err := reader.open(ctx, nodeID, accountID, runtimeID, true)
	if err != nil {
		return err
	}
	defer reader.closeSession(session)
	if err := reader.assertIdle(ctx, session); err != nil {
		return err
	}
	_, err = reader.call(ctx, session, "account/logout", map[string]any{})
	return err
}

func (reader *AccountReader) CancelLogin(ctx context.Context, nodeID, accountID, sessionID string) error {
	reader.mu.Lock()
	session := reader.logins[sessionID]
	if session == nil || session.nodeID != nodeID || session.accountID != accountID {
		reader.mu.Unlock()
		return nil
	}
	delete(reader.logins, sessionID)
	reader.mu.Unlock()
	defer reader.closeSession(session)
	session.mu.Lock()
	id, closed := session.loginID, session.closed
	session.mu.Unlock()
	if id == "" || closed {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	_, err := reader.call(ctx, session, "account/login/cancel", map[string]any{"loginId": id})
	return err
}

func (reader *AccountReader) Configure(ctx context.Context, nodeID, accountID string, params map[string]any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	id, err := randomUUID()
	if err != nil {
		return nil, err
	}
	response := make(chan accountResponse, 1)
	session := &accountSession{id: id, nodeID: nodeID, accountID: accountID, pending: map[string]chan accountResponse{"1": response}, done: make(chan struct{})}
	reader.mu.Lock()
	if len(reader.sessions) >= 64 {
		reader.mu.Unlock()
		return nil, errors.New("account operation capacity reached")
	}
	reader.sessions[id] = session
	reader.mu.Unlock()
	defer reader.closeSession(session)
	if !reader.send(nodeID, map[string]any{"type": "account.configure", "sessionId": id, "nodeAccountId": accountID, "params": params}) {
		return nil, errors.New("account channel offline")
	}
	select {
	case value := <-response:
		return value.result, value.err
	case <-ctx.Done():
		return nil, errors.New("account configuration timed out; inspect the account before retrying")
	case <-session.done:
		return nil, errors.New("account channel closed")
	}
}
