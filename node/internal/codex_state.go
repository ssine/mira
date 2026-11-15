package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Keep upstream's derived SQLite state separate from Desktop's CODEX_HOME.
// Different upstream builds (including LF/CRLF migration sources) can have
// incompatible migration checksums. Canonical Mira history stays in PostgreSQL;
// config, authentication and Desktop rollouts remain in the original home.
func codexSQLiteOverride(identityFile, binary string, overrides []string) (string, error) {
	return codexAccountSQLiteOverride(identityFile, binary, "", overrides)
}

func codexAccountSQLiteOverride(identityFile, binary, accountID string, overrides []string) (string, error) {
	for _, override := range overrides {
		key, _, _ := strings.Cut(override, "=")
		if strings.TrimSpace(key) == "sqlite_home" {
			return "", nil
		}
	}
	if os.Getenv("CODEX_SQLITE_HOME") != "" {
		return "", nil // Preserve the official explicit environment override.
	}
	if identityFile == "" {
		var err error
		identityFile, err = DefaultIdentityFile()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(identityFile) {
		return "", fmt.Errorf("Codex state requires an absolute Node identity path")
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}
	var pinned runtimeIdentity
	if err := json.Unmarshal(codexRuntimeLockJSON, &pinned); err != nil {
		return "", err
	}
	// A local candidate and an immutable published package must not share
	// migration state; ordinary Mira upgrades with the same binary reuse it.
	digest := sha256.Sum256([]byte(absolute))
	location := hex.EncodeToString(digest[:12])
	stateRoot := filepath.Join(filepath.Dir(identityFile), "state", "codex")
	key, managed := codexBinaryStateKey(identityFile, absolute, pinned.Version)
	directory := filepath.Join(stateRoot, key, location)
	if accountID != "" {
		if !nodeUUIDPattern.MatchString(accountID) {
			return "", fmt.Errorf("invalid Codex account ID")
		}
		directory = filepath.Join(directory, "accounts", accountID)
	}
	if !managed {
		adoptCodexSQLiteState(stateRoot, location, accountID, absolute, directory)
	}
	return "sqlite_home=" + strconv.Quote(directory), nil
}

// codexBinaryStateKey names the first state level after the binary that will
// run. The pinned managed package keeps its runtime version (unchanged layout).
// Any other binary, such as an explicit account codexPath, is keyed by its own
// size and modification time, so a Node pin change does not force a cold
// migration/backfill for a binary that did not change.
func codexBinaryStateKey(identityFile, binary, pinnedVersion string) (string, bool) {
	root := os.Getenv("MIRA_NODE_CODEX_CACHE")
	if root == "" || !filepath.IsAbs(root) {
		root = filepath.Join(filepath.Dir(identityFile), "runtimes", "codex")
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if relative, err := filepath.Rel(filepath.Join(root, pinnedVersion), binary); err == nil &&
		relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return pinnedVersion, true
	}
	info, err := os.Stat(binary)
	if err != nil || !info.Mode().IsRegular() {
		return pinnedVersion, true // Unknown binaries keep the previous behaviour.
	}
	fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())))
	return "binary-" + hex.EncodeToString(fingerprint[:8]), false
}

// adoptCodexSQLiteState seeds an empty state directory for an unchanged
// explicit binary from the newest directory that older Mira versions keyed by
// their runtime pin for the same binary location. Only directories written
// after the binary's current modification time qualify, so state produced by a
// different build at the same path is never reused. Copies keep the source
// intact for rollback; failures fall back to Codex creating fresh state.
func adoptCodexSQLiteState(stateRoot, location, accountID, binary, target string) {
	if codexSQLiteFiles(target) != nil {
		return
	}
	info, err := os.Stat(binary)
	if err != nil {
		return
	}
	versions, err := os.ReadDir(stateRoot)
	if err != nil {
		return
	}
	var source string
	var newest int64
	for _, version := range versions {
		if !version.IsDir() || strings.HasPrefix(version.Name(), "binary-") {
			continue
		}
		candidate := filepath.Join(stateRoot, version.Name(), location)
		if accountID != "" {
			candidate = filepath.Join(candidate, "accounts", accountID)
		}
		for _, file := range codexSQLiteFiles(candidate) {
			modified := file.ModTime().UnixNano()
			if modified >= info.ModTime().UnixNano() && modified > newest {
				newest, source = modified, candidate
			}
		}
	}
	if source == "" || os.MkdirAll(target, 0o700) != nil {
		return
	}
	for _, file := range codexSQLiteFiles(source) {
		destination := filepath.Join(target, file.Name())
		if _, err := os.Lstat(destination); err == nil {
			continue
		}
		if err := copyCodexStateFile(filepath.Join(source, file.Name()), destination, file.Mode().Perm()); err != nil {
			Log("could not adopt previous Codex state", map[string]any{"source": source, "error": err.Error()})
			return
		}
	}
	Log("adopted previous Codex state for an unchanged binary", map[string]any{"source": source, "target": target})
}

func codexSQLiteFiles(directory string) []os.FileInfo {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var files []os.FileInfo
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !(strings.HasSuffix(name, ".sqlite") || strings.HasSuffix(name, ".sqlite-wal") || strings.HasSuffix(name, ".sqlite-shm")) {
			continue
		}
		if info, err := entry.Info(); err == nil {
			files = append(files, info)
		}
	}
	return files
}

func copyCodexStateFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := destination + ".adopt"
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode|0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		os.Remove(temporary)
		return err
	}
	if err := output.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}
