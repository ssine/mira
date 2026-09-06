package imports

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// NormalizeImportedThreadHistoryModes upgrades previously imported paginated
// rollout state and session metadata to the legacy history API exposed by Mira.
func (service *Service) NormalizeImportedThreadHistoryModes(ctx context.Context) (int, error) {
	if service.Repository == nil || service.ThreadStore == nil {
		return 0, fmt.Errorf("session import dependencies are incomplete")
	}
	keys, err := service.Repository.ImportedThreadKeys(ctx)
	if err != nil {
		return 0, err
	}
	byStore := map[string][]string{}
	for _, key := range keys {
		byStore[key.StoreID] = append(byStore[key.StoreID], key.ThreadID)
	}
	stores := make([]string, 0, len(byStore))
	for storeID := range byStore {
		stores = append(stores, storeID)
	}
	sort.Strings(stores)
	normalized := 0
	for _, storeID := range stores {
		head, err := service.ThreadStore.Head(ctx, storeID)
		if err != nil {
			return normalized, err
		}
		stateChanges := []map[string]any{}
		historyChanges := []map[string]any{}
		touched := map[string]bool{}
		for _, threadID := range byStore[storeID] {
			mode := createdThreadHistoryMode(head.State, threadID)
			if mode == "paginated" {
				stateChanges = append(stateChanges, map[string]any{
					"path": []any{"created_threads", threadID, "history_mode"}, "mode": "set",
					"conflictPolicy": "compareAndSwap", "expected": map[string]any{"exists": true, "value": mode},
					"value": "legacy",
				})
				touched[threadID] = true
			}
			manifest, found := head.HistoryManifest[threadID]
			if !found {
				continue
			}
			first, found, err := service.Repository.FirstHistoryItem(ctx, storeID, threadID, manifest.Generation, head.Version)
			if err != nil {
				return normalized, err
			}
			if found && rolloutHistoryMode(first) == "legacy" {
				continue
			}
			history, err := service.ThreadStore.History(ctx, storeID, threadID, manifest.Generation, head.Version)
			if err != nil {
				return normalized, fmt.Errorf("failed to load imported thread %s: %w", threadID, err)
			}
			compatible := make([]json.RawMessage, len(history))
			changed := false
			for index, item := range history {
				compatible[index], err = legacyCompatibleItem(item)
				if err != nil {
					return normalized, err
				}
				equal, compareErr := sameRawJSON(item, compatible[index])
				if compareErr != nil {
					return normalized, compareErr
				}
				changed = changed || !equal
			}
			if changed {
				historyChanges = append(historyChanges, map[string]any{
					"threadId": threadID, "mode": "replace", "expectedGeneration": manifest.Generation,
					"expectedItemCount": manifest.ItemCount, "items": compatible,
				})
				touched[threadID] = true
			}
		}
		if len(stateChanges) == 0 && len(historyChanges) == 0 {
			continue
		}
		operationID, err := randomUUID()
		if err != nil {
			return normalized, err
		}
		headers := make(http.Header)
		headers.Set("x-codex-operation-id", operationID)
		headers.Set("x-codex-version", "mira-import-compatibility")
		if _, err := service.ThreadStore.CommitDelta(ctx, storeID, Delta{
			ExpectedVersion: head.Version, StateChanges: stateChanges, HistoryChanges: historyChanges,
		}, headers); err != nil {
			return normalized, fmt.Errorf("failed to normalize imported thread history modes in %s: %w", storeID, err)
		}
		normalized += len(touched)
	}
	return normalized, nil
}

func createdThreadHistoryMode(state map[string]any, threadID string) string {
	return stringValue(object(object(state["created_threads"])[threadID])["history_mode"])
}

func rolloutHistoryMode(raw json.RawMessage) string {
	fields, err := decodeObject(raw)
	if err != nil || stringValue(fields["type"]) != "session_meta" {
		return ""
	}
	return stringValue(object(fields["payload"])["history_mode"])
}

func legacyCompatibleItem(raw json.RawMessage) (json.RawMessage, error) {
	fields, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	if stringValue(fields["type"]) != "session_meta" {
		return append(json.RawMessage(nil), raw...), nil
	}
	return CanonicalRolloutItem(raw, false, "")
}

func sameRawJSON(left, right json.RawMessage) (bool, error) {
	a, err := decodeValue(left)
	if err != nil {
		return false, err
	}
	b, err := decodeValue(right)
	if err != nil {
		return false, err
	}
	return jsonEqual(a, b), nil
}

func randomUUID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("read secure randomness: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
