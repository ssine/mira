package node

import (
	"context"
	"errors"
	"sync"
	"time"
)

type codexResidencyStatus struct {
	Status               string `json:"status"`
	BudgetBytes          uint64 `json:"budgetBytes"`
	ResidentBytes        uint64 `json:"residentBytes"`
	EffectiveMemoryBytes uint64 `json:"effectiveMemoryBytes"`
	Error                string `json:"error,omitempty"`
}

type residencyRuntime struct {
	manager     *appServerManager
	instance    *appServerInstance
	ready       bool
	unsupported bool
}

type codexResidencyController struct {
	budget       codexMemoryBudget
	total        func() (uint64, error)
	resident     func(int) (uint64, error)
	call         func(context.Context, string, *residencyCandidate) (residencyResponse, error)
	pressure     bool
	nextEviction time.Time
}

func (client *controlClient) residencyRuntimes() []residencyRuntime {
	client.accountsMu.Lock()
	managers := map[*appServerManager]bool{client.appServer: true}
	for _, runtime := range client.accountRuntimes {
		managers[runtime.manager] = true
	}
	client.accountsMu.Unlock()
	var result []residencyRuntime
	for manager := range managers {
		if manager == nil {
			continue
		}
		manager.mu.Lock()
		instance := manager.instance
		if instance != nil && instance.command != nil && instance.command.Process != nil && !channelClosed(instance.done) {
			result = append(result, residencyRuntime{manager: manager, instance: instance, ready: instance.ready,
				unsupported: manager.residencyUnsupported == instance})
		}
		manager.mu.Unlock()
	}
	return result
}

func (client *controlClient) startCodexResidency(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller := codexResidencyController{budget: client.configuration.CodexMemoryBudget,
			total: codexEffectiveMemory, resident: codexResidentBytes, call: callCodexResidency}
		for ctx.Err() == nil {
			controller.poll(ctx, client.residencyRuntimes(), time.Now())
			if !sleepContext(ctx, 5*time.Second) {
				return
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (controller *codexResidencyController) poll(ctx context.Context, runtimes []residencyRuntime, now time.Time) {
	pollStarted := time.Now()
	if len(runtimes) == 0 {
		controller.pressure = false
		return
	}
	total, err := controller.total()
	status := codexResidencyStatus{Status: "active", EffectiveMemoryBytes: total, BudgetBytes: controller.budget.resolve(total)}
	for _, runtime := range runtimes {
		rss, sampleErr := controller.resident(runtime.instance.command.Process.Pid)
		if sampleErr != nil {
			err = sampleErr
		}
		status.ResidentBytes += rss
	}
	if err != nil || total == 0 || status.BudgetBytes == 0 {
		status.Status = "unavailable"
		status.Error = "memory accounting unavailable; runtime idle timeout remains in effect"
		for _, runtime := range runtimes {
			runtime.reportResidency(status)
		}
		// Let the control lease expire when reliable accounting is lost.
		return
	}
	if status.ResidentBytes > status.BudgetBytes {
		controller.pressure = true
	}
	if status.ResidentBytes <= status.BudgetBytes-status.BudgetBytes/10 {
		controller.pressure = false
	}
	type observation struct {
		result residencyResponse
		err    error
	}
	observations := make([]observation, len(runtimes))
	// Bound latency and fan-out independently of the control WebSocket and heartbeats.
	slots := make(chan struct{}, 16)
	var workers sync.WaitGroup
	for index, runtime := range runtimes {
		if !runtime.ready || runtime.unsupported {
			continue
		}
		workers.Add(1)
		go func(index int, runtime residencyRuntime) {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				observations[index].err = ctx.Err()
				return
			}
			defer func() { <-slots }()
			observations[index].result, observations[index].err = controller.call(ctx, runtime.instance.listenURL, nil)
		}(index, runtime)
	}
	workers.Wait()
	selected := -1
	for index, runtime := range runtimes {
		observation := observations[index]
		current := status
		switch {
		case runtime.unsupported || errors.Is(observation.err, errResidencyUnsupported):
			current.Status = "unsupported"
			current.Error = errResidencyUnsupported.Error()
		case !runtime.ready:
			current.Status = "starting"
		case observation.err != nil:
			current.Status = "unavailable"
			current.Error = observation.err.Error()
		}
		runtime.reportResidency(current)
		candidate := observation.result.Oldest
		if observation.err == nil && candidate != nil && runtime.canEvict() && (selected < 0 || candidate.IdleAt < observations[selected].result.Oldest.IdleAt || candidate.IdleAt == observations[selected].result.Oldest.IdleAt && candidate.Revision < observations[selected].result.Oldest.Revision) {
			selected = index
		}
	}
	if !controller.pressure || selected < 0 || now.Before(controller.nextEviction) || ctx.Err() != nil {
		return
	}
	runtime := runtimes[selected]
	if !runtime.beginEviction() {
		return
	}
	defer func() {
		runtime.manager.mu.Lock()
		runtime.manager.residencyEvicting = false
		runtime.manager.mu.Unlock()
	}()
	// Schedule at most one unload per 15 seconds, including lost acknowledgements.
	// Native shutdown can take 10 seconds; resample RSS before considering another.
	controller.nextEviction = now.Add(time.Since(pollStarted) + 15*time.Second)
	if _, err := controller.call(ctx, runtime.instance.listenURL, observations[selected].result.Oldest); err != nil {
		status.Status = "unavailable"
		status.Error = err.Error()
		runtime.reportResidency(status)
	}
}

func (runtime residencyRuntime) canEvict() bool {
	manager := runtime.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return runtime.canEvictLocked()
}

func (runtime residencyRuntime) beginEviction() bool {
	manager := runtime.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !runtime.canEvictLocked() {
		return false
	}
	manager.residencyEvicting = true
	return true
}

func (runtime residencyRuntime) canEvictLocked() bool {
	manager := runtime.manager
	return manager.instance == runtime.instance && !channelClosed(runtime.instance.done) &&
		!manager.residencyEvicting && !manager.transitioning && !manager.activityChecking && manager.managementSession == "" &&
		len(manager.threadManagement) == 0 && len(manager.pendingTurns) == 0
}

func (runtime residencyRuntime) reportResidency(status codexResidencyStatus) {
	manager := runtime.manager
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.instance != runtime.instance {
		return
	}
	manager.residencyStatus = status
	if status.Status == "unsupported" {
		manager.residencyUnsupported = runtime.instance
	}
}
