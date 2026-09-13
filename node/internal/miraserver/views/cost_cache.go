package views

// A cost request walks every descendant, including archived children. Keep a
// bounded working set large enough for the Web list and its subagent trees;
// the old 100-entry random eviction rescanned even a single 180-thread tree.
const costCacheLimit = 1000

func (service *Service) cachedCostProjection(key string) (cachedCost, bool) {
	service.costMu.Lock()
	defer service.costMu.Unlock()
	entry, ok := service.costCache[key]
	if !ok {
		return cachedCost{}, false
	}
	service.costLRU.MoveToFront(entry)
	return entry.Value.(cachedCost), true
}

func (service *Service) rememberCostProjection(key string, itemCount int64, state *CostProjection) {
	service.costMu.Lock()
	defer service.costMu.Unlock()
	if entry, ok := service.costCache[key]; ok {
		// A slower request must not replace a projection of newer history.
		if entry.Value.(cachedCost).itemCount <= itemCount {
			entry.Value = cachedCost{key, itemCount, state.clone()}
		}
		service.costLRU.MoveToFront(entry)
	} else {
		service.costCache[key] = service.costLRU.PushFront(cachedCost{key, itemCount, state.clone()})
	}
	for service.costLRU.Len() > costCacheLimit {
		oldest := service.costLRU.Back()
		delete(service.costCache, oldest.Value.(cachedCost).key)
		service.costLRU.Remove(oldest)
	}
}
