package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type controlClient struct {
	config  config
	runtime *capabilityRuntime
	http    *http.Client
	nodeID  string
	writeMu sync.Mutex
}

type registrationResponse struct {
	NodeID string `json:"nodeId"`
}

type controlMessage struct {
	Type       string          `json:"type"`
	RequestID  string          `json:"requestId,omitempty"`
	Capability string          `json:"capability,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"`
	SessionID  string          `json:"sessionId,omitempty"`
}

func newControlClient(configuration config, runtime *capabilityRuntime) *controlClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    androidCertificatePool(),
	}
	return &controlClient{
		config:  configuration,
		runtime: runtime,
		http:    &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

func androidCertificatePool() *x509.CertPool {
	pool, _ := x509.SystemCertPool()
	if pool == nil {
		pool = x509.NewCertPool()
	}
	entries, err := filepath.Glob("/system/etc/security/cacerts/*")
	if err != nil {
		return pool
	}
	for _, entry := range entries {
		contents, err := os.ReadFile(entry)
		if err == nil {
			pool.AppendCertsFromPEM(contents)
		}
	}
	return pool
}

func (client *controlClient) postJSON(ctx context.Context, route string, body any, result any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		client.config.ServerURL+route,
		bytes.NewReader(encoded),
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.config.Token)
	request.Header.Set("Content-Type", "application/json")
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
		return fmt.Errorf("POST %s returned %d: %s", route, response.StatusCode, payload)
	}
	if result != nil && len(payload) != 0 {
		if err := json.Unmarshal(payload, result); err != nil {
			return err
		}
	}
	return nil
}

func (client *controlClient) capabilities() map[string]any {
	rootEnabled := os.Geteuid() == 0
	androidBridge := client.config.BridgeURL != ""
	return map[string]any{
		"appServer":       false,
		"shell":           false,
		"files":           true,
		"processes":       true,
		"pty":             false,
		"screen":          rootEnabled || androidBridge,
		"input":           rootEnabled || androidBridge,
		"reverseChannel":  true,
		"nativePaths":     true,
		"rootAvailable":   rootEnabled,
		"nodeMode":        "android-native",
		"transport":       "native",
		"privilegeMode":   client.config.PrivilegeMode,
		"screenBackend":   map[bool]string{true: "android-api", false: "system"}[androidBridge],
		"permissionModel": map[bool]string{true: "android-user-grants", false: "root-provider"}[androidBridge],
	}
}

func (client *controlClient) register(ctx context.Context) error {
	device, err := client.runtime.deviceInfo(ctx)
	if err != nil {
		return err
	}
	nodeKey := client.config.NodeKey
	if nodeKey == "" {
		nodeKey = "android-native:" + device.Serial
	}
	hostname := device.Manufacturer + "-" + device.Model
	if hostname == "-" {
		hostname = "android-" + device.Serial
	}
	body := map[string]any{
		"nodeKey":                 nodeKey,
		"hostname":                hostname,
		"platform":                "android",
		"architecture":            device.ABI,
		"nodeMode":                "android-native",
		"agentVersion":            agentVersion,
		"capabilities":            client.capabilities(),
		"codexInstallations":      []any{},
		"defaultDesiredAppServer": map[string]any{"running": false, "revision": 1},
	}
	var response registrationResponse
	if err := client.postJSON(ctx, "/v1/nodes/register", body, &response); err != nil {
		return err
	}
	if response.NodeID == "" {
		return fmt.Errorf("control server returned an empty node ID")
	}
	client.nodeID = response.NodeID
	logEvent("registered Android native node", map[string]any{
		"nodeId":  client.nodeID,
		"nodeKey": nodeKey,
		"device":  device,
		"uid":     os.Geteuid(),
	})
	return nil
}

func (client *controlClient) heartbeat(ctx context.Context) error {
	status, err := client.runtime.machineStatus(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"reportedAppServer":  map[string]any{"status": "unsupported"},
		"codexInstallations": []any{},
		"capabilities":       client.capabilities(),
		"machineStatus":      status,
	}
	return client.postJSON(ctx, "/v1/nodes/"+client.nodeID+"/heartbeat", body, nil)
}

func (client *controlClient) websocketURL() (string, error) {
	parsed, err := url.Parse(client.config.ServerURL)
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
	dialer.TLSClientConfig = client.http.Transport.(*http.Transport).TLSClientConfig
	dialer.Subprotocols = []string{
		"codex-node-v1",
		"auth." + base64.RawURLEncoding.EncodeToString([]byte(client.config.Token)),
	}
	connection, response, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect control websocket: %w (HTTP %d)", err, response.StatusCode)
		}
		return fmt.Errorf("connect control websocket: %w", err)
	}
	defer connection.Close()
	stopConnection := make(chan struct{})
	defer close(stopConnection)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopConnection:
		}
	}()
	connection.SetReadLimit(1024 * 1024)
	if err := client.writeJSON(connection, map[string]any{
		"type": "hello", "nodeId": client.nodeID, "agentVersion": agentVersion, "protocolVersion": 1,
	}); err != nil {
		return err
	}
	logEvent("connected Android native reverse capability channel", map[string]any{
		"nodeId": client.nodeID,
	})

	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	defer cancelHeartbeat()
	go client.heartbeatLoop(heartbeatCtx)
	for {
		var message controlMessage
		if err := connection.ReadJSON(&message); err != nil {
			return err
		}
		switch message.Type {
		case "request":
			client.handleRequest(ctx, connection, message)
		case "appserver.open":
			_ = client.writeJSON(connection, map[string]any{
				"type": "appserver.error", "sessionId": message.SessionID,
				"error": "this Android native node does not run Codex App Server",
			})
		}
	}
}

func (client *controlClient) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(client.config.HeartbeatSeconds)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := client.heartbeat(ctx); err != nil && ctx.Err() == nil {
				logEvent("Android native heartbeat failed", map[string]any{"error": err.Error()})
			}
		}
	}
}

func (client *controlClient) handleRequest(
	ctx context.Context,
	connection *websocket.Conn,
	message controlMessage,
) {
	go func() {
		result, err := client.runtime.execute(ctx, message.Capability, message.Params)
		response := map[string]any{
			"type": "response", "requestId": message.RequestID, "ok": err == nil,
		}
		if err != nil {
			response["error"] = map[string]any{"message": err.Error()}
		} else {
			response["result"] = result
		}
		if writeErr := client.writeJSON(connection, response); writeErr != nil {
			logEvent("write native capability response failed", map[string]any{"error": writeErr.Error()})
		}
	}()
}

func (client *controlClient) writeJSON(connection *websocket.Conn, value any) error {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	return connection.WriteJSON(value)
}
