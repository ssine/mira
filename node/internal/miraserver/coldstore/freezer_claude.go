package coldstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type claudeSession struct {
	id                 string
	eventSeq, entrySeq int64
}

// FreezeClaudePass moves large "user" records (tool results) of idle Claude
// sessions. A session qualifies when it has no running turn, no turn activity
// within FreezeAfter, and a current conversation index for its root and every
// native subagent transcript, so indexing never downloads frozen bodies.
func (freezer *Freezer) FreezeClaudePass(ctx context.Context, limit int) (FreezeStats, error) {
	stats := FreezeStats{}
	cutoff := time.Now().Add(-freezer.options.FreezeAfter)
	rows, err := freezer.pool.Query(ctx, `WITH candidates AS (
 SELECT s.session_id,s.updated_at,
  coalesce((SELECT max(seq) FROM mira_claude_events e WHERE e.session_id=s.session_id),0) AS event_seq,
  coalesce((SELECT max(seq) FROM mira_claude_entries n WHERE n.session_id=s.session_id),0) AS entry_seq
 FROM mira_claude_sessions s
 WHERE s.updated_at<$1 AND s.active_turn IS NULL
  AND NOT EXISTS(SELECT 1 FROM mira_claude_turns t WHERE t.session_id=s.session_id
    AND (t.status IN ('starting','running') OR coalesce(t.completed_at,t.created_at)>$1))
  AND EXISTS(SELECT 1 FROM mira_claude_index_state i WHERE i.session_id=s.session_id AND i.subpath='' AND i.projector_version=$3
    AND NOT EXISTS(SELECT 1 FROM mira_claude_events e WHERE e.session_id=s.session_id AND e.event_type<>'stream_event' AND e.seq>i.indexed_seq))
  AND NOT EXISTS(SELECT 1 FROM mira_claude_transcripts t
    LEFT JOIN mira_claude_index_state i ON i.session_id=t.session_id AND i.subpath=t.subpath
    WHERE t.session_id=s.session_id AND t.subpath<>'' AND (i.session_id IS NULL OR i.projector_version<>$3 OR i.indexed_seq<t.next_seq-1))
)
SELECT c.session_id::text,c.event_seq,c.entry_seq FROM candidates c
 LEFT JOIN mira_cold_claude_scans x ON x.session_id=c.session_id
 WHERE x.session_id IS NULL OR x.events_seq<>c.event_seq OR x.entries_seq<>c.entry_seq
 ORDER BY c.updated_at LIMIT $2`, cutoff, limit, freezer.options.ClaudeIndexVersion)
	if err != nil {
		return stats, err
	}
	sessions := []claudeSession{}
	for rows.Next() {
		var item claudeSession
		if err := rows.Scan(&item.id, &item.eventSeq, &item.entrySeq); err != nil {
			rows.Close()
			return stats, err
		}
		sessions = append(sessions, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return stats, err
	}
	for _, session := range sessions {
		skippedBefore := stats.Skipped
		if err := freezer.freezeClaudeEvents(ctx, session, &stats); err != nil {
			return stats, fmt.Errorf("Claude session %s events: %w", session.id, err)
		}
		if err := freezer.freezeClaudeEntries(ctx, session, &stats); err != nil {
			return stats, fmt.Errorf("Claude session %s transcript: %w", session.id, err)
		}
		if stats.Skipped > skippedBefore {
			continue // retried by a later pass
		}
		stats.Threads++
		if _, err := freezer.pool.Exec(ctx, `INSERT INTO mira_cold_claude_scans(session_id,events_seq,entries_seq,scanned_at)
 VALUES($1::uuid,$2,$3,now()) ON CONFLICT(session_id) DO UPDATE SET events_seq=EXCLUDED.events_seq,entries_seq=EXCLUDED.entries_seq,scanned_at=now()`,
			session.id, session.eventSeq, session.entrySeq); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (freezer *Freezer) freezeClaudeEvents(ctx context.Context, session claudeSession, stats *FreezeStats) error {
	after := int64(0)
	for {
		rows, err := freezer.pool.Query(ctx, `SELECT seq,payload FROM mira_claude_events
 WHERE session_id=$1::uuid AND seq>$2 AND seq<=$3 AND event_type='user' AND cold_sha256 IS NULL AND pg_column_size(payload)>=$4
 ORDER BY seq LIMIT 16`, session.id, after, session.eventSeq, freezer.options.MinBytes)
		if err != nil {
			return err
		}
		type row struct {
			seq int64
			raw []byte
		}
		batch := []row{}
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.seq, &item.raw); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, item := range batch {
			after = item.seq
			if err := freezer.freezeClaudeRow(ctx, item.raw, stats, `UPDATE mira_claude_events SET payload=$3::json,cold_sha256=$4
 WHERE session_id=$1::uuid AND seq=$2 AND cold_sha256 IS NULL AND encode(sha256(convert_to(payload::text,'UTF8')),'hex')=$4`, session.id, item.seq); err != nil {
				return err
			}
		}
	}
}

func (freezer *Freezer) freezeClaudeEntries(ctx context.Context, session claudeSession, stats *FreezeStats) error {
	subpath, after := "", int64(0)
	for {
		rows, err := freezer.pool.Query(ctx, `SELECT subpath,seq,payload FROM mira_claude_entries
 WHERE session_id=$1::uuid AND (subpath,seq)>($2,$3) AND cold_sha256 IS NULL AND pg_column_size(payload)>=$4
 ORDER BY subpath,seq LIMIT 16`, session.id, subpath, after, freezer.options.MinBytes)
		if err != nil {
			return err
		}
		type row struct {
			subpath string
			seq     int64
			raw     []byte
		}
		batch := []row{}
		for rows.Next() {
			var item row
			if err := rows.Scan(&item.subpath, &item.seq, &item.raw); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		for _, item := range batch {
			subpath, after = item.subpath, item.seq
			if err := freezer.freezeClaudeRow(ctx, item.raw, stats, `UPDATE mira_claude_entries SET payload=$4::json,cold_sha256=$5
 WHERE session_id=$1::uuid AND subpath=$2 AND seq=$3 AND cold_sha256 IS NULL AND encode(sha256(convert_to(payload::text,'UTF8')),'hex')=$5`,
				session.id, item.subpath, item.seq); err != nil {
				return err
			}
		}
	}
}

// freezeClaudeRow uploads one record and swaps it for its placeholder. The
// update re-checks the stored text's SHA-256, so a row never points at bytes
// other than its own; key arguments precede the placeholder and address.
func (freezer *Freezer) freezeClaudeRow(ctx context.Context, raw []byte, stats *FreezeStats, update string, keys ...any) error {
	stub, sha, ok := ClaudeFreezable(raw)
	if !ok {
		return nil
	}
	stored, ready, err := freezer.ensureObject(ctx, sha, raw)
	var upload *UploadError
	if errors.As(err, &upload) && ctx.Err() == nil {
		stats.Skipped++
		return nil
	}
	if err != nil || !ready {
		return err
	}
	tx, err := freezer.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var locked string
	err = tx.QueryRow(ctx, `SELECT sha256 FROM mira_cold_objects WHERE sha256=$1 AND uploaded_at IS NOT NULL AND NOT deleting FOR UPDATE`, sha).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE mira_cold_objects SET orphaned_at=NULL WHERE sha256=$1`, sha); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, update, append(keys, string(stub), sha)...)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		stats.Rows++
		stats.RawBytes += int64(len(raw))
		stats.StoredBytes += stored
	}
	return nil
}
