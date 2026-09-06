package imports

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const (
	transferChunkSize  = int64(4 * 1024 * 1024)
	transferBatchSize  = 100
	transferBatchBytes = 4 * 1024 * 1024
)

func ParseRecord(line []byte, sequence int64) (RawRecord, error) {
	if !utf8.Valid(line) {
		return RawRecord{}, importError(http.StatusConflict, "invalid_session_jsonl", fmt.Sprintf("Invalid UTF-8 in JSONL record %d", sequence))
	}
	fields, err := rawObject(line)
	if err != nil {
		return RawRecord{}, importError(http.StatusConflict, "invalid_session_jsonl", fmt.Sprintf("Invalid JSONL record %d", sequence))
	}
	var kind string
	if raw, found := fields["type"]; !found || json.Unmarshal(raw, &kind) != nil {
		return RawRecord{}, wrapInvalidRecord(sequence, "invalid_session_record")
	}
	payload, found := fields["payload"]
	if !found {
		return RawRecord{}, wrapInvalidRecord(sequence, "invalid_session_record")
	}
	semantic, err := decodeObject(payload)
	if err != nil {
		return RawRecord{}, wrapInvalidRecord(sequence, "invalid_session_record")
	}
	digest := sha256.Sum256(line)
	return RawRecord{
		LineSequence: sequence,
		Raw:          append(json.RawMessage(nil), line...),
		SHA256:       hex.EncodeToString(digest[:]),
		Type:         kind,
		Payload:      append(json.RawMessage(nil), payload...),
		Semantic:     semantic,
	}, nil
}

