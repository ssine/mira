package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type controlClient struct {
	configuration config
	runtime       *capabilityRuntime
	appServer     *appServerManager
	http          *http.Client
	state         *persistedNodeState
	token         string
	nodeID        string
	desired       desiredAppServer
	writeMu       sync.Mutex
	connectionMu  sync.Mutex
	connection    *websocket.Conn
	tunnelsMu     sync.Mutex
	tunnels       map[string]*websocket.Conn
}

type registrationResponse struct {
	NodeID                   string           `json:"nodeId"`
	DesiredAppServer         desiredAppServer `json:"desiredAppServer"`
	HeartbeatIntervalSeconds int              `json:"heartbeatIntervalSeconds"`
}

type heartbeatResponse struct {
	DesiredAppServer desiredAppServer `json:"desiredAppServer"`
}

type enrollmentResponse struct {
	EnrollmentID     string `json:"enrollmentId"`
	VerificationCode string `json:"verificationCode"`
	Status           string `json:"status"`
	NodeID           string `json:"nodeId"`
	ApprovedAt       string `json:"approvedAt"`
	ExpiresAt        string `json:"expiresAt"`
}

type controlHTTPError struct {
	method string
	route  string
	status int
	body   string
}

func (value *controlHTTPError) Error() string {
	return fmt.Sprintf("%s %s returned %d: %s", value.method, value.route, value.status, value.body)
}

