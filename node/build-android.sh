#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
output_dir="$script_dir/dist"
mkdir -p "$output_dir"

mira_version=$(tr -d '\r\n' < "$repo_dir/VERSION")
mira_commit=$(git -C "$repo_dir" rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
mira_build_time=${MIRA_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
link_flags="-s -w -X github.com/ssine/mira/node/internal/node.Version=$mira_version -X github.com/ssine/mira/node/internal/node.Commit=$mira_commit -X github.com/ssine/mira/node/internal/node.BuildTime=$mira_build_time"

(cd "$script_dir" && \
  CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
    go build -trimpath -ldflags="$link_flags" -o "$output_dir/mira-node-android-arm64" ./cmd/mira-node)

echo "$output_dir/mira-node-android-arm64"
