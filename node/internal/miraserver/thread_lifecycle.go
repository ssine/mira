package miraserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func MarkThreadRead(ctx context.Context, pool *pgxpool.Pool, storeID, threadID string, body map[string]any) (operationResponse, error) {
	generation, genOK := integer(body["generation"])
	itemCount, countOK := integer(body["itemCount"])
	if !genOK || generation < 1 || !countOK || itemCount < 0 {
		return operationResponse{Status: 400, Body: map[string]any{"error": "无效的已读位置", "code": "invalid_request"}}, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return operationResponse{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockScope(ctx, tx, storeID, []string{threadID}); err != nil {
		return operationResponse{}, err
	}
	var activeGeneration, activeCount int64
	err = tx.QueryRow(ctx, `SELECT active_generation,item_count FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2`, storeID, threadID).Scan(&activeGeneration, &activeCount)
	if err == pgx.ErrNoRows {
		return operationResponse{Status: 404, Body: map[string]any{"error": "会话不存在或已删除", "code": "not_found"}}, nil
	}
	if err != nil {
		return operationResponse{}, err
	}
	if generation != activeGeneration || itemCount > activeCount {
		return operationResponse{Status: 409, Body: map[string]any{"error": "会话历史已变化，请重新读取", "code": "thread_changed"}}, nil
	}
	var savedGeneration, savedCount int64
	if err := tx.QueryRow(ctx, `INSERT INTO mira_thread_read_positions(store_id,thread_id,generation,item_count)
      VALUES($1,$2,$3,$4) ON CONFLICT(store_id,thread_id) DO UPDATE
      SET generation=EXCLUDED.generation,item_count=CASE WHEN mira_thread_read_positions.generation=EXCLUDED.generation
        THEN GREATEST(mira_thread_read_positions.item_count,EXCLUDED.item_count) ELSE EXCLUDED.item_count END,updated_at=NOW()
      RETURNING generation,item_count`, storeID, threadID, generation, itemCount).Scan(&savedGeneration, &savedCount); err != nil {
		return operationResponse{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return operationResponse{}, err
	}
	return operationResponse{Status: 200, Body: map[string]any{"threadId": threadID, "generation": savedGeneration, "readItemCount": savedCount}}, nil
}

func ThreadErasureStatus(ctx context.Context, pool *pgxpool.Pool, storeID string) (map[string]any, error) {
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM mira_thread_erasures WHERE store_id=$1 AND phase<>'complete'`, storeID).Scan(&pending); err != nil {
		return nil, err
	}
	return map[string]any{"pending": pending}, nil
}

type ErasureBatchOptions struct {
	StoreID                       *string
	EventBatchSize, ItemBatchSize int
}

func ProcessThreadErasureBatch(ctx context.Context, pool *pgxpool.Pool, options ErasureBatchOptions) (result map[string]any, returnedErr error) {
	if options.EventBatchSize == 0 {
		options.EventBatchSize = 16
	}
	if options.ItemBatchSize == 0 {
		options.ItemBatchSize = 256
	}
	if options.EventBatchSize < 1 || options.EventBatchSize > 64 || options.ItemBatchSize < 1 || options.ItemBatchSize > 1024 {
		return nil, fmt.Errorf("invalid erasure batch size")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var storeID, threadID, phase string
	var cursor int64
	selected := false
	defer func() {
		if returnedErr == nil || !selected {
			return
		}
		code := "erasure_failed"
		var databaseError *pgconn.PgError
		if errors.As(returnedErr, &databaseError) && databaseError.Code != "" {
			code = databaseError.Code
		}
		if len(code) > 80 {
			code = code[:80]
		}
		retryContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = pool.Exec(retryContext, `UPDATE mira_thread_erasures
          SET retry_at=NOW()+INTERVAL '5 seconds',last_error_code=$3 WHERE store_id=$1 AND thread_id=$2`, storeID, threadID, code)
	}()
	err = tx.QueryRow(ctx, `SELECT store_id,thread_id,phase,after_event_seq FROM mira_thread_erasures
      WHERE phase<>'complete' AND retry_at<=NOW() AND ($1::text IS NULL OR store_id=$1)
      ORDER BY updated_at,store_id,thread_id LIMIT 1 FOR UPDATE SKIP LOCKED`, options.StoreID).Scan(&storeID, &threadID, &phase, &cursor)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	selected = true
	changed := int64(0)
	if phase == "events" {
		command, err := tx.Exec(ctx, `DELETE FROM codex_store_state_changes WHERE ctid IN (
        SELECT ctid FROM codex_store_state_changes WHERE store_id=$1 AND thread_id=$2 LIMIT $3)`, storeID, threadID, options.EventBatchSize)
		if err != nil {
			return nil, err
		}
		changed = command.RowsAffected()
		cursor += changed
		if changed == 0 {
			phase = "history"
		}
	} else if phase == "history" {
		command, err := tx.Exec(ctx, `DELETE FROM codex_thread_events WHERE ctid IN (
        SELECT ctid FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2 LIMIT $3)`, storeID, threadID, options.ItemBatchSize)
		if err != nil {
			return nil, err
		}
		changed = command.RowsAffected()
		if changed == 0 {
			phase = "provenance"
		}
	} else if phase == "provenance" {
		if _, err := tx.Exec(ctx, `DELETE FROM mira_codex_session_import_segments segments USING mira_codex_session_imports imports
        WHERE segments.import_id=imports.import_id AND imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)`, storeID, threadID); err != nil {
			return nil, err
		}
		command, err := tx.Exec(ctx, `DELETE FROM mira_codex_session_import_records WHERE ctid IN (
        SELECT records.ctid FROM mira_codex_session_import_records records JOIN mira_codex_session_imports imports ON records.import_id=imports.import_id
        WHERE imports.store_id=$1 AND imports.thread_id=$2
        AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
        AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id) LIMIT $3)`, storeID, threadID, options.ItemBatchSize)
		if err != nil {
			return nil, err
		}
		changed = command.RowsAffected()
		if changed == 0 {
			if _, err := tx.Exec(ctx, `DELETE FROM mira_codex_session_imports imports WHERE store_id=$1 AND thread_id=$2
          AND NOT EXISTS(SELECT 1 FROM codex_thread_projections WHERE store_id<>$1 AND thread_id=imports.thread_id)
          AND NOT EXISTS(SELECT 1 FROM mira_codex_session_import_segments WHERE source_import_id=imports.import_id)`, storeID, threadID); err != nil {
				return nil, err
			}
			phase = "complete"
		}
	} else {
		return nil, fmt.Errorf("unknown erasure phase %q", phase)
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_thread_erasures SET phase=$3,after_event_seq=$4,updated_at=NOW(),last_error_code=NULL,
      completed_at=CASE WHEN $3='complete' THEN NOW() ELSE NULL END WHERE store_id=$1 AND thread_id=$2`, storeID, threadID, phase, cursor); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"phase": phase, "changed": changed, "afterEventSeq": cursor}, nil
}

func StartThreadErasureWorker(ctx context.Context, pool *pgxpool.Pool, logger interface{ Printf(string, ...any) }) func() {
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := time.Duration(0)
		for {
			timer := time.NewTimer(delay)
			select {
			case <-workerContext.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			result, err := ProcessThreadErasureBatch(workerContext, pool, ErasureBatchOptions{})
			if err != nil && workerContext.Err() == nil {
				logger.Printf("thread erasure batch failed: %v", err)
			}
			if result != nil {
				delay = 200 * time.Millisecond
			} else {
				delay = 3 * time.Second
			}
		}
	}()
	return func() { cancel(); <-done }
}
