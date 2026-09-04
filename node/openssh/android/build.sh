#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
android_toolchain
fetch_sources android
work=$(mktemp -d /tmp/mira-openssh-android.XXXXXX)
echo "Android build workspace: $work"
snapshot_node "$work"
export MIRA_SOURCE_CACHE="$cache" ANDROID_NDK_HOME="$ndk"
bash "$component/android/build-upstream.sh" "$work"
bash "$component/android/link.sh" "$work"
echo "Android single-image bundle: $work/bin"
