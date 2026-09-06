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
}

type accountSession struct {
	mu      sync.Mutex
	id      string
	nodeID  string
	nextID  int64
	pending map[string]chan accountResponse
	changed bool
	closed  bool
	done    chan struct{}
}

type AccountReader struct {
	mu       sync.Mutex
	send     func(string, any) bool
	timeout  time.Duration
	sessions map[string]*accountSession
}

type AccountSnapshot struct {
	Account any `json:"account"`
	Limits  any `json:"limits"`
}

func NewAccountReader(send func(string, any) bool, timeout time.Duration) *AccountReader {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return &AccountReader{send: send, timeout: timeout, sessions: map[string]*accountSession{}}
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
		reader.closeSession(session)
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
			pending <- accountResponse{err: errors.New("account RPC failed")}
		} else {
			pending <- accountResponse{result: value["result"]}
		}
	}
	return true
}

func (reader *AccountReader) Read(ctx context.Context, nodeID string) (AccountSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return AccountSnapshot{}, errors.New("account sampling stopped")
	}
	sessionID, err := randomUUID()
	if err != nil {
		return AccountSnapshot{}, err
	}
	session := &accountSession{id: sessionID, nodeID: nodeID, pending: map[string]chan accountResponse{}, done: make(chan struct{})}
	reader.mu.Lock()
	reader.sessions[sessionID] = session
	reader.mu.Unlock()
	defer reader.closeSession(session)
	readContext, cancel := context.WithTimeout(ctx, reader.timeout)
	defer cancel()
	if !reader.send(nodeID, map[string]any{"type": "appserver.open", "sessionId": sessionID}) {
		return AccountSnapshot{}, errors.New("account channel offline")
	}
	_, err = reader.call(readContext, session, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "mira_account_history", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	})
	if err != nil {
		return AccountSnapshot{}, err
	}
	if !reader.send(nodeID, map[string]any{"type": "appserver.message", "sessionId": sessionID,
		"payload": `{"method":"initialized"}`}) {
		return AccountSnapshot{}, errors.New("account channel offline")
	}
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

func (reader *AccountReader) call(ctx context.Context, session *accountSession, method string, params any) (any, error) {
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		return nil, errors.New("account channel closed")
	}
	session.nextID++
	id := fmt.Sprintf("%d", session.nextID)
	response := make(chan accountResponse, 1)
	session.pending[id] = response
	session.mu.Unlock()
	payload, err := json.Marshal(map[string]any{"id": session.nextID, "method": method, "params": params})
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
		return value.result, value.err
	case <-session.done:
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
