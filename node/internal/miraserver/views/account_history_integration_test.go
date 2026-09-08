package views

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

func TestAccountHistorySurvivesRuntimeUpgrade(t *testing.T) {
	url := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id := "17000000-0000-4000-8000-000000000019"
	_, err = pool.Exec(ctx, `INSERT INTO codex_nodes
 (node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
 VALUES($1::uuid,$1::text,'history-test','linux','amd64','linux','test','{}','[]')`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pool.Exec(ctx, "DELETE FROM mira_account_quota_samples WHERE node_id=$1", id)
		pool.Exec(ctx, "DELETE FROM codex_nodes WHERE node_id=$1", id)
	}()
	now := time.Now().UTC()
	node := &nodes.Node{NodeID: id, ReportedAppServer: map[string]any{"codexPath": "/runtimes/new/codex"}}
	for i, sample := range []struct{ key, email string }{
		{"old-runtime", "same@example.test"}, {"old-runtime", "other@example.test"},
		{RuntimeKey(node), "same@example.test"},
	} {
		at := now.Add(time.Duration(i-3) * time.Hour)
		_, err := pool.Exec(ctx, `INSERT INTO mira_account_quota_samples
  (node_id,runtime_key,sampled_at,sample_slot,status,account_type,email,remaining)
  VALUES($1,$2,$3,$4,'ok','chatgpt',$5,80)`, id, sample.key, at, at.Unix()/300, sample.email)
		if err != nil {
			t.Fatal(err)
		}
	}
	service := New(pool)
	for _, name := range []string{"24h", "7d", "30d"} {
		history, err := service.AccountHistory(ctx, node, name, now)
		if err != nil {
			t.Fatal(err)
		}
		points := history["points"].([]map[string]any)
		if len(points) != 3 || points[0]["remaining"] != float64(80) || points[1]["remaining"] != nil || points[2]["remaining"] != float64(80) {
			t.Fatalf("%s: expected old and new account samples with other-account gap, got %#v", name, points)
		}
	}
	// A further upgrade before its first sample must not make history empty.
	node.ReportedAppServer["codexPath"] = "/runtimes/next/codex"
	history, err := service.AccountHistory(ctx, node, "7d", now)
	if err != nil || history["account"] == nil || len(history["points"].([]map[string]any)) != 3 {
		t.Fatalf("pre-sample upgrade: %#v %v", history, err)
	}
}
