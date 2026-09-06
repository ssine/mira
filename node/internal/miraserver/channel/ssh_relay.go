package channel

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const (
	defaultSSHMaxSessions = 128
	defaultSSHMaxPerNode  = 32
	sshFrameLimit         = 64 * 1024
	sshConnectTimeout     = 30 * time.Second
)

var sshKeyPattern = regexp.MustCompile(`^ssh-ed25519 ([A-Za-z0-9+/]+={0,2})$`)

type SSHNodeSender interface {
	IsConnected(string) bool
	SendToNode(string, any) error
	TrySendToNode(string, any) bool
}

type SSHOptions struct {
	MaxSessions int
	MaxPerNode  int
}

type sshKeys struct {
	hostKey      string
	clientKey    string
	credentialID string
	enabled      bool
	runtime      map[string]any
}

type sshSession struct {
	id                 string
	sourceNodeID       string
	targetNodeID       string
	sourceCredentialID string
	targetCredentialID string
	principal          *foundation.Principal
	claimed            map[string]bool
	sockets            map[string]*socket
	timer              *time.Timer
	started            bool
	ready              chan struct{}
	done               chan struct{}
}

type SSHRelay struct {
	db          Database
	auth        Authenticator
	nodes       SSHNodeSender
	audit       AuditFunc
	upgrader    websocket.Upgrader
	maxSessions int
	maxPerNode  int
	mu          sync.Mutex
	sessions    map[string]*sshSession
	closed      bool
}

func NewSSHRelay(db Database, auth Authenticator, nodeChannel SSHNodeSender, audit AuditFunc, options SSHOptions) *SSHRelay {
	maxSessions := options.MaxSessions
	if maxSessions <= 0 {
		maxSessions = sessionLimit(os.Getenv("MIRA_SSH_MAX_SESSIONS"), defaultSSHMaxSessions, 4096)
	}
	maxPerNode := options.MaxPerNode
	if maxPerNode <= 0 {
		maxPerNode = sessionLimit(os.Getenv("MIRA_SSH_MAX_SESSIONS_PER_NODE"), defaultSSHMaxPerNode, 4096)
	}
	if maxPerNode > maxSessions {
		maxPerNode = maxSessions
	}
	return &SSHRelay{
		db: db, auth: auth, nodes: nodeChannel, audit: audit,
		maxSessions: maxSessions, maxPerNode: maxPerNode, sessions: map[string]*sshSession{},
		upgrader: websocket.Upgrader{ReadBufferSize: sshFrameLimit, WriteBufferSize: sshFrameLimit,
			EnableCompression: false, CheckOrigin: func(*http.Request) bool { return true }},
	}
}

func sessionLimit(value string, fallback, ceiling int) int {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 || parsed > ceiling {
		return fallback
	}
	return parsed
}

func ValidSSHKey(value string) bool {
	match := sshKeyPattern.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(match[1])
	if err != nil || len(raw) != 51 || base64.StdEncoding.EncodeToString(raw) != match[1] {
		return false
	}
	return binary.BigEndian.Uint32(raw[:4]) == 11 && string(raw[4:15]) == "ssh-ed25519" && binary.BigEndian.Uint32(raw[15:19]) == 32
}

func sshFailure(status int, message string) Result {
	return Result{Status: status, Body: map[string]any{"error": message, "code": "ssh_unavailable"}}
}

func (relay *SSHRelay) keys(ctx context.Context, nodeID string) (*sshKeys, error) {
	var value sshKeys
	var runtime []byte
	err := relay.db.QueryRow(ctx, `SELECT keys.host_key, keys.client_key, credentials.credential_id::text,
	      COALESCE((nodes.capabilities->>'ssh')::boolean, false), COALESCE(nodes.machine_status->'ssh', '{}'::jsonb)
	    FROM mira_node_ssh_keys keys JOIN mira_node_credentials credentials USING (credential_id)
	    JOIN codex_nodes nodes USING (node_id)
	    WHERE nodes.node_id = $1::uuid AND nodes.approval_status = 'approved' AND credentials.revoked_at IS NULL`, nodeID,
	).Scan(&value.hostKey, &value.clientKey, &value.credentialID, &value.enabled, &runtime)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(runtime, &value.runtime); err != nil {
		return nil, err
	}
	return &value, nil
}

