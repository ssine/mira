package miraserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Live Claude output is ephemeral. Workers send streaming deltas in the same
// ordered batches as durable SDK events; Server keeps a bounded per-session ring
// in memory and pushes it to subscribed browsers. Nothing here is authoritative:
// a restart, eviction or slow subscriber only loses partial text that the next
// durable assistant event replaces. PostgreSQL remains the history source.
const (
	claudeLiveRingItems      = 4096
	claudeLiveRingBytes      = 4 << 20
	claudeLiveSessionsMax    = 256
	claudeLiveSubscribersMax = 64
	claudeLiveIdle           = 10 * time.Minute
	claudeLiveStreamLifetime = 10 * time.Minute
	claudeLiveKeepalive      = 15 * time.Second
)

type claudeLiveItem struct {
	ID       int64           `json:"id"`
	TurnID   string          `json:"turnId"`
	AfterSeq int64           `json:"afterSeq"`
	Payload  json.RawMessage `json:"payload"`
	root     bool
}

type claudeLiveSession struct {
	next, durable int64
	turn          string
	items         []claudeLiveItem
	bytes         int
	subscribers   map[chan struct{}]struct{}
	touched       time.Time
}

type claudeLiveHub struct {
	mu          sync.Mutex
	sessions    map[string]*claudeLiveSession
	subscribers int
}

func (hub *claudeLiveHub) sessionLocked(id string) *claudeLiveSession {
	if hub.sessions == nil {
		hub.sessions = map[string]*claudeLiveSession{}
	}
	live := hub.sessions[id]
	if live == nil {
		if len(hub.sessions) >= claudeLiveSessionsMax {
			hub.evictLocked()
		}
		live = &claudeLiveSession{subscribers: map[chan struct{}]struct{}{}}
		hub.sessions[id] = live
	}
	live.touched = time.Now()
	return live
}

// evictLocked drops idle sessions without subscribers, oldest first.
func (hub *claudeLiveHub) evictLocked() {
	var oldest string
	for id, live := range hub.sessions {
		if len(live.subscribers) > 0 {
			continue
		}
		if time.Since(live.touched) > claudeLiveIdle {
			delete(hub.sessions, id)
			continue
		}
		if oldest == "" || live.touched.Before(hub.sessions[oldest].touched) {
			oldest = id
		}
	}
	if len(hub.sessions) >= claudeLiveSessionsMax && oldest != "" {
		delete(hub.sessions, oldest)
	}
}

// publish appends ephemeral items and advances the durable position. A new turn
// starts a new ring; its predecessor's partial output has been superseded.
func (hub *claudeLiveHub) publish(session, turn string, durable int64, items []claudeLiveItem) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	live := hub.sessionLocked(session)
	if turn != "" && turn != live.turn {
		live.turn, live.items, live.bytes = turn, nil, 0
	}
	if durable > live.durable {
		live.durable = durable
	}
	for _, item := range items {
		live.next++
		item.ID, item.TurnID = live.next, turn
		live.items = append(live.items, item)
		live.bytes += len(item.Payload)
	}
	for len(live.items) > claudeLiveRingItems || len(live.items) > 1 && live.bytes > claudeLiveRingBytes {
		live.bytes -= len(live.items[0].Payload)
		live.items = live.items[1:]
	}
	for signal := range live.subscribers {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}

func (hub *claudeLiveHub) subscribe(session string) (chan struct{}, func(), bool) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if hub.subscribers >= claudeLiveSubscribersMax {
		return nil, nil, false
	}
	live := hub.sessionLocked(session)
	signal := make(chan struct{}, 1)
	live.subscribers[signal] = struct{}{}
	hub.subscribers++
	return signal, func() {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		delete(live.subscribers, signal)
		hub.subscribers--
	}, true
}

// read returns items after the cursor, whether the cursor fell out of the ring,
// and the durable position.
func (hub *claudeLiveHub) read(session string, after int64, rootOnly bool) ([]claudeLiveItem, bool, int64, int64) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	live := hub.sessionLocked(session)
	reset := after > live.next || len(live.items) > 0 && after < live.items[0].ID-1
	if after > live.next {
		after = 0
	}
	out := []claudeLiveItem{}
	for _, item := range live.items {
		if item.ID > after && (!rootOnly || item.root) {
			out = append(out, item)
		}
	}
	return out, reset, live.durable, live.next
}

// claudeLiveStream serves GET sessions/{id}/live as Server-Sent Events:
//
//	event: live     data {"items":[{id,turnId,afterSeq,payload}]}
//	event: durable  data {"seq":N}  (durable events up to N exist; read them)
//	event: reset    data {}         (missed ephemeral items; drop partial text)
//
// The SSE id is the live cursor, so EventSource reconnects resume after it.
func (server *Server) claudeLiveStream(ctx context.Context, w http.ResponseWriter, r *http.Request, id string) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return claudeError(500, "Streaming is unavailable")
	}
	signal, cancel, ok := server.claudeLive.subscribe(id)
	if !ok {
		return claudeError(429, "Too many live Claude subscribers")
	}
	defer cancel()
	rootOnly := r.URL.Query().Get("scope") == "root"
	cursor, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	if raw := r.URL.Query().Get("after"); cursor == 0 && raw != "" {
		cursor, _ = strconv.ParseInt(raw, 10, 64)
	}
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	var sentDurable int64 = -1
	send := func() error {
		items, reset, durable, next := server.claudeLive.read(id, cursor, rootOnly)
		if reset {
			if _, err := fmt.Fprintf(w, "id: %d\nevent: reset\ndata: {}\n\n", next); err != nil {
				return err
			}
		}
		if len(items) > 0 {
			body, _ := json.Marshal(map[string]any{"items": items})
			if _, err := fmt.Fprintf(w, "id: %d\nevent: live\ndata: %s\n\n", items[len(items)-1].ID, body); err != nil {
				return err
			}
		}
		cursor = next
		if durable != sentDurable {
			sentDurable = durable
			if _, err := fmt.Fprintf(w, "event: durable\ndata: {\"seq\":%d}\n\n", durable); err != nil {
				return err
			}
		}
		flusher.Flush()
		return nil
	}
	if err := send(); err != nil {
		return nil
	}
	keepalive := time.NewTicker(claudeLiveKeepalive)
	defer keepalive.Stop()
	// Periodic reconnection keeps idle proxies and stale authentication bounded.
	lifetime := time.NewTimer(claudeLiveStreamLifetime)
	defer lifetime.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-lifetime.C:
			return nil
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case <-signal:
			if err := send(); err != nil {
				return nil
			}
		}
	}
}
