package node

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func residencyFixture(id string) residencyRuntime {
	instance := &appServerInstance{runtimeID: id, listenURL: id, ready: true, done: make(chan struct{}), command: &exec.Cmd{Process: &os.Process{Pid: 42}}}
	return residencyRuntime{manager: &appServerManager{instance: instance}, instance: instance, ready: true}
}

func TestCodexResidencySharedBudgetAndLRU(t *testing.T) {
	first, second := residencyFixture("first"), residencyFixture("second")
	rss := uint64(600)
	var mu sync.Mutex
	var evicted []string
	controller := codexResidencyController{budget: codexMemoryBudget{bytes: 1000}, total: func() (uint64, error) { return 10000, nil },
		resident: func(int) (uint64, error) { return rss, nil },
		call: func(_ context.Context, url string, evict *residencyCandidate) (residencyResponse, error) {
			if evict != nil {
				mu.Lock()
				evicted = append(evicted, url)
				mu.Unlock()
				return residencyResponse{EvictionScheduled: true}, nil
			}
			at := int64(200)
			if url == "second" {
				at = 100
			}
			return residencyResponse{Oldest: &residencyCandidate{ThreadID: url, Revision: url, IdleAt: at}}, nil
		},
	}
	now := time.Now()
	controller.poll(context.Background(), []residencyRuntime{first, second}, now)
	if len(evicted) != 1 || evicted[0] != "second" {
		t.Fatalf("wrong global LRU: %v", evicted)
	}
	if first.manager.residencyStatus.BudgetBytes != 1000 || first.manager.residencyStatus.ResidentBytes != 1200 {
		t.Fatalf("budget multiplied per account: %+v", first.manager.residencyStatus)
	}
	controller.poll(context.Background(), []residencyRuntime{first, second}, now.Add(5*time.Second))
	if len(evicted) != 1 {
		t.Fatal("unload was not paced")
	}
	rss = 450
	controller.poll(context.Background(), []residencyRuntime{first, second}, now.Add(20*time.Second))
	if len(evicted) != 1 || controller.pressure {
		t.Fatal("pressure did not stop below hysteresis threshold")
	}
	rss = 475
	controller.poll(context.Background(), []residencyRuntime{first, second}, now.Add(40*time.Second))
	if len(evicted) != 1 {
		t.Fatal("evicted below budget")
	}
}

func TestCodexResidencyFailuresAndReplacement(t *testing.T) {
	runtime := residencyFixture("one")
	calls := 0
	controller := codexResidencyController{budget: codexMemoryBudget{bytes: 100}, total: func() (uint64, error) { return 1000, nil },
		resident: func(int) (uint64, error) { return 200, nil },
		call: func(context.Context, string, *residencyCandidate) (residencyResponse, error) {
			calls++
			return residencyResponse{}, errResidencyUnsupported
		},
	}
	controller.poll(context.Background(), []residencyRuntime{runtime}, time.Now())
	if runtime.manager.residencyStatus.Status != "unsupported" || runtime.manager.residencyUnsupported != runtime.instance {
		t.Fatal("old runtime incorrectly reported supported")
	}
	controller.resident = func(int) (uint64, error) { return 0, errors.New("sample failed") }
	controller.poll(context.Background(), []residencyRuntime{runtime}, time.Now())
	if calls != 1 || runtime.manager.residencyStatus.Status != "unavailable" {
		t.Fatal("renewed lease without memory accounting")
	}
	controller.resident = func(int) (uint64, error) { return 200, nil }
	controller.call = func(_ context.Context, _ string, evict *residencyCandidate) (residencyResponse, error) {
		if evict != nil {
			t.Error("evicted replacement instance")
		}
		runtime.manager.mu.Lock()
		runtime.manager.instance = residencyFixture("replacement").instance
		runtime.manager.mu.Unlock()
		return residencyResponse{Oldest: &residencyCandidate{ThreadID: "old", Revision: "1", IdleAt: 1}}, nil
	}
	controller.poll(context.Background(), []residencyRuntime{runtime}, time.Now())
	if runtime.canEvict() {
		t.Fatal("stale instance allowed eviction")
	}
}

func TestCodexResidencyManagementAndExternalControl(t *testing.T) {
	runtime := residencyFixture("one")
	runtime.manager.pendingTurns = map[string]accountPendingRequest{"pending": {}}
	if runtime.canEvict() {
		t.Fatal("eviction raced unacknowledged execution")
	}
	runtime.manager.pendingTurns = nil
	runtime.manager.threadManagement = map[string]string{"thread": "handoff"}
	if runtime.canEvict() {
		t.Fatal("eviction raced handoff")
	}
	if err := runtime.manager.reserveAccountRequest(runtime.instance, "browser", []byte(`{"id":1,"method":"mira/thread/residency","params":{}}`)); err == nil {
		t.Fatal("external residency control accepted")
	}
}

func TestCodexResidencyLostEvictionAcknowledgementIsPaced(t *testing.T) {
	runtime := residencyFixture("one")
	attempts := 0
	controller := codexResidencyController{budget: codexMemoryBudget{bytes: 100}, total: func() (uint64, error) { return 1000, nil },
		resident: func(int) (uint64, error) { return 200, nil }, call: func(_ context.Context, _ string, evict *residencyCandidate) (residencyResponse, error) {
			if evict != nil {
				attempts++
				return residencyResponse{}, errors.New("ack lost")
			}
			return residencyResponse{Oldest: &residencyCandidate{ThreadID: "thread", Revision: "token", IdleAt: 100}}, nil
		}}
	now := time.Now()
	controller.poll(context.Background(), []residencyRuntime{runtime}, now)
	if runtime.manager.residencyStatus.Status != "unavailable" {
		t.Fatal("lost acknowledgement hidden")
	}
	controller.poll(context.Background(), []residencyRuntime{runtime}, now.Add(time.Second))
	if attempts != 1 {
		t.Fatal("blindly retried unacknowledged eviction")
	}
}

func TestCodexResidencyEvictionExcludesNewExecution(t *testing.T) {
	runtime := residencyFixture("one")
	if !runtime.beginEviction() {
		t.Fatal("idle runtime could not reserve eviction")
	}
	if runtime.canEvict() {
		t.Fatal("overlapping eviction accepted")
	}
	for _, method := range []string{"turn/start", "thread/resume", "thread/compact/start"} {
		if err := runtime.manager.reserveAccountRequest(runtime.instance, "browser", []byte(`{"id":1,"method":"`+method+`","params":{"threadId":"thread"}}`)); err == nil {
			t.Fatalf("%s raced eviction", method)
		}
	}
}
