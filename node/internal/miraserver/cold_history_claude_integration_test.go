package miraserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/coldstore"
	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
)

// Claude tool results of idle sessions move to the bucket; every other record
// stays hot, reads return the exact bytes, and a running session is skipped.
func TestClaudeColdHistoryRoundTrip(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	nodeID := uuid()
	if _, err := pool.Exec(ctx, `INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,
 codex_installations,approval_status,approved_at,channel_status,reported_app_server)
 VALUES($1::uuid,$1::text,'claude-cold','linux','amd64','linux','test','{}','[]','approved',now(),'{}','{}')`, nodeID); err != nil {
		t.Fatal(err)
	}
	type fixture struct{ session, turn string }
	create := func(running bool) fixture {
		f := fixture{uuid(), uuid()}
		status := "completed"
		if running {
			status = "running"
		}
		if _, err := pool.Exec(ctx, `INSERT INTO mira_claude_sessions(session_id,request_id,node_id,cwd,title) VALUES($1,$2,$3,'/work','cold')`, f.session, uuid(), nodeID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO mira_claude_turns(turn_id,session_id,node_id,revision,request,status) VALUES($1,$2,$3,1,'{}',$4)`, f.turn, f.session, nodeID, status); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO mira_claude_transcripts(session_id,subpath,thread_id,next_seq) VALUES($1,'',$2,3)`, f.session, uuid()); err != nil {
			t.Fatal(err)
		}
		return f
	}
	toolResult := func(id string) string {
		raw, _ := json.Marshal(map[string]any{
			"type": "user", "uuid": id, "parentUuid": "parent", "session_id": "s", "timestamp": "2026-09-01T00:00:00.000Z",
			"message": map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": "t1", "content": randomText(t, 48<<10) + " nul \u0000 ü",
			}}},
		})
		return string(raw)
	}
	assistant := `{"type":"assistant","uuid":"a1","timestamp":"2026-09-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"完成 ` + randomText(t, 40<<10) + `"}]}}`
	idle, running := create(false), create(true)
	originals := map[string]string{}
	for _, f := range []fixture{idle, running} {
		user := toolResult(uuid())
		for index, payload := range []string{user, assistant} {
			kind := []string{"user", "assistant"}[index]
			if _, err := pool.Exec(ctx, `INSERT INTO mira_claude_events(event_id,session_id,turn_id,payload,event_type) VALUES($1,$2,$3,$4::json,$5)`, uuid(), f.session, f.turn, payload, kind); err != nil {
				t.Fatal(err)
			}
		}
		entry := toolResult(uuid())
		if _, err := pool.Exec(ctx, `INSERT INTO mira_claude_entries(session_id,subpath,seq,entry_uuid,payload) VALUES($1,'',1,'e1',$2::json),($1,'',2,'e2',$3::json)`, f.session, entry, assistant); err != nil {
			t.Fatal(err)
		}
		originals[f.session+"/event"] = user
		originals[f.session+"/entry"] = entry
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_claude_sessions SET updated_at=now()-interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_claude_turns SET created_at=now()-interval '40 days',completed_at=now()-interval '40 days'`); err != nil {
		t.Fatal(err)
	}
	views := serverviews.New(pool)
	indexRows := func(session string) string {
		if _, err := pool.Exec(ctx, `DELETE FROM mira_claude_index_state WHERE session_id=$1`, session); err != nil {
			t.Fatal(err)
		}
		if err := views.IndexClaudeTranscript(ctx, session, ""); err != nil {
			t.Fatal(err)
		}
		var rows *string
		if err := pool.QueryRow(ctx, `SELECT json_agg(json_build_array(item_seq,role,body) ORDER BY item_seq)::text FROM mira_conversation_messages WHERE engine='claude' AND thread_id=$1`, session).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows == nil {
			return ""
		}
		return *rows
	}
	beforeIndex := indexRows(idle.session)
	indexRows(running.session)

	objects := &coldTestObjects{objects: map[string][]byte{}}
	store, err := coldstore.New(objects, coldstore.Options{CacheDir: t.TempDir(), CacheBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	coldstore.SetDefault(store)
	defer coldstore.SetDefault(nil)
	freezer := coldstore.NewFreezer(pool, objects, coldstore.FreezerOptions{
		FreezeAfter: 24 * time.Hour, MinBytes: 4 << 10, OrphanGrace: time.Hour, ClaudeIndexVersion: serverviews.ConversationIndexVersion,
	})
	stats, err := freezer.FreezeClaudePass(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Threads != 1 || stats.Rows != 2 {
		t.Fatalf("stats %+v (only the idle session's two tool results may move)", stats)
	}
	frozen := func(table, session string) int {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE session_id=$1 AND cold_sha256 IS NOT NULL`, session).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if frozen("mira_claude_events", idle.session) != 1 || frozen("mira_claude_entries", idle.session) != 1 ||
		frozen("mira_claude_events", running.session) != 0 || frozen("mira_claude_entries", running.session) != 0 {
		t.Fatal("unexpected frozen rows")
	}
	var hotAssistant int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mira_claude_events WHERE event_type='assistant' AND cold_sha256 IS NOT NULL`).Scan(&hotAssistant); err != nil || hotAssistant != 0 {
		t.Fatalf("assistant records frozen: %d %v", hotAssistant, err)
	}
	if again, err := freezer.FreezeClaudePass(ctx, 10); err != nil || again.Threads != 0 {
		t.Fatalf("unchanged session rescanned: %+v %v", again, err)
	}
	read := func(query, session string) string {
		var raw []byte
		var cold *string
		if err := pool.QueryRow(ctx, query, session).Scan(&raw, &cold); err != nil {
			t.Fatal(err)
		}
		if cold == nil {
			t.Fatal("row not frozen")
		}
		if len(raw) > 400 {
			t.Fatalf("placeholder is %d bytes", len(raw))
		}
		if !json.Valid(raw) || claudeRootOmitted(raw) {
			t.Fatal("placeholder is not a valid root-visible record")
		}
		value, err := coldstore.Hydrate(ctx, raw, cold)
		if err != nil {
			t.Fatal(err)
		}
		if coldstore.ContentDigest(raw, cold) != coldstore.Digest(value) {
			t.Fatal("content digest differs from canonical bytes")
		}
		return string(value)
	}
	if read(`SELECT payload,cold_sha256 FROM mira_claude_events WHERE session_id=$1 AND event_type='user'`, idle.session) != originals[idle.session+"/event"] ||
		read(`SELECT payload,cold_sha256 FROM mira_claude_entries WHERE session_id=$1 AND seq=1`, idle.session) != originals[idle.session+"/entry"] {
		t.Fatal("hydrated Claude record differs")
	}
	if after := indexRows(idle.session); after != beforeIndex {
		t.Fatal("rebuilt Claude index differs after freezing")
	}

	// Deleting the session cascades its rows; collection removes the objects
	// only after the grace period.
	if _, err := pool.Exec(ctx, `DELETE FROM mira_claude_sessions WHERE session_id=$1`, idle.session); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_cold_objects SET created_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := freezer.Collect(ctx); err != nil || deleted != 0 {
		t.Fatalf("collected before grace: %d %v", deleted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE mira_cold_objects SET orphaned_at=now()-interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := freezer.Collect(ctx); err != nil || deleted != objects.count() && objects.count() != 0 {
		t.Fatalf("collect: %d %v objects=%d", deleted, err, objects.count())
	}
	if objects.count() != 0 {
		t.Fatalf("objects left: %d", objects.count())
	}
}