func (service *Service) StageSessionTransfer(
	ctx context.Context,
	principal *foundation.Principal,
	nodeID string,
	summary map[string]any,
	storeID string,
	request *http.Request,
	boundary *Boundary,
	progress Context,
) (result Staged, err error) {
	if service.Repository == nil || service.Capability == nil {
		return Staged{}, fmt.Errorf("session import dependencies are incomplete")
	}
	path := stringValue(summary["path"])
	threadID := stringValue(summary["threadId"])
	sizeBytes, sizeOK := integer(summary["sizeBytes"])
	if path == "" || !validUUID(threadID) || !sizeOK || sizeBytes <= 0 {
		return Staged{}, importError(http.StatusConflict, "invalid_session", "Invalid Codex session summary")
	}
	totalBytes := sizeBytes
	if boundary != nil {
		totalBytes = boundary.EndByteOffset
	}
	if totalBytes <= 0 || totalBytes > sizeBytes {
		return Staged{}, invalidHistory("祖先历史字节边界无效")
	}
	modifiedAt := stringValue(summary["modifiedAt"])
	var codexVersion *string
	if value := stringValue(summary["codexVersion"]); value != "" {
		codexVersion = &value
	}
	transaction, err := service.Repository.BeginTransfer(ctx, TransferInput{
		StoreID: storeID, NodeID: nodeID, Path: path, ThreadID: threadID,
		ModifiedAt: modifiedAt, CodexVersion: codexVersion, Boundary: boundary,
	})
	if err != nil {
		return Staged{}, err
	}
	finished := false
	defer func() {
		if !finished {
			rollbackContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if rollbackErr := transaction.Rollback(rollbackContext); rollbackErr != nil && err == nil {
				err = rollbackErr
			}
		}
	}()

	progress.progress(map[string]any{"phase": "reading", "bytes": int64(0), "totalBytes": totalBytes, "records": int64(0), "ancestor": boundary != nil})
	hash := sha256.New()
	cursor, count := int64(0), int64(0)
	var meta map[string]any
	var carry []byte
	var batch []RawRecord
	batchBytes := 0
	observedModified := ""
	nextOrdinal := int64(0)
	ordinalInitialized := false

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := transaction.Add(ctx, batch); err != nil {
			return err
		}
		batch = nil
		batchBytes = 0
		return nil
	}
	recordLine := func(line []byte) error {
		if strings.TrimSpace(string(line)) == "" {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := ParseRecord(line, count+1)
		if err != nil {
			return err
		}
		if count == 0 && record.Type != "session_meta" {
			return importError(http.StatusConflict, "invalid_session_record", "First rollout record must be session_meta")
		}
		if record.Type == "session_meta" && meta == nil {
			meta = record.Semantic
		}
		if boundary != nil {
			if !ordinalInitialized {
				baseOrdinal := object(meta["history_base"])["end_ordinal_exclusive"]
				if baseOrdinal != nil {
					var ok bool
					nextOrdinal, ok = integer(baseOrdinal)
					if !ok {
						return invalidHistory("祖先历史序号与引用边界不一致")
					}
				}
				ordinalInitialized = true
			}
			ordinal, ok := integer(record.SemanticEnvelopeOrdinal(line))
			if !ok || ordinal != nextOrdinal {
				return invalidHistory("祖先历史序号与引用边界不一致")
			}
			nextOrdinal++
		}
		count++
		record.LineSequence = count
		batch = append(batch, record)
		batchBytes += len(line)
		if len(batch) >= transferBatchSize || batchBytes >= transferBatchBytes {
			return flush()
		}
		return nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return Staged{}, err
		}
		limit := totalBytes - cursor
		if limit > transferChunkSize {
			limit = transferChunkSize
		}
		value, err := service.Capability.Invoke(ctx, principal, nodeID, "codexSessions", map[string]any{
			"action": "read", "path": path, "cursor": cursor, "limit": limit, "encoding": "base64",
		}, channel.InvokeContext{Request: request, Timeout: 120 * time.Second, AuditMetadata: map[string]any{"purpose": "codex_session_import"}})
		if err != nil {
			return Staged{}, err
		}
		chunk, ok := value.(map[string]any)
		if !ok {
			return Staged{}, fmt.Errorf("Invalid Node session chunk")
		}
		chunkCursor, cursorOK := integer(chunk["cursor"])
		nextCursor, nextOK := integer(chunk["nextCursor"])
		chunkSize, chunkSizeOK := integer(chunk["sizeBytes"])
		content, contentOK := chunk["content"].(string)
		eof, eofOK := chunk["eof"].(bool)
		if !cursorOK || chunkCursor != cursor || !nextOK || nextCursor < cursor || !chunkSizeOK || !contentOK || !eofOK {
			return Staged{}, fmt.Errorf("Invalid Node session chunk")
		}
		chunkModified := stringValue(chunk["modifiedAt"])
		if observedModified == "" {
			observedModified = chunkModified
		}
		if chunkSize != sizeBytes || (observedModified != "" && (observedModified != chunkModified || observedModified != modifiedAt)) {
			return Staged{}, importError(http.StatusConflict, "session_changed", "源会话在导入期间发生变化，请停止桌面端写入后重新扫描")
		}
		var payload []byte
		if stringValue(chunk["encoding"]) == "base64" {
			payload, err = base64.StdEncoding.DecodeString(content)
		} else {
			payload = []byte(content)
		}
		if err != nil || nextCursor-cursor != int64(len(payload)) || (!eof && len(payload) == 0) {
			return Staged{}, fmt.Errorf("Invalid session chunk length")
		}
		if nextCursor > totalBytes {
			return Staged{}, fmt.Errorf("Node read past requested source boundary")
		}
		done := nextCursor == totalBytes
		if boundary != nil && done && (len(payload) == 0 || payload[len(payload)-1] != '\n') {
			return Staged{}, invalidHistory("祖先字节边界必须位于完整 JSONL 记录末尾")
		}
		_, _ = hash.Write(payload)
		carry = append(carry, payload...)
		for {
			index := bytes.IndexByte(carry, '\n')
			if index < 0 {
				break
			}
			if err := recordLine(carry[:index]); err != nil {
				return Staged{}, err
			}
			carry = append([]byte(nil), carry[index+1:]...)
		}
		cursor = nextCursor
		if err := flush(); err != nil {
			return Staged{}, err
		}
		progress.progress(map[string]any{"phase": "reading", "bytes": cursor, "totalBytes": totalBytes, "records": count, "ancestor": boundary != nil})
		if done {
			if err := recordLine(carry); err != nil {
				return Staged{}, err
			}
			if err := flush(); err != nil {
				return Staged{}, err
			}
			break
		}
		if eof {
			return Staged{}, fmt.Errorf("Incomplete source prefix")
		}
	}
	if cursor != totalBytes || count == 0 || meta == nil || stringValue(meta["id"]) != threadID {
		return Staged{}, fmt.Errorf("Incomplete session or mismatched thread identity")
	}
	if boundary != nil && nextOrdinal != boundary.EndOrdinalExclusive {
		return Staged{}, invalidHistory("祖先字节边界与序号边界不一致")
	}
	if boundary == nil && service.ThreadStore != nil {
		if err := service.ThreadStore.AssertNotDeleted(ctx, storeID, []string{threadID}); err != nil {
			return Staged{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Staged{}, err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	importID, duplicate, err := transaction.Finish(ctx, TransferFinal{
		ThreadID: threadID, SHA256: digest, SizeBytes: cursor, ItemCount: count, Meta: meta,
	})
	if err != nil {
		return Staged{}, err
	}
	finished = true
	return Staged{ImportID: importID, Meta: meta, Count: count, SizeBytes: cursor, Duplicate: duplicate, Boundary: boundary}, nil
}

func (record RawRecord) SemanticEnvelopeOrdinal(line []byte) any {
	fields, err := decodeObject(line)
	if err != nil {
		return nil
	}
	return fields["ordinal"]
}
