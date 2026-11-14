package coldstore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FreezerOptions controls background movement of idle history to the bucket.
type FreezerOptions struct {
	// FreezeAfter is the idle period of a conversation and the minimum age of
	// a row before its payload may move.
	FreezeAfter time.Duration
	// MinBytes is the minimum stored (compressed) payload size worth moving.
	MinBytes int
	// StartDelay postpones the first pass, so a Supervisor rollback after a
	// failed health check never meets placeholders written by this version.
	StartDelay time.Duration
	// OrphanGrace is how long an unreferenced object is kept before deletion.
	OrphanGrace time.Duration
	Logger      *log.Logger
}

func (options *FreezerOptions) normalize() {
	if options.FreezeAfter <= 0 {
		options.FreezeAfter = 30 * 24 * time.Hour
	}
	if options.MinBytes <= 0 {
		options.MinBytes = 16 << 10
	}
	if options.StartDelay < 0 {
		options.StartDelay = 0
	}
	if options.OrphanGrace <= 0 {
		options.OrphanGrace = 24 * time.Hour
	}
	if options.Logger == nil {
		options.Logger = log.Default()
	}
}

// Freezer moves eligible payloads and collects unreferenced objects.
type Freezer struct {
	pool    *pgxpool.Pool
	objects ObjectStore
	options FreezerOptions
}

func NewFreezer(pool *pgxpool.Pool, objects ObjectStore, options FreezerOptions) *Freezer {
	options.normalize()
	return &Freezer{pool: pool, objects: objects, options: options}
}

// FreezeStats summarizes one pass.
type FreezeStats struct {
	Threads, Rows int
	RawBytes      int64
	StoredBytes   int64
}

