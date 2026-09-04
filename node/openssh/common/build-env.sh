#!/usr/bin/env bash
# Sourced by platform builders; configuration refers to build hosts, not Nodes.
component=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
repo=$(cd "$component/../.." && pwd)
cache=${MIRA_SOURCE_CACHE:-"$component/.cache/sources"}
cache=$(realpath -m "$cache")
mira_version=$(tr -d '\r\n' < "$repo/VERSION")
mira_commit=$(git -C "$repo" rev-parse --short=12 HEAD)
mira_build_time=${MIRA_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
metadata="-X github.com/ssine/mira/node/internal.BundledOpenSSH=true -X github.com/ssine/mira/node/internal.Version=$mira_version -X github.com/ssine/mira/node/internal.Commit=$mira_commit -X github.com/ssine/mira/node/internal.BuildTime=$mira_build_time"
jobs=${MIRA_OPENSSH_JOBS:-8}
if [[ -n ${MIRA_OPENSSH_OUTPUT:-} ]]; then
  [[ $MIRA_OPENSSH_OUTPUT == /* && ! -e $MIRA_OPENSSH_OUTPUT && ! -L $MIRA_OPENSSH_OUTPUT ]] || { echo 'MIRA_OPENSSH_OUTPUT must be a fresh absolute directory' >&2; exit 1; }
fi

snapshot_node() {
  mkdir -p "$1/node"
  tar -C "$repo/node" -cf - go.mod go.sum cmd internal | tar -C "$1/node" -xf -
  cp "$component/common/go-export.go.in" "$1/node/cmd/mira-node/openssh_export.go"
}

fetch_sources() {
  node "$component/fetch-sources.mjs" "$cache" "$1"
}

android_toolchain() {
  local version
  version=$(tr -d '\r\n' < "$repo/node/android/NDK_VERSION")
  ndk=${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-"${ANDROID_HOME:-${ANDROID_SDK_ROOT:-$repo/.android-sdk}}/ndk/$version"}}
  toolchain="$ndk/toolchains/llvm/prebuilt/linux-x86_64/bin"
  cc="$toolchain/aarch64-linux-android26-clang"
  test -x "$cc" || { echo "Android NDK $version is required: $cc" >&2; return 1; }
}
