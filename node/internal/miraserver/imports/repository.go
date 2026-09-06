package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Database interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type PostgresRepository struct {
	Pool Database
}

func (repository *PostgresRepository) ApprovedNodes(ctx context.Context) ([]NodeRuntime, error) {
	rows, err := repository.Pool.Query(ctx, `SELECT node_id::text,hostname,platform,node_mode,capabilities,last_seen_at,
            COALESCE((channel_status->>'connected')::boolean,false) AS connected
      FROM codex_nodes WHERE approval_status='approved'
      ORDER BY COALESCE((channel_status->>'connected')::boolean,false) DESC,last_seen_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []NodeRuntime{}
	for rows.Next() {
		var node NodeRuntime
		var capabilities []byte
		if err := rows.Scan(&node.NodeID, &node.Hostname, &node.Platform, &node.NodeMode, &capabilities, &node.LastSeenAt, &node.Connected); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(capabilities, &node.Capabilities); err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func (repository *PostgresRepository) PreviousImports(ctx context.Context, nodeID, storeID string, paths []string) (map[string]PreviousImport, error) {
	if len(paths) == 0 {
		return map[string]PreviousImport{}, nil
	}
	rows, err := repository.Pool.Query(ctx, `SELECT DISTINCT ON (source_path) source_path,source_sha256,status,thread_id,
            store_id,source_size_bytes,source_modified_at,import_id::text,created_at
      FROM mira_codex_session_imports imports
	  WHERE source_node_id=$1 AND store_id=$2 AND source_path=ANY($3::text[])
        AND NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=imports.store_id AND thread_id=imports.thread_id AND action='delete')
	  ORDER BY source_path,created_at DESC`, nodeID, storeID, paths)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]PreviousImport{}
	for rows.Next() {
		var value PreviousImport
		if err := rows.Scan(&value.SourcePath, &value.SourceSHA256, &value.Status, &value.ThreadID, &value.StoreID,
			&value.SourceSizeBytes, &value.SourceModifiedAt, &value.ImportID, &value.CreatedAt); err != nil {
			return nil, err
		}
		result[value.SourcePath] = value
	}
	return result, rows.Err()
}

func (repository *PostgresRepository) ValidRuntime(ctx context.Context, nodeID string) (string, bool, error) {
	var result string
	err := repository.Pool.QueryRow(ctx, `SELECT node_id::text FROM codex_nodes
      WHERE node_id=$1 AND approval_status='approved' AND capabilities->>'appServer'='true'`, nodeID).Scan(&result)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return result, err == nil, err
}

func (repository *PostgresRepository) BeginTransfer(ctx context.Context, input TransferInput) (TransferTransaction, error) {
	transaction, err := repository.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := transaction.Exec(ctx, `CREATE TEMP TABLE mira_session_transfer (
      line_seq BIGINT PRIMARY KEY,raw_record JSON NOT NULL,raw_sha256 TEXT NOT NULL
    ) ON COMMIT DROP`); err != nil {
		_ = transaction.Rollback(ctx)
		return nil, err
	}
	return &postgresTransfer{tx: transaction, input: input}, nil
}

func (repository *PostgresRepository) MarkImport(ctx context.Context, importID, status string, eventSequence *int64, errorCode *string) error {
	_, err := repository.Pool.Exec(ctx, `UPDATE mira_codex_session_imports SET status=$2,store_event_seq=$3,
      error_code=$4,updated_at=NOW() WHERE import_id=$1 AND (status<>'imported' OR $2='imported')`,
		importID, status, eventSequence, errorCode)
	return err
}

func (repository *PostgresRepository) ImportedThreadKeys(ctx context.Context) ([]ThreadKey, error) {
	rows, err := repository.Pool.Query(ctx, `SELECT DISTINCT store_id,thread_id FROM mira_codex_session_imports
      WHERE status='imported' ORDER BY store_id,thread_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ThreadKey{}
	for rows.Next() {
		var value ThreadKey
		if err := rows.Scan(&value.StoreID, &value.ThreadID); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func (repository *PostgresRepository) FirstHistoryItem(ctx context.Context, storeID, threadID string, generation, version int64) (json.RawMessage, bool, error) {
	var value []byte
	err := repository.Pool.QueryRow(ctx, `SELECT payload FROM codex_thread_events_versioned
      WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 AND item_seq=1`,
		storeID, threadID, generation, version).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return json.RawMessage(value), err == nil, err
}

type postgresTransfer struct {
	tx       pgx.Tx
	input    TransferInput
	mu       sync.Mutex
	finished bool
}

func (transfer *postgresTransfer) Add(ctx context.Context, records []RawRecord) error {
	transfer.mu.Lock()
	defer transfer.mu.Unlock()
	if transfer.finished {
		return fmt.Errorf("session transfer is already closed")
	}
	if len(records) == 0 {
		return nil
	}
	arguments := make([]any, 0, len(records)*3)
	tuples := make([]string, 0, len(records))
	for index, record := range records {
		base := index*3 + 1
		tuples = append(tuples, fmt.Sprintf("($%d,$%d::json,$%d)", base, base+1, base+2))
		arguments = append(arguments, record.LineSequence, string(record.Raw), record.SHA256)
	}
	_, err := transfer.tx.Exec(ctx, `INSERT INTO mira_session_transfer(line_seq,raw_record,raw_sha256) VALUES `+strings.Join(tuples, ","), arguments...)
	return err
}

func (transfer *postgresTransfer) Finish(ctx context.Context, final TransferFinal) (string, bool, error) {
	transfer.mu.Lock()
	defer transfer.mu.Unlock()
	if transfer.finished {
		return "", false, fmt.Errorf("session transfer is already closed")
	}
	defer func() { transfer.finished = true }()
	if _, err := transfer.tx.Exec(ctx, "SELECT version FROM codex_store_heads WHERE store_id=$1 FOR UPDATE", transfer.input.StoreID); err != nil {
		_ = transfer.tx.Rollback(ctx)
		return "", false, err
	}
	if transfer.input.Boundary == nil {
		var deleted string
		err := transfer.tx.QueryRow(ctx, `SELECT thread_id FROM mira_thread_actions
        WHERE store_id=$1 AND action='delete' AND thread_id=$2 LIMIT 1`, transfer.input.StoreID, final.ThreadID).Scan(&deleted)
		if err == nil {
			_ = transfer.tx.Rollback(ctx)
			return "", false, importError(410, "thread_deleted", "此会话已永久删除，不能继续写入或恢复。")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			_ = transfer.tx.Rollback(ctx)
			return "", false, err
		}
	}
	boundary, err := json.Marshal(transfer.input.Boundary)
	if err != nil {
		_ = transfer.tx.Rollback(ctx)
		return "", false, err
	}
	var modified any
	if transfer.input.ModifiedAt != "" {
		modified = transfer.input.ModifiedAt
	}
	var importID string
	err = transfer.tx.QueryRow(ctx, `INSERT INTO mira_codex_session_imports
      (store_id,thread_id,source_node_id,source_path,source_sha256,source_size_bytes,
       source_modified_at,source_codex_version,source_item_count,source_boundary,status)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,'staged')
	  ON CONFLICT(source_node_id,source_path,source_sha256) DO NOTHING RETURNING import_id::text`,
		transfer.input.StoreID, final.ThreadID, transfer.input.NodeID, transfer.input.Path, final.SHA256, final.SizeBytes,
		modified, transfer.input.CodexVersion, final.ItemCount, string(boundary)).Scan(&importID)
	duplicate := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !duplicate {
		_ = transfer.tx.Rollback(ctx)
		return "", false, err
	}
	if !duplicate {
		if _, err := transfer.tx.Exec(ctx, `INSERT INTO mira_codex_session_import_records(import_id,line_seq,raw_record,raw_sha256)
        SELECT $1,line_seq,raw_record,raw_sha256 FROM mira_session_transfer ORDER BY line_seq`, importID); err != nil {
			_ = transfer.tx.Rollback(ctx)
			return "", false, err
		}
	} else {
		var existingStoreID string
		if err := transfer.tx.QueryRow(ctx, `SELECT import_id::text,store_id FROM mira_codex_session_imports
      WHERE source_node_id=$1 AND source_path=$2 AND source_sha256=$3`,
			transfer.input.NodeID, transfer.input.Path, final.SHA256).Scan(&importID, &existingStoreID); err != nil {
			_ = transfer.tx.Rollback(ctx)
			return "", false, err
		}
		if existingStoreID != transfer.input.StoreID {
			_ = transfer.tx.Rollback(ctx)
			return "", false, importError(409, "store_scope_conflict", "This source is already imported in another store; cross-store copies require a later schema compatibility release")
		}
	}
	if err := transfer.tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return importID, duplicate, nil
}

func (transfer *postgresTransfer) Rollback(ctx context.Context) error {
	transfer.mu.Lock()
	defer transfer.mu.Unlock()
	if transfer.finished {
		return nil
	}
	transfer.finished = true
	return transfer.tx.Rollback(ctx)
}
