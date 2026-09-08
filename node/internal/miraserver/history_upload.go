package miraserver

// History uploads are disposable transport staging. Only CommitDelta publishes
// canonical records, boundaries and its receipt in one transaction.
import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const historyChunkBytes = 4 * 1024 * 1024

var historyUploadPattern = regexp.MustCompile(`^/v2/stores/([^/]+)/history-uploads/([0-9a-f-]{36})(/seal)?$`)

func uploadError(status int, message string) error {
	return &HTTPError{Status: status, Code: "history_upload", Message: message}
}

func (s *Server) routeHistoryUpload(ctx context.Context, w http.ResponseWriter, r *http.Request) (bool, error) {
	m := historyUploadPattern.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return false, nil
	}
	if p, err := s.authorize(ctx, w, r, "trusted", authOptions{ClientType: "codex"}); err != nil || p == nil {
		return true, err
	}
	store, err := requireStoreID(m[1])
	if err != nil {
		return true, err
	}
	if (m[3] != "" && r.Method != http.MethodPost) || (m[3] == "" && r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodGet && r.Method != http.MethodDelete) {
		return true, uploadError(405, "unsupported upload method")
	}
	id := m[2]
	if !operationIDPattern.MatchString(id) {
		return true, uploadError(400, "invalid upload id")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(ctx)
	if r.Method == http.MethodPost && m[3] == "" {
		var body struct {
			ThreadID   string `json:"threadId"`
			ItemCount  int64  `json:"itemCount"`
			TotalBytes int64  `json:"totalBytes"`
		}
		if err = foundation.ReadJSON(r, &body, 4096); err != nil {
			return true, err
		}
		if body.ThreadID == "" || len(body.ThreadID) > 256 || body.ItemCount < 0 || body.TotalBytes < 0 {
			return true, uploadError(400, "invalid upload metadata")
		}
		if err = assertThreadsNotDeleted(ctx, tx, store, []string{body.ThreadID}); err != nil {
			return true, err
		}
		// Repeated begin must retain the exact descriptor. Clients keep the
		// same UUID while retrying, and must not reuse expired staging IDs.
		_, err = tx.Exec(ctx, `INSERT INTO mira_history_uploads(store_id,upload_id,thread_id,item_count,total_bytes) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, store, id, body.ThreadID, body.ItemCount, body.TotalBytes)
		if err != nil {
			return true, err
		}
		var thread string
		var count, total int64
		err = tx.QueryRow(ctx, `SELECT thread_id,item_count,total_bytes FROM mira_history_uploads WHERE store_id=$1 AND upload_id=$2`, store, id).Scan(&thread, &count, &total)
		if err != nil {
			return true, err
		}
		if thread != body.ThreadID || count != body.ItemCount || total != body.TotalBytes {
			return true, uploadError(409, "upload descriptor changed")
		}
	}
	var thread, status string
	var count, total, received int64
	err = tx.QueryRow(ctx, `SELECT thread_id,status,item_count,total_bytes,received_bytes FROM mira_history_uploads WHERE store_id=$1 AND upload_id=$2 FOR UPDATE`, store, id).Scan(&thread, &status, &count, &total, &received)
	if err == pgx.ErrNoRows {
		return true, uploadError(404, "history upload not found")
	}
	if err != nil {
		return true, err
	}
	if r.Method == http.MethodDelete {
		if status == "committed" {
			return true, uploadError(409, "history already committed")
		}
		_, err = tx.Exec(ctx, `UPDATE mira_history_uploads SET status='cancelled',updated_at=NOW() WHERE store_id=$1 AND upload_id=$2`, store, id)
		if err != nil {
			return true, err
		}
		status = "cancelled"
	} else if status == "cancelled" {
		return true, uploadError(410, "history upload cancelled")
	}
	if r.Method == http.MethodPut && m[3] == "" {
		offset, e := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		if e != nil || offset < 0 {
			return true, uploadError(400, "invalid chunk offset")
		}
		data, e := io.ReadAll(io.LimitReader(r.Body, historyChunkBytes+1))
		if e != nil {
			return true, e
		}
		if len(data) == 0 || len(data) > historyChunkBytes {
			return true, uploadError(413, "history chunk must contain 1 byte to 4 MiB")
		}
		if offset < received {
			var previous []byte
			e = tx.QueryRow(ctx, `SELECT data FROM mira_history_upload_chunks WHERE store_id=$1 AND upload_id=$2 AND byte_offset=$3`, store, id, offset).Scan(&previous)
			if e != nil || !bytes.Equal(previous, data) {
				return true, uploadError(409, "chunk retry differs from accepted bytes")
			}
		} else {
			if status != "uploading" || offset != received || int64(len(data)) > total-received {
				return true, uploadError(409, "unexpected history chunk offset or size")
			}
			if _, e = tx.Exec(ctx, `INSERT INTO mira_history_upload_chunks(store_id,upload_id,byte_offset,data) VALUES($1,$2,$3,$4)`, store, id, offset, data); e != nil {
				return true, e
			}
			received += int64(len(data))
			if _, e = tx.Exec(ctx, `UPDATE mira_history_uploads SET received_bytes=$3,updated_at=NOW() WHERE store_id=$1 AND upload_id=$2`, store, id, received); e != nil {
				return true, e
			}
		}
	} else if r.Method == http.MethodPost && m[3] == "/seal" {
		if status == "uploading" {
			if received != total {
				return true, uploadError(409, "history upload is incomplete")
			}
			if err = sealHistoryUpload(ctx, tx, store, id, count, total); err != nil {
				return true, err
			}
			if _, err = tx.Exec(ctx, `UPDATE mira_history_uploads SET status='sealed',updated_at=NOW() WHERE store_id=$1 AND upload_id=$2`, store, id); err != nil {
				return true, err
			}
			status = "sealed"
		}
	} else if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete && r.Method != http.MethodPut {
		return true, uploadError(405, "unsupported upload method")
	}
	if err = tx.Commit(ctx); err != nil {
		return true, err
	}
	return true, writeJSON(w, 200, map[string]any{"uploadId": id, "threadId": thread, "status": status, "receivedBytes": received, "totalBytes": total, "itemCount": count})
}

// Read at most one transport chunk at a time; JSON decoding retains at most one
// rollout record, never a complete uploaded history. Record boundaries may fall
// anywhere in the byte stream, including inside UTF-8 or an escaped NUL.
type historyUploadReader struct {
	ctx           context.Context
	tx            pgx.Tx
	store, id     string
	offset, total int64
	pending       []byte
}

func (r *historyUploadReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.offset == r.total {
			return 0, io.EOF
		}
		if err := r.tx.QueryRow(r.ctx, `SELECT data FROM mira_history_upload_chunks WHERE store_id=$1 AND upload_id=$2 AND byte_offset=$3`, r.store, r.id, r.offset).Scan(&r.pending); err != nil {
			return 0, err
		}
		if len(r.pending) == 0 {
			return 0, io.ErrNoProgress
		}
		r.offset += int64(len(r.pending))
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
func sealHistoryUpload(ctx context.Context, tx pgx.Tx, store, id string, count, total int64) error {
	decoder := json.NewDecoder(&historyUploadReader{ctx: ctx, tx: tx, store: store, id: id, total: total})
	for seq := int64(1); seq <= count; seq++ {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return uploadError(400, fmt.Sprintf("invalid history record %d", seq))
		}
		var value any
		if err := decodeArray(raw, &value); err != nil {
			return err
		}
		hash, err := digestJSON(value)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO mira_history_upload_items(store_id,upload_id,item_seq,payload,payload_sha256) VALUES($1,$2,$3,$4::json,$5)`, store, id, seq, []byte(raw), hash); err != nil {
			return err
		}
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return uploadError(400, "history upload record count mismatch")
	}
	return nil
}

