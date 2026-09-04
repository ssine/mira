package node

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The lock describes source compatibility, not the Mira release version. Bump
// the Mira patch revision whenever the upstream baseline or patch changes.
//
//go:embed codex-runtime.json
var codexRuntimeLockJSON []byte

type runtimeIdentity struct {
	SchemaVersion   int    `json:"schemaVersion"`
	Version         string `json:"version"`
	UpstreamVersion string `json:"upstreamVersion"`
	PatchSHA256     string `json:"patchSHA256"`
}

type runtimeFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}

type runtimeTarget struct {
	Archive string        `json:"archive"`
	Size    int64         `json:"size"`
	SHA256  string        `json:"sha256"`
	Files   []runtimeFile `json:"files"`
}

type runtimeManifest struct {
	runtimeIdentity
	Targets map[string]runtimeTarget `json:"targets"`
}

type runtimeReceipt struct {
	runtimeIdentity
	Platform string        `json:"platform"`
	Files    []runtimeFile `json:"files"`
}

type codexRuntimeStore struct {
	identity runtimeIdentity
	platform string
	root     string
	baseURL  string
	local    string // explicit offline release directory, never implicitly trusted
	legacy   []string
	client   *http.Client
}

const maxRuntimeArchive = int64(1024 * 1024 * 1024)
const maxRuntimeExpanded = 2 * maxRuntimeArchive

func newCodexRuntimeStore(identityFile string) (*codexRuntimeStore, error) {
	var pinned runtimeIdentity
	if err := json.Unmarshal(codexRuntimeLockJSON, &pinned); err != nil {
		return nil, err
	}
	if runtime.GOARCH != "amd64" || (runtime.GOOS != "linux" && runtime.GOOS != "windows") {
		return nil, fmt.Errorf("managed Codex runtime is unavailable on %s/%s; use CODEX_BINARY with a compatible build on supported desktop platforms", runtime.GOOS, runtime.GOARCH)
	}
	if identityFile == "" {
		var err error
		identityFile, err = DefaultIdentityFile()
		if err != nil {
			return nil, err
		}
	}
	root := os.Getenv("MIRA_NODE_CODEX_CACHE")
	if root == "" {
		root = filepath.Join(filepath.Dir(identityFile), "runtimes", "codex")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("MIRA_NODE_CODEX_CACHE must be absolute")
	}
	store := &codexRuntimeStore{
		identity: pinned, platform: runtime.GOOS + "-" + runtime.GOARCH, root: root,
		baseURL: "https://github.com/ssine/mira/releases/download/codex-v" + pinned.Version,
		client: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return fmt.Errorf("runtime download cannot redirect outside HTTPS")
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many runtime download redirects")
			}
			return nil
		}},
	}
	if executable, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		directory := filepath.Dir(executable)
		store.legacy = append(store.legacy, filepath.Join(directory, "mira-codex-package"))
		versions := filepath.Dir(directory)
		if filepath.Base(versions) == "versions" {
			entries, _ := os.ReadDir(versions)
			for i := len(entries) - 1; i >= 0; i-- {
				if entries[i].IsDir() && releaseVersionPattern.MatchString(entries[i].Name()) {
					store.legacy = append(store.legacy, filepath.Join(versions, entries[i].Name(), "mira-codex-package"))
				}
			}
		}
	}
	return store, nil
}

func (store *codexRuntimeStore) directory() string {
	return filepath.Join(store.root, store.identity.Version, store.platform)
}
func (store *codexRuntimeStore) entrypoint() string {
	name := "codex"
	if strings.HasPrefix(store.platform, "windows-") {
		name += ".exe"
	}
	return filepath.Join(store.directory(), "bin", name)
}

func safeRuntimePath(name string) bool {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
	}
	return true
}

func validRuntimeDigest(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == sha256.Size && strings.ToLower(value) == value
}

