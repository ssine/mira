package channel

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDisconnectedProxyObservesActiveTurnUntilCompletion(t *testing.T) {
	client := &proxy{sessionID: "session", runningThreads: map[string]string{"thread": "turn"},
		boundThreadIDs: map[string]bool{"thread": true}, nodeMessagesDone: make(chan struct{}),
		socket: &socket{internalWrite: func([]byte) error { t.Fatal("wrote to detached browser"); return nil }, internalClose: func() {}},
	}
	broker := &Channel{proxies: map[string]*proxy{"session": client}, nodeSockets: map[string]*socket{}}
	broker.markProxyClientClosed(client, false)
	if broker.proxies["session"] != client {
		t.Fatal("active proxy was detached from Node")
	}
	complete := func(turn string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": map[string]any{"id": turn, "status": "completed"}}})
		if err := broker.forwardAppServerMessage(context.Background(), client, raw); err != nil {
			t.Fatal(err)
		}
	}
	complete("old-turn")
	if broker.proxies["session"] != client {
		t.Fatal("stale completion detached the running turn")
	}
	complete("turn")
	if len(broker.proxies) != 0 {
		t.Fatal("completed proxy leaked")
	}
	select {
	case <-client.nodeMessagesDone:
	default:
		t.Fatal("proxy worker did not stop")
	}
}

func TestFailedDetachedProxyCleansUpImmediately(t *testing.T) {
	client := &proxy{sessionID: "session", clientClosed: true, runningThreads: map[string]string{"thread": "turn"}, nodeMessagesDone: make(chan struct{})}
	broker := &Channel{proxies: map[string]*proxy{"session": client}, nodeSockets: map[string]*socket{}}
	broker.markProxyClientClosed(client, true)
	if len(broker.proxies) != 0 {
		t.Fatal("failed detached proxy leaked")
	}
}
