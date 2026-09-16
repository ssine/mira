package views

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

type heldCostReads struct {
	started chan struct{}
	release chan struct{}
}

func (trace *heldCostReads) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT events.item_seq::text,events.payload") {
		trace.started <- struct{}{}
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*heldCostReads) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestPostgresCostReadsLeaveCapacityForLiveRequests(t *testing.T) {
	endpoint := os.Getenv("MIRA_VIEWS_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("set MIRA_VIEWS_TEST_DATABASE_URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	trace := &heldCostReads{started: make(chan struct{}, 3), release: make(chan struct{})}
	config.MaxConns, config.ConnConfig.Tracer = 3, trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	defer close(trace.release)
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	service := New(pool)
	thread := Thread{ThreadID: "cost-capacity-fixture", Generation: 1, ItemCount: 1}
	done := make(chan error, 2)
	for range 2 {
		go func() {
			done <- service.applyCostRows(ctx, "cost-capacity-fixture", thread, NewCostProjection(false, nil), 0)
		}()
	}
	for range 2 {
		select {
		case <-trace.started:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// Both history readers retain their connections while ordinary requests
	// still get a connection. A third cost request can cancel without entering SQL.
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	waiting, stop := context.WithCancel(ctx)
	stop()
	if err := service.applyCostRows(waiting, "cost-capacity-fixture", thread, NewCostProjection(false, nil), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled queued cost read: %v", err)
	}
	if len(trace.started) != 0 {
		t.Fatal("queued cost read acquired a database connection")
	}
	// A cached projection needs no history slot, even while both are occupied.
	if err := service.applyCostRows(ctx, "cost-capacity-fixture", thread, NewCostProjection(false, nil), 1); err != nil {
		t.Fatal(err)
	}
	trace.release <- struct{}{}
	trace.release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}
