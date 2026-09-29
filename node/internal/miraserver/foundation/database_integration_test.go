package foundation

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestInitializeDatabaseIntegration(t *testing.T) {
	databaseURL := os.Getenv("MIRA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("MIRA_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := InitializeDatabase(ctx, pool, CurrentSchemaVersion()-1); err != nil {
		t.Fatal(err)
	}
	var expandedFrom int
	if err := pool.QueryRow(ctx, `SELECT MAX(version) FROM codex_schema_migrations`).Scan(&expandedFrom); err != nil || expandedFrom != CurrentSchemaVersion()-1 {
		t.Fatalf("pre-upgrade schema=%d err=%v", expandedFrom, err)
	}
	// An old Server writes only state. Existing and future rows must acquire
	// the derived scalar without changing that rollback SQL contract.
	const store = "cost-source-upgrade-fixture"
	defer func() {
		if _, err := pool.Exec(ctx, `DELETE FROM codex_thread_projections WHERE store_id=$1`, store); err != nil {
			t.Error(err)
		}
	}()
	if _, err := pool.Exec(ctx, `INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq)
 VALUES($1,'fork',1,0,'{"createdThread":{"forked_from_id":"parent"}}',1)`, store); err != nil {
		t.Fatal(err)
	}
	if err := InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	checkFork := func(want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, `SELECT cost_forked_from_id FROM codex_thread_projections WHERE store_id=$1 AND thread_id='fork'`, store).Scan(&got); err != nil || got != want {
			t.Fatalf("derived fork=%q want=%q err=%v", got, want, err)
		}
	}
	checkFork("parent")
	for _, state := range []string{`{"createdThread":{"forked_from_id":"replacement"}}`, `{}`} {
		if _, err := pool.Exec(ctx, `UPDATE codex_thread_projections SET state=$2::jsonb WHERE store_id=$1`, store, state); err != nil {
			t.Fatal(err)
		}
		if state == "{}" {
			checkFork("")
		} else {
			checkFork("replacement")
		}
	}
	// Re-entry validates every immutable name/checksum without rerunning SQL.
	if err := InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var version, count int
	if err := pool.QueryRow(ctx, `SELECT MAX(version), COUNT(*) FROM codex_schema_migrations`).Scan(&version, &count); err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() || count != CurrentSchemaVersion() {
		t.Fatalf("migration ledger has max=%d count=%d, want %d", version, count, CurrentSchemaVersion())
	}
	var importConstraints int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM pg_constraint WHERE conrelid='mira_codex_session_imports'::regclass
		AND conname IN ('mira_codex_session_imports_source_node_id_source_path_sourc_key','mira_codex_session_imports_store_source_unique')`).Scan(&importConstraints); err != nil || importConstraints != 2 {
		t.Fatalf("expand migration did not retain rollback compatibility: constraints=%d err=%v", importConstraints, err)
	}
}
