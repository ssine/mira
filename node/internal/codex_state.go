package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	// migration state; ordinary Mira upgrades with the same runtime can reuse it.
	digest := sha256.Sum256([]byte(absolute))
	directory := filepath.Join(filepath.Dir(identityFile), "state", "codex", pinned.Version, hex.EncodeToString(digest[:12]))
	return "sqlite_home=" + strconv.Quote(directory), nil
}
