package views

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const threadPageCacheBytes = 32 << 20
const threadPageTTL = 15 * time.Minute

type ThreadPageOptions struct {
	View, ParentID, ThreadID, ProjectKey, Cursor string
	Archived, Head                               bool
	Limit                                        int
	IDs                                          []string
}

type ThreadPage struct {
	Paged        bool            `json:"paged"`
	Data         []Thread        `json:"data"`
	Projects     []ThreadProject `json:"projects"`
	NextCursor   *string         `json:"nextCursor"`
	NextParentID *string         `json:"nextParentId,omitempty"`
	Removed      []string        `json:"removed,omitempty"`
	Total        int             `json:"total"`
}

type threadPageSnapshot struct {
	id, scope string
	ids       []string
	at        time.Time
	bytes     int
}

func pageError(status int, code, message string) error {
	return &foundation.HTTPError{Status: status, Code: code, Message: message}
}

func (options ThreadPageOptions) scope(storeID string) string {
	value, _ := json.Marshal([]any{storeID, options.View, options.Archived, options.ParentID, options.ProjectKey})
	return string(value)
}

// Snapshot cursors retain only an ordered ID list. Recency changes cannot move
// an unread row across a page boundary. Cache eviction/expiry is explicit and
// clients restart the cursor while preserving rows already read.
func (service *Service) pageSnapshot(scope, cursor string, ids []string, limit int) ([]string, *string, int, error) {
	service.pageMu.Lock()
	defer service.pageMu.Unlock()
	now := service.now()
	if service.pageCache == nil {
		service.pageCache = map[string]*list.Element{}
	}
	for item := service.pageLRU.Back(); item != nil; {
		previous := item.Prev()
		snapshot := item.Value.(*threadPageSnapshot)
		if now.Sub(snapshot.at) > threadPageTTL {
			service.pageBytes -= snapshot.bytes
			delete(service.pageCache, snapshot.id)
			service.pageLRU.Remove(item)
		}
		item = previous
	}
	offset := 0
	var snapshot *threadPageSnapshot
	if cursor != "" {
		parts := strings.Split(cursor, ".")
		if len(parts) != 3 || parts[0] != "tl1" || len(parts[1]) != 32 {
			return nil, nil, 0, pageError(400, "invalid_cursor", "无效的列表游标")
		}
		parsed, err := strconv.Atoi(parts[2])
		if err != nil || parsed < 0 {
			return nil, nil, 0, pageError(400, "invalid_cursor", "无效的列表游标")
		}
		offset = parsed
		entry := service.pageCache[parts[1]]
		if entry == nil {
			return nil, nil, 0, pageError(410, "thread_cursor_expired", "列表游标已过期，请重新加载")
		}
		snapshot = entry.Value.(*threadPageSnapshot)
		if snapshot.scope != scope || offset > len(snapshot.ids) {
			return nil, nil, 0, pageError(400, "invalid_cursor", "列表游标与当前筛选不匹配")
		}
		snapshot.at = now
		service.pageLRU.MoveToFront(entry)
		ids = snapshot.ids
	}
	end := min(offset+limit, len(ids))
	page := append([]string{}, ids[offset:end]...)
	if end == len(ids) {
		return page, nil, len(ids), nil
	}
	if snapshot == nil {
		bytes := len(scope) + len(ids)*16
		for _, id := range ids {
			bytes += len(id)
		}
		if bytes > threadPageCacheBytes {
			return nil, nil, 0, pageError(503, "thread_page_busy", "列表快照暂时过大，请按项目加载")
		}
		for service.pageBytes+bytes > threadPageCacheBytes || service.pageLRU.Len() >= 128 {
			item := service.pageLRU.Back()
			old := item.Value.(*threadPageSnapshot)
			service.pageBytes -= old.bytes
			delete(service.pageCache, old.id)
			service.pageLRU.Remove(item)
		}
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, nil, 0, err
		}
		snapshot = &threadPageSnapshot{id: hex.EncodeToString(raw[:]), scope: scope, ids: append([]string{}, ids...), at: now, bytes: bytes}
		service.pageCache[snapshot.id] = service.pageLRU.PushFront(snapshot)
		service.pageBytes += bytes
	}
	next := "tl1." + snapshot.id + "." + strconv.Itoa(end)
	return page, &next, len(ids), nil
}

func (service *Service) ListThreadPage(ctx context.Context, storeID string, options ThreadPageOptions) (ThreadPage, error) {
	result := ThreadPage{Paged: true, Data: []Thread{}}
	if !safeStorePattern.MatchString(storeID) || options.Limit < 1 || options.Limit > 100 || len(options.IDs) > 100 || len(options.Cursor) > 128 || len(options.ProjectKey) > 16384 {
		return result, pageError(400, "invalid_request", "无效的列表查询")
	}
	directory, err := service.threadDirectory(ctx, storeID, options.Archived)
	if err != nil {
		return result, err
	}
	ids := []string{}
	switch options.View {
	case "roots":
		result.Projects = directory.projects
		for _, item := range directory.roots {
			if options.ProjectKey == "" || item.project.Key == options.ProjectKey {
				ids = append(ids, item.id)
			}
		}
	case "children":
		parent := directory.byID[options.ParentID]
		if parent == nil {
			return result, pageError(404, "not_found", "父会话不在当前列表中")
		}
		for _, item := range parent.children {
			ids = append(ids, item.id)
		}
	case "path":
		item := directory.byID[options.ThreadID]
		for item != nil && len(ids) < options.Limit {
			ids = append(ids, item.id)
			item = item.parent
		}
		if item != nil {
			next := item.id
			result.NextParentID = &next
		}
	case "refresh":
		ids = append(ids, options.IDs...)
	default:
		return result, pageError(400, "invalid_request", "无效的列表视图")
	}
	result.Total = len(ids)
	if options.View == "roots" || options.View == "children" {
		if options.Head {
			ids = ids[:min(options.Limit, len(ids))]
		} else {
			ids, result.NextCursor, result.Total, err = service.pageSnapshot(options.scope(storeID), options.Cursor, ids, options.Limit)
			if err != nil {
				return result, err
			}
		}
	}
	threads, err := service.listThreadIDs(ctx, storeID, ids)
	if err != nil {
		return result, err
	}
	byID := map[string]Thread{}
	for _, thread := range threads {
		if thread.Archived != options.Archived {
			continue
		}
		if item := directory.byID[thread.ThreadID]; item != nil {
			thread.ListRoot = item.parent == nil
			count, direct := item.descendants, len(item.children)
			thread.SubagentCount, thread.ChildCount = &count, &direct
			if !item.updated.IsZero() {
				updated := formatTime(item.updated)
				thread.UpdatedAt = &updated
			}
		}
		byID[thread.ThreadID] = thread
	}
	for _, id := range ids {
		if thread, ok := byID[id]; ok {
			result.Data = append(result.Data, thread)
		} else if options.View == "refresh" {
			result.Removed = append(result.Removed, id)
		}
	}
	return result, nil
}
