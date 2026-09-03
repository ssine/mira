#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
output_dir="$script_dir/dist"
mkdir -p "$output_dir"

# Android has no resolv.conf for Go's pure DNS client. Link the system resolver
# through the NDK so domains follow Android's active network and Private DNS.
ndk_version=$(tr -d '\r\n' < "$script_dir/android/NDK_VERSION")
sdk_root=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}
ndk_root=${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-"$sdk_root/ndk/$ndk_version"}}
case "$(uname -s)" in
  Linux) ndk_host=linux-x86_64 ;;
  Darwin) ndk_host=darwin-x86_64 ;;
  *) echo "Build Android from Linux/WSL or macOS with the Android NDK." >&2; exit 1 ;;
esac
android_cc="$ndk_root/toolchains/llvm/prebuilt/$ndk_host/bin/aarch64-linux-android26-clang"
if [ ! -x "$android_cc" ]; then
  echo "Android NDK $ndk_version is required. Install with sdkmanager 'ndk;$ndk_version' and set ANDROID_HOME or ANDROID_NDK_HOME." >&2
  exit 1
fi

mira_version=$(tr -d '\r\n' < "$repo_dir/VERSION")
mira_commit=$(git -C "$repo_dir" rev-parse --short=12 HEAD 2>/dev/null || printf unknown)
mira_build_time=${MIRA_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
link_flags="-s -w -extldflags=-Wl,-z,max-page-size=16384 -X github.com/ssine/mira/node/internal/node.Version=$mira_version -X github.com/ssine/mira/node/internal/node.Commit=$mira_commit -X github.com/ssine/mira/node/internal/node.BuildTime=$mira_build_time"

(cd "$script_dir" && \
  CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC="$android_cc" \
    go build -trimpath -tags=netcgo -ldflags="$link_flags" -o "$output_dir/mira-node-android-arm64" ./cmd/mira-node)

node "$repo_dir/scripts/check-android-build.mjs" "$output_dir/mira-node-android-arm64"

echo "$output_dir/mira-node-android-arm64"
