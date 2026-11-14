package foundation

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ColdStorageConfig selects an optional S3-compatible bucket for large,
// immutable history payloads. Reading frozen history requires it; moving
// payloads additionally requires Freeze.
type ColdStorageConfig struct {
	Endpoint, Region, Bucket, Prefix string
	AccessKeyID, SecretAccessKey     string
	PathStyle                        bool
	CacheDir                         string
	CacheBytes                       int64
	Freeze                           bool
	FreezeAfter                      time.Duration
	MinBytes                         int
	UploadConcurrency                int
	TCPCongestion                    string
}

func (config ColdStorageConfig) Enabled() bool { return config.Bucket != "" }

var congestionName = regexp.MustCompile(`^[a-z0-9_]{1,15}$`)

func parseColdStorage(lookup func(string) (string, bool)) (ColdStorageConfig, error) {
	value := func(name string) string { return strings.TrimSpace(envOr(lookup, "MIRA_NODE_COLD_STORAGE_"+name, "")) }
	config := ColdStorageConfig{
		Endpoint: value("ENDPOINT"), Region: value("REGION"), Bucket: value("BUCKET"), Prefix: value("PREFIX"),
		AccessKeyID: value("ACCESS_KEY_ID"), SecretAccessKey: value("SECRET_ACCESS_KEY"),
		PathStyle: value("PATH_STYLE") == "true", CacheDir: value("CACHE_DIR"),
		Freeze: value("FREEZE") == "true", CacheBytes: 2 << 30, FreezeAfter: 30 * 24 * time.Hour, MinBytes: 16 << 10,
		UploadConcurrency: 4, TCPCongestion: value("TCP_CONGESTION"),
	}
	if config.Bucket == "" {
		for _, name := range []string{"ENDPOINT", "ACCESS_KEY_ID", "SECRET_ACCESS_KEY", "SECRET_ACCESS_KEY_FILE", "FREEZE"} {
			if value(name) != "" {
				return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_%s requires MIRA_NODE_COLD_STORAGE_BUCKET", name)
			}
		}
		return config, nil
	}
	if file := value("SECRET_ACCESS_KEY_FILE"); file != "" {
		if config.SecretAccessKey != "" {
			return ColdStorageConfig{}, fmt.Errorf("set only one of MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY and _SECRET_ACCESS_KEY_FILE")
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_SECRET_ACCESS_KEY_FILE: %w", err)
		}
		config.SecretAccessKey = strings.TrimSpace(string(raw))
	}
	if config.Endpoint == "" || config.Region == "" || config.AccessKeyID == "" || config.SecretAccessKey == "" {
		return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_BUCKET requires _ENDPOINT, _REGION, _ACCESS_KEY_ID and a secret access key")
	}
	if config.Prefix != "" && !strings.HasSuffix(config.Prefix, "/") {
		config.Prefix += "/"
	}
	if config.CacheDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_CACHE_DIR is required: %w", err)
		}
		config.CacheDir = filepath.Join(base, "mira", "cold-history")
	} else if !filepath.IsAbs(config.CacheDir) {
		return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_CACHE_DIR must be absolute")
	}
	if text := value("CACHE_BYTES"); text != "" {
		bytes, err := strconv.ParseInt(text, 10, 64)
		if err != nil || bytes < 0 {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_CACHE_BYTES must be a non-negative integer")
		}
		config.CacheBytes = bytes
	}
	if text := value("FREEZE_AFTER"); text != "" {
		duration, err := time.ParseDuration(text)
		if err != nil || duration < time.Hour {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_FREEZE_AFTER must be a duration of at least 1h")
		}
		config.FreezeAfter = duration
	}
	if text := value("UPLOAD_CONCURRENCY"); text != "" {
		count, err := strconv.Atoi(text)
		if err != nil || count < 1 || count > 64 {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_UPLOAD_CONCURRENCY must be an integer between 1 and 64")
		}
		config.UploadConcurrency = count
	}
	if config.TCPCongestion != "" && !congestionName.MatchString(config.TCPCongestion) {
		return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_TCP_CONGESTION must be a kernel congestion control name such as bbr")
	}
	if text := value("MIN_BYTES"); text != "" {
		bytes, err := strconv.Atoi(text)
		if err != nil || bytes < 4<<10 {
			return ColdStorageConfig{}, fmt.Errorf("MIRA_NODE_COLD_STORAGE_MIN_BYTES must be an integer of at least 4096")
		}
		config.MinBytes = bytes
	}
	return config, nil
}
