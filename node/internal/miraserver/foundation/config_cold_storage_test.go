package foundation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestColdStorageConfig(t *testing.T) {
	parse := func(values map[string]string) (ColdStorageConfig, error) {
		return parseColdStorage(func(name string) (string, bool) {
			value, ok := values[name]
			return value, ok
		})
	}
	if config, err := parse(nil); err != nil || config.Enabled() {
		t.Fatalf("disabled by default: %+v %v", config, err)
	}
	if _, err := parse(map[string]string{"MIRA_NODE_COLD_STORAGE_ENDPOINT": "https://oss.example"}); err == nil {
		t.Fatal("endpoint without bucket accepted")
	}
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("s3cr3t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	config, err := parse(map[string]string{
		"MIRA_NODE_COLD_STORAGE_ENDPOINT": "https://oss-cn-beijing.aliyuncs.com", "MIRA_NODE_COLD_STORAGE_REGION": "oss-cn-beijing",
		"MIRA_NODE_COLD_STORAGE_BUCKET": "history", "MIRA_NODE_COLD_STORAGE_PREFIX": "homeserver",
		"MIRA_NODE_COLD_STORAGE_ACCESS_KEY_ID": "id", "MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY_FILE": secret,
		"MIRA_NODE_COLD_STORAGE_CACHE_DIR": cache, "MIRA_NODE_COLD_STORAGE_FREEZE": "true",
		"MIRA_NODE_COLD_STORAGE_FREEZE_AFTER": "1440h", "MIRA_NODE_COLD_STORAGE_MIN_BYTES": "65536",
		"MIRA_NODE_COLD_STORAGE_UPLOAD_CONCURRENCY": "16", "MIRA_NODE_COLD_STORAGE_TCP_CONGESTION": "bbr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled() || config.Prefix != "homeserver/" || config.SecretAccessKey != "s3cr3t" || !config.Freeze ||
		config.FreezeAfter != 60*24*time.Hour || config.MinBytes != 65536 || config.CacheDir != cache || config.CacheBytes != 2<<30 ||
		config.UploadConcurrency != 16 || config.TCPCongestion != "bbr" {
		t.Fatalf("parsed: %+v", config)
	}
	for name, value := range map[string]string{
		"MIRA_NODE_COLD_STORAGE_FREEZE_AFTER": "5m", "MIRA_NODE_COLD_STORAGE_MIN_BYTES": "10",
		"MIRA_NODE_COLD_STORAGE_CACHE_DIR": "relative", "MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY": "both",
		"MIRA_NODE_COLD_STORAGE_UPLOAD_CONCURRENCY": "65", "MIRA_NODE_COLD_STORAGE_TCP_CONGESTION": "bbr; rm",
	} {
		values := map[string]string{
			"MIRA_NODE_COLD_STORAGE_ENDPOINT": "https://oss.example", "MIRA_NODE_COLD_STORAGE_REGION": "r",
			"MIRA_NODE_COLD_STORAGE_BUCKET": "b", "MIRA_NODE_COLD_STORAGE_ACCESS_KEY_ID": "id",
			"MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY_FILE": secret, name: value,
		}
		if _, err := parse(values); err == nil || !strings.Contains(err.Error(), "MIRA_NODE_COLD_STORAGE") {
			t.Fatalf("%s=%s accepted: %v", name, value, err)
		}
	}
}
