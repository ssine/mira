package views

import (
	"context"
	"fmt"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// ListThreads returns Web thread summaries enriched from canonical lifecycle,
// read-position, token-usage, model, and reasoning-effort records.
func (service *Service) ListThreads(ctx context.Context, storeID string, limit int, threadID *string, archived *bool) ([]Thread, error) {
	if !safeStorePattern.MatchString(storeID) {
		return nil, &foundation.HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id"}
	}
	// Expand the projection once: each separate state operator would detoast
	// the same potentially large JSONB (including runtime instructions) again.
	// Projection state is always an object; its optional fields remain nullable.
	rows, err := service.pool.Query(ctx, `SELECT projections.thread_id, projections.parent_thread_id, projections.source_kind,
            COALESCE(NULLIF(summary.name, ''), projections.title) AS title,
            summary.name AS name, COALESCE(actions.action='archive',false) AS archived,
            projections.cwd, projections.item_count::text,
            summary.metadata->'token_usage' AS token_usage,
            summary.metadata->'model' AS model,
            summary."createdThread"->>'forked_from_id' AS forked_from_id,
            COALESCE(summary."createdThread" #>> '{metadata,timestamp}',
                     summary.metadata->>'created_at') AS created_at,
            projections.active_generation::text, activity.updated_at,
            imports.import_id::text, imports.source_node_id::text, imports.source_codex_version, imports.source_item_count::text,
            imports.created_at AS imported_at, runtimes.node_id::text AS runtime_node_id,
            runtimes.bound_at AS runtime_bound_at, runtimes.node_account_id::text
     FROM codex_thread_projections projections
     CROSS JOIN LATERAL jsonb_to_record(projections.state)
       AS summary(name text, metadata jsonb, "createdThread" jsonb)
     LEFT JOIN LATERAL (
       SELECT action FROM mira_thread_actions WHERE store_id=projections.store_id AND thread_id=projections.thread_id
       ORDER BY action_seq DESC LIMIT 1
     ) actions ON TRUE
     LEFT JOIN LATERAL (
       SELECT value::timestamptz AS updated_at
       FROM (VALUES
         (1, summary.metadata->>'updated_at'),
         (2, summary.metadata->>'advance_recency_at'),
         (3, summary.metadata->>'created_at'),
         (4, summary."createdThread" #>> '{metadata,timestamp}')
       ) timestamps(priority, value)
       WHERE value ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T'
         AND pg_input_is_valid(value, 'timestamp with time zone')
       ORDER BY priority LIMIT 1
     ) activity ON TRUE
     LEFT JOIN LATERAL (
       SELECT import_id, source_node_id, source_codex_version, source_item_count, created_at
       FROM mira_codex_session_imports
       WHERE store_id = projections.store_id AND thread_id = projections.thread_id AND status = 'imported'
       ORDER BY created_at DESC LIMIT 1
     ) imports ON TRUE
     LEFT JOIN mira_codex_thread_runtimes runtimes
       ON runtimes.store_id = projections.store_id AND runtimes.thread_id = projections.thread_id
     WHERE projections.store_id = $1 AND ($3::text IS NULL OR projections.thread_id = $3)
       AND ($4::boolean IS NULL OR COALESCE(actions.action='archive',false)=$4)
     ORDER BY activity.updated_at DESC NULLS LAST, projections.thread_id DESC LIMIT $2`, storeID, limit, threadID, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	threads := []Thread{}
	for rows.Next() {
		var thread Thread
		var itemCount, generation string
		var usageRaw, modelRaw []byte
		var updatedAt, importedAt, runtimeBoundAt *time.Time
		var importedCount *string
		if err := rows.Scan(
			&thread.ThreadID, &thread.ParentThreadID, &thread.SourceKind, &thread.Title, &thread.Name,
			&thread.Archived, &thread.Cwd, &itemCount, &usageRaw, &modelRaw, &thread.ForkedFromID,
			&thread.CreatedAt, &generation, &updatedAt, &thread.ImportID, &thread.SourceNodeID,
			&thread.SourceCodexVersion, &importedCount, &importedAt, &thread.RuntimeNodeID, &runtimeBoundAt, &thread.NodeAccountID,
		); err != nil {
			return nil, err
		}
		thread.ItemCount, err = postgresTextInt(itemCount)
		if err != nil {
			return nil, err
		}
		thread.Generation, err = postgresTextInt(generation)
		if err != nil {
			return nil, err
		}
		thread.TokenUsage, err = decodeObject(usageRaw)
		if err != nil {
			return nil, err
		}
		if len(modelRaw) > 0 {
			var model any
			if err := decodeJSON(modelRaw, &model); err != nil {
				return nil, err
			}
			if value, ok := model.(string); ok {
				thread.Model = &value
			}
		}
		thread.UpdatedAt, thread.ImportedAt, thread.RuntimeBoundAt = optionalTime(updatedAt), optionalTime(importedAt), optionalTime(runtimeBoundAt)
		if importedCount != nil {
			value, parseErr := postgresTextInt(*importedCount)
			if parseErr != nil {
				return nil, parseErr
			}
			thread.ImportedItemCount = &value
		}
		threads = append(threads, thread)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	threads, err = service.addHistorySummaries(ctx, storeID, threads)
	if err != nil {
		return nil, fmt.Errorf("add thread history summaries: %w", err)
	}
	threads, err = service.addActivityReachability(ctx, threads)
	if err != nil {
		return nil, fmt.Errorf("add thread activity reachability: %w", err)
	}
	threads, err = service.addReadStates(ctx, storeID, threads)
	if err != nil {
		return nil, fmt.Errorf("add thread read state: %w", err)
	}
	return threads, nil
}
