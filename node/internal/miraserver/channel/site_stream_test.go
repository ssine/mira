package channel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestSiteStreamSharedBudgetAllowsOneNodeBeyond32(t *testing.T) {
	ch := &Channel{siteStreamBudget: 40, nodeSockets: map[string]*socket{"node": {}}}
	defer ch.CloseSiteStreams("", "")
	for i := 0; i < 40; i++ {
		if _, err := ch.reserveSiteStream(context.Background(), fmt.Sprint(i), "site", "node"); err != nil {
			t.Fatalf("stream %d was limited by a Node/site quota: %v", i, err)
		}
	}
	if ch.HasSiteCapacity() {
		t.Fatal("shared budget was exceeded")
	}
	_, err := ch.reserveSiteStream(context.Background(), "overflow", "site", "node")
	var capacity *Error
	if !errors.As(err, &capacity) || capacity.Code != "site_busy" {
		t.Fatalf("unexpected exhaustion result: %v", err)
	}
}

func TestSiteStreamReclaimsBeforeAdmissionAndRetainsActiveStreams(t *testing.T) {
	ch := &Channel{siteStreamBudget: 2, nodeSockets: map[string]*socket{"a": {}, "b": {}}}
	defer ch.CloseSiteStreams("", "")
	active, _ := ch.reserveSiteStream(context.Background(), "active", "active-site", "a")
	idle, _ := ch.reserveSiteStream(context.Background(), "idle", "idle-site", "a")
	ch.SetSiteIdleReclaimer(func() { ch.CloseSiteStreams("", "idle-site") })
	if _, err := ch.reserveSiteStream(context.Background(), "replacement", "new-site", "b"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-idle.done:
	default:
		t.Fatal("idle stream was not reclaimed")
	}
	select {
	case <-active.done:
		t.Fatal("active stream was reclaimed")
	default:
	}
	_, err := ch.reserveSiteStream(context.Background(), "overflow", "new-site", "b")
	if err == nil || len(ch.siteStreams) != 2 {
		t.Fatal("all-active budget was bypassed")
	}
}

func TestSiteStreamConcurrentAdmissionKeepsSharedBudget(t *testing.T) {
	ch := &Channel{siteStreamBudget: 8, nodeSockets: map[string]*socket{"node": {}}}
	defer ch.CloseSiteStreams("", "")
	ch.SetSiteIdleReclaimer(func() {})
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Go(func() { _, _ = ch.reserveSiteStream(context.Background(), fmt.Sprint(i), "site", "node") })
	}
	wg.Wait()
	if len(ch.siteStreams) != 8 {
		t.Fatalf("concurrent admission used %d streams", len(ch.siteStreams))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ch.reserveSiteStream(ctx, "cancelled", "site", "node"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reservation: %v", err)
	}
}
