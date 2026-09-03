#!/usr/bin/env sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
output_dir=${1:-"$repo_dir/dist"}
version=$(tr -d '\r\n' < "$repo_dir/VERSION")
commit=$(git -C "$repo_dir" rev-parse --short=12 HEAD)
build_time=${MIRA_BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
link_flags="-s -w -X github.com/ssine/mira/node/internal/node.Version=$version -X github.com/ssine/mira/node/internal/node.Commit=$commit -X github.com/ssine/mira/node/internal/node.BuildTime=$build_time"
staging_dir=$(mktemp -d)
trap 'rm -rf "$staging_dir"' EXIT HUP INT TERM

mkdir -p "$output_dir"
output_dir=$(CDPATH= cd -- "$output_dir" && pwd)

build_unix() {
  os=$1
  architecture=$2
  package_dir="$staging_dir/mira_${version}_${os}_${architecture}"
  mkdir -p "$package_dir"
  (cd "$repo_dir/node" && CGO_ENABLED=0 GOOS="$os" GOARCH="$architecture" \
    go build -trimpath -ldflags="$link_flags" -o "$package_dir/mira-node" ./cmd/mira-node && \
    CGO_ENABLED=0 GOOS="$os" GOARCH="$architecture" \
    go build -trimpath -ldflags="$link_flags" -o "$package_dir/mira" ./cmd/mira)
  codex_source=""
  case "$architecture" in
    amd64) codex_source=${MIRA_CODEX_LINUX_AMD64:-} ;;
    arm64) codex_source=${MIRA_CODEX_LINUX_ARM64:-} ;;
  esac
  if [ -n "$codex_source" ]; then
    cp "$codex_source" "$package_dir/mira-codex"
    chmod 755 "$package_dir/mira-codex"
    host_source=""
    case "$architecture" in
      amd64) host_source=${MIRA_CODEX_HOST_LINUX_AMD64:-} ;;
      arm64) host_source=${MIRA_CODEX_HOST_LINUX_ARM64:-} ;;
    esac
    [ -n "$host_source" ] || { printf 'Mira Codex host bundle is missing for Linux %s\n' "$architecture" >&2; exit 1; }
    cp "$host_source" "$package_dir/codex-code-mode-host"
    chmod 755 "$package_dir/codex-code-mode-host"
  elif [ "${MIRA_REQUIRE_CODEX_BUNDLE:-false}" = true ] && [ "$architecture" = amd64 ]; then
    printf 'Mira Codex bundle is required for Linux %s\n' "$architecture" >&2
    exit 1
  fi
  tar -C "$staging_dir" -czf "$output_dir/mira_${version}_${os}_${architecture}.tar.gz" "mira_${version}_${os}_${architecture}"
}

build_windows() {
  architecture=$1
  package_dir="$staging_dir/mira_${version}_windows_${architecture}"
  mkdir -p "$package_dir"
  (cd "$repo_dir/node" && CGO_ENABLED=0 GOOS=windows GOARCH="$architecture" \
    go build -trimpath -ldflags="$link_flags" -o "$package_dir/mira-node.exe" ./cmd/mira-node && \
    CGO_ENABLED=0 GOOS=windows GOARCH="$architecture" \
    go build -trimpath -ldflags="$link_flags" -o "$package_dir/mira.exe" ./cmd/mira)
  if [ -n "${MIRA_CODEX_WINDOWS_AMD64:-}" ]; then
    cp "$MIRA_CODEX_WINDOWS_AMD64" "$package_dir/mira-codex.exe"
    [ -n "${MIRA_CODEX_HOST_WINDOWS_AMD64:-}" ] || { printf 'Mira Codex host bundle is missing for Windows %s\n' "$architecture" >&2; exit 1; }
    cp "$MIRA_CODEX_HOST_WINDOWS_AMD64" "$package_dir/codex-code-mode-host.exe"
  elif [ "${MIRA_REQUIRE_CODEX_BUNDLE:-false}" = true ]; then
    printf 'Mira Codex bundle is required for Windows %s\n' "$architecture" >&2
    exit 1
  fi
  (cd "$staging_dir" && zip -qr "$output_dir/mira_${version}_windows_${architecture}.zip" "mira_${version}_windows_${architecture}")
}

build_unix linux amd64
build_unix linux arm64
build_windows amd64
cp "$repo_dir/scripts/install.sh" "$output_dir/install.sh"
cp "$repo_dir/scripts/install.ps1" "$output_dir/install.ps1"
(cd "$output_dir" && sha256sum mira_*_linux_*.tar.gz mira_*_windows_*.zip install.sh install.ps1 > SHA256SUMS)

printf 'Built Mira %s (%s) in %s\n' "$version" "$commit" "$output_dir"
