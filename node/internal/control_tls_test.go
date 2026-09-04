package node

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestReverseChannelAfterHTTP2Registration(t *testing.T) {
	httpsProtocol := make(chan int, 1)
	hello := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{Subprotocols: []string{"mira-node-v1"}}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/probe" {
			httpsProtocol <- r.ProtoMajor
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		var message map[string]any
		if connection.ReadJSON(&message) == nil {
			hello <- message
		}
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	configuration := config{ServerURL: server.URL, HeartbeatInterval: time.Hour}
	client := newControlClient(configuration, nil)
	certificates := x509.NewCertPool()
	certificates.AddCert(server.Certificate())
	client.http.Transport.(*http.Transport).TLSClientConfig.RootCAs = certificates
	defer client.http.CloseIdleConnections()
	client.nodeID, client.token = "test-node", "test-only-credential"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var response map[string]any
	if err := client.postJSON(ctx, "/probe", map[string]any{}, &response); err != nil {
		t.Fatal(err)
	}
	if protocol := <-httpsProtocol; protocol != 2 {
		t.Fatalf("test must negotiate HTTP/2 first, got HTTP/%d", protocol)
	}
	_ = client.serve(ctx)
	select {
	case message := <-hello:
		if message["type"] != "hello" {
			t.Fatalf("unexpected handshake: %v", message)
		}
	default:
		t.Fatal("WebSocket did not upgrade after the HTTP/2 request")
	}
}
