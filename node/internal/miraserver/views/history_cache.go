package views

import (
	"context"
	"maps"
)

const historyCacheLimit = 1000

// Only immutable rollout-derived scalars are cached. Node reachability, account
// bindings, metadata and shared read positions are resolved for every request.
type historyKey struct {
	storeID, threadID     string
	generation, itemCount int64
}

type historySummary struct {
	key              historyKey
	activity, usage  map[string]any
	model, effort    *string
	latestVisibleSeq int64
}

func (service *Service) cachedHistory(key historyKey) (historySummary, bool) {
	service.historyMu.Lock()
	defer service.historyMu.Unlock()
	entry, ok := service.historyCache[key]
	if !ok {
		return historySummary{}, false
	}
	service.historyLRU.MoveToFront(entry)
	return entry.Value.(historySummary), true
}

func (service *Service) rememberHistory(summary historySummary) {
	service.historyMu.Lock()
	defer service.historyMu.Unlock()
	if entry, ok := service.historyCache[summary.key]; ok {
		service.historyLRU.MoveToFront(entry)
		return
	}
	service.historyCache[summary.key] = service.historyLRU.PushFront(summary)
	for service.historyLRU.Len() > historyCacheLimit {
		oldest := service.historyLRU.Back()
		delete(service.historyCache, oldest.Value.(historySummary).key)
		service.historyLRU.Remove(oldest)
	}
}

func (service *Service) addHistorySummaries(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	keyFor := func(thread Thread) historyKey {
		return historyKey{storeID, thread.ThreadID, thread.Generation, thread.ItemCount}
	}
	summaries := make(map[historyKey]historySummary, len(threads))
	pending := []Thread{}
	for _, thread := range threads {
		key := keyFor(thread)
		if summary, ok := service.cachedHistory(key); ok {
			summaries[key] = summary
		} else {
			// Metadata can change without an append. Cache only the canonical
			// fallback and apply current metadata after loading it.
			thread.TokenUsage, thread.Model, thread.ReasoningEffort = nil, nil, nil
			pending = append(pending, thread)
		}
	}
	if len(pending) > 0 {
		var err error
		for _, enrich := range []func(context.Context, string, []Thread) ([]Thread, error){
			service.addActivityHistory, service.addReadHistory, service.addTokenUsage, service.addModelSettings,
		} {
			pending, err = enrich(ctx, storeID, pending)
			if err != nil {
				return nil, err
			}
		}
		for _, thread := range pending {
			latest, _ := safeInteger(thread.ReadState["latestItemSeq"])
			summary := historySummary{keyFor(thread), thread.Activity, thread.TokenUsage, thread.Model, thread.ReasoningEffort, latest}
			summaries[summary.key] = summary
			service.rememberHistory(summary)
		}
	}
	for index := range threads {
		thread := &threads[index]
		summary := summaries[keyFor(*thread)]
		thread.Activity = maps.Clone(summary.activity)
		thread.ReadState = map[string]any{"latestItemSeq": summary.latestVisibleSeq}
		thread.TokenUsage = NormalizeTokenUsage(thread.TokenUsage)
		if thread.TokenUsage == nil {
			thread.TokenUsage = maps.Clone(summary.usage)
		}
		if validModel(thread.Model) == nil {
			thread.Model = nil
			if summary.model != nil {
				model := *summary.model
				thread.Model = &model
			}
		}
		if summary.effort != nil {
			effort := *summary.effort
			thread.ReasoningEffort = &effort
		}
	}
	return threads, nil
}
