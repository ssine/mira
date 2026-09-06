package accountsampler

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type fakeAccountReader struct {
	read func(context.Context, string) (channel.AccountSnapshot, error)
}

func (reader fakeAccountReader) Read(ctx context.Context, nodeID string) (channel.AccountSnapshot, error) {
	return reader.read(ctx, nodeID)
}

func TestSampleIntegration(t *testing.T) {
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
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	nodeID := testUUID(t)
	_, err = pool.Exec(ctx, `INSERT INTO codex_nodes
		(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,
		 codex_installations,approval_status,approved_at,channel_status,reported_app_server)
		VALUES($1::uuid,$1::text,'quota-go-test','linux','amd64','linux','test',
		 '{"appServer":true}','[]','approved',NOW(),'{"connected":true}',
		 '{"status":"running","codexHome":"/test"}')`, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM mira_account_quota_samples WHERE node_id=$1", nodeID)
		_, _ = pool.Exec(context.Background(), "DELETE FROM codex_nodes WHERE node_id=$1", nodeID)
	}()

	registry := nodes.New(pool, nodes.Options{})
	node, err := registry.Get(ctx, nodeID, false)
	if err != nil || node == nil {
		t.Fatalf("load test Node: node=%v err=%v", node, err)
	}
	connected := fakeConnectivity{nodeID: true}
	var reads atomic.Int32
	reader := fakeAccountReader{read: func(context.Context, string) (channel.AccountSnapshot, error) {
		reads.Add(1)
		return channel.AccountSnapshot{
			Account: map[string]any{
				"type": "chatgpt", "email": "quota@example.test", "planType": "pro", "token": "never-persist",
			},
			Limits: map[string]any{
				"rateLimits": map[string]any{"secondary": map[string]any{
					"windowDurationMins": 10080, "usedPercent": 10, "resetsAt": 2_000_000_000,
				}},
				"rateLimitResetCredits": map[string]any{"availableCount": 0},
			},
		}, nil
	}}
	now := time.Now().UTC().Truncate(time.Millisecond)
	sampler := New(pool, registry, connected, reader, Options{Now: func() time.Time { return now }})
	results := make(chan bool, 3)
	errorsFound := make(chan error, 3)
	for range 3 {
		go func() {
			inserted, err := sampler.Sample(ctx, node)
			results <- inserted
			errorsFound <- err
		}()
	}
	inserted := 0
	for range 3 {
		if err := <-errorsFound; err != nil {
			t.Fatal(err)
		}
		if <-results {
			inserted++
		}
	}
	if inserted != 1 || reads.Load() != 1 {
		t.Fatalf("concurrent samples inserted=%d reads=%d, want 1 and 1", inserted, reads.Load())
	}
	if again, err := sampler.Sample(ctx, node); err != nil || again {
		t.Fatalf("durable five-minute deduplication returned inserted=%v err=%v", again, err)
	}

	var status string
	var remaining float64
	var resetCount int64
	var persisted string
	err = pool.QueryRow(ctx, `SELECT status,remaining,reset_count,row_to_json(sample)::text
		FROM mira_account_quota_samples sample WHERE node_id=$1`, nodeID,
	).Scan(&status, &remaining, &resetCount, &persisted)
	if err != nil {
		t.Fatal(err)
	}
	if status != "ok" || remaining != 90 || resetCount != 0 {
		t.Fatalf("stored status=%q remaining=%v resetCount=%d", status, remaining, resetCount)
	}
	if containsAny(persisted, "never-persist", "token") {
		t.Fatalf("stored sample contains a sensitive account field: %s", persisted)
	}

	// A response from a replaced runtime must not be stored under the old key.
	now = now.Add(SampleInterval)
	started := make(chan struct{})
	release := make(chan struct{})
	sampler.accounts = fakeAccountReader{read: func(context.Context, string) (channel.AccountSnapshot, error) {
		close(started)
		<-release
		return channel.AccountSnapshot{Account: map[string]any{"type": "chatgpt"}}, nil
	}}
	pending := make(chan bool)
	pendingError := make(chan error)
	go func() {
		value, err := sampler.Sample(ctx, node)
		pending <- value
		pendingError <- err
	}()
	<-started
	if _, err := pool.Exec(ctx, `UPDATE codex_nodes
		SET reported_app_server='{"status":"running","codexHome":"/replacement"}' WHERE node_id=$1`, nodeID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if value, err := <-pending, <-pendingError; err != nil || value {
		t.Fatalf("replaced runtime returned inserted=%v err=%v", value, err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM mira_account_quota_samples WHERE node_id=$1", nodeID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("replaced runtime left %d samples, want 1", count)
	}
}

func testUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatal(err)
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if len(needle) > 0 {
			for index := 0; index+len(needle) <= len(value); index++ {
				if value[index:index+len(needle)] == needle {
					return true
				}
			}
		}
	}
	return false
}
