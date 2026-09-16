package views

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type costPageTrace struct {
	mu       sync.Mutex
	after    []int64
	cancelAt int
	cancel   context.CancelFunc
}

func (trace *costPageTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT events.item_seq::text,events.payload") {
		trace.mu.Lock()
		defer trace.mu.Unlock()
		trace.after = append(trace.after, data.Args[3].(int64))
		if len(trace.after) == trace.cancelAt {
			trace.cancel()
		}
	}
	return ctx
}

func (*costPageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresCostProgressAndCheckpointBoundaries(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
	}
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	config, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	trace := &costPageTrace{}
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := "cost-progress-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, table := range []string{"codex_thread_events", "codex_thread_projections", "codex_store_events", "codex_store_heads"} {
			if _, err := pool.Exec(cleanup, "DELETE FROM "+table+" WHERE store_id=$1", store); err != nil {
				t.Error(err)
			}
		}
	})
	exec(`INSERT INTO codex_store_heads(store_id,version,history_floor) VALUES($1,1,0)`, store)
	exec(`INSERT INTO codex_store_events(store_id,operation_id,event_seq,previous_event_seq,result_version,appended_item_count)
 VALUES($1,'329e5c70-1436-4100-a71b-b4619f280b78',1,0,1,1201)`, store)
	thread := Thread{ThreadID: "root", Generation: 1, ItemCount: 1201}
	exec(`INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq)
 VALUES($1,'root',1,1201,'{}',1)`, store)
	records := []string{}
	for i := range 1201 {
		item := contextRecord("gpt-6-astra", "turn")
		if i > 0 {
			item = usageRecord(usage(int64(i)*100000, int64(i)*80000, int64(i)*1000), usage(100000, 80000, 1000), "turn")
		}
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, string(raw))
	}
	exec(`INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,event_format_version,payload,payload_sha256,operation_id)
 SELECT $1,'root',1,seq,1,raw::json,'fixture','329e5c70-1436-4100-a71b-b4619f280b78'
 FROM unnest($2::text[]) WITH ORDINALITY AS r(raw,seq)`, store, records)
	service := New(pool)
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	trace.cancelAt, trace.cancel = 2, cancel
	if _, err := service.costProjection(interrupted, store, thread); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation on second page: %v", err)
	}
	entry, ok := service.cachedCostProjection(costProjectionKey(store, thread, false))
	if !ok || entry.itemCount != 256 || entry.state.priced != 255 {
		t.Fatalf("lost completed page or cached incomplete progress: %#v", entry)
	}
	state, err := service.costProjection(ctx, store, thread)
	if err != nil || state.priced != 1200 || !closeFloat(totalDollars(&state.costTotals), 396) {
		t.Fatalf("resumed cost duplicated or omitted requests: %#v %v", state, err)
	}
	if !reflect.DeepEqual(trace.after, []int64{0, 256, 256, 512, 768, 1024}) {
		t.Fatalf("retry rescanned completed history: %v", trace.after)
	}
	// One background page is enough to seed a cold statistics request. Only
	// its unprojected tail may be scanned; the response must still be complete.
	source := accountCostSource{store: store, thread: "root", generation: 1, count: 1201, key: `["", null]`}
	if err := service.projectAccountCostPage(ctx, source); err != nil {
		t.Fatal(err)
	}
	service = New(pool)
	trace.after = nil
	trace.cancelAt = 0
	statistics, err := service.GetThreadStatistics(ctx, store, thread)
	if err != nil || statistics.CostEstimate["status"] != "complete" || !closeFloat(statistics.CostEstimate["amount"].(float64), 396) {
		t.Fatalf("partial checkpoint did not catch up: %#v %v", statistics, err)
	}
	if !reflect.DeepEqual(trace.after, []int64{1024}) {
		t.Fatalf("checkpoint tail read: %v", trace.after)
	}
	if err := service.projectAccountCostPage(ctx, source); err != nil {
		t.Fatal(err)
	}
	// Wrong generation, parser/prices, fork/import source, or a cursor ahead
	// of the requested snapshot must never seed the statistics cache.
	for _, change := range []string{"generation=2", "revision='obsolete'", "source_key='obsolete'", "item_seq=1202"} {
		exec("UPDATE mira_account_cost_checkpoints SET "+change+" WHERE store_id=$1", store)
		service = New(pool)
		if err := service.loadStatisticsCheckpoints(ctx, store, []Thread{thread}); err != nil {
			t.Fatal(err)
		}
		if _, ok := service.cachedCostProjection(costProjectionKey(store, thread, true)); ok {
			t.Fatalf("reused incompatible checkpoint: %s", change)
		}
		exec(`UPDATE mira_account_cost_checkpoints SET generation=1,revision=$2,source_key=$3,item_seq=1201 WHERE store_id=$1`, store, accountCostRevision, source.key)
	}
	for _, snapshot := range []Thread{
		{ThreadID: "root", Generation: 2, ItemCount: 1201},
		{ThreadID: "root", Generation: 1, ItemCount: 1200},
		{ThreadID: "root", Generation: 1, ItemCount: 1201, ForkedFromID: &store},
	} {
		service = New(pool)
		if err := service.loadStatisticsCheckpoints(ctx, store, []Thread{snapshot}); err != nil {
			t.Fatal(err)
		}
		if _, ok := service.cachedCostProjection(costProjectionKey(store, snapshot, true)); ok {
			t.Fatalf("reused checkpoint from a different snapshot: %#v", snapshot)
		}
	}
	service = New(pool)
	trace.after = nil
	statistics, err = service.GetThreadStatistics(ctx, store, thread)
	if err != nil || statistics.CostEstimate["pricedRequests"] != int64(1200) || len(trace.after) != 0 {
		t.Fatalf("complete checkpoint should need no history reads: %#v %v %v", statistics, trace.after, err)
	}
}
