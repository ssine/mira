package node

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

const ProtocolVersion = 1

// These values are overridden by release builds with -ldflags -X. Version is
// kept at the repository release version so local and platform builds report a
// useful semantic version even when no Git metadata was injected.
var (
	Version   = "0.13.4"
	Commit    = "unknown"
	BuildTime = "unknown"
)

type BuildMetadata struct {
	Version         string `json:"version"`
	Commit          string `json:"commit"`
	BuildTime       string `json:"buildTime"`
	ProtocolVersion int    `json:"protocolVersion"`
	GoVersion       string `json:"goVersion"`
	Platform        string `json:"platform"`
	Architecture    string `json:"architecture"`
}

func CurrentBuild() BuildMetadata {
	return BuildMetadata{
		Version: Version, Commit: Commit, BuildTime: BuildTime, ProtocolVersion: ProtocolVersion,
		GoVersion: runtime.Version(), Platform: runtime.GOOS, Architecture: runtime.GOARCH,
	}
}

func PrintVersion(name string, jsonOutput bool) error {
	build := CurrentBuild()
	if jsonOutput {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"schemaVersion": 1, "program": name, "build": build})
	}
	fmt.Printf("%s %s (%s, %s/%s)\n", name, build.Version, build.Commit, build.Platform, build.Architecture)
	return nil
}
