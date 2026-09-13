package miraserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const accountCostCacheLimit = 64

type accountCostKey struct{ name, rangeName, zone, day string }
type accountCostJob struct {
	done chan struct{}
	err  error
}
type accountCostEntry struct {
	data                                      map[string]any
	updatedAt, expiresAt, retryAt, lastAccess time.Time
	job                                       *accountCostJob
	err                                       error
}

// A disposable projection shared by every administrator client. PostgreSQL
// remains authoritative; no browser connection owns the shared calculation.
type accountCostCache struct {
	mu      sync.Mutex
	entries map[accountCostKey]*accountCostEntry
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	closed  bool
	slots   chan struct{}
	now     func() time.Time
	load    func(context.Context, string, string, string) (map[string]any, error)
}

func newAccountCostCache(ctx context.Context, load func(context.Context, string, string, string) (map[string]any, error)) *accountCostCache {
	ctx, cancel := context.WithCancel(ctx)
	return &accountCostCache{entries: make(map[accountCostKey]*accountCostEntry), ctx: ctx, cancel: cancel,
		slots: make(chan struct{}, 2), now: time.Now, load: load}
}

func (cache *accountCostCache) Close() {
	cache.mu.Lock()
	cache.closed = true
	cache.cancel()
	cache.mu.Unlock()
	cache.wg.Wait()
}

func (cache *accountCostCache) Get(ctx context.Context, name, rangeName, zone string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if rangeName == "" {
		rangeName = "7d"
	}
	if zone == "" {
		zone = "UTC"
	}
	location, err := time.LoadLocation(zone)
	if name == "" || len(name) > 128 || (rangeName != "24h" && rangeName != "7d" && rangeName != "30d") || err != nil {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "name, range (24h, 7d, 30d) and a valid timezone are required"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := cache.now()
	// Never reuse yesterday's calendar-day window across local midnight.
	key := accountCostKey{name, rangeName, zone, now.In(location).Format("2006-01-02")}
	cache.mu.Lock()
	if cache.closed {
		cache.mu.Unlock()
		return nil, context.Canceled
	}
	entry := cache.entries[key]
	if entry == nil {
		if len(cache.entries) >= accountCostCacheLimit {
			var oldest accountCostKey
			var found bool
			for candidate, value := range cache.entries {
				if value.job == nil && (!found || value.lastAccess.Before(cache.entries[oldest].lastAccess)) {
					oldest, found = candidate, true
				}
			}
			if !found {
				cache.mu.Unlock()
				return nil, &foundation.HTTPError{Status: 503, Code: "cost_busy", Message: "cost statistics are busy; retry shortly"}
			}
			delete(cache.entries, oldest)
		}
		entry = &accountCostEntry{}
		cache.entries[key] = entry
	}
	entry.lastAccess = now
	if !now.Before(entry.expiresAt) && !now.Before(entry.retryAt) && entry.job == nil {
		entry.job = &accountCostJob{done: make(chan struct{})}
		cache.wg.Add(1)
		go cache.refresh(key, entry, entry.job)
	}
	if entry.data != nil {
		result := accountCostResult(entry, now)
		cache.mu.Unlock()
		return result, nil
	}
	job := entry.job
	err = entry.err
	cache.mu.Unlock()
	if job == nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-job.done:
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if job.err != nil {
			return nil, job.err
		}
		return accountCostResult(entry, cache.now()), nil
	}
}

func accountCostResult(entry *accountCostEntry, now time.Time) map[string]any {
	result := make(map[string]any, len(entry.data)+1)
	for key, value := range entry.data {
		result[key] = value
	}
	result["cache"] = map[string]any{"updatedAt": entry.updatedAt.UTC().Format(time.RFC3339Nano),
		"expiresAt": entry.expiresAt.UTC().Format(time.RFC3339Nano), "stale": !now.Before(entry.expiresAt), "refreshing": entry.job != nil}
	return result
}

func (cache *accountCostCache) refresh(key accountCostKey, entry *accountCostEntry, job *accountCostJob) {
	defer cache.wg.Done()
	ctx, cancel := context.WithTimeout(cache.ctx, 60*time.Second)
	defer cancel()
	var data map[string]any
	var err error
	defer func() {
		if recover() != nil {
			err = errors.New("cost statistics calculation failed")
		}
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if err == nil {
			entry.data, entry.updatedAt = data, cache.now()
			entry.expiresAt = entry.updatedAt.Add(5 * time.Minute)
			entry.retryAt = time.Time{}
		} else {
			// Keep an earlier successful amount; never replace it with zero.
			entry.retryAt = cache.now().Add(30 * time.Second)
		}
		entry.err, job.err, entry.job = err, err, nil
		close(job.done)
	}()
	select {
	case <-ctx.Done():
		err = ctx.Err()
		return
	case cache.slots <- struct{}{}:
		defer func() { <-cache.slots }()
	}
	data, err = cache.load(ctx, key.name, key.rangeName, key.zone)
}
