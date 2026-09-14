//go:build !android

package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type activityTestRequest struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type activityTestReply struct {
	result       any
	notification any
}

func activityTestServer(t *testing.T, reply func(activityTestRequest) any) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var request activityTestRequest
			if err := connection.ReadJSON(&request); err != nil {
				return
			}
			if request.Method == "initialized" {
				continue
			}
			var result any = map[string]any{}
			if request.Method != "initialize" {
				result = reply(request)
			}
			if value, ok := result.(activityTestReply); ok {
				if err := connection.WriteJSON(value.notification); err != nil {
					return
				}
				result = value.result
			}
			if err := connection.WriteJSON(map[string]any{"id": request.ID, "result": result}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

func activityTestManager(listenURL string) *appServerManager {
	manager := newAppServerManager(config{})
	manager.instance = &appServerInstance{listenURL: listenURL, ready: true, done: make(chan struct{})}
	return manager
}

func confirmTestAccountIdle(manager *appServerManager, ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return manager.confirmAccountIdleLocked(ctx)
}

func TestAccountActivityRefreshAfterMissedCompletion(t *testing.T) {
	for _, status := range []string{"unloaded", "idle", "systemError", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			url := activityTestServer(t, func(request activityTestRequest) any {
				if request.Method == "thread/loaded/list" {
					ids := []string{}
					if status != "unloaded" {
						ids = append(ids, "thread-one")
					}
					return map[string]any{"data": ids}
				}
				if request.Method != "thread/read" || request.Params["includeTurns"] != false {
					t.Errorf("unexpected probe: %#v", request)
				}
				return map[string]any{"thread": map[string]any{"id": "thread-one", "status": map[string]string{"type": status}}}
			})
			manager := activityTestManager(url)
			manager.activeThreads = map[string]bool{"thread-one": true}
			if err := confirmTestAccountIdle(manager, context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(manager.activeThreads) != 0 {
				t.Fatal("missed completion left account busy")
			}
		})
	}
}

func TestAccountActivityFindsUnobservedChildOnLaterPage(t *testing.T) {
	url := activityTestServer(t, func(request activityTestRequest) any {
		if request.Method == "thread/loaded/list" {
			if request.Params["cursor"] == "children" {
				return map[string]any{"data": []string{"child"}}
			}
			return map[string]any{"data": []string{"parent"}, "nextCursor": "children"}
		}
		id := request.Params["threadId"].(string)
		status := "idle"
		if id == "child" {
			status = "active"
		}
		return map[string]any{"thread": map[string]any{"id": id, "status": map[string]string{"type": status}}}
	})
	manager := activityTestManager(url)
	if err := confirmTestAccountIdle(manager, context.Background()); err == nil {
		t.Fatal("active child was treated as idle")
	}
	if len(manager.activeThreads) != 1 || !manager.activeThreads["child"] {
		t.Fatalf("real activity was not projected: %#v", manager.activeThreads)
	}
}

func TestAccountActivityUncertainSnapshotsCannotClearBusy(t *testing.T) {
	for _, scenario := range []string{"missing-list", "missing-status", "unknown-status", "repeated-cursor", "new-child", "status-changed", "query-disconnected"} {
		t.Run(scenario, func(t *testing.T) {
			lists := 0
			url := activityTestServer(t, func(request activityTestRequest) any {
				if request.Method == "thread/loaded/list" {
					lists++
					switch scenario {
					case "missing-list":
						return map[string]any{}
					case "repeated-cursor":
						return map[string]any{"data": []string{}, "nextCursor": "same"}
					case "new-child":
						if lists > 1 {
							return map[string]any{"data": []string{"thread-one", "child"}}
						}
					case "status-changed":
						if lists > 1 {
							return activityTestReply{result: map[string]any{"data": []string{"thread-one"}}, notification: map[string]any{
								"method": "thread/status/changed", "params": map[string]any{"threadId": "thread-one", "status": map[string]string{"type": "active"}},
							}}
						}
					}
					return map[string]any{"data": []string{"thread-one"}}
				}
				if scenario == "missing-status" {
					return map[string]any{"thread": map[string]string{"id": "thread-one"}}
				}
				status := "idle"
				if scenario == "unknown-status" {
					status = "future-active-status"
				}
				return map[string]any{"thread": map[string]any{"id": "thread-one", "status": map[string]string{"type": status}}}
			})
			if scenario == "query-disconnected" {
				url = "ws://127.0.0.1:0"
			}
			manager := activityTestManager(url)
			manager.activeThreads = map[string]bool{"thread-one": true}
			if err := confirmTestAccountIdle(manager, context.Background()); err == nil {
				t.Fatal("uncertain snapshot allowed stopping")
			}
			if !manager.activeThreads["thread-one"] {
				t.Fatal("uncertain query cleared busy evidence")
			}
			if manager.activityChecking {
				t.Fatal("failed probe leaked its execution gate")
			}
		})
	}
}

