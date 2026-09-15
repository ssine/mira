package views

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestThreadDirectory(t *testing.T) {
	now := time.Now()
	d := buildThreadDirectory([]*directoryThread{
		{id: "root", nodeID: "node", cwd: `C:\Work\`, updated: now.Add(-time.Hour)},
		{id: "sibling", nodeID: "node", cwd: "c:/work"},
		{id: "child", parentID: "root", nodeID: "other", cwd: "/elsewhere"},
		{id: "grandchild", parentID: "child", updated: now},
		{id: "orphan", parentID: "archived"},
		{id: "cycle-b", parentID: "cycle-a"}, {id: "cycle-a", parentID: "cycle-b"},
		{id: "self", parentID: "self"},
	})
	if len(d.roots) != 5 || d.roots[0].id != "root" || !d.roots[0].updated.Equal(now) || d.byID["root"].descendants != 2 {
		t.Fatalf("wrong roots/recency/count: %+v", d.roots)
	}
	if d.byID["cycle-a"].parent != nil || d.byID["cycle-b"].parent.id != "cycle-a" {
		t.Fatal("cycle root is not deterministic")
	}
	if d.byID["root"].project.Key != d.byID["sibling"].project.Key || d.projects[0].Count != 2 {
		t.Fatalf("project normalization: %+v", d.projects)
	}
	if d.byID["orphan"].parent != nil {
		t.Fatal("missing parent hid orphan")
	}
}

func TestThreadPageSnapshot(t *testing.T) {
	service := New(nil)
	now := time.Now()
	service.now = func() time.Time { return now }
	first, next, total, err := service.pageSnapshot("scope", "", []string{"a", "b", "c", "d"}, 2)
	if err != nil || total != 4 || len(first) != 2 || next == nil {
		t.Fatalf("first: %v %v %v", first, next, err)
	}
	second, end, _, err := service.pageSnapshot("scope", *next, []string{"d", "a", "b", "c"}, 2)
	if err != nil || end != nil || fmt.Sprint(second) != "[c d]" {
		t.Fatalf("unstable cursor: %v %v", second, err)
	}
	_, _, _, err = service.pageSnapshot("different-project", *next, nil, 2)
	var httpErr *foundation.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != "invalid_cursor" {
		t.Fatalf("scope: %v", err)
	}
	now = now.Add(threadPageTTL + time.Second)
	_, _, _, err = service.pageSnapshot("scope", *next, nil, 2)
	if !errors.As(err, &httpErr) || httpErr.Code != "thread_cursor_expired" {
		t.Fatalf("expiry: %v", err)
	}
	for i := 0; i < 140; i++ {
		if _, _, _, err = service.pageSnapshot(fmt.Sprint(i), "", []string{"a", "b"}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if service.pageLRU.Len() > 128 || service.pageBytes > threadPageCacheBytes {
		t.Fatal("unbounded cache")
	}
}

func TestThreadPagesIntegration(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("MIRA_VIEWS_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := fmt.Sprintf("pages-%d", time.Now().UnixNano())
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, table := range []string{"mira_thread_actions", "codex_thread_projections", "codex_store_heads"} {
			exec("DELETE FROM "+table+" WHERE store_id=$1", store)
		}
	}()
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, store)
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,cwd,state,through_event_seq)
 SELECT $1,'root-'||lpad(i::text,4,'0'),1,0,CASE WHEN i=350 THEN '/old-project' ELSE '/work' END,
 jsonb_build_object('metadata',jsonb_build_object('updated_at',to_char('2026-09-16T00:00:00Z'::timestamptz-i*interval '1 minute','YYYY-MM-DD"T"HH24:MI:SS"Z"'))),1
 FROM generate_series(1,350) i`, store)
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq)
 SELECT $1,'child-'||lpad(i::text,4,'0'),'root-0001',1,0,'{"metadata":{"updated_at":"2026-09-17T00:00:00Z"}}',1 FROM generate_series(1,1000) i`, store)
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,parent_thread_id,active_generation,item_count,state,through_event_seq)
 VALUES($1,'grandchild','child-1000',1,0,'{}',1)`, store)
	service := New(pool)
	query := func(options ThreadPageOptions) ThreadPage {
		t.Helper()
		if options.Limit == 0 {
			options.Limit = 50
		}
		p, err := service.ListThreadPage(ctx, store, options)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := query(ThreadPageOptions{View: "roots"})
	if len(first.Data) != 50 || first.Total != 350 || len(first.Projects) != 2 || first.NextCursor == nil {
		t.Fatalf("root page incomplete: %d/%d projects=%v", len(first.Data), first.Total, first.Projects)
	}
	if first.Data[0].ThreadID != "root-0001" || *first.Data[0].SubagentCount != 1001 || *first.Data[0].ChildCount != 1000 {
		t.Fatalf("root tree summary: %+v", first.Data[0])
	}
	// A new head and a recency change must not move an unread root past its cursor.
	exec(`UPDATE codex_thread_projections SET state='{"metadata":{"updated_at":"2026-09-18T00:00:00Z"}}' WHERE store_id=$1 AND thread_id='root-0300'`, store)
	service.InvalidateThreadDirectory(store)
	seen := map[string]bool{}
	current := first
	for {
		for _, row := range current.Data {
			if seen[row.ThreadID] {
				t.Fatal("duplicate root")
			}
			seen[row.ThreadID] = true
		}
		if current.NextCursor == nil {
			break
		}
		current = query(ThreadPageOptions{View: "roots", Cursor: *current.NextCursor})
	}
	if len(seen) != 350 {
		t.Fatalf("missing roots: %d", len(seen))
	}
	children := query(ThreadPageOptions{View: "children", ParentID: "root-0001"})
	if children.Total != 1000 || len(children.Data) != 50 || children.Data[0].ThreadID != "child-1000" {
		t.Fatalf("child page: %+v", children)
	}
	for _, row := range children.Data {
		if row.ThreadID == "grandchild" {
			t.Fatal("grandchild loaded eagerly")
		}
	}
	path := query(ThreadPageOptions{View: "path", ThreadID: "grandchild", Limit: 2})
	if len(path.Data) != 2 || path.NextParentID == nil || *path.NextParentID != "root-0001" {
		t.Fatalf("path: %+v", path)
	}
	for _, project := range first.Projects {
		if project.Cwd == "/old-project" {
			p := query(ThreadPageOptions{View: "roots", ProjectKey: project.Key})
			if len(p.Data) != 1 || p.Data[0].ThreadID != "root-0350" {
				t.Fatal("old project unreachable")
			}
		}
	}
	exec(`INSERT INTO mira_thread_actions(store_id,thread_id,action,operation_id,generation) VALUES($1,'root-0001','archive','d079b7da-4072-4c02-b012-1eb9001fc211',1)`, store)
	service.InvalidateThreadDirectory(store)
	refresh := query(ThreadPageOptions{View: "refresh", IDs: []string{"root-0001", "missing", "root-0002"}})
	if len(refresh.Data) != 1 || len(refresh.Removed) != 2 {
		t.Fatalf("refresh removals: %+v", refresh)
	}
	orphan := query(ThreadPageOptions{View: "path", ThreadID: "child-1000"})
	if len(orphan.Data) != 1 || !orphan.Data[0].ListRoot {
		t.Fatalf("archived parent hid child: %+v", orphan)
	}
	archived := query(ThreadPageOptions{View: "roots", Archived: true})
	if len(archived.Data) != 1 || archived.Data[0].ThreadID != "root-0001" {
		t.Fatal("archive filter")
	}
	legacy, err := service.ListThreads(ctx, store, 300, nil, nil)
	if err != nil || len(legacy) != 300 {
		t.Fatalf("legacy list: %d %v", len(legacy), err)
	}
}
