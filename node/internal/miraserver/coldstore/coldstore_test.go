package coldstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type memoryObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    atomic.Int64
	fail    error
}

func newMemoryObjects() *memoryObjects { return &memoryObjects{objects: map[string][]byte{}} }

func (store *memoryObjects) Put(_ context.Context, key string, body []byte) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail != nil {
		return store.fail
	}
	store.objects[key] = append([]byte(nil), body...)
	return nil
}

func (store *memoryObjects) Get(_ context.Context, key string) ([]byte, error) {
	store.gets.Add(1)
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.fail != nil {
		return nil, store.fail
	}
	body, ok := store.objects[key]
	if !ok {
		return nil, ErrObjectNotFound
	}
	return append([]byte(nil), body...), nil
}

func (store *memoryObjects) Delete(_ context.Context, key string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.objects, key)
	return nil
}

func TestFreezableKeepsMetadataHot(t *testing.T) {
	big := strings.Repeat("x", 1000)
	cases := []struct {
		name   string
		record string
		freeze bool
	}{
		{"tool output", `{"timestamp":"2026-10-01T00:00:00Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c","output":"` + big + `"}}`, true},
		{"function output", `{"type":"response_item","payload":{"type":"function_call_output","output":"` + big + `"}}`, true},
		{"reasoning", `{"type":"response_item","payload":{"type":"reasoning","summary":[]}}`, true},
		{"user message", `{"type":"response_item","payload":{"type":"message","role":"user","content":[]}}`, true},
		{"assistant message", `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[]}}`, false},
		{"compacted", `{"type":"compacted","payload":{"message":"summary","replacement_history":[]}}`, true},
		{"exec end", `{"type":"event_msg","payload":{"type":"exec_command_end","stdout":"` + big + `"}}`, true},
		{"agent message", `{"type":"event_msg","payload":{"type":"agent_message","message":"hi"}}`, false},
		{"token count", `{"type":"event_msg","payload":{"type":"token_count","info":{}}}`, false},
		{"session meta", `{"type":"session_meta","payload":{"id":"t"}}`, false},
		{"turn context", `{"type":"turn_context","payload":{"model":"m"}}`, false},
		{"output mentioning lifecycle", `{"type":"response_item","payload":{"type":"function_call_output","output":"{\"type\": \"task_complete\"}","x":{"type":"task_complete"}}}`, false},
		{"not json", `{"type":`, false},
	}
	for _, item := range cases {
		stub, sha, ok := Freezable([]byte(item.record))
		if ok != item.freeze {
			t.Fatalf("%s: freeze=%v want %v", item.name, ok, item.freeze)
		}
		if !ok {
			continue
		}
		if sha != Digest([]byte(item.record)) {
			t.Fatalf("%s: wrong content address", item.name)
		}
		var original, placeholder map[string]any
		_ = json.Unmarshal([]byte(item.record), &original)
		if err := json.Unmarshal(stub, &placeholder); err != nil {
			t.Fatalf("%s: placeholder is not JSON: %v", item.name, err)
		}
		if placeholder["type"] != original["type"] {
			t.Fatalf("%s: placeholder type %v", item.name, placeholder["type"])
		}
		originalPayload, _ := original["payload"].(map[string]any)
		payload, _ := placeholder["payload"].(map[string]any)
		for _, key := range []string{"type", "role"} {
			if originalPayload[key] != nil && payload[key] != originalPayload[key] {
				t.Fatalf("%s: placeholder lost payload.%s", item.name, key)
			}
		}
		if original["timestamp"] != nil && placeholder["timestamp"] != original["timestamp"] {
			t.Fatalf("%s: placeholder lost timestamp", item.name)
		}
		if len(stub) > 400 {
			t.Fatalf("%s: placeholder is %d bytes", item.name, len(stub))
		}
	}
}

func TestEncodeDecodeVerifiesContent(t *testing.T) {
	raw := []byte(`{"type":"response_item","payload":{"type":"function_call_output","output":"\u0000 ü"}}`)
	encoded, err := Encode(raw)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(encoded, Digest(raw))
	if err != nil || string(decoded) != string(raw) {
		t.Fatalf("round trip: %q %v", decoded, err)
	}
	if _, err := Decode(encoded, Digest([]byte("other"))); err == nil {
		t.Fatal("wrong content address accepted")
	}
}

func TestStoreCachesAndCoalescesDownloads(t *testing.T) {
	objects := newMemoryObjects()
	raw := []byte(`{"type":"compacted","payload":{"message":"` + strings.Repeat("y", 5000) + `"}}`)
	sha := Digest(raw)
	encoded, _ := Encode(raw)
	_ = objects.Put(context.Background(), ObjectKey(sha), encoded)
	store, err := New(objects, Options{CacheDir: t.TempDir(), CacheBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	SetDefault(store)
	defer SetDefault(nil)
	var group sync.WaitGroup
	for range 16 {
		group.Add(1)
		go func() {
			defer group.Done()
			value, err := Hydrate(context.Background(), []byte("{}"), &sha)
			if err != nil || string(value) != string(raw) {
				t.Errorf("hydrate: %v", err)
			}
		}()
	}
	group.Wait()
	if err := Prefetch(context.Background(), []string{sha, sha}); err != nil {
		t.Fatal(err)
	}
	if gets := objects.gets.Load(); gets != 1 {
		t.Fatalf("downloads: %d", gets)
	}
	// A cache file with different content is discarded, not served.
	if err := writeFile(store.cachePath(sha), []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if value, err := store.Payload(context.Background(), sha); err != nil || string(value) != string(raw) {
		t.Fatalf("tampered cache: %v", err)
	}
	if objects.gets.Load() != 2 {
		t.Fatal("tampered cache was served")
	}
}

func TestHydrateFailsExplicitly(t *testing.T) {
	SetDefault(nil)
	raw := []byte(`{"a":1}`)
	if value, err := Hydrate(context.Background(), raw, nil); err != nil || string(value) != string(raw) {
		t.Fatal("hot row changed")
	}
	sha := Digest(raw)
	if _, err := Hydrate(context.Background(), []byte("{}"), &sha); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("missing store: %v", err)
	}
	objects := newMemoryObjects()
	store, _ := New(objects, Options{})
	SetDefault(store)
	defer SetDefault(nil)
	if _, err := Hydrate(context.Background(), []byte("{}"), &sha); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("missing object: %v", err)
	}
	encoded, _ := Encode([]byte(`{"a":2}`))
	_ = objects.Put(context.Background(), ObjectKey(sha), encoded)
	if _, err := Hydrate(context.Background(), []byte("{}"), &sha); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("corrupt object: %v", err)
	}
}

func TestS3Escape(t *testing.T) {
	if got := s3Escape("/a b+c/ü~.-_"); got != "/a%20b%2Bc/%C3%BC~.-_" {
		t.Fatal(got)
	}
}

func writeFile(path string, body []byte) error { return os.WriteFile(path, body, 0o600) }
