#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
repo_dir=$(cd "$script_dir/.." && pwd)
mkdir -p "$script_dir/dist"
# An override must be a complete, verified bundle, never a plain Go executable.
bundle=${MIRA_ANDROID_OPENSSH_BUNDLE:-}
if [[ -z $bundle ]]; then
  output=$(mktemp -d /tmp/mira-android-package.XXXXXX)
  MIRA_OPENSSH_OUTPUT="$output/bundle" bash "$script_dir/openssh/build.sh" android
  bundle="$output/bundle"
fi
node "$script_dir/openssh/verify-package.mjs" "$bundle" android arm64
node "$repo_dir/scripts/check-android-build.mjs" "$bundle/mira-node"
node "$script_dir/openssh/android/check-image.mjs" "$bundle/mira-node"
cp "$bundle/mira-node" "$script_dir/dist/mira-node-android-arm64"
chmod 755 "$script_dir/dist/mira-node-android-arm64"
mkdir -p "$script_dir/dist/openssh-notices"
cp -R "$bundle/licenses" "$bundle/openssh.json" "$script_dir/dist/openssh-notices/"
echo "$script_dir/dist/mira-node-android-arm64"
