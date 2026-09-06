#!/usr/bin/env sh
# Mira Linux bootstrapper. It downloads one verified binary image and delegates
# persistent service ownership to `mira install`. All later updates go through
# the running Supervisor (`mira update`), never through this script.
set -eu

server=""
version=latest
role=node
service_owner=""
service_manager=""
service_scope=""
release_directory=""
state_dir="${MIRA_STATE_DIR:-$HOME/.local/share/mira}"

while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) server=$2; shift 2 ;;
    --version) version=$2; shift 2 ;;
    --role) role=$2; shift 2 ;;
    --service-owner) service_owner=$2; shift 2 ;;
    --service-manager) service_manager=$2; shift 2 ;;
    --service-scope) service_scope=$2; shift 2 ;;
    --state-dir|--prefix) state_dir=$2; shift 2 ;;
    --release-directory) release_directory=$2; shift 2 ;;
    --update)
      printf '%s\n' 'Installer-driven updates were removed. Run: mira update' >&2
      exit 2 ;;
    --help)
      printf '%s\n' 'Usage: install.sh [--server URL] [--version VERSION] [--role node|server] [--service-owner nix|mira] [--service-manager auto|systemd|procd] [--service-scope user|system] [--state-dir DIR] [--release-directory DIR]'
      exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

[ "$(uname -s)" = Linux ] || { printf '%s\n' 'Use install.ps1 on Windows.' >&2; exit 1; }
case "$(uname -m)" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) printf '%s\n' 'Unsupported CPU architecture' >&2; exit 1 ;;
esac
case "$role" in node|server) ;; *) printf '%s\n' '--role must be node or server' >&2; exit 2 ;; esac
case "$service_owner" in ""|nix|mira) ;; *) printf '%s\n' '--service-owner must be nix or mira' >&2; exit 2 ;; esac
case "$service_manager" in ""|auto|systemd|procd) ;; *) printf '%s\n' '--service-manager must be auto, systemd, or procd' >&2; exit 2 ;; esac
case "$service_scope" in ""|user|system) ;; *) printf '%s\n' '--service-scope must be user or system' >&2; exit 2 ;; esac
case "$state_dir" in /*) ;; *) printf '%s\n' '--state-dir must be absolute' >&2; exit 2 ;; esac

for program in curl tar sha256sum awk mktemp grep; do
  command -v "$program" >/dev/null 2>&1 || { printf 'Required command is missing: %s\n' "$program" >&2; exit 1; }
done
if [ "$version" = latest ]; then
  release_url=$(curl --fail --silent --show-error --location --output /dev/null --write-out '%{url_effective}' https://github.com/ssine/mira/releases/latest)
  version=${release_url##*/}
fi
version=${version#v}
printf '%s\n' "$version" | awk '/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ { valid=1 } END { exit !valid }' || {
  printf '%s\n' 'Expected major.minor.patch' >&2; exit 1;
}

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
asset="mira_${version}_linux_${architecture}.tar.gz"
base_url="https://github.com/ssine/mira/releases/download/v${version}"
download() {
  if [ -n "$release_directory" ]; then cp "$release_directory/$1" "$stage/$1"
  else curl --fail --silent --show-error --location --retry 3 "$base_url/$1" --output "$stage/$1"
  fi
}
download SHA256SUMS
download "$asset"
expected=$(awk -v name="$asset" '$2 == name { print $1 }' "$stage/SHA256SUMS")
actual=$(sha256sum "$stage/$asset" | awk '{print $1}')
[ -n "$expected" ] && [ "$actual" = "$expected" ] || { printf '%s\n' 'Release checksum verification failed' >&2; exit 1; }
tar -xzf "$stage/$asset" -C "$stage"
package_dir="$stage/mira_${version}_linux_${architecture}"
[ -x "$package_dir/mira" ] || { printf '%s\n' 'Release has no Mira executable' >&2; exit 1; }
"$package_dir/mira" --version | grep -F "$version" >/dev/null || { printf '%s\n' 'Release version validation failed' >&2; exit 1; }

set -- install --state-dir "$state_dir" --role "$role"
[ -n "$service_owner" ] && set -- "$@" --service-owner "$service_owner"
[ -n "$service_manager" ] && set -- "$@" --service-manager "$service_manager"
[ -n "$service_scope" ] && set -- "$@" --service-scope "$service_scope"
[ -n "$server" ] && set -- "$@" --server-url "$server"
if [ -z "$service_owner" ] && grep -Eiq '^(ID|ID_LIKE)=.*nixos' /etc/os-release 2>/dev/null && [ -r /dev/tty ]; then
  # curl | sh consumes stdin. Keep the required Nix-vs-Mira ownership choice
  # interactive by handing the native installer the controlling terminal.
  "$package_dir/mira" "$@" </dev/tty
else
  "$package_dir/mira" "$@"
fi

bin_dir="$HOME/.local/bin"
mkdir -p "$bin_dir"
if [ -e "$bin_dir/mira" ] || [ -L "$bin_dir/mira" ]; then
  current=$(readlink "$bin_dir/mira" 2>/dev/null || true)
  [ "$current" = "$state_dir/current/mira" ] || { printf 'Refusing to replace unrelated path: %s\n' "$bin_dir/mira" >&2; exit 1; }
fi
ln -sfn "$state_dir/current/mira" "$bin_dir/mira"

printf '\nMira %s installed as %s. Later updates: %s/mira update --state-dir %s\n' "$version" "$role" "$bin_dir" "$state_dir"
case ":$PATH:" in *":$bin_dir:"*) ;; *) printf 'Add %s to PATH.\n' "$bin_dir" ;; esac