func (store *codexRuntimeStore) validateFiles(files []runtimeFile) error {
	if len(files) == 0 || len(files) > 4096 {
		return fmt.Errorf("invalid runtime file count")
	}
	seen := map[string]bool{}
	exact := map[string]runtimeFile{}
	var total int64
	for _, file := range files {
		key := strings.ToLower(file.Path)
		if !safeRuntimePath(file.Path) || file.Path == ".mira-runtime.json" || seen[key] || file.Size < 0 || file.Size > maxRuntimeArchive || !validRuntimeDigest(file.SHA256) || (file.Mode != 0644 && file.Mode != 0755) {
			return fmt.Errorf("invalid runtime file: %q", file.Path)
		}
		seen[key] = true
		exact[file.Path] = file
		total += file.Size
		if total > maxRuntimeExpanded {
			return fmt.Errorf("runtime expanded size exceeds limit")
		}
	}
	suffix := ""
	if strings.HasPrefix(store.platform, "windows-") {
		suffix = ".exe"
	}
	required := []string{"codex-package.json", "bin/codex" + suffix, "bin/codex-code-mode-host" + suffix, "codex-path/rg" + suffix}
	if suffix == "" {
		required = append(required, "codex-resources/bwrap")
	} else {
		required = append(required, "codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe")
	}
	for _, name := range required {
		file, exists := exact[name]
		if !exists || file.Size == 0 {
			return fmt.Errorf("runtime missing required file %s", name)
		}
		if suffix == "" && name != "codex-package.json" && file.Mode != 0755 {
			return fmt.Errorf("runtime entrypoint/helper is not executable: %s", name)
		}
	}
	return nil
}

// Fast local completeness check; hashes are verified at installation/migration.
// Local same-user write access is not a security boundary against code tampering.
func (store *codexRuntimeStore) cached() (string, error) {
	content, err := os.ReadFile(filepath.Join(store.directory(), ".mira-runtime.json"))
	if err != nil {
		return "", err
	}
	var receipt runtimeReceipt
	if len(content) > 1024*1024 || json.Unmarshal(content, &receipt) != nil || receipt.runtimeIdentity != store.identity || receipt.Platform != store.platform {
		return "", fmt.Errorf("cached runtime identity mismatch")
	}
	if err := store.validateFiles(receipt.Files); err != nil {
		return "", err
	}
	for _, file := range receipt.Files {
		info, err := os.Lstat(filepath.Join(store.directory(), filepath.FromSlash(file.Path)))
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() != file.Size {
			return "", fmt.Errorf("cached runtime file changed: %s", file.Path)
		}
	}
	return store.entrypoint(), nil
}

func (store *codexRuntimeStore) openAsset(ctx context.Context, name string) (io.ReadCloser, error) {
	if !safeRuntimePath(name) || strings.Contains(name, "/") {
		return nil, fmt.Errorf("invalid runtime asset name")
	}
	if store.local != "" {
		return os.Open(filepath.Join(store.local, name))
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, store.baseURL+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "mira/"+Version)
	response, err := store.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("download %s: HTTP %d (required release codex-v%s)", name, response.StatusCode, store.identity.Version)
	}
	return response.Body, nil
}

func (store *codexRuntimeStore) manifest(ctx context.Context) (*runtimeTarget, error) {
	body, err := store.openAsset(ctx, "codex-runtime.json")
	if err != nil {
		return nil, err
	}
	defer body.Close()
	content, err := io.ReadAll(io.LimitReader(body, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	var manifest runtimeManifest
	if len(content) > 1024*1024 || json.Unmarshal(content, &manifest) != nil || manifest.runtimeIdentity != store.identity {
		return nil, fmt.Errorf("Codex runtime manifest does not match the pinned baseline/patch")
	}
	target, ok := manifest.Targets[store.platform]
	if !ok {
		return nil, fmt.Errorf("Codex release has no %s runtime", store.platform)
	}
	if target.Size <= 0 || target.Size > maxRuntimeArchive || !validRuntimeDigest(target.SHA256) || !safeRuntimePath(target.Archive) || strings.Contains(target.Archive, "/") {
		return nil, fmt.Errorf("invalid Codex runtime archive metadata")
	}
	if err := store.validateFiles(target.Files); err != nil {
		return nil, err
	}
	return &target, nil
}

func copyRuntimeFile(ctx context.Context, destination string, source io.Reader, file runtimeFile) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(file.Mode))
	if err != nil {
		return err
	}
	defer output.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(&runtimeContextReader{ctx, source}, file.Size+1))
	if err != nil {
		return err
	}
	if n != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
		return fmt.Errorf("runtime checksum/size mismatch: %s", file.Path)
	}
	return output.Close()
}

