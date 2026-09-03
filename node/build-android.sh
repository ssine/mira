#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
output_dir="$script_dir/dist"
mkdir -p "$output_dir"

(cd "$script_dir" && \
  CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
    go build -trimpath -ldflags='-s -w' -o "$output_dir/mira-node-android-arm64" ./cmd/mira-node)

echo "$output_dir/mira-node-android-arm64"
