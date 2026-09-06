package accountsampler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type fakeRegistry struct {
	nodes []nodes.Node
}

func (registry *fakeRegistry) List(context.Context, bool) ([]nodes.Node, error) {
	return append([]nodes.Node(nil), registry.nodes...), nil
}

func (registry *fakeRegistry) Get(_ context.Context, nodeID string, _ bool) (*nodes.Node, error) {
	for index := range registry.nodes {
		if registry.nodes[index].NodeID == nodeID {
			copy := registry.nodes[index]
			return &copy, nil
		}
	}
	return nil, nil
}

type fakeConnectivity map[string]bool

func (connected fakeConnectivity) IsConnected(nodeID string) bool { return connected[nodeID] }

func eligibleNode(id string) nodes.Node {
	return nodes.Node{
		NodeID: id, ApprovalStatus: "approved", Status: "online",
		Capabilities: map[string]any{"appServer": true},
		ReportedAppServer: map[string]any{
			"status": "running", "codexHome": "/home/test", "codexPath": "/usr/bin/codex",
		},
	}
}

func TestEligibleAndRuntimeKey(t *testing.T) {
	node := eligibleNode("node-1")
	connected := fakeConnectivity{"node-1": true}
	if !Eligible(&node, connected) {
		t.Fatal("expected approved, online App Server to be eligible")
	}
	if got, want := RuntimeKey(&node), "15bce7e62242d978bd7d2525e074585aeb934e6df58f3f4d1bf9dfc691dabfda"; got != want {
		t.Fatalf("runtime key = %q, want JavaScript fixture %q", got, want)
	}
	checks := []func(*nodes.Node){
		func(value *nodes.Node) { value.ApprovalStatus = "pending" },
		func(value *nodes.Node) { value.Status = "offline" },
		func(value *nodes.Node) { value.Capabilities["appServer"] = false },
		func(value *nodes.Node) { value.ReportedAppServer["status"] = "stopped" },
	}
	for index, change := range checks {
		copy := eligibleNode("node-1")
		change(&copy)
		if Eligible(&copy, connected) {
			t.Fatalf("ineligible variant %d was accepted", index)
		}
	}
	if Eligible(&node, fakeConnectivity{}) {
		t.Fatal("disconnected Node was accepted")
	}
}

func TestProjectWeeklyQuota(t *testing.T) {
	result := map[string]any{
		"rateLimitsByLimitId": map[string]any{
			"codex": map[string]any{
				"limitId": "codex",
				"primary": map[string]any{"windowDurationMins": 300, "usedPercent": 99},
				"secondary": map[string]any{
					"windowDurationMins": json.Number("10080"),
					"usedPercent":        json.Number("10.5"),
					"resetsAt":           json.Number("2000000000"),
				},
			},
		},
		"rateLimits":            map[string]any{"primary": map[string]any{"windowDurationMins": 10080, "usedPercent": 100}},
		"rateLimitResetCredits": map[string]any{"availableCount": json.Number("0")},
	}
	quota := ProjectWeeklyQuota(result)
	if quota.Remaining == nil || *quota.Remaining != 89.5 {
		t.Fatalf("remaining = %v, want 89.5", quota.Remaining)
	}
	if quota.ResetsAtMillis == nil || *quota.ResetsAtMillis != 2_000_000_000_000 {
		t.Fatalf("resetsAt = %v, want milliseconds", quota.ResetsAtMillis)
	}
	if quota.ResetCount == nil || *quota.ResetCount != 0 {
		t.Fatalf("resetCount = %v, want zero to be preserved", quota.ResetCount)
	}

	clamped := ProjectWeeklyQuota(map[string]any{
		"rateLimits": map[string]any{"primary": map[string]any{"windowDurationMins": 10080, "usedPercent": 150}},
	})
	if clamped.Remaining == nil || *clamped.Remaining != 0 {
		t.Fatalf("remaining = %v, want clamped zero", clamped.Remaining)
	}
	foreign := ProjectWeeklyQuota(map[string]any{
		"rateLimitsByLimitId": map[string]any{"codex": map[string]any{
			"limitId": "other", "primary": map[string]any{"windowDurationMins": 10080, "usedPercent": 10},
		}},
		"rateLimits": map[string]any{"primary": map[string]any{"windowDurationMins": 10080, "usedPercent": 20}},
	})
	if foreign.Remaining != nil {
		t.Fatal("a present but foreign codex snapshot must not fall back to rateLimits")
	}
}

func TestSafeTextMatchesJavaScriptUTF16Limit(t *testing.T) {
	if got := safeText(strings.Repeat("🙂", 256)); got == nil {
		t.Fatal("512 UTF-16 code units should be accepted")
	}
	if got := safeText(strings.Repeat("🙂", 257)); got != nil {
		t.Fatal("514 UTF-16 code units should be rejected")
	}
	if got := safeText("secret\nvalue"); got != nil {
		t.Fatal("C0 control characters should be rejected")
	}
	if got := safeText(42); got != nil {
		t.Fatal("non-string values should be rejected")
	}
}

func TestSamplerUsesTwoBoundedWorkers(t *testing.T) {
	registry := &fakeRegistry{}
	connected := fakeConnectivity{}
	for index := range 5 {
		id := string(rune('a' + index))
		registry.nodes = append(registry.nodes, eligibleNode(id))
		connected[id] = true
	}
	started := make(chan struct{}, len(registry.nodes))
	release := make(chan struct{})
	var active, maximum, completed atomic.Int32
	sampler := New(nil, registry, connected, nil, Options{
		InitialDelay: time.Millisecond,
		TickInterval: time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sample: func(context.Context, *nodes.Node) (bool, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			started <- struct{}{}
			<-release
			completed.Add(1)
			return true, nil
		},
	})
	sampler.Start(context.Background())
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("two workers did not begin")
		}
	}
	select {
	case <-started:
		t.Fatal("more than two workers ran concurrently")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	deadline := time.NewTimer(time.Second)
	poll := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer poll.Stop()
	for completed.Load() != int32(len(registry.nodes)) {
		select {
		case <-poll.C:
		case <-deadline.C:
			t.Fatalf("completed %d samples, want %d", completed.Load(), len(registry.nodes))
		}
	}
	sampler.Close()
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
}

func TestCloseCancelsAndWaitsForReader(t *testing.T) {
	registry := &fakeRegistry{nodes: []nodes.Node{eligibleNode("node-1")}}
	started := make(chan struct{})
	stopped := make(chan struct{})
	sampler := New(nil, registry, fakeConnectivity{"node-1": true}, nil, Options{
		InitialDelay: time.Millisecond,
		TickInterval: time.Hour,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Sample: func(ctx context.Context, _ *nodes.Node) (bool, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return false, nil
		},
	})
	sampler.Start(context.Background())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sample did not start")
	}
	closed := make(chan struct{})
	go func() {
		sampler.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not wait for the canceled reader")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("Close returned before the reader observed cancellation")
	}
	sampler.Close()
}
