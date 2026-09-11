package channel

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

var (
	nodeConnectPattern = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/connect$`)
	appServerPattern   = regexp.MustCompile(`(?i)^/v1/nodes/([0-9a-f-]{36})/app-server$`)
	sshSessionPattern  = regexp.MustCompile(`(?i)^/v1/ssh/sessions/([0-9a-f-]{36})/(source|target)$`)
)

type pendingCall struct {
	nodeID string
	done   chan invocationResult
}

type invocationResult struct {
	value any
	err   error
}

type proxy struct {
	mu                     sync.Mutex
	targetNodeID           string
	nodeAccountID          string
	accountID              string
	runtimeID              string
	defaultAccount         bool
	callerNodeID           string
	actorKey               string
	sessionID              string
	socket                 *socket
	storeID                string
	target                 *nodes.Node
	threadID               string
	threadRequestBindings  map[string]*string
	threadRequestMethods   map[string]string
	idempotentThreadStarts map[string]string
	boundThreadIDs         map[string]bool
	ephemeralStartRequests map[string]bool
	ephemeralThreadIDs     map[string]bool
	toolFreeStartRequests  map[string]bool
	toolFreeThreadIDs      map[string]bool
	clientClosed           bool
	abandoning             bool
	detachTimer            *time.Timer
	nodeMessages           chan map[string]any
	nodeMessagesDone       chan struct{}
	pendingNodeBytes       int
}

type threadStartWaiter struct {
	proxy *proxy
	id    any
}

type activeThreadStart struct {
	owner   *proxy
	waiters []threadStartWaiter
}

type Channel struct {
	db          Database
	nodes       NodeRegistry
	auth        Authenticator
	audit       AuditFunc
	logger      *slog.Logger
	upgrader    websocket.Upgrader
	detachGrace time.Duration

	mu           sync.Mutex
	nodeSockets  map[string]*socket
	pending      map[string]pendingCall
	proxies      map[string]*proxy
	threadStarts map[string]*activeThreadStart
	statusWrites map[string]chan struct{}
	closed       bool

	capabilities *CapabilityService
	accounts     *AccountReader
	ssh          *SSHRelay
	trustProxy   bool
}

func New(options Options) (*Channel, error) {
	if options.Database == nil || options.Nodes == nil || options.Auth == nil {
		return nil, fmt.Errorf("channel database, Node registry, and authenticator are required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	audit := options.Audit
	if audit == nil {
		audit = func(ctx context.Context, event foundation.AuditEvent) error {
			return foundation.AppendAudit(ctx, options.Database, event, options.TrustProxyHeaders)
		}
	}
	detachGrace := options.DetachedThreadStartGrace
	if detachGrace <= 0 {
		detachGrace = DefaultDetachedThreadStartGrace
	}
	channel := &Channel{
		db: options.Database, nodes: options.Nodes, auth: options.Auth, audit: audit,
		logger: logger, detachGrace: detachGrace, trustProxy: options.TrustProxyHeaders,
		nodeSockets: map[string]*socket{}, pending: map[string]pendingCall{}, proxies: map[string]*proxy{},
		threadStarts: map[string]*activeThreadStart{}, statusWrites: map[string]chan struct{}{},
		upgrader: websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096,
			EnableCompression: false, CheckOrigin: func(*http.Request) bool { return true }},
	}
	channel.accounts = NewAccountReader(channel.TrySendToNode, options.AccountTimeout)
	channel.capabilities = NewCapabilityService(options.Nodes, channel, audit)
	channel.ssh = NewSSHRelay(options.Database, options.Auth, channel, audit, SSHOptions{
		MaxSessions: options.SSHMaxSessions, MaxPerNode: options.SSHMaxSessionsPerNode,
	})
	return channel, nil
}

func (channel *Channel) Capabilities() *CapabilityService { return channel.capabilities }
func (channel *Channel) Accounts() *AccountReader         { return channel.accounts }
func (channel *Channel) SSH() *SSHRelay                   { return channel.ssh }

func (channel *Channel) UpdateProxyDesiredAppServer(nodeID string, desired map[string]any) {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	for _, proxy := range channel.proxies {
		if proxy.targetNodeID == nodeID {
			proxy.mu.Lock()
			if proxy.target == nil {
				proxy.mu.Unlock()
				continue
			}
			copy := *proxy.target
			// Workspace policy is Node-wide; account runtime configuration is not.
			copy.DesiredAppServer = make(map[string]any, len(proxy.target.DesiredAppServer))
			for key, value := range proxy.target.DesiredAppServer {
				copy.DesiredAppServer[key] = value
			}
			for _, key := range []string{"defaultCwd", "developerInstructionsFile"} {
				if value, exists := desired[key]; exists {
					copy.DesiredAppServer[key] = value
				}
			}
			proxy.target = &copy
			proxy.mu.Unlock()
		}
	}
}

func (channel *Channel) IsConnected(nodeID string) bool {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	_, ok := channel.nodeSockets[nodeID]
	return ok
}

// Handles reports whether request targets one of the WebSocket routes owned by
// Channel. It lets the main Server route upgrades without duplicating patterns.
func (channel *Channel) Handles(request *http.Request) bool {
	if request == nil || request.URL == nil {
		return false
	}
	path := request.URL.Path
	return nodeConnectPattern.MatchString(path) || appServerPattern.MatchString(path) || sshSessionPattern.MatchString(path)
}

func (channel *Channel) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if match := sshSessionPattern.FindStringSubmatch(request.URL.Path); match != nil {
		channel.ssh.serveUpgrade(response, request, match[1], match[2])
		return
	}
	if match := nodeConnectPattern.FindStringSubmatch(request.URL.Path); match != nil {
		channel.serveNodeUpgrade(response, request, match[1])
		return
	}
	if match := appServerPattern.FindStringSubmatch(request.URL.Path); match != nil {
		channel.serveProxyUpgrade(response, request, match[1])
		return
	}
	http.NotFound(response, request)
}

func (channel *Channel) serveNodeUpgrade(response http.ResponseWriter, request *http.Request, nodeID string) {
	if !hasWebSocketProtocol(request, "mira-node-v1") {
		http.Error(response, "websocket subprotocol required", http.StatusBadRequest)
		return
	}
	token, ok := ProtocolToken(request)
	if !ok {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	principal, err := channel.auth.AuthenticateNodeToken(request.Context(), token, "node")
	if err != nil {
		channel.logger.Error("Node websocket authorization failed", "error", err)
		http.Error(response, "unauthorized", 401)
		return
	}
	if principal == nil {
		http.Error(response, "unauthorized", 401)
		return
	}
	if principal.Revoked || principal.NodeID != nodeID {
		http.Error(response, "forbidden", 403)
		return
	}
	connection, err := channel.upgrader.Upgrade(response, request, http.Header{"Sec-WebSocket-Protocol": []string{"mira-node-v1"}})
	if err != nil {
		return
	}
	channel.attachNode(nodeID, newSocket(connection, MaxChannelPayload))
}

func (channel *Channel) serveProxyUpgrade(response http.ResponseWriter, request *http.Request, targetNodeID string) {
	if !hasWebSocketProtocol(request, "mira-client-v1") {
		http.Error(response, "websocket subprotocol required", http.StatusBadRequest)
		return
	}
	storeID, ok := safeStoreID(request.URL.Query().Get("storeId"), request.URL.Query().Has("storeId"))
	if !ok {
		http.Error(response, "bad request", http.StatusBadRequest)
		return
	}
	var principal *foundation.Principal
	var err error
	if token, found := ProtocolToken(request); found {
		principal, err = channel.auth.AuthenticateNodeToken(request.Context(), token, "app-server")
	} else {
		principal, err = channel.auth.Authenticate(request.Context(), request, "app-server")
	}
	if err != nil {
		channel.logger.Error("App Server websocket authorization failed", "error", err)
		http.Error(response, "unauthorized", 401)
		return
	}
	if principal == nil {
		http.Error(response, "unauthorized", 401)
		return
	}
	if principal.Revoked || !channel.auth.Permits(principal, "trusted") {
		http.Error(response, "forbidden", 403)
		return
	}
	if principal.Kind == "admin" && !sameBrowserOrigin(request, channel.trustProxy) {
		http.Error(response, "forbidden origin", http.StatusForbidden)
		return
	}
	target, err := channel.nodes.Get(request.Context(), targetNodeID, false)
	if err != nil {
		channel.logger.Error("read App Server target", "error", err)
		http.Error(response, "internal server error", 500)
		return
	}
	if target == nil {
		http.Error(response, "not found", 404)
		return
	}
	if enabled, _ := target.Capabilities["appServer"].(bool); !enabled {
		http.Error(response, "conflict", 409)
		return
	}
	account, err := nodes.SelectAccount(target, request.URL.Query().Get("nodeAccountId"))
	if err != nil {
		http.Error(response, err.Error(), http.StatusConflict)
		return
	}
	if account != nil {
		target = nodes.AccountNode(target, *account)
	}
	connection, err := channel.upgrader.Upgrade(response, request, http.Header{"Sec-WebSocket-Protocol": []string{"mira-client-v1"}})
	if err != nil {
		return
	}
	channel.attachProxy(targetNodeID, newSocket(connection, MaxChannelPayload), principal, storeID, target)
}

func sameBrowserOrigin(request *http.Request, trustProxy bool) bool {
	origin := request.Header.Get("Origin")
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	expectedScheme := "http"
	if request.TLS != nil {
		expectedScheme = "https"
	} else if trustProxy {
		forwarded := strings.TrimSpace(strings.Split(request.Header.Get("X-Forwarded-Proto"), ",")[0])
		if forwarded == "http" || forwarded == "https" {
			expectedScheme = forwarded
		}
	}
	return strings.EqualFold(parsed.Scheme, expectedScheme) && strings.EqualFold(parsed.Host, request.Host)
}

func (channel *Channel) attachNode(nodeID string, connection *socket) {
	channel.mu.Lock()
	previous := channel.nodeSockets[nodeID]
	channel.nodeSockets[nodeID] = connection
	channel.mu.Unlock()
	if previous != nil {
		channel.rejectNodeWork(nodeID)
		previous.close(websocket.CloseServiceRestart, "replaced")
	}
	channel.writeNodeStatus(nodeID, map[string]any{"connected": true, "connectedAt": channelTimestamp(), "protocolVersion": 1})
	go channel.readNode(nodeID, connection)
}

func (channel *Channel) readNode(nodeID string, connection *socket) {
	defer func() {
		channel.mu.Lock()
		if channel.nodeSockets[nodeID] != connection {
			channel.mu.Unlock()
			return
		}
		delete(channel.nodeSockets, nodeID)
		channel.mu.Unlock()
		channel.writeNodeStatus(nodeID, map[string]any{"connected": false, "disconnectedAt": channelTimestamp(), "protocolVersion": 1})
		channel.rejectNodeWork(nodeID)
	}()
	for {
		_, payload, err := connection.connection.ReadMessage()
		if err != nil {
			return
		}
		message, err := decodeObject(payload)
		if err != nil {
			connection.close(websocket.CloseInvalidFramePayloadData, "invalid JSON")
			return
		}
		if err := channel.handleNodeMessage(nodeID, connection, message); err != nil {
			channel.logger.Error("Node channel message failed", "nodeId", nodeID, "error", err)
			connection.close(websocket.CloseInternalServerErr, "node channel message failed")
			return
		}
	}
}

func (channel *Channel) handleNodeMessage(nodeID string, connection *socket, message map[string]any) error {
	if channel.accounts.Handle(nodeID, message) {
		return nil
	}
	messageType, _ := message["type"].(string)
	if messageType == "response" {
		requestID, ok := message["requestId"].(string)
		if !ok {
			return nil
		}
		channel.mu.Lock()
		pending, found := channel.pending[requestID]
		if found && pending.nodeID == nodeID {
			delete(channel.pending, requestID)
		} else {
			found = false
		}
		channel.mu.Unlock()
		if !found {
			return nil
		}
		if success, _ := message["ok"].(bool); success {
			pending.done <- invocationResult{value: message["result"]}
		} else {
			errorRecord, _ := message["error"].(map[string]any)
			text := stringValue(errorRecord["message"])
			if text == "" {
				text = "Node request failed"
			}
			pending.done <- invocationResult{err: channelError(text, 400, "node_request_failed")}
		}
		return nil
	}
	sessionID, _ := message["sessionId"].(string)
	if sessionID == "" {
		return nil
	}
	channel.mu.Lock()
	proxy := channel.proxies[sessionID]
	channel.mu.Unlock()
	if proxy == nil || proxy.targetNodeID != nodeID {
		return nil
	}
	if proxy.nodeMessages != nil {
		size := len(stringValue(message["payload"]))
		proxy.mu.Lock()
		fits := proxy.pendingNodeBytes+size <= MaxChannelPayload
		if fits {
			proxy.pendingNodeBytes += size
		}
		proxy.mu.Unlock()
		if fits {
			select {
			case proxy.nodeMessages <- message:
				return nil
			default:
			}
		}
		proxy.socket.close(websocket.CloseTryAgainLater, "App Server client is too slow")
		channel.markProxyClientClosed(proxy, true)
		return nil
	}
	return channel.handleProxyNodeMessage(proxy, message)
}

func (channel *Channel) handleProxyNodeMessage(proxy *proxy, message map[string]any) error {
	messageType, _ := message["type"].(string)
	switch messageType {
	case "appserver.message":
		payload, _ := message["payload"].(string)
		return channel.forwardAppServerMessage(requestContext(), proxy, []byte(payload))
	case "appserver.error":
		text := fmt.Sprint(message["error"])
		if text == "<nil>" {
			text = "app-server tunnel failed"
		}
		proxy.socket.close(websocket.CloseInternalServerErr, text)
		channel.markProxyClientClosed(proxy, true)
	case "appserver.closed":
		proxy.socket.close(websocket.CloseNormalClosure, "app-server closed")
		channel.markProxyClientClosed(proxy, true)
	}
	return nil
}

func (channel *Channel) SendToNode(nodeID string, message any) error {
	channel.mu.Lock()
	connection := channel.nodeSockets[nodeID]
	channel.mu.Unlock()
	if connection == nil {
		return channelError("target Node is offline", 503, "node_offline")
	}
	if err := connection.writeJSON(message); err != nil {
		return channelError("target Node is offline", 503, "node_offline")
	}
	return nil
}

func (channel *Channel) TrySendToNode(nodeID string, message any) bool {
	return channel.SendToNode(nodeID, message) == nil
}

func (channel *Channel) Invoke(ctx context.Context, nodeID, capability string, params map[string]any, timeout time.Duration) (any, error) {
	requestID, err := randomUUID()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultCapabilityTimeout
	}
	done := make(chan invocationResult, 1)
	channel.mu.Lock()
	if channel.closed {
		channel.mu.Unlock()
		return nil, channelError("target Node is offline", 503, "node_offline")
	}
	channel.pending[requestID] = pendingCall{nodeID: nodeID, done: done}
	channel.mu.Unlock()
	if err := channel.SendToNode(nodeID, map[string]any{"type": "request", "requestId": requestID, "capability": capability, "params": params}); err != nil {
		channel.mu.Lock()
		delete(channel.pending, requestID)
		channel.mu.Unlock()
		return nil, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.value, result.err
	case <-ctx.Done():
		channel.mu.Lock()
		delete(channel.pending, requestID)
		channel.mu.Unlock()
		return nil, ctx.Err()
	case <-timer.C:
		channel.mu.Lock()
		delete(channel.pending, requestID)
		channel.mu.Unlock()
		return nil, channelError("capability request timed out", 504, "capability_timeout")
	}
}

func (channel *Channel) writeNodeStatus(nodeID string, status any) {
	channel.mu.Lock()
	previous := channel.statusWrites[nodeID]
	done := make(chan struct{})
	channel.statusWrites[nodeID] = done
	channel.mu.Unlock()
	go func() {
		if previous != nil {
			<-previous
		}
		if err := channel.nodes.SetChannelStatus(context.Background(), nodeID, status); err != nil {
			channel.logger.Error("Node channel status update failed", "nodeId", nodeID, "error", err)
		}
		close(done)
		channel.mu.Lock()
		if channel.statusWrites[nodeID] == done {
			delete(channel.statusWrites, nodeID)
		}
		channel.mu.Unlock()
	}()
}

func (channel *Channel) rejectNodeWork(nodeID string) {
	channel.accounts.CloseNode(nodeID)
	channel.ssh.DisconnectNode(nodeID)
	channel.mu.Lock()
	for requestID, pending := range channel.pending {
		if pending.nodeID == nodeID {
			delete(channel.pending, requestID)
			pending.done <- invocationResult{err: channelError("target Node disconnected", 503, "node_offline")}
		}
	}
	var affected []*proxy
	for _, proxy := range channel.proxies {
		if proxy.targetNodeID == nodeID {
			affected = append(affected, proxy)
		}
	}
	channel.mu.Unlock()
	for _, proxy := range affected {
		proxy.socket.close(websocket.CloseInternalServerErr, "node disconnected")
		channel.markProxyClientClosed(proxy, true)
	}
}

func (channel *Channel) DisconnectNode(nodeID, reason string) {
	if reason == "" {
		reason = "revoked"
	}
	channel.accounts.CloseNode(nodeID)
	channel.ssh.DisconnectNode(nodeID)
	channel.mu.Lock()
	connection := channel.nodeSockets[nodeID]
	var affected []*proxy
	for _, proxy := range channel.proxies {
		if proxy.targetNodeID == nodeID || proxy.callerNodeID == nodeID {
			affected = append(affected, proxy)
		}
	}
	channel.mu.Unlock()
	if connection != nil {
		connection.close(websocket.ClosePolicyViolation, reason)
	}
	for _, proxy := range affected {
		proxy.socket.close(websocket.ClosePolicyViolation, reason)
		channel.markProxyClientClosed(proxy, true)
	}
}

func (channel *Channel) Close() error {
	channel.mu.Lock()
	if channel.closed {
		channel.mu.Unlock()
		return nil
	}
	channel.closed = true
	nodes := make([]*socket, 0, len(channel.nodeSockets))
	for _, value := range channel.nodeSockets {
		nodes = append(nodes, value)
	}
	proxies := make([]*proxy, 0, len(channel.proxies))
	for _, value := range channel.proxies {
		proxies = append(proxies, value)
	}
	for id, pending := range channel.pending {
		delete(channel.pending, id)
		pending.done <- invocationResult{err: channelError("Server shutting down", 503, "node_offline")}
	}
	channel.mu.Unlock()
	channel.accounts.Close()
	channel.ssh.Close()
	for _, connection := range nodes {
		connection.close(websocket.CloseGoingAway, "server shutting down")
	}
	for _, proxy := range proxies {
		proxy.socket.close(websocket.CloseGoingAway, "server shutting down")
		channel.markProxyClientClosed(proxy, true)
	}
	return nil
}

func requestContext() context.Context { return context.Background() }

func channelTimestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }
