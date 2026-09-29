package channel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

type loadedProbe struct {
	done    chan struct{}
	expires time.Time
	checked time.Time
	loaded  bool
	err     error
}

// ThreadLoaded observes the native loaded-ID directory only. It never reads
// history, renews residency leases, resumes a thread or subscribes to it.
// Codex's loaded-list cursor is an exclusive, lexically ordered UUID cursor;
// asking immediately before the requested UUID needs only one bounded item.
func (reader *AccountReader) ThreadLoaded(ctx context.Context, nodeID, accountID, runtimeID, threadID string) (bool, time.Time, error) {
	key := nodeID + ":" + accountID + ":" + runtimeID + ":" + threadID
	reader.mu.Lock()
	if reader.loaded == nil {
		reader.loaded = map[string]*loadedProbe{}
	}
	now := time.Now()
	for id, p := range reader.loaded {
		if !p.expires.IsZero() && now.After(p.expires) {
			delete(reader.loaded, id)
		}
	}
	if pending := reader.loaded[key]; pending != nil {
		reader.mu.Unlock()
		select {
		case <-pending.done:
			return pending.loaded, pending.checked, pending.err
		case <-ctx.Done():
			return false, time.Time{}, ctx.Err()
		}
	}
	if reader.loadedRunning >= 8 || len(reader.loaded) >= 512 {
		reader.mu.Unlock()
		return false, time.Time{}, errors.New("loaded-state probes busy")
	}
	probe := &loadedProbe{done: make(chan struct{})}
	reader.loaded[key] = probe
	reader.loadedRunning++
	reader.mu.Unlock()
	loaded, err := reader.probeThreadLoaded(ctx, nodeID, accountID, runtimeID, threadID)
	reader.mu.Lock()
	probe.loaded, probe.err, probe.checked = loaded, err, time.Now()
	probe.expires = probe.checked.Add(5 * time.Second)
	reader.loadedRunning--
	close(probe.done)
	reader.mu.Unlock()
	return loaded, probe.checked, err
}

func (reader *AccountReader) probeThreadLoaded(ctx context.Context, nodeID, accountID, runtimeID, threadID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	raw, err := hex.DecodeString(strings.ReplaceAll(threadID, "-", ""))
	if err != nil || len(raw) != 16 {
		return false, errors.New("invalid thread UUID")
	}
	params := map[string]any{"limit": 1}
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] > 0 {
			raw[i]--
			params["cursor"] = fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
			break
		}
		raw[i] = 255
	}
	session, err := reader.open(ctx, nodeID, accountID, runtimeID)
	if err != nil {
		return false, err
	}
	defer reader.closeSession(session)
	value, err := reader.call(ctx, session, "thread/loaded/list", params)
	if err != nil {
		return false, err
	}
	result, ok := value.(map[string]any)
	if !ok {
		return false, errors.New("invalid loaded-state response")
	}
	data, ok := result["data"].([]any)
	if !ok || len(data) > 1 {
		return false, errors.New("invalid loaded-state page")
	}
	if len(data) == 0 {
		return false, nil
	}
	id, ok := data[0].(string)
	if !ok {
		return false, errors.New("invalid loaded-state ID")
	}
	return id == strings.ToLower(threadID), nil
}
