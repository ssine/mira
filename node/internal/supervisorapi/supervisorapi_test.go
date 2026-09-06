package supervisorapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/supervisor"
)

type fakeManager struct {
	started chan context.Context
	release chan struct{}
	err     error
	once    sync.Once
}

func newFakeManager(err error) *fakeManager {
	return &fakeManager{started: make(chan context.Context, 1), release: make(chan struct{}), err: err}
}

func (manager *fakeManager) ApplyUpdate(ctx context.Context, version string) (supervisor.Candidate, error) {
	manager.started <- ctx
	select {
	case <-manager.release:
		return supervisor.Candidate{Version: version}, manager.err
	case <-ctx.Done():
		return supervisor.Candidate{}, ctx.Err()
	}
}

func (manager *fakeManager) finish() {
	manager.once.Do(func() { close(manager.release) })
}

func waitStarted(t *testing.T, manager *fakeManager) context.Context {
	t.Helper()
	select {
	case ctx := <-manager.started:
		return ctx
	case <-time.After(3 * time.Second):
		t.Fatal("update manager did not start")
		return nil
	}
}

func closeServer(t *testing.T, server *Server, manager *fakeManager) {
	t.Helper()
	manager.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := server.Close(ctx); err != nil {
		t.Errorf("close Supervisor API: %v", err)
	}
}

func TestRequestDisconnectDoesNotCancelUpdate(t *testing.T) {
	stateDir := t.TempDir()
	manager := newFakeManager(nil)
	server, err := Start(Config{StateDir: stateDir, Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeServer(t, server, manager) })
	client, err := Discover(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	address := strings.TrimPrefix(client.Endpoint, "http://")
	connection, err := net.Dial("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"version":"2.0.0"}`
	request := fmt.Sprintf("POST /v1/update HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", address, client.Token, len(body), body)
	if _, err := connection.Write([]byte(request)); err != nil {
		connection.Close()
		t.Fatal(err)
	}
	// Closing without reading the response simulates the CLI/SSH transport
	// disappearing as soon as it submitted a complete request.
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	managerContext := waitStarted(t, manager)
	select {
	case <-managerContext.Done():
		t.Fatalf("request disconnect cancelled manager: %v", managerContext.Err())
	case <-time.After(50 * time.Millisecond):
	}
	status, err := client.Status(context.Background())
	if err != nil || status.Phase != PhaseSwitching {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	manager.finish()
	finished, err := client.Wait(context.Background(), status.OperationID)
	if err != nil || finished.Phase != PhaseSucceeded || finished.FinishedAt == nil {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
	assertPrivateFiles(t, stateDir)
}

func TestRolledBackStatusAndSingleFlightPersistAfterClientCancellation(t *testing.T) {
	stateDir := t.TempDir()
	manager := newFakeManager(NewRolledBackError(errors.New("new Server failed health check; old Server restored")))
	server, err := Start(Config{StateDir: stateDir, Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeServer(t, server, manager) })
	client, err := Discover(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	operation, err := client.RequestUpdate(requestContext, "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	cancelRequest()
	managerContext := waitStarted(t, manager)
	if managerContext.Err() != nil {
		t.Fatalf("manager inherited request cancellation: %v", managerContext.Err())
	}
	if _, err := client.RequestUpdate(context.Background(), "3.0.0"); !errors.Is(err, ErrUpdateInProgress) {
		t.Fatalf("concurrent update error: %v", err)
	}
	manager.finish()
	finished, err := client.Wait(context.Background(), operation.OperationID)
	if err != nil || finished.Phase != PhaseRolledBack || !strings.Contains(finished.Error, "old Server restored") {
		t.Fatalf("finished=%+v err=%v", finished, err)
	}
	persisted, err := readStatus(server.paths.status)
	if err != nil || persisted.Phase != PhaseRolledBack || persisted.OperationID != operation.OperationID {
		t.Fatalf("persisted=%+v err=%v", persisted, err)
	}
}

func TestAPIRejectsMissingTokenAndNonLoopbackListener(t *testing.T) {
	manager := newFakeManager(nil)
	server, err := Start(Config{StateDir: t.TempDir(), Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeServer(t, server, manager) })
	response, err := http.Get(server.URL() + "/v1/update/status")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without bearer token=%d", response.StatusCode)
	}

	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	used := false
	_, err = Start(Config{
		StateDir: t.TempDir(), Manager: newFakeManager(nil),
		Listen: func(_, _ string) (net.Listener, error) {
			if used {
				return nil, errors.New("listener reused")
			}
			used = true
			return listener, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "loopback-only") {
		t.Fatalf("non-loopback listener error: %v", err)
	}
}

func TestStartupMarksInterruptedOperationFailed(t *testing.T) {
	stateDir := t.TempDir()
	paths, err := controlPaths(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	interrupted := OperationStatus{OperationID: "0123456789abcdef0123456789abcdef", Phase: PhaseSwitching, Version: "1.2.3", CreatedAt: now, UpdatedAt: now}
	if err := writeStatus(paths.status, interrupted); err != nil {
		t.Fatal(err)
	}
	manager := newFakeManager(nil)
	server, err := Start(Config{StateDir: stateDir, Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeServer(t, server, manager) })
	client, err := Discover(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := client.Status(context.Background())
	if err != nil || status.Phase != PhaseFailed || status.FinishedAt == nil || !strings.Contains(status.Error, "restarted") {
		t.Fatalf("recovered status=%+v err=%v", status, err)
	}
}

func TestCloseDoesNotCancelUpdateAndWaitSurvivesEndpointRemoval(t *testing.T) {
	stateDir := t.TempDir()
	manager := newFakeManager(nil)
	server, err := Start(Config{StateDir: stateDir, Manager: manager})
	if err != nil {
		t.Fatal(err)
	}
	client, err := Discover(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := client.RequestUpdate(context.Background(), "2.0.0")
	if err != nil {
		t.Fatal(err)
	}
	managerContext := waitStarted(t, manager)
	shortContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = server.Close(shortContext)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close while update runs returned %v", err)
	}
	select {
	case <-managerContext.Done():
		t.Fatalf("Close cancelled update manager: %v", managerContext.Err())
	default:
	}
	manager.finish()
	closeContext, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelClose()
	if err := server.Close(closeContext); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{EndpointFileName, TokenFileName} {
		if _, err := os.Stat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discovery file %s remains after Close: %v", name, err)
		}
	}
	finished, err := client.Wait(context.Background(), operation.OperationID)
	if err != nil || finished.Phase != PhaseSucceeded {
		t.Fatalf("Wait did not use persisted handoff status: status=%+v err=%v", finished, err)
	}
}

func assertPrivateFiles(t *testing.T, stateDir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	for _, name := range []string{EndpointFileName, TokenFileName, StatusFileName} {
		info, err := os.Stat(stateDir + string(os.PathSeparator) + name)
		if err != nil {
			t.Fatal(err)
		}
		if permissions := info.Mode().Perm(); permissions != 0600 {
			t.Errorf("%s permissions=%o", name, permissions)
		}
	}
}