type controlMessage struct {
	Type       string          `json:"type"`
	RequestID  string          `json:"requestId,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	SessionID  string          `json:"sessionId,omitempty"`
	Payload    string          `json:"payload,omitempty"`
}

func newControlClient(configuration config, runtimeValue *capabilityRuntime) *controlClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool()}
	return &controlClient{
		configuration: configuration, runtime: runtimeValue,
		token:     configuration.Token,
		appServer: newAppServerManager(configuration),
		http:      &http.Client{Transport: transport, Timeout: 30 * time.Second},
		tunnels:   make(map[string]*websocket.Conn),
	}
}

func (client *controlClient) requestJSON(ctx context.Context, method string, route string, token string, body any, result any) error {
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, client.configuration.ServerURL+route, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("X-Mira-Client-Type", "node")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &controlHTTPError{method: method, route: route, status: response.StatusCode, body: string(payload)}
	}
	if result != nil && len(payload) != 0 {
		if err := json.Unmarshal(payload, result); err != nil {
			return err
		}
	}
	return nil
}

func (client *controlClient) postJSON(ctx context.Context, route string, body any, result any) error {
	return client.requestJSON(ctx, http.MethodPost, route, client.token, body, result)
}

func (client *controlClient) registrationBody(ctx context.Context) map[string]any {
	identity := client.runtime.identity(ctx)
	machineStatus, _ := client.runtime.machineStatus(ctx)
	return map[string]any{
		"nodeKey": identity.NodeKey, "hostname": identity.Hostname,
		"platform": identity.Platform, "architecture": identity.Architecture,
		"nodeMode": identity.Mode, "nodeVersion": Version, "nodeBuild": CurrentBuild(),
		"capabilities":       client.runtime.advertisedCapabilities(ctx),
		"codexInstallations": client.appServer.installationsView(),
		"machineStatus":      machineStatus,
		"defaultDesiredAppServer": map[string]any{
			"running":         client.configuration.AppServerAutoStart && supportsAppServer(),
			"listenUrl":       client.configuration.AppServerListenURL,
			"codexPath":       nullableString(client.configuration.CodexBinary),
			"codexHome":       nullableString(client.configuration.AppServerCodexHome),
			"configOverrides": []string{}, "revision": 1,
		},
	}
}

func (client *controlClient) ensureCredential(ctx context.Context, body map[string]any) error {
	if client.token != "" {
		if nodeTokenPattern.FindStringSubmatch(client.token) == nil {
			return fmt.Errorf("configured token is not a Mira Node credential; remove the old shared token and enroll this node")
		}
		return nil
	}
	identity := client.runtime.identity(ctx)
	if client.state == nil {
		state, err := loadOrCreateNodeState(client.configuration, identity)
		if err != nil {
			return err
		}
		client.state = state
	}
	if client.state.NodeID != "" && client.state.Enrollment.Status == "approved" {
		client.token = client.state.Token
		return nil
	}
	if client.state.Enrollment.ID == "" {
		secretHash, err := client.state.credentialSecretHash()
		if err != nil {
			return err
		}
		requestBody := make(map[string]any, len(body)+2)
		for key, value := range body {
			requestBody[key] = value
		}
		requestBody["credentialId"] = client.state.CredentialID
		requestBody["credentialSecretHash"] = secretHash
		var enrollment enrollmentResponse
		if err := client.requestJSON(ctx, http.MethodPost, "/v1/node-enrollments", "", requestBody, &enrollment); err != nil {
			return err
		}
		if enrollment.EnrollmentID == "" {
			return fmt.Errorf("Mira Server returned an incomplete enrollment request")
		}
		client.state.Enrollment = enrollmentState{
			ID: enrollment.EnrollmentID, VerificationCode: enrollment.VerificationCode,
			Status: enrollment.Status, ExpiresAt: enrollment.ExpiresAt,
		}
		if err := client.state.save(client.configuration.IdentityFile); err != nil {
			return err
		}
		Log("Mira Node enrollment requested", map[string]any{
			"enrollmentId":     enrollment.EnrollmentID,
			"verificationCode": enrollment.VerificationCode,
			"nodeKey":          identity.NodeKey,
		})
		return fmt.Errorf("waiting for administrator approval; verification code %s", enrollment.VerificationCode)
	}

	var enrollment enrollmentResponse
	if err := client.requestJSON(
		ctx,
		http.MethodGet,
		"/v1/node-enrollments/"+client.state.Enrollment.ID,
		client.state.Token,
		nil,
		&enrollment,
	); err != nil {
		return err
	}
	client.state.Enrollment.Status = enrollment.Status
	switch enrollment.Status {
	case "pending":
		return fmt.Errorf("waiting for administrator approval; verification code %s", client.state.Enrollment.VerificationCode)
	case "rejected":
		if err := client.state.save(client.configuration.IdentityFile); err != nil {
			return err
		}
		return fmt.Errorf("node enrollment was rejected; reset the local identity to request approval again")
	case "expired":
		if err := client.state.resetCredential(); err != nil {
			return err
		}
		if err := client.state.save(client.configuration.IdentityFile); err != nil {
			return err
		}
		return fmt.Errorf("node enrollment expired; a new request will be created")
	case "approved":
		if enrollment.NodeID == "" {
			return fmt.Errorf("approved enrollment is missing nodeId")
		}
		client.state.markApproved(enrollment.NodeID, enrollment.ApprovedAt)
		if err := client.state.save(client.configuration.IdentityFile); err != nil {
			return err
		}
		client.token = client.state.Token
		Log("Mira Node enrollment approved", map[string]any{"nodeId": enrollment.NodeID, "nodeKey": identity.NodeKey})
		return nil
	default:
		return fmt.Errorf("Mira Server returned unknown enrollment status %q", enrollment.Status)
	}
}

func (client *controlClient) register(ctx context.Context) error {
	if err := client.appServer.discover(ctx); err != nil {
		Log("Codex discovery failed", map[string]any{"error": err.Error()})
	}
	identity := client.runtime.identity(ctx)
	body := client.registrationBody(ctx)
	if err := client.ensureCredential(ctx, body); err != nil {
		return err
	}
	client.appServer.setNodeCredential(client.token)
	var response registrationResponse
	if err := client.postJSON(ctx, "/v1/nodes/register", body, &response); err != nil {
		if httpError, ok := err.(*controlHTTPError); ok && httpError.status == http.StatusForbidden && client.state != nil {
			_ = client.appServer.reconcile(ctx, desiredAppServer{Running: false})
			if resetErr := client.state.resetCredential(); resetErr == nil {
				_ = client.state.save(client.configuration.IdentityFile)
				client.token = ""
				return fmt.Errorf("Node credential was revoked; a fresh enrollment will be submitted")
			}
		}
		return err
	}
	if response.NodeID == "" {
		return fmt.Errorf("control server returned an empty node ID")
	}
	client.nodeID = response.NodeID
	client.desired = response.DesiredAppServer
	if response.HeartbeatIntervalSeconds > 0 && firstEnv("MIRA_NODE_HEARTBEAT_SECONDS", "NODE_AGENT_HEARTBEAT_SECONDS") == "" {
		client.configuration.HeartbeatInterval = time.Duration(response.HeartbeatIntervalSeconds) * time.Second
	}
	Log("registered Mira Node", map[string]any{"nodeId": client.nodeID, "nodeKey": identity.NodeKey, "nodeMode": identity.Mode})
	return nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (client *controlClient) heartbeat(ctx context.Context) error {
	status, err := client.runtime.machineStatus(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"reportedAppServer":  client.appServer.report(),
		"codexInstallations": client.appServer.installationsView(),
		"capabilities":       client.runtime.advertisedCapabilities(ctx),
		"machineStatus":      status,
	}
	var response heartbeatResponse
	if err := client.postJSON(ctx, "/v1/nodes/"+client.nodeID+"/heartbeat", body, &response); err != nil {
		return err
	}
	client.desired = response.DesiredAppServer
	return nil
}

func (client *controlClient) reconcile(ctx context.Context) error {
	return client.appServer.reconcile(ctx, client.desired)
}

func (client *controlClient) websocketURL() (string, error) {
	parsed, err := url.Parse(client.configuration.ServerURL)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", fmt.Errorf("unsupported control server scheme: %s", parsed.Scheme)
	}
	parsed.Path = "/v1/nodes/" + client.nodeID + "/connect"
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func (client *controlClient) serve(ctx context.Context) error {
	endpoint, err := client.websocketURL()
	if err != nil {
		return err
	}
	dialer := *websocket.DefaultDialer
	// net/http adds h2 to its TLS config after the first HTTPS request. Gorilla
	// uses an HTTP/1.1 Upgrade handshake, so sharing that config breaks WSS after
	// enrollment against an HTTP/2-enabled reverse proxy such as Caddy.
	dialer.TLSClientConfig = client.http.Transport.(*http.Transport).TLSClientConfig.Clone()
	dialer.TLSClientConfig.NextProtos = []string{"http/1.1"}
	dialer.Subprotocols = []string{
		"mira-node-v1",
		"auth." + base64.RawURLEncoding.EncodeToString([]byte(client.token)),
	}
	connection, response, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect control websocket: %w (HTTP %d)", err, response.StatusCode)
		}
		return fmt.Errorf("connect control websocket: %w", err)
	}
	client.connectionMu.Lock()
	client.connection = connection
	client.connectionMu.Unlock()
	stopConnection := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopConnection:
		}
	}()
	defer func() {
		close(stopConnection)
		client.connectionMu.Lock()
		if client.connection == connection {
			client.connection = nil
		}
		client.connectionMu.Unlock()
		client.closeTunnels()
		connection.Close()
	}()
	connection.SetReadLimit(16 * 1024 * 1024)
	if err := client.writeControl(map[string]any{
		"type": "hello", "nodeId": client.nodeID, "nodeVersion": Version, "protocolVersion": ProtocolVersion,
	}); err != nil {
		return err
	}
	Log("connected reverse capability channel", map[string]any{"nodeId": client.nodeID})

	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go client.heartbeatLoop(loopCtx)
	for {
		var message controlMessage
		if err := connection.ReadJSON(&message); err != nil {
			return err
		}
		client.handleMessage(loopCtx, message)
	}
}

func (client *controlClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(client.configuration.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := client.heartbeat(ctx); err != nil {
				if ctx.Err() == nil {
					Log("heartbeat failed", map[string]any{"error": err.Error()})
				}
				continue
			}
			if err := client.reconcile(ctx); err != nil && ctx.Err() == nil {
				Log("App Server reconciliation failed", map[string]any{"error": err.Error()})
			}
		}
	}
}

func (client *controlClient) handleMessage(ctx context.Context, message controlMessage) {
	switch message.Type {
	case "request":
		go func() {
			result, err := client.runtime.execute(ctx, message.Capability, message.Params)
			response := map[string]any{"type": "response", "requestId": message.RequestID, "ok": err == nil}
			if err != nil {
				response["error"] = map[string]any{"message": err.Error()}
			} else {
				response["result"] = result
			}
			if writeErr := client.writeControl(response); writeErr != nil && ctx.Err() == nil {
				Log("write capability response failed", map[string]any{"error": writeErr.Error()})
			}
		}()
	case "appserver.open":
		// Preserve control-channel ordering: the server may forward the first
		// initialize message immediately after appserver.open.
		if err := client.openAppServerTunnel(ctx, message.SessionID); err != nil {
			_ = client.writeControl(map[string]any{"type": "appserver.error", "sessionId": message.SessionID, "error": err.Error()})
		}
	case "appserver.message":
		client.tunnelsMu.Lock()
		tunnel := client.tunnels[message.SessionID]
		client.tunnelsMu.Unlock()
		if tunnel == nil {
			_ = client.writeControl(map[string]any{"type": "appserver.error", "sessionId": message.SessionID, "error": "tunnel is not open"})
			return
		}
		if err := tunnel.WriteMessage(websocket.TextMessage, []byte(message.Payload)); err != nil {
			_ = client.writeControl(map[string]any{"type": "appserver.error", "sessionId": message.SessionID, "error": err.Error()})
		}
	case "appserver.close":
		client.closeTunnel(message.SessionID)
	}
}

func (client *controlClient) writeControl(value any) error {
	client.connectionMu.Lock()
	connection := client.connection
	client.connectionMu.Unlock()
	if connection == nil {
		return fmt.Errorf("control channel is offline")
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return connection.WriteJSON(value)
}

func (client *controlClient) openAppServerTunnel(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("sessionId is required")
	}
	listenURL, ok := client.appServer.readyListenURL()
	if !ok {
		return fmt.Errorf("local App Server is not running")
	}
	client.tunnelsMu.Lock()
	if client.tunnels[sessionID] != nil {
		client.tunnelsMu.Unlock()
		return nil
	}
	client.tunnelsMu.Unlock()
	tunnel, _, err := websocket.DefaultDialer.DialContext(ctx, listenURL, nil)
	if err != nil {
		return fmt.Errorf("connect local App Server: %w", err)
	}
	tunnel.SetReadLimit(16 * 1024 * 1024)
	client.tunnelsMu.Lock()
	client.tunnels[sessionID] = tunnel
	client.tunnelsMu.Unlock()
	if err := client.writeControl(map[string]any{"type": "appserver.opened", "sessionId": sessionID}); err != nil {
		client.closeTunnel(sessionID)
		return err
	}
	go func() {
		defer func() {
			client.closeTunnel(sessionID)
			_ = client.writeControl(map[string]any{"type": "appserver.closed", "sessionId": sessionID})
		}()
		for {
			messageType, payload, err := tunnel.ReadMessage()
			if err != nil {
				return
			}
			if messageType != websocket.TextMessage {
				continue
			}
			if err := client.writeControl(map[string]any{"type": "appserver.message", "sessionId": sessionID, "payload": string(payload)}); err != nil {
				return
			}
		}
	}()
	return nil
}

func (client *controlClient) closeTunnel(sessionID string) {
	client.tunnelsMu.Lock()
	tunnel := client.tunnels[sessionID]
	delete(client.tunnels, sessionID)
	client.tunnelsMu.Unlock()
	if tunnel != nil {
		_ = tunnel.Close()
	}
}

func (client *controlClient) closeTunnels() {
	client.tunnelsMu.Lock()
	tunnels := client.tunnels
	client.tunnels = make(map[string]*websocket.Conn)
	client.tunnelsMu.Unlock()
	for _, tunnel := range tunnels {
		_ = tunnel.Close()
	}
}

func (client *controlClient) close() {
	client.connectionMu.Lock()
	connection := client.connection
	client.connection = nil
	client.connectionMu.Unlock()
	if connection != nil {
		_ = connection.Close()
	}
	client.closeTunnels()
	client.appServer.close()
}
