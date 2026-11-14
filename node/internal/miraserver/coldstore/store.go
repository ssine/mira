// Package coldstore keeps large, immutable Codex history payloads in an
// S3-compatible bucket while PostgreSQL retains every history row, its
// metadata, ordering and a small typed placeholder. Readers that need the
// canonical body call Hydrate; it returns exactly the original JSON bytes or
// an explicit error, never a partial record.
package coldstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ErrUnavailable is returned when a frozen row is read but no cold store is
// configured in this Server process.
var ErrUnavailable = errors.New("cold history storage is not configured on this Server")

// Store hydrates frozen payloads. It is safe for concurrent use.
type Store struct {
	objects    ObjectStore
	cacheDir   string
	cacheBytes int64
	flights    sync.Map // sha -> *flight
	sweeping   atomic.Bool
	fetches    atomic.Int64
}

type flight struct {
	done  chan struct{}
	value []byte
	err   error
}

type Options struct {
	CacheDir   string // empty disables the disk cache
	CacheBytes int64
}

func New(objects ObjectStore, options Options) (*Store, error) {
	if objects == nil {
		return nil, errors.New("cold store requires an object store")
	}
	if options.CacheDir != "" {
		if !filepath.IsAbs(options.CacheDir) {
			return nil, errors.New("cold store cache directory must be absolute")
		}
		if err := os.MkdirAll(options.CacheDir, 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{objects: objects, cacheDir: options.CacheDir, cacheBytes: options.CacheBytes}, nil
}

// maxPayloadBytes bounds one decompressed canonical payload.
const maxPayloadBytes = 1 << 30

var active atomic.Pointer[Store]

// SetDefault installs the process-wide store used by Hydrate. One Mira Server
// worker owns one database, so a single store per process is sufficient.
func SetDefault(store *Store) { active.Store(store) }

func Default() *Store { return active.Load() }

// ObjectKey is the content address of one canonical payload.
func ObjectKey(sha string) string { return "payloads/" + sha[:2] + "/" + sha + ".json.gz" }

// Hydrate returns the canonical payload for a history row. Rows that were
// never frozen are returned unchanged.
func Hydrate(ctx context.Context, raw []byte, coldSHA *string) ([]byte, error) {
	if coldSHA == nil || *coldSHA == "" {
		return raw, nil
	}
	store := Default()
	if store == nil {
		return nil, ErrUnavailable
	}
	return store.Payload(ctx, *coldSHA)
}

// Prefetch warms the cache for the given content addresses with bounded
// concurrency, so a following ordered read does not wait for each object.
func Prefetch(ctx context.Context, shas []string) error {
	if len(shas) == 0 {
		return nil
	}
	store := Default()
	if store == nil {
		return ErrUnavailable
	}
	unique := map[string]bool{}
	for _, sha := range shas {
		unique[sha] = true
	}
	work := make(chan string)
	errs := make(chan error, 1)
	var group sync.WaitGroup
	for range min(8, len(unique)) {
		group.Add(1)
		go func() {
			defer group.Done()
			for sha := range work {
				if _, err := store.Payload(ctx, sha); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}
feed:
	for sha := range unique {
		select {
		case work <- sha:
		case <-ctx.Done():
			break feed
		case err := <-errs:
			errs <- err
			break feed
		}
	}
	close(work)
	group.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	return ctx.Err()
}

// Payload returns the verified canonical bytes for one content address.
func (store *Store) Payload(ctx context.Context, sha string) ([]byte, error) {
	if !validSHA(sha) {
		return nil, fmt.Errorf("invalid cold payload address")
	}
	if value, ok := store.cached(sha); ok {
		return value, nil
	}
	created := &flight{done: make(chan struct{})}
	existing, loaded := store.flights.LoadOrStore(sha, created)
	current := existing.(*flight)
	if !loaded {
		// Other readers may share this download; one caller's cancellation
		// must not fail theirs.
		fetchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		current.value, current.err = store.fetch(fetchContext, sha)
		cancel()
		store.flights.Delete(sha)
		close(current.done)
	}
	select {
	case <-current.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if current.err != nil {
		return nil, current.err
	}
	return current.value, nil
}

func (store *Store) fetch(ctx context.Context, sha string) ([]byte, error) {
	store.fetches.Add(1)
	compressed, err := store.objects.Get(ctx, ObjectKey(sha))
	if err != nil {
		return nil, fmt.Errorf("read cold history payload %s: %w", sha[:12], err)
	}
	value, err := Decode(compressed, sha)
	if err != nil {
		return nil, err
	}
	store.remember(sha, value)
	return value, nil
}

// Encode compresses one canonical payload for storage.
func Encode(raw []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// Decode decompresses an object and verifies its content address.
func Decode(compressed []byte, sha string) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("decode cold history payload %s: %w", sha[:12], err)
	}
	value, err := io.ReadAll(io.LimitReader(reader, maxPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decode cold history payload %s: %w", sha[:12], err)
	}
	if len(value) > maxPayloadBytes {
		return nil, fmt.Errorf("cold history payload %s exceeds %d bytes", sha[:12], maxPayloadBytes)
	}
	if Digest(value) != sha {
		return nil, fmt.Errorf("cold history payload %s failed integrity verification", sha[:12])
	}
	return value, nil
}

// Digest is the content address of exact payload bytes.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validSHA(sha string) bool {
	if len(sha) != 64 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

func (store *Store) cachePath(sha string) string {
	return filepath.Join(store.cacheDir, sha[:2], sha+".json")
}

func (store *Store) cached(sha string) ([]byte, bool) {
	if store.cacheDir == "" {
		return nil, false
	}
	path := store.cachePath(sha)
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	if Digest(value) != sha {
		_ = os.Remove(path)
		return nil, false
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	return value, true
}

func (store *Store) remember(sha string, value []byte) {
	if store.cacheDir == "" || store.cacheBytes <= 0 || int64(len(value)) > store.cacheBytes/4 {
		return
	}
	path := store.cachePath(sha)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".fill-*")
	if err != nil {
		return
	}
	_, writeErr := temporary.Write(value)
	closeErr := temporary.Close()
	if writeErr != nil || closeErr != nil || os.Rename(temporary.Name(), path) != nil {
		_ = os.Remove(temporary.Name())
		return
	}
	if store.sweeping.CompareAndSwap(false, true) {
		go func() {
			defer store.sweeping.Store(false)
			store.sweep()
		}()
	}
}

// sweep removes least recently used cache files until the cache fits.
func (store *Store) sweep() {
	type entry struct {
		path string
		size int64
		used time.Time
	}
	entries := []entry{}
	total := int64(0)
	_ = filepath.WalkDir(store.cacheDir, func(path string, item os.DirEntry, err error) error {
		if err != nil || item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return nil
		}
		entries = append(entries, entry{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if total <= store.cacheBytes {
		return
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].used.Before(entries[right].used) })
	for _, item := range entries {
		if total <= store.cacheBytes*9/10 {
			break
		}
		if os.Remove(item.path) == nil {
			total -= item.size
		}
	}
}
