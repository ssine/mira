package imports

import (
	"context"
	"net/http"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func historyBoundary(meta map[string]any) (*Boundary, error) {
	value := meta["history_base"]
	if value == nil || value == false || value == "" || isJSONZero(value) {
		return nil, nil
	}
	base := object(value)
	if base == nil {
		return nil, invalidHistory("祖先历史引用格式无效")
	}
	threadID := stringValue(base["thread_id"])
	ordinal, ordinalOK := integer(base["end_ordinal_exclusive"])
	byteOffset, byteOK := integer(base["end_byte_offset"])
	if !validUUID(threadID) || !ordinalOK || ordinal < 1 || !byteOK || byteOffset < 1 {
		return nil, invalidHistory("祖先历史引用格式无效")
	}
	return &Boundary{ThreadID: threadID, EndOrdinalExclusive: ordinal, EndByteOffset: byteOffset}, nil
}

func isJSONZero(value any) bool {
	integer, ok := integer(value)
	return ok && integer == 0
}

func (service *Service) StageSessionLineage(
	ctx context.Context,
	principal *foundation.Principal,
	nodeID string,
	summary map[string]any,
	storeID string,
	request *http.Request,
	staged Staged,
	progress Context,
) (ExpandedLineage, error) {
	lineage := []Staged{staged}
	seen := map[string]bool{}
	current := staged
	source := summary
	for {
		if err := ctx.Err(); err != nil {
			return ExpandedLineage{}, err
		}
		base, err := historyBoundary(current.Meta)
		if err != nil {
			return ExpandedLineage{}, err
		}
		if base == nil {
			break
		}
		if seen[base.ThreadID] {
			return ExpandedLineage{}, invalidHistory("祖先历史引用包含循环")
		}
		seen[base.ThreadID] = true
		progress.progress(map[string]any{"phase": "resolving", "ancestors": len(lineage)})
		resolved, err := service.Capability.Invoke(ctx, principal, nodeID, "codexSessions", map[string]any{
			"action": "resolve", "path": stringValue(source["path"]), "rolloutId": base.ThreadID,
		}, channel.InvokeContext{Request: request, Timeout: 120 * time.Second, AuditMetadata: map[string]any{"purpose": "codex_session_lineage"}})
		if err != nil {
			return ExpandedLineage{}, err
		}
		next, ok := resolved.(map[string]any)
		if !ok || stringValue(next["path"]) == "" || !validUUID(stringValue(next["threadId"])) {
			return ExpandedLineage{}, invalidHistory("找不到有效的祖先历史源文件，请更新节点并检查源会话")
		}
		current, err = service.StageSessionTransfer(ctx, principal, nodeID, next, storeID, request, base, progress)
		if err != nil {
			return ExpandedLineage{}, err
		}
		lineage = append(lineage, current)
		source = next
	}

	segments := []Segment{{ImportID: staged.ImportID, FirstLine: 1, Count: 1}}
	for index := len(lineage) - 1; index >= 0; index-- {
		part := lineage[index]
		if part.Count > 1 {
			segments = append(segments, Segment{
				ImportID: part.ImportID, FirstLine: 2, Count: part.Count - 1, Boundary: part.Boundary,
			})
		}
	}
	var count int64
	for _, segment := range segments {
		count += segment.Count
	}
	return ExpandedLineage{Segments: segments, Count: count, AncestorCount: len(lineage) - 1}, nil
}
