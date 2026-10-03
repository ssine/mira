package node

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/resourcewire"
)

func previewRuntime(t *testing.T) (*capabilityRuntime, string) {
	t.Helper()
	root := t.TempDir()
	r, e := newCapabilityRuntime(config{AllowedRoots: []string{root}})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.close)
	return r, root
}
func TestFilePreviewRangesAndSiteRoot(t *testing.T) {
	r, root := previewRuntime(t)
	site := filepath.Join(root, "site")
	os.Mkdir(site, 0700)
	f := filepath.Join(site, "a.txt")
	os.WriteFile(f, []byte("0123456789"), 0600)
	w := httptest.NewRecorder()
	p := resourcewire.Request{Path: f, Root: site, Method: "GET", Headers: http.Header{"Range": []string{"bytes=2-5"}}}
	if e := r.serveFileContent(context.Background(), w, p); e != nil {
		t.Fatal(e)
	}
	if w.Code != 206 || w.Body.String() != "2345" || w.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	etag := w.Header().Get("ETag")
	p.Headers.Set("If-Range", `"changed"`)
	w = httptest.NewRecorder()
	r.serveFileContent(context.Background(), w, p)
	if w.Code != 200 || w.Body.Len() != 10 {
		t.Fatal("If-Range must send a fresh whole version")
	}
	p.Headers = http.Header{"If-None-Match": []string{etag}}
	w = httptest.NewRecorder()
	r.serveFileContent(context.Background(), w, p)
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	outside := filepath.Join(root, "private.txt")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(site, "leak.txt"))
	p.Path = filepath.Join(site, "leak.txt")
	if e := r.serveFileContent(context.Background(), httptest.NewRecorder(), p); e == nil {
		t.Fatal("site root symlink escaped")
	}
	p.Path = filepath.Join(root, "..", "outside")
	if e := r.serveFileContent(context.Background(), httptest.NewRecorder(), p); e == nil {
		t.Fatal("allowed roots escaped")
	}
}
func TestDirectoryPreviewOver10000Entries(t *testing.T) {
	r, root := previewRuntime(t)
	for i := 0; i < 10003; i++ {
		if e := os.WriteFile(filepath.Join(root, fmt.Sprintf("%05d", i)), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	cursor := 0
	seen := map[string]bool{}
	for {
		value, e := r.listFilePage(root, fileParams{Cursor: cursor, PageSize: 250})
		if e != nil {
			t.Fatal(e)
		}
		page := value.(map[string]any)
		entries := page["entries"].([]map[string]any)
		if len(entries) > 250 {
			t.Fatal("unbounded page")
		}
		for _, entry := range entries {
			p := entry["path"].(string)
			if seen[p] {
				t.Fatal("duplicate page entry")
			}
			seen[p] = true
		}
		cursor = page["nextCursor"].(int)
		if !page["hasMore"].(bool) {
			break
		}
	}
	if len(seen) != 10003 {
		t.Fatal(len(seen))
	}
}
func TestZIPPreviewVirtualTreeDuplicateAndRange(t *testing.T) {
	r, root := previewRuntime(t)
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	for _, name := range []string{"docs/a.md", "docs/img.png", "dup.txt", "dup.txt", "../escape.txt"} {
		f, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		f.Write([]byte("abcdefghij"))
	}
	z.Close()
	target := filepath.Join(root, "a.zip")
	os.WriteFile(target, data.Bytes(), 0600)
	result, e := r.archiveFile(target, fileParams{Action: "list", Archive: true, PageSize: 250})
	if e != nil {
		t.Fatal(e)
	}
	entries := result.(map[string]any)["entries"].([]map[string]any)
	if len(entries) != 3 || entries[0]["type"] != "directory" {
		t.Fatal(entries)
	}
	_, e = r.archiveFile(target, fileParams{Action: "stat", Archive: true, ArchiveEntry: "dup.txt"})
	if e == nil {
		t.Fatal("duplicate selected silently")
	}
	id := 2
	_, e = r.archiveFile(target, fileParams{Action: "stat", Archive: true, ArchiveEntry: "dup.txt", EntryID: &id})
	if e != nil {
		t.Fatal(e)
	}
	id = 4
	_, e = r.archiveFile(target, fileParams{Action: "read", Archive: true, ArchiveEntry: "../escape.txt", EntryID: &id})
	if e == nil {
		t.Fatal("unsafe ZIP entry accepted")
	}
	w := httptest.NewRecorder()
	p := resourcewire.Request{Path: target, Archive: true, ArchiveEntry: "docs/a.md", Root: "docs", Method: "GET", Headers: http.Header{"Range": []string{"bytes=-3"}}}
	if e = r.serveFileContent(context.Background(), w, p); e != nil {
		t.Fatal(e)
	}
	if w.Code != 206 || w.Body.String() != "hij" {
		t.Fatal(w.Code, w.Body.String())
	}
	if r.fileCacheBytes != 10 {
		t.Fatal("selected-entry cache missing")
	}
	r.closeZIPCache()
	if r.fileCacheBytes != 0 {
		t.Fatal("cache cleanup leaked")
	}
	p.ArchiveEntry = "dup.txt"
	if e = r.serveFileContent(context.Background(), httptest.NewRecorder(), p); e == nil {
		t.Fatal("ZIP root escaped")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = r.archiveFile(target, fileParams{Action: "read", ArchiveEntry: "docs/a.md", operationContext: ctx}); e == nil {
		t.Fatal("canceled archive capability read continued")
	}
	p.Root = ""
	p.ArchiveEntry = "docs/a.md"
	if e = r.serveFileContent(ctx, httptest.NewRecorder(), p); e == nil {
		t.Fatal("canceled preparation continued")
	}
}

func TestZIPCacheReusesAndEvictsChangedVersions(t *testing.T) {
	r, root := previewRuntime(t)
	defer r.close()
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	entry, _ := z.Create("a.txt")
	entry.Write([]byte("cached bytes"))
	z.Close()
	target := filepath.Join(root, "a.zip")
	os.WriteFile(target, data.Bytes(), 0600)
	p := resourcewire.Request{Path: target, Archive: true, ArchiveEntry: "a.txt", Method: "GET", Headers: http.Header{"Range": []string{"bytes=0-2"}}}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		if e := r.serveFileContent(context.Background(), w, p); e != nil {
			t.Fatal(e)
		}
		if w.Body.String() != "cac" {
			t.Fatal(w.Body.String())
		}
	}
	if len(r.fileCache) != 1 || r.fileCacheBytes != 12 {
		t.Fatal("cache was not reused")
	}
	for _, e := range r.fileCache {
		if e.refs != 0 {
			t.Fatal("cache pin leaked")
		}
		if _, err := os.Stat(e.path); err != nil {
			t.Fatal(err)
		}
	}
	r.closeZIPCache()
	if r.fileCacheBytes != 0 || len(r.fileCache) != 0 {
		t.Fatal("cache cleanup")
	}
}

func TestZIPEntryOrdinalRequiresSameVersion(t *testing.T) {
	r, root := previewRuntime(t)
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	f, _ := z.Create("a.txt")
	f.Write([]byte("one"))
	z.Close()
	target := filepath.Join(root, "a.zip")
	os.WriteFile(target, data.Bytes(), 0600)
	result, e := r.archiveFile(target, fileParams{Action: "list", PageSize: 250})
	if e != nil {
		t.Fatal(e)
	}
	version := result.(map[string]any)["archiveVersion"].(string)
	if e = os.Chtimes(target, time.Now(), time.Now().Add(time.Second)); e != nil {
		t.Fatal(e)
	}
	id := 0
	if _, e = r.archiveFile(target, fileParams{Action: "stat", ArchiveEntry: "a.txt", EntryID: &id, ArchiveVersion: version}); e == nil {
		t.Fatal("changed archive ordinal was reused")
	}
}

func TestZIPIndexBudgetChecksActualDirectory(t *testing.T) {
	_, root := previewRuntime(t)
	// archive/zip accepts inaccurate EOCD sizes and scans the actual headers.
	// That compatibility must not bypass our pre-allocation entry budget.
	const count = 100001
	data := make([]byte, count*46+22)
	for i := 0; i < count; i++ {
		binary.LittleEndian.PutUint32(data[i*46:], 0x02014b50)
	}
	end := data[count*46:]
	binary.LittleEndian.PutUint32(end, 0x06054b50)
	truncatedCount := uint16(count % 65536)
	binary.LittleEndian.PutUint16(end[8:], truncatedCount)
	binary.LittleEndian.PutUint16(end[10:], truncatedCount)
	target := filepath.Join(root, "misreported.zip")
	if e := os.WriteFile(target, data, 0600); e != nil {
		t.Fatal(e)
	}
	if z, f, e := openPreviewZIP(target); e == nil {
		f.Close()
		t.Fatalf("oversized actual directory accepted: %d", len(z.File))
	}
	// ZIP64 can be signaled by the record count alone, while the size fits.
	data = make([]byte, 56+20+22)
	binary.LittleEndian.PutUint32(data, 0x06064b50)
	binary.LittleEndian.PutUint64(data[4:], 44)
	binary.LittleEndian.PutUint64(data[32:], count)
	binary.LittleEndian.PutUint32(data[56:], 0x07064b50)
	binary.LittleEndian.PutUint32(data[76:], 0x06054b50)
	binary.LittleEndian.PutUint16(data[86:], 0xffff)
	if e := os.WriteFile(target, data, 0600); e != nil {
		t.Fatal(e)
	}
	if _, f, e := openPreviewZIP(target); e == nil {
		f.Close()
		t.Fatal("ZIP64 count bypassed index budget")
	}
}

func TestZIPCacheConcurrentReaders(t *testing.T) {
	r, root := previewRuntime(t)
	var data bytes.Buffer
	z := zip.NewWriter(&data)
	entry, e := z.Create("large.txt")
	if e != nil {
		t.Fatal(e)
	}
	payload := bytes.Repeat([]byte("concurrent cached content\n"), 40000)
	if _, e = entry.Write(payload); e != nil {
		t.Fatal(e)
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(root, "a.zip")
	if e = os.WriteFile(target, data.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	archive, f, e := openPreviewZIP(target)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	type result struct {
		file    *os.File
		release func()
		err     error
	}
	const readers = 16
	results := make(chan result, readers)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < readers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			file, release, err := r.openCachedZIPEntry(context.Background(), "shared-entry", archive.File[0], root)
			results <- result{file, release, err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		content, err := io.ReadAll(result.file)
		result.release()
		if err != nil || !bytes.Equal(content, payload) {
			t.Fatal("reader received incomplete cache", err)
		}
	}
	r.fileCacheMu.Lock()
	defer r.fileCacheMu.Unlock()
	if len(r.fileCache) != 1 || r.fileCacheBytes != int64(len(payload)) || r.fileCache["shared-entry"].refs != 0 {
		t.Fatal("concurrent preparation or references leaked")
	}
}
