package views

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
)

type ThreadProject struct {
	Key    string `json:"key"`
	NodeID string `json:"nodeId"`
	Cwd    string `json:"cwd"`
	Count  int    `json:"count"`
}

type directoryThread struct {
	id, parentID, nodeID, cwd string
	updated                   time.Time
	windows                   bool
	parent                    *directoryThread
	children                  []*directoryThread
	descendants               int
	project                   ThreadProject
}

type threadDirectory struct {
	byID     map[string]*directoryThread
	roots    []*directoryThread
	projects []ThreadProject
}

type cachedThreadDirectory struct {
	at        time.Time
	directory *threadDirectory
}

func directoryLess(a, b *directoryThread) bool {
	if !a.updated.Equal(b.updated) {
		return a.updated.After(b.updated)
	}
	return a.id > b.id
}

func directoryProject(item *directoryThread) ThreadProject {
	path := item.cwd
	windows := item.windows || len(path) > 2 && path[1] == ':' && (path[2] == '/' || path[2] == '\\') || strings.HasPrefix(path, `\\`)
	if windows {
		path = strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	}
	path = strings.TrimRight(path, "/")
	if path == "" && item.cwd != "" {
		path = "/"
	}
	var key bytes.Buffer
	encoder := json.NewEncoder(&key)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode([]string{item.nodeID, path})
	return ThreadProject{Key: strings.TrimSuffix(key.String(), "\n"), NodeID: item.nodeID, Cwd: item.cwd}
}

// Only identifiers and navigation metadata are materialized, never histories or
// expensive usage projections. Cycles are broken deterministically in this view.
func buildThreadDirectory(items []*directoryThread) *threadDirectory {
	directory := &threadDirectory{byID: map[string]*directoryThread{}, projects: []ThreadProject{}}
	for _, item := range items {
		directory.byID[item.id] = item
	}
	for _, item := range items {
		if item.parentID != item.id {
			item.parent = directory.byID[item.parentID]
		}
	}
	done := map[string]bool{}
	for _, item := range items {
		path := []*directoryThread{}
		positions := map[string]int{}
		for current := item; current != nil && !done[current.id]; current = current.parent {
			if start, cycle := positions[current.id]; cycle {
				root := current
				for _, member := range path[start:] {
					if member.id < root.id {
						root = member
					}
				}
				root.parent = nil
				break
			}
			positions[current.id] = len(path)
			path = append(path, current)
		}
		for _, member := range path {
			done[member.id] = true
		}
	}
	for _, item := range items {
		if item.parent == nil {
			directory.roots = append(directory.roots, item)
		} else {
			item.parent.children = append(item.parent.children, item)
		}
	}
	order := append([]*directoryThread{}, directory.roots...)
	for i := 0; i < len(order); i++ {
		order = append(order, order[i].children...)
	}
	for i := len(order) - 1; i >= 0; i-- {
		item := order[i]
		for _, child := range item.children {
			item.descendants += 1 + child.descendants
			if child.updated.After(item.updated) {
				item.updated = child.updated
			}
		}
		sort.Slice(item.children, func(i, j int) bool { return directoryLess(item.children[i], item.children[j]) })
	}
	sort.Slice(directory.roots, func(i, j int) bool { return directoryLess(directory.roots[i], directory.roots[j]) })
	projects := map[string]int{}
	for _, item := range directory.roots {
		item.project = directoryProject(item)
		index, exists := projects[item.project.Key]
		if !exists {
			index = len(directory.projects)
			projects[item.project.Key] = index
			directory.projects = append(directory.projects, item.project)
		}
		directory.projects[index].Count++
	}
	return directory
}

func (service *Service) threadDirectory(ctx context.Context, storeID string, archived bool) (*threadDirectory, error) {
	key := storeID + "\x00" + map[bool]string{true: "1", false: "0"}[archived]
	service.directoryMu.Lock()
	defer service.directoryMu.Unlock()
	if cached := service.directories[key]; cached != nil && service.now().Sub(cached.at) < 3*time.Second {
		return cached.directory, nil
	}
	rows, err := service.pool.Query(ctx, `SELECT p.thread_id,COALESCE(p.parent_thread_id,''),COALESCE(p.cwd,''),
  COALESCE(r.node_id::text,imports.source_node_id::text,''),COALESCE(n.platform='windows',false),activity.updated_at
 FROM codex_thread_projections p
 CROSS JOIN LATERAL jsonb_to_record(p.state) s(metadata jsonb,"createdThread" jsonb)
 LEFT JOIN LATERAL (SELECT action FROM mira_thread_actions WHERE store_id=p.store_id AND thread_id=p.thread_id ORDER BY action_seq DESC LIMIT 1) a ON TRUE
 LEFT JOIN LATERAL (
  SELECT value::timestamptz updated_at FROM (VALUES
   (1,s.metadata->>'updated_at'),(2,s.metadata->>'advance_recency_at'),(3,s.metadata->>'created_at'),(4,s."createdThread"#>>'{metadata,timestamp}')
  ) stamps(priority,value)
  WHERE value~'^[0-9]{4}-[0-9]{2}-[0-9]{2}T' AND pg_input_is_valid(value,'timestamp with time zone') ORDER BY priority LIMIT 1
 ) activity ON TRUE
 LEFT JOIN mira_codex_thread_runtimes r ON r.store_id=p.store_id AND r.thread_id=p.thread_id
 LEFT JOIN LATERAL (SELECT source_node_id FROM mira_codex_session_imports WHERE store_id=p.store_id AND thread_id=p.thread_id AND status='imported' ORDER BY created_at DESC LIMIT 1) imports ON TRUE
 LEFT JOIN codex_nodes n ON n.node_id=COALESCE(r.node_id,imports.source_node_id)
 WHERE p.store_id=$1 AND COALESCE(a.action='archive',false)=$2`, storeID, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*directoryThread{}
	for rows.Next() {
		item := &directoryThread{}
		var updated *time.Time
		if err := rows.Scan(&item.id, &item.parentID, &item.cwd, &item.nodeID, &item.windows, &updated); err != nil {
			return nil, err
		}
		if updated != nil {
			item.updated = *updated
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	directory := buildThreadDirectory(items)
	if service.directories == nil {
		service.directories = map[string]*cachedThreadDirectory{}
	}
	// Large directories are processed for this request but not retained. This
	// bounds cache memory without imposing a row-count limit on navigation.
	bytes := 0
	for _, item := range items {
		bytes += len(item.id) + len(item.parentID) + len(item.nodeID) + len(item.cwd) + 256
	}
	if bytes <= 8<<20 {
		if len(service.directories) >= 4 {
			oldest := ""
			for key, cached := range service.directories {
				if oldest == "" || cached.at.Before(service.directories[oldest].at) {
					oldest = key
				}
			}
			delete(service.directories, oldest)
		}
		service.directories[key] = &cachedThreadDirectory{at: service.now(), directory: directory}
	}
	return directory, nil
}

// Administrative archive/delete actions must update navigation immediately.
// Live runtime writes are discovered by the short directory TTL.
func (service *Service) InvalidateThreadDirectory(storeID string) {
	service.directoryMu.Lock()
	defer service.directoryMu.Unlock()
	delete(service.directories, storeID+"\x000")
	delete(service.directories, storeID+"\x001")
}
