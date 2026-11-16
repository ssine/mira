package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxProbePaths     = 64
	maxProbePathBytes = 4096
)

// probeFiles resolves references the way the Agent that wrote them would
// ("~", cwd-relative, absolute) and reports which exist. One bounded call
// replaces a stat round trip per reference; nothing is read or changed.
func (runtime *capabilityRuntime) probeFiles(params fileParams) (any, error) {
	if len(params.Paths) == 0 || len(params.Paths) > maxProbePaths {
		return nil, fmt.Errorf("probe requires 1 to %d paths", maxProbePaths)
	}
	cwd := ""
	if params.Cwd != "" {
		if !filepath.IsAbs(params.Cwd) || strings.ContainsRune(params.Cwd, 0) {
			return nil, fmt.Errorf("cwd must be an absolute path")
		}
		cwd = filepath.Clean(params.Cwd)
	}
	entries := make([]map[string]any, 0, len(params.Paths))
	for _, input := range params.Paths {
		entries = append(entries, runtime.probeFile(input, cwd, params.home))
	}
	result := map[string]any{"entries": entries}
	if params.home != "" {
		result["home"] = params.home
	}
	return result, nil
}

func (runtime *capabilityRuntime) probeFile(input, cwd, home string) map[string]any {
	entry := map[string]any{"input": input, "exists": false}
	if input == "" || len(input) > maxProbePathBytes || strings.ContainsRune(input, 0) {
		entry["reason"] = "invalid"
		return entry
	}
	candidate, reason := expandProbePath(input, cwd, home)
	if reason != "" {
		entry["reason"] = reason
		return entry
	}
	candidate = filepath.Clean(candidate)
	entry["path"] = candidate
	resolved, err := runtime.authorize(candidate, true)
	if err != nil {
		entry["reason"] = "forbidden"
		return entry
	}
	// Follow links: a reference to a linked directory opens as a directory.
	value, err := os.Stat(resolved)
	if err != nil {
		if !os.IsNotExist(err) {
			entry["reason"] = "unreadable"
		}
		return entry
	}
	entry["exists"] = true
	entry["type"] = "other"
	if value.IsDir() {
		entry["type"] = "directory"
	} else if value.Mode().IsRegular() {
		entry["type"] = "file"
	}
	entry["size"] = value.Size()
	entry["modifiedAt"] = value.ModTime().UTC().Format(time.RFC3339Nano)
	return entry
}

// expandProbePath returns an absolute candidate or the reason none exists.
// Only "~" and "~/..." mean home; "~name" stays an ordinary relative name.
func expandProbePath(input, cwd, home string) (string, string) {
	separated := func(prefix string) bool {
		return strings.HasPrefix(input, prefix+"/") || (os.PathSeparator == '\\' && strings.HasPrefix(input, prefix+`\`))
	}
	if input == "~" || separated("~") {
		if home == "" {
			return "", "home_unavailable"
		}
		return filepath.Join(home, strings.TrimLeft(input[1:], `/\`)), ""
	}
	if filepath.IsAbs(input) {
		return input, ""
	}
	if cwd == "" {
		return "", "relative"
	}
	// A rooted path without a drive belongs to the drive of cwd on Windows.
	if volume := filepath.VolumeName(cwd); volume != "" && (strings.HasPrefix(input, "/") || strings.HasPrefix(input, `\`)) && filepath.VolumeName(input) == "" {
		return volume + input, ""
	}
	return filepath.Join(cwd, input), ""
}