func TestAccountActivityProbeGatesExecutionWithoutBlockingReports(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	url := activityTestServer(t, func(request activityTestRequest) any {
		close(entered)
		<-release
		return map[string]any{"data": []string{}}
	})
	manager := activityTestManager(url)
	// report() expects a process for a live instance, without operating on it.
	manager.instance.command = &exec.Cmd{Process: &os.Process{Pid: 1}}
	done := make(chan error, 1)
	go func() { done <- confirmTestAccountIdle(manager, context.Background()) }()
	<-entered
	defer close(release)
	if manager.report()["status"] != "running" {
		t.Fatal("probe hid the running instance")
	}
	for _, method := range []string{"thread/start", "thread/resume", "thread/fork", "turn/start", "turn/steer", "thread/compact/start", "thread/realtime/start"} {
		request, _ := json.Marshal(map[string]any{"id": 1, "method": method, "params": map[string]string{"threadId": "thread-one"}})
		if err := manager.reserveAccountRequest(manager.instance, "browser", request); err == nil {
			t.Errorf("probe admitted %s", method)
		}
	}
	if err := manager.beginThreadManagement("handoff", []string{"thread-one"}); err == nil {
		t.Error("probe admitted a competing handoff")
	}
	if err := manager.beginAccountManagement("login"); err == nil {
		t.Error("probe admitted credential management")
	}
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func TestAccountActivityProbeCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	url := activityTestServer(t, func(request activityTestRequest) any {
		close(entered)
		<-release
		return map[string]any{"data": []string{}}
	})
	defer close(release)
	manager := activityTestManager(url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- confirmTestAccountIdle(manager, ctx) }()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil || manager.activityChecking {
			t.Fatalf("cancelled probe accepted idle or retained gate: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled probe did not finish")
	}
}

func TestAccountActivityPendingAndManagementRemainProtected(t *testing.T) {
	for _, kind := range []string{"pending", "login", "handoff"} {
		t.Run(kind, func(t *testing.T) {
			manager := activityTestManager("ws://127.0.0.1:0")
			switch kind {
			case "pending":
				manager.pendingTurns = map[string]accountPendingRequest{"browser:1": {Method: "thread/start"}}
			case "login":
				manager.managementSession = "login"
			case "handoff":
				manager.threadManagement = map[string]string{"thread-one": "handoff"}
			}
			if err := confirmTestAccountIdle(manager, context.Background()); err == nil {
				t.Fatal("unconfirmed operation allowed stopping")
			}
			if kind == "pending" && len(manager.pendingTurns) != 1 {
				t.Fatal("lost response was silently treated as acknowledgement")
			}
		})
	}
}

func TestAccountActivityStoppedProcessClearsOldMarkers(t *testing.T) {
	manager := activityTestManager("")
	close(manager.instance.done)
	manager.activeThreads = map[string]bool{"thread-one": true}
	manager.pendingTurns = map[string]accountPendingRequest{"browser:1": {Method: "thread/start"}}
	manager.lastError = "账号仍有任务执行，请先结束任务"
	if err := manager.reconcile(context.Background(), desiredAppServer{}); err != nil {
		t.Fatal(err)
	}
	if len(manager.activeThreads) != 0 || len(manager.pendingTurns) != 0 || manager.lastError != "" {
		t.Fatal("stopped process retained execution markers or stale error")
	}
}