func (relay *SSHRelay) Publish(ctx context.Context, principal *foundation.Principal, body map[string]any) (Result, error) {
	hostKey, hostOK := body["hostKey"].(string)
	clientKey, clientOK := body["clientKey"].(string)
	if !hostOK || !clientOK || !ValidSSHKey(hostKey) || !ValidSSHKey(clientKey) || hostKey == clientKey {
		return sshFailure(400, "two distinct Ed25519 public keys are required"), nil
	}
	var savedHost, savedClient string
	err := relay.db.QueryRow(ctx, `INSERT INTO mira_node_ssh_keys (credential_id, host_key, client_key)
	    SELECT credential_id, $2, $3 FROM mira_node_credentials
	    WHERE credential_id = $1::uuid AND revoked_at IS NULL
	    ON CONFLICT (credential_id) DO UPDATE SET host_key = mira_node_ssh_keys.host_key
	    RETURNING host_key, client_key`, principal.CredentialID, hostKey, clientKey,
	).Scan(&savedHost, &savedClient)
	if err == pgx.ErrNoRows {
		return sshFailure(403, "Node credential is no longer approved"), nil
	}
	if err != nil {
		return Result{}, err
	}
	if savedHost != hostKey || savedClient != clientKey {
		return sshFailure(409, "SSH key change requires a new Node credential"), nil
	}
	return Result{Status: 200, Body: map[string]any{"status": "registered"}}, nil
}

func sshUsername(keys *sshKeys) (string, string, bool) {
	backend := stringValue(keys.runtime["backend"])
	username := "mira"
	if backend == "openssh" {
		username = stringValue(keys.runtime["username"])
	}
	if backend == "" {
		backend = "builtin"
	}
	if username == "" || len(username) > 256 {
		return "", backend, false
	}
	for _, character := range username {
		if character <= 0x1f || character == 0x7f || character == '"' {
			return "", backend, false
		}
	}
	return username, backend, true
}

func (relay *SSHRelay) Describe(ctx context.Context, targetNodeID string) (Result, error) {
	target, err := relay.keys(ctx, targetNodeID)
	if err != nil {
		return Result{}, err
	}
	if target == nil || !target.enabled {
		return sshFailure(409, "target Node does not support SSH yet"), nil
	}
	username, backend, ok := sshUsername(target)
	if !ok {
		return sshFailure(409, "target Node has an invalid SSH account"), nil
	}
	return Result{Status: 200, Body: map[string]any{"hostKey": target.hostKey, "username": username, "protocolVersion": 1, "backend": backend}}, nil
}

