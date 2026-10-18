package miraserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

type discardStoreResponse struct {
	header http.Header
	bytes  int64
}

func (w *discardStoreResponse) Header() http.Header { return w.header }
func (w *discardStoreResponse) WriteHeader(int)     {}
func (w *discardStoreResponse) Write(p []byte) (int, error) {
	w.bytes += int64(len(p))
	return len(p), nil
}

// Opt-in, synthetic capacity experiment; never use a production dump. Heap
// sampling and in-process output measure materialization, not WAN latency.
func TestStoreReadCapacity(t *testing.T) {
	if os.Getenv("MIRA_STREAM_CAPACITY_TEST") != "1" {
		t.Skip("set MIRA_STREAM_CAPACITY_TEST=1 for the synthetic 225 MiB directory")
	}
	pool, store, thread, _ := streamFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO codex_store_state_entries(store_id,field,entry_key,is_root,thread_id,value)
	 SELECT $1,'created_threads','synthetic-'||i,false,'synthetic-'||i,
	 json_build_object('thread_id','synthetic-'||i,'base_instructions',repeat('x',32768))
	 FROM generate_series(1,7200) i`, store); err != nil {
		t.Fatal(err)
	}
	measure := func(name string, read func(http.ResponseWriter) error) {
		t.Helper()
		pool.Reset() // Do not retain the legacy aggregate in pgx connection buffers.
		runtime.GC()
		runtime.GC() // Clear encoding/json sync.Pool buffers from the preceding run.
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		var peak atomic.Uint64
		peak.Store(before.HeapAlloc)
		done, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					var stats runtime.MemStats
					runtime.ReadMemStats(&stats)
					if stats.HeapAlloc > peak.Load() {
						peak.Store(stats.HeapAlloc)
					}
				}
			}
		}()
		response := &discardStoreResponse{header: http.Header{}}
		start := time.Now()
		err := read(response)
		elapsed := time.Since(start)
		close(done)
		<-stopped
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %.3fs, output %.1f MiB, allocated %.1f MiB, sampled heap peak %.1f MiB (baseline %.1f MiB)",
			name, elapsed.Seconds(), float64(response.bytes)/(1<<20), float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), float64(peak.Load())/(1<<20), float64(before.HeapAlloc)/(1<<20))
	}
	measure("head materialized", func(w http.ResponseWriter) error {
		value, err := GetStoreHead(ctx, pool, store, nil)
		if err != nil {
			return err
		}
		return writeJSON(w, 200, value)
	})
	measure("head streamed", func(w http.ResponseWriter) error {
		return streamStoreHead(ctx, pool, w, store, nil)
	})
	if _, err := pool.Exec(ctx, `INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,operation_id,payload,payload_sha256,event_format_version)
	 SELECT store_id,thread_id,generation,i,operation_id,json_build_object('type','future','payload',repeat('y',1048576)),'fixture',event_format_version
	 FROM codex_thread_events CROSS JOIN generate_series(3,130) i WHERE store_id=$1 AND thread_id=$2 AND item_seq=1`, store, thread); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE codex_thread_revisions SET item_count=130 WHERE store_id=$1`, store); err != nil {
		t.Fatal(err)
	}
	measure("history materialized", func(w http.ResponseWriter) error {
		result, err := GetThreadHistory(ctx, pool, store, thread, nil, nil)
		if err != nil {
			return err
		}
		return writeJSON(w, result.Status, result.Body)
	})
	measure("history streamed", func(w http.ResponseWriter) error {
		return (&Server{pool: pool}).streamThreadHistory(ctx, w, httptest.NewRequest("GET", "/", nil), store, thread, nil, nil)
	})
}
