package views

import (
	"context"
	"encoding/json"
)

// The background account projector already parses the same canonical usage.
// Restore its bounded parser state in batches instead of replaying every
// descendant's raw history after a Server restart or cache eviction. Any tail
// beyond its cursor is still read synchronously, so totals keep their existing
// freshness and completeness semantics.
func (service *Service) loadStatisticsCheckpoints(ctx context.Context, storeID string, threads []Thread) error {
	wanted := map[string]Thread{}
	ids := []string{}
	for _, thread := range threads {
		if thread.ItemCount == 0 {
			continue
		}
		cached, ok := service.cachedCostProjection(costProjectionKey(storeID, thread, true))
		if ok && cached.itemCount == thread.ItemCount {
			continue
		}
		wanted[thread.ThreadID] = thread
		ids = append(ids, thread.ThreadID)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := service.pool.Query(ctx, `WITH sources AS (`+accountCostSourcesSQL+`)
 SELECT s.thread_id,c.generation,s.fork,c.item_seq,c.checkpoint FROM sources s
 JOIN mira_account_cost_checkpoints c USING(store_id,thread_id)
 WHERE s.store_id=$1 AND s.thread_id=ANY($2::text[]) AND c.generation=s.active_generation
 AND c.revision=$3 AND c.source_key=s.source_key AND c.item_seq>0`, storeID, ids, accountCostRevision)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, fork string
		var generation, cursor int64
		var raw []byte
		if err := rows.Scan(&id, &generation, &fork, &cursor, &raw); err != nil {
			return err
		}
		thread := wanted[id]
		expectedFork := ""
		if thread.ForkedFromID != nil {
			expectedFork = *thread.ForkedFromID
		}
		// A concurrent append/replacement must not leak a newer snapshot into
		// this request. Mismatches fall back to the canonical scanner.
		if generation != thread.Generation || cursor > thread.ItemCount || fork != expectedFork {
			continue
		}
		var saved accountCostCheckpoint
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		state, err := saved.restore()
		if err != nil {
			return err
		}
		// Account checkpoints omit turn buckets. Keep them separate from the
		// per-turn cache, which must still price all requested historical turns.
		service.rememberCostProjection(costProjectionKey(storeID, thread, true), cursor, state)
	}
	return rows.Err()
}
