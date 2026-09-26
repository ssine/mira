// Package executionstate reconciles execution routes with canonical lifecycle
// records. Callers hold the store/thread lock used by turn reservations.
package executionstate

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Keep this identical to schema 18's partial index predicate. Errors are
// candidates only; they do not establish completion without a terminal marker.
const lifecyclePredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(task_started|turn_started|task_complete|turn_complete|turn_aborted|error)"'`

type route struct {
	binding, runtime, state, turn string
	revision                      int64
}

func readRoute(ctx context.Context, tx pgx.Tx, store, thread string, generation int64) (route, error) {
	var r route
	err := tx.QueryRow(ctx, `SELECT node_account_id::text,runtime_id,revision,state,COALESCE(turn_id,'')
 FROM mira_codex_execution_routes WHERE store_id=$1 AND thread_id=$2 AND generation=$3`, store, thread, generation).
		Scan(&r.binding, &r.runtime, &r.revision, &r.state, &r.turn)
	return r, err
}

type marker struct {
	seq     int64
	turn    string
	started bool
}

func parseMarker(raw []byte, seq int64) (marker, bool) {
	var record struct {
		Type    string `json:"type"`
		Payload struct {
			Type   string `json:"type"`
			TurnID string `json:"turn_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &record) != nil || record.Type != "event_msg" {
		return marker{}, false
	}
	p := record.Payload
	if len(p.TurnID) > 256 || strings.ContainsRune(p.TurnID, 0) {
		p.TurnID = ""
	}
	switch p.Type {
	case "task_started", "turn_started":
		return marker{seq: seq, turn: p.TurnID, started: true}, true
	case "task_complete", "turn_complete", "turn_aborted":
		return marker{seq: seq, turn: p.TurnID}, true
	default:
		return marker{}, false
	}
}

func apply(ctx context.Context, tx pgx.Tx, store, thread string, generation int64, r route, m marker) error {
	if m.turn == "" {
		return nil
	}
	state, kind := "idle", "turn/completed"
	if m.started {
		state, kind = "running", "turn/started"
	}
	_, err := tx.Exec(ctx, `WITH changed AS (
 UPDATE mira_codex_execution_routes SET state=$7,turn_id=$8,updated_at=NOW()
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND node_account_id=$4::uuid AND runtime_id=$5 AND revision=$6
 AND (($7='running' AND (state='starting' OR (state IN ('idle','running') AND turn_id IS DISTINCT FROM $8)))
   OR ($7='idle' AND state='running' AND turn_id=$8))
 RETURNING *
 ) INSERT INTO mira_codex_execution_events(operation_id,store_id,thread_id,generation,node_account_id,runtime_id,revision,kind,turn_id,detail)
 SELECT gen_random_uuid(),store_id,thread_id,generation,node_account_id,runtime_id,revision,$9,turn_id,
 jsonb_build_object('source','canonical_history','itemSeq',$10::bigint) FROM changed`,
		store, thread, generation, r.binding, r.runtime, r.revision, state, m.turn, kind, m.seq)
	return err
}

// ApplyAppends runs in the history commit transaction, before acknowledgement.
// Only newly appended records in an existing generation are live observations;
// copied imports, replacements and forks must not replay execution transitions.
func ApplyAppends(ctx context.Context, tx pgx.Tx, store, thread string, generation, after, through int64) error {
	r, err := readRoute(ctx, tx, store, thread, generation)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	for after < through {
		rows, err := tx.Query(ctx, `SELECT item_seq,payload FROM codex_thread_events
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4 AND item_seq<=$5
 AND `+lifecyclePredicate+` ORDER BY item_seq LIMIT 32`, store, thread, generation, after, through)
		if err != nil {
			return err
		}
		markers := make([]marker, 0, 32)
		count := 0
		for rows.Next() {
			var raw []byte
			if err = rows.Scan(&after, &raw); err != nil {
				break
			}
			count++
			if m, ok := parseMarker(raw, after); ok {
				markers = append(markers, m)
			}
		}
		rows.Close()
		if err != nil {
			return err
		}
		if err = rows.Err(); err != nil {
			return err
		}
		for _, m := range markers {
			if err = apply(ctx, tx, store, thread, generation, r, m); err != nil {
				return err
			}
		}
		if count < 32 {
			break
		}
	}
	return nil
}

// ReconcileCompleted repairs an old running route only from a matching terminal
// marker at the current generation/count. It never clears a starting reservation:
// a new turn may have been accepted before its first history record is written.
func ReconcileCompleted(ctx context.Context, tx pgx.Tx, store, thread string) error {
	var generation, through int64
	err := tx.QueryRow(ctx, `SELECT p.active_generation,p.item_count FROM codex_thread_projections p
 JOIN mira_codex_execution_routes r ON r.store_id=p.store_id AND r.thread_id=p.thread_id AND r.generation=p.active_generation
 WHERE p.store_id=$1 AND p.thread_id=$2 AND r.state='running' AND r.turn_id IS NOT NULL`, store, thread).Scan(&generation, &through)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	r, err := readRoute(ctx, tx, store, thread, generation)
	if err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT item_seq,payload FROM codex_thread_events
 WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq<=$4
 AND `+lifecyclePredicate+` ORDER BY item_seq DESC LIMIT 32`, store, thread, generation, through)
	if err != nil {
		return err
	}
	var latest *marker
	for rows.Next() {
		var raw []byte
		var seq int64
		if err = rows.Scan(&seq, &raw); err != nil {
			break
		}
		if m, ok := parseMarker(raw, seq); ok {
			latest = &m
			break
		}
	}
	rows.Close()
	if err != nil {
		return err
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if latest == nil || latest.started || latest.turn == "" || latest.turn != r.turn {
		return nil
	}
	return apply(ctx, tx, store, thread, generation, r, *latest)
}
