package coldstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// TestLiveObjectStore exercises a real S3-compatible bucket. It runs only when
// MIRA_COLD_LIVE_ENDPOINT, _REGION, _BUCKET, _ACCESS_KEY_ID and
// _SECRET_ACCESS_KEY are set; MIRA_COLD_LIVE_PREFIX defaults to "livetest/".
func TestLiveObjectStore(t *testing.T) {
	endpoint := os.Getenv("MIRA_COLD_LIVE_ENDPOINT")
	if endpoint == "" {
		t.Skip("live object store not configured")
	}
	prefix := os.Getenv("MIRA_COLD_LIVE_PREFIX")
	if prefix == "" {
		prefix = "livetest/"
	}
	store, err := NewS3(ObjectConfig{
		Endpoint: endpoint, Region: os.Getenv("MIRA_COLD_LIVE_REGION"), Bucket: os.Getenv("MIRA_COLD_LIVE_BUCKET"),
		Prefix: prefix, AccessKeyID: os.Getenv("MIRA_COLD_LIVE_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("MIRA_COLD_LIVE_SECRET_ACCESS_KEY"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	key := "probe/" + hex.EncodeToString(random) + " +ü.bin"
	body := make([]byte, 300<<10)
	_, _ = rand.Read(body)
	if err := store.Put(ctx, key, body); err != nil {
		t.Fatal(err)
	}
	read, err := store.Get(ctx, key)
	if err != nil || !bytes.Equal(read, body) {
		t.Fatalf("read back mismatch: %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("deleted object still readable: %v", err)
	}
}
