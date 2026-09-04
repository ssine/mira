package node

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func runtimeDigest(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

type runtimeFixture struct {
	store    *codexRuntimeStore
	manifest runtimeManifest
	archive  []byte
	content  map[string][]byte
	requests atomic.Int32
	server   *httptest.Server
}

func makeRuntimeFixture(t *testing.T, platform string) *runtimeFixture {
	t.Helper()
	var pinned runtimeIdentity
	if err := json.Unmarshal(codexRuntimeLockJSON, &pinned); err != nil {
		t.Fatal(err)
	}
	suffix, extension := "", ".tar.gz"
	if platform == "windows-amd64" {
		suffix, extension = ".exe", ".zip"
	}
	names := []string{"codex-package.json", "bin/codex" + suffix, "bin/codex-code-mode-host" + suffix, "codex-path/rg" + suffix}
	if suffix == "" {
		names = append(names, "codex-resources/bwrap")
	} else {
		names = append(names, "codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe")
	}
	fixture := &runtimeFixture{content: map[string][]byte{}}
	var files []runtimeFile
	for _, name := range names {
		content := []byte("fixture " + name)
		fixture.content[name] = content
		files = append(files, runtimeFile{Path: name, Size: int64(len(content)), SHA256: runtimeDigest(content), Mode: 0755})
	}
	store := &codexRuntimeStore{identity: pinned, platform: platform, root: t.TempDir()}
	fixture.store = store
	prefix := "mira-codex_" + pinned.Version + "_" + strings.ReplaceAll(platform, "-", "_")
	var buffer bytes.Buffer
	if suffix != "" {
		writer := zip.NewWriter(&buffer)
		for _, file := range files {
			entry, err := writer.Create(prefix + "/" + file.Path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write(fixture.content[file.Path]); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&buffer)
		writer := tar.NewWriter(compressed)
		for _, file := range files {
			if err := writer.WriteHeader(&tar.Header{Name: prefix + "/" + file.Path, Size: file.Size, Mode: 0755, Typeflag: tar.TypeReg}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(fixture.content[file.Path]); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fixture.archive = buffer.Bytes()
	fixture.manifest = runtimeManifest{runtimeIdentity: pinned, Targets: map[string]runtimeTarget{
		platform: {Archive: prefix + extension, Size: int64(buffer.Len()), SHA256: runtimeDigest(buffer.Bytes()), Files: files},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.requests.Add(1)
		if r.URL.Path == "/codex-runtime.json" {
			_ = json.NewEncoder(w).Encode(fixture.manifest)
			return
		}
		if r.URL.Path == "/"+fixture.manifest.Targets[platform].Archive {
			_, _ = w.Write(fixture.archive)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	fixture.server, store.baseURL, store.client = server, server.URL, server.Client()
	return fixture
}

func TestCodexRuntimeDownloadCacheAndIndependentNodeUpdate(t *testing.T) {
	for _, platform := range []string{"linux-amd64", "windows-amd64"} {
		t.Run(platform, func(t *testing.T) {
			fixture := makeRuntimeFixture(t, platform)
			binary, err := fixture.store.ensure(context.Background(), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if binary != fixture.store.entrypoint() || fixture.requests.Load() != 2 {
				t.Fatal("wrong runtime/download count")
			}
			for name, expected := range fixture.content {
				actual, err := os.ReadFile(filepath.Join(fixture.store.directory(), filepath.FromSlash(name)))
				if err != nil || !bytes.Equal(actual, expected) {
					t.Fatalf("companion lost: %s: %v", name, err)
				}
			}
			// A newer Node has the same lock and stable cache, with no live network.
			fixture.server.Close()
			nextNode := *fixture.store
			nextNode.baseURL = "http://127.0.0.1:1"
			if got, err := nextNode.ensure(context.Background(), io.Discard); err != nil || got != binary {
				t.Fatalf("Node update redownloaded Codex: %s %v", got, err)
			}
		})
	}
}

func TestCodexRuntimeConcurrentInstall(t *testing.T) {
	fixture := makeRuntimeFixture(t, "linux-amd64")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := fixture.store.ensure(context.Background(), io.Discard); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if fixture.requests.Load() != 2 {
		t.Fatalf("concurrent downloads: %d", fixture.requests.Load())
	}
}

func TestCodexRuntimeLegacyMigrationRequiresEveryFileHash(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			fixture := makeRuntimeFixture(t, "linux-amd64")
			old := t.TempDir()
			for name, content := range fixture.content {
				if changed && name == "bin/codex" {
					content = bytes.Repeat([]byte("x"), len(content))
				}
				file := filepath.Join(old, filepath.FromSlash(name))
				if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, content, 0700); err != nil {
					t.Fatal(err)
				}
			}
			fixture.store.legacy = []string{old}
			if _, err := fixture.store.ensure(context.Background(), io.Discard); err != nil {
				t.Fatal(err)
			}
			expected := int32(1)
			if changed {
				expected = 2
			}
			if fixture.requests.Load() != expected {
				t.Fatalf("incorrect migration decision: %d", fixture.requests.Load())
			}
			if _, err := os.Stat(filepath.Join(old, "bin", "codex")); err != nil {
				t.Fatal("old runtime must remain for rollback")
			}
		})
	}
}

func TestCodexRuntimeRejectsUntrustedOrIncompletePackages(t *testing.T) {
	for _, kind := range []string{"wrong-patch", "wrong-version", "missing-host", "bad-file-hash", "bad-archive-hash", "truncated", "oversized", "traversal", "duplicate-file", "duplicate-case"} {
		t.Run(kind, func(t *testing.T) {
			fixture := makeRuntimeFixture(t, "linux-amd64")
			target := fixture.manifest.Targets[fixture.store.platform]
			switch kind {
			case "wrong-patch":
				fixture.manifest.PatchSHA256 = strings.Repeat("1", 64)
			case "wrong-version":
				fixture.manifest.Version = "0.151.0-mira.99"
			case "missing-host":
				target.Files = append(target.Files[:2:2], target.Files[3:]...)
			case "bad-file-hash":
				target.Files[0].SHA256 = strings.Repeat("0", 64)
			case "bad-archive-hash":
				target.SHA256 = strings.Repeat("0", 64)
			case "truncated":
				fixture.archive = fixture.archive[:len(fixture.archive)/2]
			case "oversized":
				target.Size = maxRuntimeArchive + 1
			case "traversal":
				target.Files[0].Path = "../escape"
			case "duplicate-file":
				target.Files = append(target.Files, target.Files[0])
			case "duplicate-case":
				duplicate := target.Files[0]
				duplicate.Path = strings.ToUpper(duplicate.Path)
				target.Files = append(target.Files, duplicate)
			}
			fixture.manifest.Targets[fixture.store.platform] = target
			if _, err := fixture.store.ensure(context.Background(), io.Discard); err == nil {
				t.Fatal("accepted broken runtime")
			}
			if _, err := os.Stat(fixture.store.directory()); !os.IsNotExist(err) {
				t.Fatalf("published partial cache: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Dir(fixture.store.directory()))
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".runtime-") {
					t.Fatal("staging leak")
				}
			}
		})
	}
}

func TestCodexRuntimeArchiveRejectsTraversalAndSymlinks(t *testing.T) {
	for _, kind := range []string{"tar-traversal", "tar-symlink", "zip-traversal", "zip-symlink"} {
		t.Run(kind, func(t *testing.T) {
			fixture := makeRuntimeFixture(t, "linux-amd64")
			target := fixture.manifest.Targets[fixture.store.platform]
			prefix := strings.TrimSuffix(target.Archive, ".tar.gz") + "/"
			name := prefix + "../outside"
			if strings.HasSuffix(kind, "symlink") {
				name = prefix + target.Files[0].Path
			}
			var buffer bytes.Buffer
			if strings.HasPrefix(kind, "zip") {
				writer := zip.NewWriter(&buffer)
				header := &zip.FileHeader{Name: name}
				if strings.HasSuffix(kind, "symlink") {
					header.SetMode(os.ModeSymlink | 0755)
				}
				entry, _ := writer.CreateHeader(header)
				_, _ = entry.Write([]byte("/etc/passwd"))
				_ = writer.Close()
				target.Archive = strings.TrimSuffix(target.Archive, ".tar.gz") + ".zip"
			} else {
				compressed := gzip.NewWriter(&buffer)
				writer := tar.NewWriter(compressed)
				header := &tar.Header{Name: name, Mode: 0755, Typeflag: tar.TypeReg}
				if strings.HasSuffix(kind, "symlink") {
					header.Typeflag, header.Linkname = tar.TypeSymlink, "/etc/passwd"
				}
				_ = writer.WriteHeader(header)
				_ = writer.Close()
				_ = compressed.Close()
			}
			fixture.archive, target.Size, target.SHA256 = buffer.Bytes(), int64(buffer.Len()), runtimeDigest(buffer.Bytes())
			fixture.manifest.Targets[fixture.store.platform] = target
			if _, err := fixture.store.ensure(context.Background(), io.Discard); err == nil {
				t.Fatal("accepted malicious archive")
			}
		})
	}
}

func TestCodexRuntimeCorruptCacheIsNotSilentlyOverwritten(t *testing.T) {
	fixture := makeRuntimeFixture(t, "linux-amd64")
	binary, err := fixture.store.ensure(context.Background(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("broken"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ensure(context.Background(), io.Discard); err == nil || !strings.Contains(err.Error(), "move it aside") {
		t.Fatalf("unexpected corruption handling: %v", err)
	}
	if fixture.requests.Load() != 2 {
		t.Fatal("damaged immutable cache was redownloaded")
	}
}

func TestCodexRuntimeLockCancellationAndRecovery(t *testing.T) {
	fixture := makeRuntimeFixture(t, "linux-amd64")
	parent := filepath.Dir(fixture.store.directory())
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(parent, fixture.store.platform+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if locked, err := tryRuntimeLock(lock); !locked || err != nil {
		t.Fatalf("lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := fixture.store.ensure(ctx, io.Discard); err != context.DeadlineExceeded {
		t.Fatalf("wait was not cancellable: %v", err)
	}
	if fixture.requests.Load() != 0 {
		t.Fatal("network access while blocked on lock")
	}
	lock.Close()
	if _, err := fixture.store.ensure(context.Background(), io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRuntimeStatusNeedsNeitherEnrollmentNorNetwork(t *testing.T) {
	t.Setenv("MIRA_NODE_CODEX_CACHE", t.TempDir())
	var output, errors bytes.Buffer
	if exit := RunCLI(context.Background(), []string{"--json", "codex-runtime", "status"}, nil, &output, &errors); exit != 0 {
		t.Fatalf("status: %d %s", exit, errors.String())
	}
	if !strings.Contains(output.String(), `"installed":false`) {
		t.Fatal(output.String())
	}
}

func TestAppServerPreparesCodexWithoutBlockingAndCancelsOnStop(t *testing.T) {
	t.Setenv("MIRA_NODE_CODEX_CACHE", t.TempDir())
	manager := newAppServerManager(config{})
	defer manager.close()
	started, cancelled := make(chan struct{}), make(chan struct{})
	manager.runtimeInstall = func(ctx context.Context) (string, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}
	if err := manager.reconcile(context.Background(), desiredAppServer{Running: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("preparation not started")
	}
	if report := manager.report(); report["runtimePreparing"] != true || report["status"] != "starting" {
		t.Fatal(report)
	}
	// A second heartbeat must not start a second download.
	if err := manager.reconcile(context.Background(), desiredAppServer{Running: true}); err != nil {
		t.Fatal(err)
	}
	if err := manager.reconcile(context.Background(), desiredAppServer{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel download")
	}
}

func TestAppServerExplicitCodexDoesNotTriggerRuntimeDownload(t *testing.T) {
	t.Setenv("MIRA_NODE_CODEX_CACHE", t.TempDir())
	manager := newAppServerManager(config{CodexBinary: filepath.Join(t.TempDir(), "missing-codex")})
	defer manager.close()
	manager.runtimeInstall = func(context.Context) (string, error) {
		t.Error("explicit path must not silently download a replacement")
		return "", fmt.Errorf("unexpected download")
	}
	if err := manager.reconcile(context.Background(), desiredAppServer{Running: true}); err == nil {
		t.Fatal("missing configured Codex must fail")
	}
}
