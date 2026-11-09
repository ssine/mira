package views

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// LatestAccountSettings reads the complete account history independently of
// browser pagination. The settings themselves are rebuilt from canonical items.
func (service *Service) LatestAccountSettings(ctx context.Context, bindingID string) (map[string]any, error) {
	var thread Thread
	var storeID string
	err := service.pool.QueryRow(ctx, `SELECT p.store_id,p.thread_id,p.active_generation,p.item_count
	 FROM codex_thread_projections p JOIN mira_codex_thread_runtimes r USING(store_id,thread_id)
	 LEFT JOIN LATERAL (
	  SELECT value::timestamptz updated_at FROM unnest(p.directory_timestamps) WITH ORDINALITY stamps(value,priority)
	  WHERE value~'^[0-9]{4}-[0-9]{2}-[0-9]{2}T' AND pg_input_is_valid(value,'timestamp with time zone') ORDER BY priority LIMIT 1
	 ) activity ON TRUE
	 WHERE r.node_account_id=$1::uuid AND p.parent_thread_id IS NULL
	 ORDER BY activity.updated_at DESC NULLS LAST,p.thread_id DESC LIMIT 1`, bindingID).Scan(&storeID, &thread.ThreadID, &thread.Generation, &thread.ItemCount)
	if err == pgx.ErrNoRows {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	threads, err := service.addModelSettings(ctx, storeID, []Thread{thread})
	if err != nil {
		return nil, err
	}
	return map[string]any{"threadId": thread.ThreadID, "model": threads[0].Model, "effort": threads[0].ReasoningEffort, "serviceTier": threads[0].ServiceTier}, nil
}
