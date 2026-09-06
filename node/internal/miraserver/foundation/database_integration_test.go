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
	if err := InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
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
