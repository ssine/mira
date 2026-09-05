#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
case $(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo 'Unsupported Linux build architecture' >&2; exit 1 ;; esac
fetch_sources linux
work=$(mktemp -d /tmp/mira-openssh-linux.XXXXXX)
echo "Linux build workspace: $work"
snapshot_node "$work"
mkdir "$work/bin" "$work/combined"
cp "$cache/openssh-10.5p1.tar.gz" "$cache/openssl-3.5.5.tar.gz" "$work/"
cp "$component/common/dispatcher-unix.c" "$work/dispatcher.c"
cp "$component/linux/build-inner.sh" "$component/common/objects.mk" "$component/common/go-init.ld" "$work/"
goroot=$(go -C "$repo/node" env GOROOT)
modcache=$(go -C "$repo/node" env GOMODCACHE)
builder=${MIRA_OPENSSH_BUILDER_IMAGE:-mira-openssh-musl-builder}
if [[ -z ${MIRA_OPENSSH_BUILDER_IMAGE:-} ]]; then
  docker build -t "$builder" -f "$component/linux/Dockerfile" "$component/linux"
fi
cache_args=()
if [[ -n ${MIRA_OPENSSH_NATIVE_CACHE:-} ]]; then
  mkdir -p "$MIRA_OPENSSH_NATIVE_CACHE"
  cache_args=(-v "$(realpath "$MIRA_OPENSSH_NATIVE_CACHE"):/mira-build-cache" -e MIRA_BUILD_CACHE=/mira-build-cache)
fi
docker run --rm --network=none -v "$work:$work" -v "$goroot:/opt/go:ro" -v "$modcache:$modcache:ro" \
  "${cache_args[@]}" \
  -e GOROOT=/opt/go -e GOTOOLCHAIN=local -e GOMODCACHE="$modcache" -e GOPROXY=off \
  -e MIRA_OPENSSH_METADATA="$metadata" -e MIRA_OPENSSH_JOBS="$jobs" \
  -w "$work" "$builder" bash ./build-inner.sh
node "$component/manifest.mjs" "$work/bin" linux "$arch" "$work"
if [[ -n ${MIRA_OPENSSH_OUTPUT:-} ]]; then cp -a "$work/bin" "$MIRA_OPENSSH_OUTPUT"; fi
echo "Linux single-image bundle: $work/bin"
