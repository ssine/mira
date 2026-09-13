package miraserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountCostCacheSharesWorkAfterClientCancellation(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	cache := newAccountCostCache(context.Background(), func(ctx context.Context, name, span, zone string) (map[string]any, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		return map[string]any{"estimate": map[string]any{"amount": 13.0}}, nil
	})
	defer cache.Close()
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := cache.Get(ctx, "API", "7d", "UTC"); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled client: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := cache.Get(context.Background(), " API ", "", "")
			if err != nil || result["estimate"].(map[string]any)["amount"] != 13.0 {
				t.Errorf("shared result: %v, %v", result, err)
			}
		}()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d scans for identical requests", calls.Load())
	}
	result, err := cache.Get(context.Background(), "API", "7d", "UTC")
	if err != nil || result["cache"].(map[string]any)["stale"] != false {
		t.Fatalf("fresh cache: %v, %v", result, err)
	}
}

func TestAccountCostCacheStaleRefreshAndFailureBackoff(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC).UnixNano())
	var calls atomic.Int32
	var fail atomic.Bool
	gate := make(chan struct{})
	cache := newAccountCostCache(context.Background(), func(ctx context.Context, _, _, _ string) (map[string]any, error) {
		call := calls.Add(1)
		if call == 2 {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if fail.Load() {
			return nil, errors.New("database unavailable")
		}
		return map[string]any{"estimate": map[string]any{"amount": float64(call)}}, nil
	})
	cache.now = func() time.Time { return time.Unix(0, now.Load()) }
	defer cache.Close()
	get := func() map[string]any {
		t.Helper()
		value, err := cache.Get(context.Background(), "API", "7d", "UTC")
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	get()
	now.Add(int64(6 * time.Minute))
	fail.Store(true)
	result := get()
	state := result["cache"].(map[string]any)
	if state["stale"] != true || state["refreshing"] != true || result["estimate"].(map[string]any)["amount"] != 1.0 {
		t.Fatalf("stale result must return before refresh completes: %v", result)
	}
	close(gate)
	cache.wg.Wait()
	result = get()
	if calls.Load() != 2 || result["estimate"].(map[string]any)["amount"] != 1.0 || result["cache"].(map[string]any)["refreshing"] != false {
		t.Fatalf("failure must preserve old amount and back off: %v", result)
	}
	now.Add(int64(31 * time.Second))
	fail.Store(false)
	get()
	cache.wg.Wait()
	result = get()
	if calls.Load() != 3 || result["cache"].(map[string]any)["stale"] != false || result["estimate"].(map[string]any)["amount"] != 3.0 {
		t.Fatalf("refresh did not replace stale result: %v", result)
	}
}

func TestAccountCostCacheKeysBoundsAndShutdown(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Date(2026, 9, 13, 23, 59, 0, 0, time.UTC).UnixNano())
	var calls atomic.Int32
	cache := newAccountCostCache(context.Background(), func(context.Context, string, string, string) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"estimate": map[string]any{"amount": 0.0}}, nil
	})
	cache.now = func() time.Time { return time.Unix(0, now.Load()) }
	defer cache.Close()
	get := func(name, span, zone string) {
		t.Helper()
		if _, err := cache.Get(context.Background(), name, span, zone); err != nil {
			t.Fatal(err)
		}
	}
	get("API", "7d", "UTC")
	get("API", "24h", "UTC")
	get("API", "7d", "Asia/Shanghai")
	get("Other", "7d", "UTC")
	now.Add(int64(2 * time.Minute))
	get("API", "7d", "UTC")
	if calls.Load() != 5 {
		t.Fatalf("account/range/timezone/local date mixed: %d", calls.Load())
	}
	if _, err := cache.Get(context.Background(), "API", "invalid", "UTC"); err == nil {
		t.Fatal("invalid range cached")
	}
	for i := 0; i < accountCostCacheLimit+5; i++ {
		get(fmt.Sprint(i), "7d", "UTC")
	}
	if len(cache.entries) > accountCostCacheLimit {
		t.Fatal("unbounded cache")
	}
	cache.Close()
	if _, err := cache.Get(context.Background(), "API", "7d", "UTC"); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed cache accepted work: %v", err)
	}
}

func TestAccountCostCacheBoundsConcurrentScansAndCancelsShutdown(t *testing.T) {
	var active, maximum atomic.Int32
	started := make(chan struct{}, 8)
	cache := newAccountCostCache(context.Background(), func(ctx context.Context, _, _, _ string) (map[string]any, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old; old = maximum.Load() {
			if maximum.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	defer cache.Close()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := cache.Get(context.Background(), fmt.Sprint(i), "7d", "UTC")
			if !errors.Is(err, context.Canceled) {
				t.Errorf("shutdown: %v", err)
			}
		}(i)
	}
	<-started
	<-started
	cache.Close()
	wg.Wait()
	if maximum.Load() > 2 || active.Load() != 0 {
		t.Fatalf("scan bound/shutdown failed: max=%d active=%d", maximum.Load(), active.Load())
	}
}
