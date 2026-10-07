package node

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/sitewire"
	"github.com/ssine/mira/node/internal/transport"
)

var siteSessionID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (client *controlClient) startSiteStream(ctx context.Context, message controlMessage) {
	var params struct {
		Port int `json:"port"`
	}
	if len(message.Params) > 1024 || !siteSessionID.MatchString(message.SessionID) || json.Unmarshal(message.Params, &params) != nil || params.Port < 1 || params.Port > 65535 {
		return
	}
	client.siteMu.Lock()
	if client.siteWorkers == nil {
		client.siteWorkers = map[string]context.CancelFunc{}
	}
	if len(client.siteWorkers) >= 32 || client.siteWorkers[message.SessionID] != nil {
		client.siteMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	client.siteWorkers[message.SessionID] = cancel
	client.siteMu.Unlock()
	go func() {
		defer cancel()
		defer func() { client.siteMu.Lock(); delete(client.siteWorkers, message.SessionID); client.siteMu.Unlock() }()
		u, err := url.Parse(client.endpoints.endpoint(ctx))
		if err != nil {
			return
		}
		if u.Scheme == "https" {
			u.Scheme = "wss"
		} else {
			u.Scheme = "ws"
		}
		u.Path = "/v1/site-streams/" + message.SessionID
		u.RawQuery = ""
		u.Fragment = ""
		dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, ReadBufferSize: sitewire.FrameBytes, WriteBufferSize: sitewire.FrameBytes,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: platformCertificatePool()},
			Subprotocols:    []string{sitewire.Protocol, "auth." + base64.RawURLEncoding.EncodeToString([]byte(client.token))}}
		ws, _, err := transport.Dial(ctx, &dialer, u.String(), nil, client.controlTransportMode())
		if err != nil {
			return
		}
		defer ws.Close()
		local, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(params.Port)))
		_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err != nil {
			_ = ws.WriteJSON(sitewire.Ready{Error: "port_unavailable"})
			return
		}
		defer local.Close()
		if err = ws.WriteJSON(sitewire.Ready{Ready: true}); err != nil {
			return
		}
		_ = ws.SetWriteDeadline(time.Time{})
		stream := sitewire.New(ws)
		sitewire.Bridge(ctx, stream, local.(*net.TCPConn))
	}()
}

func (client *controlClient) stopSiteStream(id string) {
	client.siteMu.Lock()
	cancel := client.siteWorkers[id]
	client.siteMu.Unlock()
	if cancel != nil {
		cancel()
	}
}