func (relay *SSHRelay) Create(ctx context.Context, principal *foundation.Principal, targetNodeID string, request *http.Request) (Result, error) {
	source, err := relay.keys(ctx, principal.NodeID)
	if err != nil {
		return Result{}, err
	}
	target, err := relay.keys(ctx, targetNodeID)
	if err != nil {
		return Result{}, err
	}
	if source == nil || source.credentialID != principal.CredentialID {
		return sshFailure(409, "publish this Node's SSH public keys first"), nil
	}
	if target == nil || !target.enabled || !relay.nodes.IsConnected(targetNodeID) {
		return sshFailure(409, "target Node is offline or does not support SSH yet"), nil
	}
	username, _, ok := sshUsername(target)
	if !ok {
		return sshFailure(409, "target Node has an invalid SSH account"), nil
	}
	sessionID, err := randomUUID()
	if err != nil {
		return Result{}, err
	}
	session := &sshSession{id: sessionID, sourceNodeID: principal.NodeID, targetNodeID: targetNodeID,
		sourceCredentialID: source.credentialID, targetCredentialID: target.credentialID, principal: principal,
		claimed: map[string]bool{}, sockets: map[string]*socket{}, ready: make(chan struct{}), done: make(chan struct{})}
	relay.mu.Lock()
	if relay.closed || len(relay.sessions) >= relay.maxSessions || relay.sessionCountLocked(principal.NodeID) >= relay.maxPerNode || relay.sessionCountLocked(targetNodeID) >= relay.maxPerNode {
		relay.mu.Unlock()
		return sshFailure(429, "SSH session limit reached"), nil
	}
	relay.sessions[sessionID] = session
	session.timer = time.AfterFunc(sshConnectTimeout, func() { relay.end(sessionID, "connect timeout") })
	relay.mu.Unlock()
	if relay.audit != nil {
		if err := relay.audit(ctx, foundation.AuditEvent{Action: "ssh.requested", Principal: principal, TargetNodeID: targetNodeID,
			Request: request, Metadata: map[string]any{"sessionId": sessionID, "protocolVersion": 1}}); err != nil {
			relay.end(sessionID, "open failed")
			return Result{}, err
		}
	}
	relay.mu.Lock()
	current := relay.sessions[sessionID] == session
	relay.mu.Unlock()
	if !current {
		return sshFailure(409, "SSH session cancelled"), nil
	}
	if err := relay.nodes.SendToNode(targetNodeID, map[string]any{"type": "ssh.open", "sessionId": sessionID,
		"sourceNodeId": principal.NodeID, "clientPublicKey": source.clientKey}); err != nil {
		relay.end(sessionID, "open failed")
		return Result{}, err
	}
	return Result{Status: 201, Body: map[string]any{"sessionId": sessionID, "hostKey": target.hostKey, "username": username, "protocolVersion": 1}}, nil
}

func (relay *SSHRelay) SessionCount(nodeID string) int {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	return relay.sessionCountLocked(nodeID)
}
func (relay *SSHRelay) sessionCountLocked(nodeID string) int {
	count := 0
	for _, session := range relay.sessions {
		if session.sourceNodeID == nodeID || session.targetNodeID == nodeID {
			count++
		}
	}
	return count
}

func (relay *SSHRelay) serveUpgrade(response http.ResponseWriter, request *http.Request, sessionID, side string) {
	if !hasWebSocketProtocol(request, "mira-ssh-v1") {
		http.Error(response, "rejected", 400)
		return
	}
	token, ok := ProtocolToken(request)
	if !ok {
		http.Error(response, "rejected", 403)
		return
	}
	principal, err := relay.auth.AuthenticateNodeToken(request.Context(), token, "ssh")
	if err != nil || principal == nil || principal.Revoked {
		http.Error(response, "rejected", 403)
		return
	}
	relay.mu.Lock()
	session := relay.sessions[sessionID]
	if session == nil || principal.NodeID != sideNodeID(session, side) || principal.CredentialID != sideCredentialID(session, side) {
		relay.mu.Unlock()
		http.Error(response, "rejected", 404)
		return
	}
	if session.claimed[side] {
		relay.mu.Unlock()
		http.Error(response, "rejected", 409)
		return
	}
	session.claimed[side] = true
	relay.mu.Unlock()
	connection, err := relay.upgrader.Upgrade(response, request, http.Header{"Sec-WebSocket-Protocol": []string{"mira-ssh-v1"}})
	if err != nil {
		relay.mu.Lock()
		if relay.sessions[sessionID] == session {
			delete(session.claimed, side)
		}
		relay.mu.Unlock()
		return
	}
	socket := newSocket(connection, sshFrameLimit)
	relay.mu.Lock()
	if relay.sessions[sessionID] != session {
		relay.mu.Unlock()
		socket.close(websocket.ClosePolicyViolation, "session closed")
		return
	}
	session.sockets[side] = socket
	if session.ready == nil {
		session.ready = make(chan struct{})
	}
	if session.done == nil {
		session.done = make(chan struct{})
	}
	ready := len(session.sockets) == 2 && !session.started
	if ready {
		session.started = true
		session.timer.Stop()
		close(session.ready)
	}
	relay.mu.Unlock()
	other := "source"
	if side == "source" {
		other = "target"
	}
	go relay.pipe(sessionID, side, other)
}

