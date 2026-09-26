//go:build !android

package node

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCodexResidencyRPC(t *testing.T) {
	candidate := &residencyCandidate{ThreadID: "thread", Revision: "revision", IdleAt: 100}
	url := activityTestServer(t, func(request activityTestRequest) any {
		if request.Method != "mira/thread/residency" {
			t.Errorf("unexpected RPC %s", request.Method)
		}
		payload, _ := json.Marshal(request.Params["evict"])
		var actual residencyCandidate
		if json.Unmarshal(payload, &actual) != nil || !reflect.DeepEqual(&actual, candidate) {
			t.Error("eviction token changed")
		}
		return activityTestReply{notification: map[string]any{"method": "thread/status/changed", "params": map[string]any{}}, result: residencyResponse{EvictionScheduled: true}}
	})
	result, err := callCodexResidency(context.Background(), url, candidate)
	if err != nil || result != (residencyResponse{EvictionScheduled: true}) {
		t.Fatalf("RPC: %+v %v", result, err)
	}
}

func TestCodexResidencyRPCUnsupportedAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name  string
		stall bool
	}{{"unsupported", false}, {"cancel", true}} {
		t.Run(test.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					var request activityTestRequest
					if conn.ReadJSON(&request) != nil {
						return
					}
					if request.Method == "initialized" {
						continue
					}
					if request.Method == "initialize" {
						_ = conn.WriteJSON(map[string]any{"id": request.ID, "result": map[string]any{}})
						continue
					}
					if test.stall {
						_, _, _ = conn.ReadMessage()
						return
					}
					_ = conn.WriteJSON(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601}})
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := callCodexResidency(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err == nil {
				t.Fatal("failure accepted")
			}
			if !test.stall && !errors.Is(err, errResidencyUnsupported) {
				t.Fatalf("wrong compatibility result: %v", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("cancellation did not close local RPC")
			}
		})
	}
}

func TestCodexResidencyRPCMalformedResponse(t *testing.T) {
	url := activityTestServer(t, func(activityTestRequest) any { return map[string]any{} })
	if _, err := callCodexResidency(context.Background(), url, nil); err == nil {
		t.Fatal("missing lease response accepted")
	}
}