type runtimeContextReader struct {
	context.Context
	io.Reader
}

func (reader *runtimeContextReader) Read(p []byte) (int, error) {
	if err := reader.Err(); err != nil {
		return 0, err
	}
	return reader.Reader.Read(p)
}

func (store *codexRuntimeStore) migrate(ctx context.Context, stage string, target *runtimeTarget) (bool, error) {
	for _, old := range store.legacy {
		// Check content before creating anything. Never trust an old package solely
		// because its upstream version matches: official builds lack Mira's patch.
		matches := true
		for _, file := range target.Files {
			name := filepath.Join(old, filepath.FromSlash(file.Path))
			info, err := os.Lstat(name)
			if err != nil || !info.Mode().IsRegular() || info.Size() != file.Size {
				matches = false
				break
			}
			input, err := os.Open(name)
			if err != nil {
				matches = false
				break
			}
			hash := sha256.New()
			_, err = io.Copy(hash, &runtimeContextReader{ctx, input})
			input.Close()
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if err != nil || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		for _, file := range target.Files {
			input, err := os.Open(filepath.Join(old, filepath.FromSlash(file.Path)))
			if err != nil {
				return false, err
			}
			err = copyRuntimeFile(ctx, filepath.Join(stage, filepath.FromSlash(file.Path)), input, file)
			input.Close()
			if err != nil {
				return false, err
			}
		}
		return true, nil
	}
	return false, nil
}

func (store *codexRuntimeStore) extract(ctx context.Context, archive, stage string, target *runtimeTarget) error {
	files := map[string]runtimeFile{}
	for _, file := range target.Files {
		files[file.Path] = file
	}
	prefix := "mira-codex_" + store.identity.Version + "_" + strings.ReplaceAll(store.platform, "-", "_") + "/"
	visit := func(name string, regular, directory bool, size int64, reader io.Reader) error {
		if !strings.HasPrefix(name, prefix) {
			return fmt.Errorf("unexpected runtime archive entry %q", name)
		}
		name = strings.TrimPrefix(name, prefix)
		if directory {
			if name == "" || safeRuntimePath(strings.TrimSuffix(name, "/")) {
				return nil
			}
			return fmt.Errorf("invalid archive directory")
		}
		file, exists := files[name]
		if !regular || !exists || size != file.Size {
			return fmt.Errorf("unexpected/duplicate runtime file %q", name)
		}
		delete(files, name)
		return copyRuntimeFile(ctx, filepath.Join(stage, filepath.FromSlash(name)), reader, file)
	}
	if strings.HasSuffix(target.Archive, ".zip") {
		reader, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer reader.Close()
		if len(reader.File) > 8192 {
			return fmt.Errorf("too many archive entries")
		}
		for _, entry := range reader.File {
			body, err := entry.Open()
			if err != nil {
				return err
			}
			err = visit(entry.Name, entry.Mode().IsRegular(), entry.FileInfo().IsDir(), int64(entry.UncompressedSize64), body)
			body.Close()
			if err != nil {
				return err
			}
		}
	} else if strings.HasSuffix(target.Archive, ".tar.gz") {
		input, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer input.Close()
		compressed, err := gzip.NewReader(input)
		if err != nil {
			return err
		}
		defer compressed.Close()
		reader := tar.NewReader(io.LimitReader(compressed, maxRuntimeExpanded+16*1024*1024))
		for count := 0; ; count++ {
			entry, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if count >= 8192 {
				return fmt.Errorf("too many archive entries")
			}
			if err := visit(entry.Name, entry.Typeflag == tar.TypeReg, entry.Typeflag == tar.TypeDir, entry.Size, reader); err != nil {
				return err
			}
		}
	} else {
		return fmt.Errorf("unsupported Codex archive format")
	}
	if len(files) != 0 {
		return fmt.Errorf("runtime archive is incomplete")
	}
	return nil
}

func (store *codexRuntimeStore) ensure(ctx context.Context, progress io.Writer) (string, error) {
	if binary, err := store.cached(); err == nil {
		return binary, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	parent := filepath.Dir(store.directory())
	if err := os.MkdirAll(parent, 0700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(parent, store.platform+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close() // OS releases the lock on crash/exit; no stale-lock deletion.
	for {
		locked, err := tryRuntimeLock(lock)
		if err != nil {
			return "", err
		}
		if locked {
			break
		}
		if !sleepContext(ctx, 250*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	if binary, err := store.cached(); err == nil {
		return binary, nil
	}
	if _, err := os.Lstat(store.directory()); err == nil {
		return "", fmt.Errorf("cached Codex runtime is incomplete or changed: %s; move it aside for diagnosis before reinstalling", store.directory())
	} else if !os.IsNotExist(err) {
		return "", err
	}
	fmt.Fprintf(progress, "Preparing Codex %s (%s); Mira Node is a separate download.\n", store.identity.Version, store.platform)
	target, err := store.manifest(ctx)
	if err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(parent, ".runtime-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	migrated, err := store.migrate(ctx, stage, target)
	if err != nil {
		return "", err
	}
	if migrated {
		fmt.Fprintln(progress, "Reused a verified Codex package from an older Mira installation (no archive download).")
	} else {
		fmt.Fprintf(progress, "Downloading %s (%.1f MiB)...\n", target.Archive, float64(target.Size)/(1024*1024))
		body, err := store.openAsset(ctx, target.Archive)
		if err != nil {
			return "", err
		}
		archive := filepath.Join(stage, ".archive")
		err = copyRuntimeFile(ctx, archive, body, runtimeFile{Path: target.Archive, Size: target.Size, SHA256: target.SHA256, Mode: 0600})
		body.Close()
		if err != nil {
			return "", err
		}
		if err := store.extract(ctx, archive, stage, target); err != nil {
			return "", err
		}
		if err := os.Remove(archive); err != nil {
			return "", err
		}
	}
	content, err := json.Marshal(runtimeReceipt{store.identity, store.platform, target.Files})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(stage, ".mira-runtime.json"), content, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(stage, store.directory()); err != nil {
		return "", err
	}
	return store.cached()
}

func managedCodexCandidates(configured, identityFile string) []string {
	if configured != "" {
		return codexCandidatePaths(configured)
	}
	var result []string
	if store, err := newCodexRuntimeStore(identityFile); err == nil {
		if binary, err := store.cached(); err == nil {
			result = append(result, binary)
		}
	}
	return append(result, codexCandidatePaths("")...)
}

func runCodexRuntime(ctx context.Context, options cliOptions, args []string, stderr io.Writer) (any, error) {
	if len(args) == 0 || (args[0] != "status" && args[0] != "install") {
		return nil, fmt.Errorf("usage: mira codex-runtime <status|install [--release-directory DIR]>")
	}
	set := flagSet("codex-runtime " + args[0])
	directory := set.String("release-directory", "", "explicit offline Codex release directory")
	if err := set.Parse(args[1:]); err != nil {
		return nil, err
	}
	if set.NArg() != 0 || (args[0] == "status" && *directory != "") {
		return nil, fmt.Errorf("unexpected codex-runtime arguments")
	}
	store, err := newCodexRuntimeStore(options.Identity)
	if err != nil {
		return nil, err
	}
	store.local = *directory
	binary, cachedErr := store.cached()
	if args[0] == "install" {
		binary, err = store.ensure(ctx, stderr)
		if err != nil {
			return nil, err
		}
		cachedErr = nil
	}
	view := map[string]any{"version": store.identity.Version, "upstreamVersion": store.identity.UpstreamVersion, "releaseTag": "codex-v" + store.identity.Version, "platform": store.platform, "directory": store.directory(), "installed": cachedErr == nil}
	if cachedErr == nil {
		view["codexPath"] = binary
	} else if !os.IsNotExist(cachedErr) {
		view["error"] = cachedErr.Error()
	}
	return view, nil
}