func sideNodeID(session *sshSession, side string) string {
	if side == "source" {
		return session.sourceNodeID
	}
	return session.targetNodeID
}
func sideCredentialID(session *sshSession, side string) string {
	if side == "source" {
		return session.sourceCredentialID
	}
	return session.targetCredentialID
}

func (relay *SSHRelay) pipe(sessionID, from, to string) {
	relay.mu.Lock()
	session := relay.sessions[sessionID]
	if session == nil {
		relay.mu.Unlock()
		return
	}
	source, ready, done := session.sockets[from], session.ready, session.done
	relay.mu.Unlock()
	for {
		messageType, payload, err := source.connection.ReadMessage()
		if err != nil {
			relay.end(sessionID, "transport closed")
			return
		}
		if messageType != websocket.BinaryMessage {
			relay.end(sessionID, "binary frames required")
			return
		}
		if len(payload) > sshFrameLimit {
			relay.end(sessionID, "frame too large")
			return
		}
		select {
		case <-ready:
		case <-done:
			return
		}
		relay.mu.Lock()
		current := relay.sessions[sessionID]
		var target *socket
		if current != nil {
			target = current.sockets[to]
		}
		relay.mu.Unlock()
		if target == nil {
			relay.end(sessionID, "transport closed")
			return
		}
		if err := target.write(websocket.BinaryMessage, payload); err != nil {
			relay.end(sessionID, "transport error")
			return
		}
	}
}

func (relay *SSHRelay) end(sessionID, reason string) {
	relay.mu.Lock()
	session := relay.sessions[sessionID]
	if session == nil {
		relay.mu.Unlock()
		return
	}
	delete(relay.sessions, sessionID)
	session.timer.Stop()
	if session.done != nil {
		close(session.done)
	}
	sockets := make([]*socket, 0, len(session.sockets))
	for _, socket := range session.sockets {
		sockets = append(sockets, socket)
	}
	relay.mu.Unlock()
	for _, socket := range sockets {
		socket.close(websocket.CloseNormalClosure, reason)
	}
	relay.nodes.TrySendToNode(session.targetNodeID, map[string]any{"type": "ssh.close", "sessionId": sessionID})
	if relay.audit != nil {
		go func() {
			_ = relay.audit(context.Background(), foundation.AuditEvent{Action: "ssh.closed", Principal: session.principal,
				TargetNodeID: session.targetNodeID, Metadata: map[string]any{"sessionId": sessionID, "reason": reason}})
		}()
	}
}

func (relay *SSHRelay) DisconnectNode(nodeID string) {
	relay.mu.Lock()
	var sessions []string
	for id, session := range relay.sessions {
		if session.sourceNodeID == nodeID || session.targetNodeID == nodeID {
			sessions = append(sessions, id)
		}
	}
	relay.mu.Unlock()
	for _, id := range sessions {
		relay.end(id, "Node disconnected or revoked")
	}
}

func (relay *SSHRelay) Close() {
	relay.mu.Lock()
	relay.closed = true
	ids := make([]string, 0, len(relay.sessions))
	for id := range relay.sessions {
		ids = append(ids, id)
	}
	relay.mu.Unlock()
	for _, id := range ids {
		relay.end(id, "Server stopping")
	}
}

func (relay *SSHRelay) String() string {
	return fmt.Sprintf("SSHRelay(%d/%d)", relay.maxSessions, relay.maxPerNode)
}
