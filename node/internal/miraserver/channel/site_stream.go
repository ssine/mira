package channel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/ssine/mira/node/internal/sitewire"
	"github.com/ssine/mira/node/internal/transport"
)

var siteStreamPattern = regexp.MustCompile(`(?i)^/v1/site-streams/([0-9a-f-]{36})$`)

type siteStream struct {
	nodeID, siteID string
	epoch          *socket
	ready          chan transport.Conn
	done           chan struct{}
	once           sync.Once
	mu             sync.Mutex
	connection     transport.Conn
	attached       bool // guarded by channel.mu, including the upgrade reservation
}

func (channel *Channel) SetSiteIdleReclaimer(reclaim func()) {
	channel.mu.Lock()
	channel.siteIdleReclaimer = reclaim
	channel.mu.Unlock()
}

func (channel *Channel) siteBudgetLocked() int {
	if channel.siteStreamBudget == 0 {
		return sitewire.DefaultStreamBudget
	}
	return channel.siteStreamBudget
}

func (channel *Channel) HasSiteCapacity() bool {
	channel.mu.Lock()
	defer channel.mu.Unlock()
	return !channel.closed && !channel.siteStreamsClosed && len(channel.siteStreams) < channel.siteBudgetLocked()
}

func (channel *Channel) reserveSiteStream(ctx context.Context, id, siteID, nodeID string) (*siteStream, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		channel.mu.Lock()
		epoch := channel.nodeSockets[nodeID]
		if channel.closed || channel.siteStreamsClosed || epoch == nil {
			channel.mu.Unlock()
			return nil, channelError("Node is offline", 503, "node_offline")
		}
		if len(channel.siteStreams) >= channel.siteBudgetLocked() {
			reclaim := channel.siteIdleReclaimer
			channel.mu.Unlock()
			if attempt == 0 && reclaim != nil {
				reclaim()
				continue
			}
			return nil, channelError("site stream capacity exhausted", 429, "site_busy")
		}
		if channel.siteStreams == nil {
			channel.siteStreams = map[string]*siteStream{}
		}
		s := &siteStream{nodeID: nodeID, siteID: siteID, epoch: epoch, ready: make(chan transport.Conn, 1), done: make(chan struct{})}
		channel.siteStreams[id] = s
		channel.mu.Unlock()
		return s, nil
	}
	panic("unreachable site reservation")
}

type siteStreamConn struct {
	*sitewire.Conn
	cleanup func()
}

func (conn *siteStreamConn) Close() error {
	err := conn.Conn.Close()
	conn.cleanup()
	return err
}

func (s *siteStream) close() {
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		close(s.done)
		if s.connection != nil {
			_ = s.connection.Close()
		}
	})
}

func (channel *Channel) CloseSiteStreams(nodeID, siteID string) {
	channel.mu.Lock()
	var streams []*siteStream
	for id, s := range channel.siteStreams {
		if (nodeID == "" || s.nodeID == nodeID) && (siteID == "" || s.siteID == siteID) {
			streams = append(streams, s)
			delete(channel.siteStreams, id)
		}
	}
	channel.mu.Unlock()
	for _, s := range streams {
		s.close()
	}
}

func (channel *Channel) serveSiteUpgrade(w http.ResponseWriter, r *http.Request, id string) {
	token, ok := ProtocolToken(r)
	if !ok || !hasWebSocketProtocol(r, sitewire.Protocol) {
		http.Error(w, "unauthorized", 401)
		return
	}
	actor, err := channel.auth.AuthenticateNodeToken(r.Context(), token, "node")
	if err != nil || actor == nil || actor.Revoked {
		http.Error(w, "unauthorized", 401)
		return
	}
	channel.mu.Lock()
	s := channel.siteStreams[id]
	if s == nil || s.nodeID != actor.NodeID || s.attached || channel.nodeSockets[s.nodeID] != s.epoch {
		channel.mu.Unlock()
		http.Error(w, "stream expired", 409)
		return
	}
	s.attached = true
	channel.mu.Unlock()
	ws, err := channel.upgradeTransport(w, r, sitewire.Protocol)
	if err != nil {
		s.close()
		return
	}
	channel.mu.Lock()
	if channel.siteStreams[id] != s || channel.nodeSockets[s.nodeID] != s.epoch {
		channel.mu.Unlock()
		_ = ws.Close()
		return
	}
	s.mu.Lock()
	s.connection = ws
	s.mu.Unlock()
	ws.SetReadLimit(sitewire.FrameBytes)
	s.ready <- ws
	channel.mu.Unlock()
	if r.URL.Query().Get("transport") != "https" {
		<-s.done
	}
}

// DialSite returns a real byte-stream boundary to net/http. Each Node dial uses
// an independent outbound binary transport, bound to the current control epoch.
func (channel *Channel) DialSite(ctx context.Context, siteID, nodeID string, port int) (net.Conn, error) {
	if port < 1 || port > 65535 {
		return nil, channelError("invalid port", 400, "invalid_port")
	}
	id, err := randomUUID()
	if err != nil {
		return nil, err
	}
	s, err := channel.reserveSiteStream(ctx, id, siteID, nodeID)
	if err != nil {
		return nil, err
	}
	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			s.close()
			// Send close before publishing the free slot: a competing dial must
			// not enqueue site.open ahead of the Node's worker reclamation.
			channel.TrySendToNode(nodeID, map[string]any{"type": "site.close", "sessionId": id})
			channel.mu.Lock()
			if channel.siteStreams[id] == s {
				delete(channel.siteStreams, id)
			}
			channel.mu.Unlock()
		})
	}
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	if err = s.epoch.writeJSON(map[string]any{"type": "site.open", "sessionId": id, "params": map[string]any{"port": port}}); err != nil {
		return nil, err
	}
	timer := time.NewTimer(20 * time.Second)
	defer timer.Stop()
	var raw transport.Conn
	select {
	case raw = <-s.ready:
	case <-s.done:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, channelError("site connection timed out", 504, "site_connect_timeout")
	}
	_ = raw.SetReadDeadline(time.Now().Add(15 * time.Second))
	ackDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = raw.Close()
		case <-s.done:
			_ = raw.Close()
		case <-ackDone:
		}
	}()
	var ready sitewire.Ready
	err = raw.ReadJSON(&ready)
	close(ackDone)
	if err != nil {
		return nil, fmt.Errorf("site dial acknowledgement: %w", err)
	}
	if !ready.Ready || ready.Error != "" {
		return nil, channelError("Node port is unavailable", 502, "port_unavailable")
	}
	_ = raw.SetReadDeadline(time.Time{})
	stream := sitewire.New(raw)
	go func() {
		select {
		case <-stream.Done():
		case <-s.done:
			stream.Close()
		}
		cleanup()
	}()
	ok = true
	return &siteStreamConn{Conn: stream, cleanup: cleanup}, nil
}

func (channel *Channel) ShutdownSiteStreams() {
	channel.mu.Lock()
	channel.siteStreamsClosed = true
	channel.mu.Unlock()
	channel.CloseSiteStreams("", "")
}
