package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ssine/mira/node/internal/resourcewire"
	"github.com/ssine/mira/node/internal/transport"
)

var fileStreamPattern = regexp.MustCompile(`(?i)^/v1/file-streams/([0-9a-f-]{36})$`)

type fileStream struct {
	nodeID     string
	epoch      *socket
	ready      chan transport.Conn
	done       chan struct{}
	once       sync.Once
	connection transport.Conn
}

func (s *fileStream) close() {
	s.once.Do(func() {
		close(s.done)
		if s.connection != nil {
			s.connection.Close()
		}
	})
}
func (channel *Channel) closeFileStreams(nodeID string) {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	for id, s := range channel.fileStreams {
		if nodeID == "" || s.nodeID == nodeID {
			s.close()
			delete(channel.fileStreams, id)
		}
	}
}
func (channel *Channel) serveFileUpgrade(w http.ResponseWriter, r *http.Request, id string) {
	token, ok := ProtocolToken(r)
	if !ok || !hasWebSocketProtocol(r, resourcewire.Protocol) {
		http.Error(w, "unauthorized", 401)
		return
	}
	actor, err := channel.auth.AuthenticateNodeToken(r.Context(), token, "node")
	if err != nil || actor == nil || actor.Revoked {
		http.Error(w, "unauthorized", 401)
		return
	}
	channel.mu.Lock()
	s := channel.fileStreams[id]
	if s == nil || s.nodeID != actor.NodeID || s.connection != nil || channel.nodeSockets[s.nodeID] != s.epoch {
		channel.mu.Unlock()
		http.Error(w, "stream expired", 409)
		return
	}
	ws, err := channel.upgradeTransport(w, r, resourcewire.Protocol)
	if err != nil {
		channel.mu.Unlock()
		return
	}
	s.connection = ws
	ws.SetReadLimit(resourcewire.FrameBytes)
	s.ready <- ws
	channel.mu.Unlock()
	if r.URL.Query().Get("transport") != "https" {
		<-s.done
	}
}

// StreamFile connects a single authenticated outbound Node socket to this HTTP
// response. No per-file goroutine buffers, Base64 payloads or whole-file reads.
func (channel *Channel) StreamFile(ctx context.Context, w http.ResponseWriter, r *http.Request, nodeID string, p resourcewire.Request) error {
	id, err := randomUUID()
	if err != nil {
		return err
	}
	channel.mu.Lock()
	epoch := channel.nodeSockets[nodeID]
	if channel.closed || channel.fileStreamsClosed || epoch == nil {
		channel.mu.Unlock()
		return channelError("Node is offline", 503, "node_offline")
	}
	count := 0
	for _, s := range channel.fileStreams {
		if s.nodeID == nodeID {
			count++
		}
	}
	if len(channel.fileStreams) >= 128 || count >= 32 {
		channel.mu.Unlock()
		return channelError("file stream concurrency limit reached", 429, "resource_busy")
	}
	if channel.fileStreams == nil {
		channel.fileStreams = map[string]*fileStream{}
	}
	s := &fileStream{nodeID: nodeID, epoch: epoch, ready: make(chan transport.Conn, 1), done: make(chan struct{})}
	channel.fileStreams[id] = s
	channel.mu.Unlock()
	defer func() {
		channel.mu.Lock()
		s.close()
		delete(channel.fileStreams, id)
		channel.mu.Unlock()
		channel.TrySendToNode(nodeID, map[string]any{"type": "file.close", "sessionId": id})
	}()
	p.Method = r.Method
	p.Headers = http.Header{}
	for _, key := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(key); v != "" {
			if len(v) > 4096 {
				return channelError("file request header exceeds 4096 bytes", 400, "invalid_request")
			}
			p.Headers.Set(key, v)
		}
	}
	if err = epoch.writeJSON(map[string]any{"type": "file.open", "sessionId": id, "params": p}); err != nil {
		return err
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	var ws transport.Conn
	select {
	case ws = <-s.ready:
	case <-s.done:
		return fmt.Errorf("file stream disconnected")
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return channelError("file stream connection timed out; upgrade the Node", 504, "stream_timeout")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
			ws.Close()
		case <-stop:
		}
	}()
	ws.SetReadDeadline(time.Now().Add(2 * time.Minute))
	var meta resourcewire.Response
	if err = ws.ReadJSON(&meta); err != nil {
		return fmt.Errorf("file preparation failed: %w", err)
	}
	if meta.Status < 200 || meta.Status > 599 {
		return fmt.Errorf("invalid file response")
	}
	for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "ETag", "Last-Modified"} {
		if v := meta.Headers.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(meta.Status)
	buffer := make([]byte, resourcewire.FrameBytes)
	for {
		ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		kind, reader, e := ws.NextReader()
		if e != nil {
			return e
		}
		if kind == websocket.TextMessage {
			var end struct {
				Done bool `json:"done"`
			}
			if e = json.NewDecoder(reader).Decode(&end); e != nil || !end.Done {
				return fmt.Errorf("invalid file end marker")
			}
			return nil
		}
		if kind != websocket.BinaryMessage {
			return fmt.Errorf("invalid file frame")
		}
		if _, e = io.CopyBuffer(w, reader, buffer); e != nil {
			return e
		}
	}
}

// Opaque, transient identity used to expire sites after a control reconnect.
func (channel *Channel) NodeConnectionEpoch(nodeID string) any {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return channel.nodeSockets[nodeID]
}
func (channel *Channel) ShutdownFileStreams() {
	channel.mu.Lock()
	channel.fileStreamsClosed = true
	channel.mu.Unlock()
	channel.closeFileStreams("")
}