// Compare only fingerprints inside PostgreSQL. The fingerprints use the same
// canonical JSON routine as ordinary history writes; raw JSON is copied intact.
func planUploadedHistory(ctx context.Context, tx pgx.Tx, store string, entry *historyEntry, change requestedHistoryChange) (historyPlan, error) {
	var target, status string
	var items int64
	err := tx.QueryRow(ctx, `SELECT thread_id,status,item_count FROM mira_history_uploads WHERE store_id=$1 AND upload_id=$2 FOR UPDATE`, store, change.UploadID).Scan(&target, &status, &items)
	if err == pgx.ErrNoRows {
		return historyPlan{}, uploadError(404, "history upload not found")
	}
	if err != nil {
		return historyPlan{}, err
	}
	if target != change.ThreadID || status != "sealed" {
		return historyPlan{}, uploadError(409, "history upload is not sealed for this thread")
	}
	var generation, count int64
	if entry != nil {
		generation, count = entry.Generation, entry.ItemCount
	}
	exact := change.ExpectedGeneration == generation && change.ExpectedItemCount == count
	if !exact && !(change.Mode == "append" && change.ExpectedGeneration == generation && change.ExpectedItemCount <= count) {
		return historyPlan{Conflict: map[string]any{"error": "thread generation conflict", "threadId": change.ThreadID}}, nil
	}
	var overlap int64
	limit := count
	offset := int64(0)
	if change.Mode == "append" {
		offset = change.ExpectedItemCount
		limit = count - offset
	}
	if limit > items {
		limit = items
	}
	if limit > 0 {
		// Missing records are mismatches, rather than silently shortening a prefix.
		err = tx.QueryRow(ctx, `SELECT COALESCE(MIN(u.item_seq)-1,$6) FROM mira_history_upload_items u
   LEFT JOIN codex_thread_events e ON e.store_id=u.store_id AND e.thread_id=$3 AND e.generation=$4 AND e.item_seq=u.item_seq+$5
   WHERE u.store_id=$1 AND u.upload_id=$2 AND u.item_seq<=$6 AND (e.item_seq IS NULL OR e.payload_sha256<>u.payload_sha256)`, store, change.UploadID, change.ThreadID, generation, offset, limit).Scan(&overlap)
		if err != nil {
			return historyPlan{}, err
		}
	}
	start, base := overlap, count
	if change.Mode == "replace" {
		if entry != nil && overlap == count && items == count {
			return historyPlan{Next: entry, UploadID: change.UploadID, UploadStart: items, UploadBase: count}, nil
		}
		if entry == nil || overlap != count {
			generation++
			start = 0
			base = 0
		} else {
			base = count
		}
	}
	if generation == 0 {
		generation = 1
	}
	next := historyEntry{Generation: generation, ItemCount: base + items - start}
	return historyPlan{Changed: entry == nil || items > start || change.Mode == "replace", Next: &next, UploadID: change.UploadID, UploadStart: start, UploadBase: base, UploadCount: items - start}, nil
}

