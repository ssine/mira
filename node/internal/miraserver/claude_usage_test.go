package miraserver

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaudeUsageCacheSharesReadsAndKeepsLastValue(t *testing.T) {
	var cache claudeUsageCache
	var calls atomic.Int32
	release := make(chan struct{})
	load := func(context.Context) (any, error) {
		calls.Add(1)
		<-release
		return map[string]any{"available": true}, nil
	}
	var group sync.WaitGroup
	results := make([]claudeUsageEntry, 4)
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			entry, err := cache.read(context.Background(), "a", 1, claudeUsageTTL, load)
			if err != nil {
				t.Error(err)
			}
			results[index] = entry
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	group.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent readers started %d loads", calls.Load())
	}
	for _, entry := range results {
		if entry.usage == nil || entry.message != "" || entry.observedAt.IsZero() {
			t.Fatalf("shared result: %+v", entry)
		}
	}

	// Fresh entries are reused; a forced refresh inside the minimum interval is ignored.
	if _, err := cache.read(context.Background(), "a", 1, claudeUsageMinRefresh, load); err != nil || calls.Load() != 1 {
		t.Fatalf("fresh/forced reuse: calls=%d err=%v", calls.Load(), err)
	}

	// An expired entry whose refresh fails keeps the last value and reports why.
	cache.mu.Lock()
	cache.entries["a"].expiresAt = time.Now().Add(-time.Second)
	cache.mu.Unlock()
	failed, err := cache.read(context.Background(), "a", 1, claudeUsageTTL, func(context.Context) (any, error) {
		return nil, claudeError(409, "Claude account Node is offline")
	})
	if err != nil || failed.usage == nil || failed.status != 409 || failed.message != "Claude account Node is offline" {
		t.Fatalf("stale fallback: %+v %v", failed, err)
	}

	// A credential change discards the previous account's observation.
	changed, err := cache.read(context.Background(), "a", 2, claudeUsageTTL, func(context.Context) (any, error) {
		return nil, errors.New("boom")
	})
	if err != nil || changed.usage != nil || changed.status != 502 || changed.message != "boom" {
		t.Fatalf("revision change: %+v %v", changed, err)
	}
}