func TestAccountActivityResponseOrderingAndRuntimeIsolation(t *testing.T) {
	manager := newAppServerManager(config{})
	request := []byte(`{"id":1,"method":"turn/start","params":{"threadId":"thread-one"}}`)
	if err := manager.reserveAccountRequest(manager.instance, "browser", request); err != nil {
		t.Fatal(err)
	}
	manager.observeAccountResponse(nil, "browser", []byte(`{"id":1,"method":"item/tool/call","params":{}}`))
	if len(manager.pendingTurns) != 1 {
		t.Fatal("server request with colliding ID released client request")
	}
	manager.observeAccountThread([]byte(`{"method":"turn/completed","params":{"threadId":"thread-one"}}`))
	if len(manager.pendingTurns) != 1 {
		t.Fatal("completion incorrectly acknowledged a pending request")
	}
	manager.observeAccountResponse(nil, "browser", []byte(`{"id":1,"result":{"turn":{"id":"turn-one","status":"inProgress"}}}`))
	if len(manager.pendingTurns) != 0 || len(manager.activeThreads) != 0 {
		t.Fatal("late startup response resurrected completed activity")
	}
	for _, status := range []string{"completed", "interrupted", "failed"} {
		if err := manager.reserveAccountRequest(manager.instance, "browser", request); err != nil {
			t.Fatal(err)
		}
		manager.observeAccountResponse(nil, "browser", []byte(fmt.Sprintf(`{"id":1,"result":{"turn":{"status":%q}}}`, status)))
		if len(manager.activeThreads) != 0 {
			t.Fatal("terminal response marked account busy")
		}
	}
	old := &appServerInstance{done: make(chan struct{})}
	manager.instance = &appServerInstance{done: make(chan struct{})}
	if err := manager.reserveAccountRequest(old, "browser", request); err == nil || len(manager.pendingTurns) != 0 {
		t.Fatal("old tunnel request polluted new runtime reservations")
	}
	manager.observeAccountResponse(old, "browser", []byte(`{"method":"turn/started","params":{"threadId":"old-thread"}}`))
	if len(manager.activeThreads) != 0 {
		t.Fatal("old runtime notification polluted the new runtime")
	}
}

func TestAccountTunnelDisconnectDrainsPendingResponse(t *testing.T) {
	for _, closeAll := range []bool{false, true} {
		t.Run(fmt.Sprint(closeAll), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			url := activityTestServer(t, func(request activityTestRequest) any {
				close(entered)
				<-release
				return map[string]any{"turn": map[string]string{"status": "completed"}}
			})
			manager := activityTestManager(url)
			upgrader := websocket.Upgrader{}
			control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connection, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer connection.Close()
				for {
					if _, _, err := connection.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer control.Close()
			connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(control.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			client := &controlClient{appServer: manager, connection: connection,
				tunnels: map[string]*websocket.Conn{}, tunnelAccounts: map[string]appServerTunnelAccount{}}
			if err := client.openAppServerTunnel(context.Background(), "browser"); err != nil {
				t.Fatal(err)
			}
			defer client.closeTunnels()
			client.handleMessage(context.Background(), controlMessage{Type: "appserver.message", SessionID: "browser",
				Payload: `{"id":1,"method":"turn/start","params":{"threadId":"thread-one"}}`})
			<-entered
			if closeAll {
				client.closeTunnels()
			} else {
				client.closeTunnel("browser")
			}
			if !manager.hasPendingAccountRequests(manager.instance, "browser") {
				t.Error("disconnect acknowledged an unfinished request")
			}
			close(release)
			deadline := time.Now().Add(time.Second)
			for manager.hasPendingAccountRequests(manager.instance, "browser") {
				if time.Now().After(deadline) {
					t.Fatal("detached tunnel discarded the local acknowledgement")
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestAccountActivityProcessHelper(t *testing.T) {
	if os.Getenv("MIRA_TEST_ACTIVITY_PROCESS") == "1" {
		for {
			time.Sleep(time.Hour)
		}
	}
}

func TestAccountStopRetiresIdleProcessWithStaleBusyCache(t *testing.T) {
	url := activityTestServer(t, func(request activityTestRequest) any {
		return map[string]any{"data": []string{}}
	})
	manager := activityTestManager(url)
	instance := manager.instance
	command := exec.Command(os.Args[0], "-test.run=^TestAccountActivityProcessHelper$")
	command.Env = append(os.Environ(), "MIRA_TEST_ACTIVITY_PROCESS=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	instance.command = command
	go func() { _ = command.Wait(); close(instance.done) }()
	t.Cleanup(func() {
		if !channelClosed(instance.done) {
			_ = command.Process.Kill()
			<-instance.done
		}
	})
	manager.activeThreads = map[string]bool{"lost-completion": true}
	if err := manager.reconcile(context.Background(), desiredAppServer{}); err != nil {
		t.Fatal(err)
	}
	if !channelClosed(instance.done) || manager.instance != nil || manager.report()["status"] != "stopped" {
		t.Fatal("idle account did not stop")
	}
	if manager.lastError != "" || len(manager.activeThreads) != 0 {
		t.Fatal("stopped account retained busy state")
	}
}