// Expiration is a staging retention period, never a limit on conversation size.
// Remove bounded batches so a completed multi-gigabyte upload cannot monopolize
// the erasure worker. A live upload updates its timestamp on every chunk.
func cleanupHistoryUploads(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var store, id string
	err = tx.QueryRow(ctx, `SELECT store_id,upload_id::text FROM mira_history_uploads
  WHERE status IN ('committed','cancelled') OR updated_at<NOW()-INTERVAL '24 hours'
 OR EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=mira_history_uploads.store_id AND a.thread_id=mira_history_uploads.thread_id AND a.action='delete')
  ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&store, &id)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE mira_history_uploads SET status='cancelled' WHERE store_id=$1 AND upload_id=$2 AND status NOT IN ('committed','cancelled')`, store, id); err != nil {
		return err
	}
	result, err := tx.Exec(ctx, `DELETE FROM mira_history_upload_chunks WHERE store_id=$1 AND upload_id=$2 AND byte_offset IN
  (SELECT byte_offset FROM mira_history_upload_chunks WHERE store_id=$1 AND upload_id=$2 ORDER BY byte_offset LIMIT 8)`, store, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		result, err = tx.Exec(ctx, `DELETE FROM mira_history_upload_items WHERE store_id=$1 AND upload_id=$2 AND item_seq IN
   (SELECT item_seq FROM mira_history_upload_items WHERE store_id=$1 AND upload_id=$2 ORDER BY item_seq LIMIT 16)`, store, id)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			if _, err = tx.Exec(ctx, `DELETE FROM mira_history_uploads WHERE store_id=$1 AND upload_id=$2`, store, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