// Start runs the freezer and collector until the returned stop function is
// called or ctx ends. Errors are logged and retried on the next pass.
func (freezer *Freezer) Start(ctx context.Context) func() {
	runContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !sleep(runContext, freezer.options.StartDelay) {
			return
		}
		lastCollect := time.Time{}
		for {
			stats, err := freezer.FreezePass(runContext, 32)
			if err != nil && runContext.Err() == nil {
				freezer.options.Logger.Printf("cold history freeze pass failed: %v", err)
			} else if stats.Rows > 0 {
				freezer.options.Logger.Printf("cold history froze %d payloads in %d conversations (%d raw bytes, %d stored)",
					stats.Rows, stats.Threads, stats.RawBytes, stats.StoredBytes)
			}
			if time.Since(lastCollect) >= time.Hour {
				if deleted, err := freezer.Collect(runContext); err != nil && runContext.Err() == nil {
					freezer.options.Logger.Printf("cold history collection failed: %v", err)
				} else if deleted > 0 {
					freezer.options.Logger.Printf("cold history deleted %d unreferenced objects", deleted)
				}
				lastCollect = time.Now()
			}
			wait := 10 * time.Minute
			if stats.Threads > 0 && err == nil {
				wait = time.Second
			}
			if !sleep(runContext, wait) {
				return
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func sleep(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

type candidateThread struct {
	store, thread     string
	generation, count int64
}

// FreezePass processes up to limit idle conversations that changed since
// their last scan.
func (freezer *Freezer) FreezePass(ctx context.Context, limit int) (FreezeStats, error) {
	stats := FreezeStats{}
	cutoff := time.Now().Add(-freezer.options.FreezeAfter)
	rows, err := freezer.pool.Query(ctx, `SELECT p.store_id,p.thread_id,p.active_generation,p.item_count
 FROM codex_thread_projections p
 LEFT JOIN mira_cold_thread_scans s ON s.store_id=p.store_id AND s.thread_id=p.thread_id
 WHERE p.updated_at<$1 AND p.item_count>0
   AND (s.thread_id IS NULL OR s.generation<>p.active_generation OR s.item_count<>p.item_count)
   AND NOT EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=p.store_id AND a.thread_id=p.thread_id AND a.action='delete')
 ORDER BY p.updated_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return stats, err
	}
	threads := []candidateThread{}
	for rows.Next() {
		var item candidateThread
		if err := rows.Scan(&item.store, &item.thread, &item.generation, &item.count); err != nil {
			rows.Close()
			return stats, err
		}
		threads = append(threads, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, err
	}
	for _, thread := range threads {
		if err := freezer.freezeThread(ctx, thread, cutoff, &stats); err != nil {
			return stats, fmt.Errorf("conversation %s: %w", thread.thread, err)
		}
		stats.Threads++
		if _, err := freezer.pool.Exec(ctx, `INSERT INTO mira_cold_thread_scans(store_id,thread_id,generation,item_count,scanned_at)
 VALUES($1,$2,$3,$4,now()) ON CONFLICT(store_id,thread_id) DO UPDATE SET generation=EXCLUDED.generation,item_count=EXCLUDED.item_count,scanned_at=now()`,
			thread.store, thread.thread, thread.generation, thread.count); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

type candidateRow struct {
	seq     int64
	raw     []byte
	payload string
}

func (freezer *Freezer) freezeThread(ctx context.Context, thread candidateThread, cutoff time.Time, stats *FreezeStats) error {
	after := int64(0)
	for {
		rows, err := freezer.pool.Query(ctx, `SELECT item_seq,payload,payload_sha256 FROM codex_thread_events
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5
   AND cold_sha256 IS NULL AND created_at<$6 AND pg_column_size(payload)>=$7
 ORDER BY item_seq LIMIT 16`, thread.store, thread.thread, thread.generation, after, thread.count, cutoff, freezer.options.MinBytes)
		if err != nil {
			return err
		}
		batch := []candidateRow{}
		for rows.Next() {
			var row candidateRow
			if err := rows.Scan(&row.seq, &row.raw, &row.payload); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, row)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, row := range batch {
			after = row.seq
			stub, sha, ok := Freezable(row.raw)
			if !ok {
				continue
			}
			stored, ready, err := freezer.ensureObject(ctx, sha, row.raw)
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
			frozen, err := freezer.replace(ctx, thread, row, stub, sha)
			if err != nil {
				return err
			}
			if frozen {
				stats.Rows++
				stats.RawBytes += int64(len(row.raw))
				stats.StoredBytes += stored
			}
		}
	}
}

// ensureObject uploads one payload unless an uploaded object already holds
// the same bytes. It reports false while the object is being deleted.
func (freezer *Freezer) ensureObject(ctx context.Context, sha string, raw []byte) (int64, bool, error) {
	if _, err := freezer.pool.Exec(ctx, `INSERT INTO mira_cold_objects(sha256,raw_bytes,stored_bytes) VALUES($1,$2,0) ON CONFLICT(sha256) DO NOTHING`, sha, len(raw)); err != nil {
		return 0, false, err
	}
	var uploaded, deleting bool
	var stored int64
	if err := freezer.pool.QueryRow(ctx, `SELECT uploaded_at IS NOT NULL,deleting,stored_bytes FROM mira_cold_objects WHERE sha256=$1`, sha).Scan(&uploaded, &deleting, &stored); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if deleting {
		return 0, false, nil
	}
	if uploaded {
		return 0, true, nil
	}
	encoded, err := Encode(raw)
	if err != nil {
		return 0, false, err
	}
	// Verify the exact bytes that will be uploaded decode to the original
	// before any canonical row points at them. Content-MD5 then protects the
	// transfer; no download is needed.
	if _, err := Decode(encoded, sha); err != nil {
		return 0, false, err
	}
	if err := freezer.objects.Put(ctx, ObjectKey(sha), encoded); err != nil {
		return 0, false, err
	}
	command, err := freezer.pool.Exec(ctx, `UPDATE mira_cold_objects SET uploaded_at=now(),stored_bytes=$2 WHERE sha256=$1 AND NOT deleting`, sha, len(encoded))
	if err != nil {
		return 0, false, err
	}
	return int64(len(encoded)), command.RowsAffected() == 1, nil
}

// replace swaps one canonical body for its placeholder. The object row lock
// serializes with the collector; payload_sha256 guards against a row whose
// content is not the one that was uploaded.
func (freezer *Freezer) replace(ctx context.Context, thread candidateThread, row candidateRow, stub []byte, sha string) (bool, error) {
	tx, err := freezer.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var locked string
	err = tx.QueryRow(ctx, `SELECT sha256 FROM mira_cold_objects WHERE sha256=$1 AND uploaded_at IS NOT NULL AND NOT deleting FOR UPDATE`, sha).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_cold_objects SET orphaned_at=NULL WHERE sha256=$1`, sha); err != nil {
		return false, err
	}
	command, err := tx.Exec(ctx, `UPDATE codex_thread_events SET payload=$6::json,cold_sha256=$7
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq=$4 AND payload_sha256=$5 AND cold_sha256 IS NULL`,
		thread.store, thread.thread, thread.generation, row.seq, row.payload, stub, sha)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return command.RowsAffected() == 1, nil
}

// Collect deletes objects that no history row has referenced for the grace
// period, for example after a conversation was permanently deleted.
func (freezer *Freezer) Collect(ctx context.Context) (int, error) {
	if _, err := freezer.pool.Exec(ctx, `UPDATE mira_cold_objects o SET orphaned_at=NULL
 WHERE orphaned_at IS NOT NULL AND NOT deleting AND EXISTS(SELECT 1 FROM codex_thread_events e WHERE e.cold_sha256=o.sha256)`); err != nil {
		return 0, err
	}
	if _, err := freezer.pool.Exec(ctx, `UPDATE mira_cold_objects o SET orphaned_at=now()
 WHERE orphaned_at IS NULL AND NOT deleting AND created_at<now()-interval '1 hour'
   AND NOT EXISTS(SELECT 1 FROM codex_thread_events e WHERE e.cold_sha256=o.sha256)`); err != nil {
		return 0, err
	}
	if _, err := freezer.pool.Exec(ctx, `DELETE FROM mira_cold_thread_scans s WHERE NOT EXISTS(
 SELECT 1 FROM codex_thread_projections p WHERE p.store_id=s.store_id AND p.thread_id=s.thread_id)`); err != nil {
		return 0, err
	}
	grace := time.Now().Add(-freezer.options.OrphanGrace)
	deleted := 0
	for {
		rows, err := freezer.pool.Query(ctx, `UPDATE mira_cold_objects o SET deleting=true
 WHERE sha256 IN (SELECT sha256 FROM mira_cold_objects WHERE deleting OR orphaned_at<$1 LIMIT 64)
   AND (deleting OR (orphaned_at<$1 AND NOT EXISTS(SELECT 1 FROM codex_thread_events e WHERE e.cold_sha256=o.sha256)))
 RETURNING sha256`, grace)
		if err != nil {
			return deleted, err
		}
		shas := []string{}
		for rows.Next() {
			var sha string
			if err := rows.Scan(&sha); err != nil {
				rows.Close()
				return deleted, err
			}
			shas = append(shas, sha)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return deleted, err
		}
		if len(shas) == 0 {
			return deleted, nil
		}
		for _, sha := range shas {
			if err := freezer.objects.Delete(ctx, ObjectKey(sha)); err != nil {
				return deleted, err
			}
			if _, err := freezer.pool.Exec(ctx, `DELETE FROM mira_cold_objects WHERE sha256=$1 AND deleting`, sha); err != nil {
				return deleted, err
			}
			deleted++
		}
	}
}
