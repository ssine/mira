package miraserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Rows pass through a small output buffer instead of a whole-response buffer.
// Raw JSON bypasses decoding/re-encoding (including unknown fields, large
// integers and escaped NUL). PostgreSQL remains authoritative.
type storeJSONStream struct {
	out *bufio.Writer
	err error
}

func (s *storeJSONStream) raw(value []byte) {
	if s.err == nil {
		_, s.err = s.out.Write(value)
	}
}

func (s *storeJSONStream) text(value string) {
	if s.err == nil {
		_, s.err = s.out.WriteString(value)
	}
}

func (s *storeJSONStream) value(value any) {
	if s.err != nil {
		return
	}
	var encoded []byte
	encoded, s.err = json.Marshal(value)
	s.raw(encoded)
}

func streamStoreJSON(response http.ResponseWriter, emit func(*storeJSONStream) error) error {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Del("Content-Length")
	response.WriteHeader(http.StatusOK)
	s := &storeJSONStream{out: bufio.NewWriterSize(response, 32<<10)}
	if err := emit(s); err != nil || s.err != nil {
		// A late database failure or cancellation must be a failed transfer,
		// never a valid truncated history or a second JSON error after HTTP 200.
		panic(http.ErrAbortHandler)
	}
	if err := s.out.Flush(); err != nil {
		panic(http.ErrAbortHandler)
	}
	return nil
}

func streamStoreHead(ctx context.Context, pool *pgxpool.Pool, response http.ResponseWriter, storeID string, threadIDs []string) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var version, floor int64
	var updated *time.Time
	err = tx.QueryRow(ctx, `SELECT version,history_floor,updated_at FROM codex_store_heads WHERE store_id=$1`, storeID).Scan(&version, &floor, &updated)
	if err == pgx.ErrNoRows {
		return writeJSON(response, 200, headView(storeHead{State: map[string]any{}, HistoryManifest: map[string]historyEntry{}}))
	}
	if err != nil {
		return err
	}
	// Fetch keys without detoasting values. Per-field backward primary-key
	// scans put the root first without sorting all of the store's large JSON.
	rows, err := tx.Query(ctx, `SELECT DISTINCT field FROM codex_store_state_entries
	 WHERE store_id=$1 AND ($2::text[] IS NULL OR is_root OR thread_id=ANY($2)) ORDER BY field`, storeID, nullableStrings(threadIDs))
	if err != nil {
		return err
	}
	fields, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return streamStoreJSON(response, func(s *storeJSONStream) error {
		s.text(`{"version":`)
		s.value(version)
		s.text(`,"historyFloor":`)
		s.value(floor)
		s.text(`,"updatedAt":`)
		if updated == nil {
			s.text("null")
		} else {
			s.value(updated.UTC().Format(time.RFC3339Nano))
		}
		s.text(`,"state":{`)
		for i, field := range fields {
			if i > 0 {
				s.text(",")
			}
			s.value(field)
			s.text(":")
			if err := streamStateField(ctx, tx, s, storeID, field, threadIDs); err != nil {
				return err
			}
		}
		s.text(`},"historyManifest":{`)
		rows, err := tx.Query(ctx, `SELECT thread_id,active_generation,item_count FROM codex_thread_projections
		 WHERE store_id=$1 AND ($2::text[] IS NULL OR thread_id=ANY($2))`, storeID, nullableStrings(threadIDs))
		if err != nil {
			return err
		}
		defer rows.Close()
		first := true
		for rows.Next() {
			var id string
			var entry historyEntry
			if err := rows.Scan(&id, &entry.Generation, &entry.ItemCount); err != nil {
				return err
			}
			if !first {
				s.text(",")
			}
			first = false
			s.value(id)
			s.text(":")
			s.value(entry)
			if s.err != nil {
				return s.err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.text("}}")
		return nil
	})
}

func streamStateField(ctx context.Context, tx pgx.Tx, s *storeJSONStream, storeID, field string, threadIDs []string) error {
	rows, err := tx.Query(ctx, `SELECT is_root,entry_key,value FROM codex_store_state_entries
	 WHERE store_id=$1 AND field=$3 AND ($2::text[] IS NULL OR is_root OR thread_id=ANY($2))
	 ORDER BY is_root DESC,entry_key DESC`, storeID, nullableStrings(threadIDs), field)
	if err != nil {
		return err
	}
	defer rows.Close()
	var root json.RawMessage
	var rootObject map[string]json.RawMessage
	opened, first := false, true
	member := func(key string, raw []byte) {
		if !opened {
			s.text("{")
			opened = true
		}
		if !first {
			s.text(",")
		}
		first = false
		s.value(key)
		s.text(":")
		s.raw(raw)
	}
	for rows.Next() {
		var isRoot bool
		var key string
		var raw []byte
		if err := rows.Scan(&isRoot, &key, &raw); err != nil {
			return err
		}
		if isRoot {
			root = bytes.TrimSpace(raw)
			if len(root) > 0 && root[0] == '{' {
				if err := json.Unmarshal(root, &rootObject); err != nil {
					return err
				}
			}
			continue
		}
		// Match stateFromEntries: child entries override root members, and
		// replace a scalar root with an object. Emit no duplicate JSON keys.
		delete(rootObject, key)
		member(key, raw)
		if s.err != nil {
			return s.err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !opened {
		if root == nil {
			s.text("{}")
		} else {
			s.raw(root)
		}
		return s.err
	}
	for key, value := range rootObject {
		member(key, value)
	}
	s.text("}")
	return s.err
}

func (server *Server) streamThreadHistory(ctx context.Context, response http.ResponseWriter, request *http.Request, storeID, threadID string, generation, throughVersion *int64) error {
	tx, err := server.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, version, err := prepareThreadHistory(ctx, tx, storeID, threadID, generation, throughVersion)
	if err != nil {
		return err
	}
	if result.Status != 200 {
		return writeJSON(response, result.Status, result.Body)
	}
	if err := addInputRecovery(ctx, tx, request, storeID, threadID, &result); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, threadHistoryItemsSQL, storeID, threadID, result.Body["generation"], version)
	if err != nil {
		return err
	}
	defer rows.Close()
	return streamStoreJSON(response, func(s *storeJSONStream) error {
		metadata, err := json.Marshal(result.Body)
		if err != nil {
			return err
		}
		s.raw(metadata[:len(metadata)-1])
		s.text(`,"items":[`)
		var count int64
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			if count > 0 {
				s.text(",")
			}
			s.raw(raw)
			count++
			if s.err != nil {
				return s.err
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if count != result.Body["itemCount"].(int64) {
			return fmt.Errorf("thread history changed within read snapshot")
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		s.text("]}")
		return nil
	})
}
