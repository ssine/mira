#!/usr/bin/env sh
# Mira per-user installer. No root access is required.
set -eu

server=""
version=latest
update=false
no_service=false
release_directory=""
prefix=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) server=$2; shift 2 ;;
    --version) version=$2; shift 2 ;;
    --update) update=true; shift ;;
    --no-service) no_service=true; shift ;;
    --release-directory) release_directory=$2; shift 2 ;;
    --prefix) prefix=$2; shift 2 ;;
    --help) printf '%s\n' 'Usage: install.sh [--server URL] [--version VERSION] [--update] [--no-service]'; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
done

for program in curl tar sha256sum; do
  command -v "$program" >/dev/null 2>&1 || { printf 'Required command missing: %s\n' "$program" >&2; exit 1; }
done
[ "$(uname -s)" = Linux ] || { printf '%s\n' 'This installer supports Linux and WSL. Use the PowerShell installer on Windows.' >&2; exit 1; }
case "$(uname -m)" in x86_64|amd64) architecture=amd64 ;; aarch64|arm64) architecture=arm64 ;; *) printf '%s\n' 'Unsupported CPU architecture' >&2; exit 1 ;; esac

install_root="$HOME/.local/share/mira"
bin_dir="$HOME/.local/bin"
service_dir="$HOME/.config/systemd/user"
service_file="$service_dir/mira-node.service"
if [ -n "$prefix" ]; then
  case "$prefix" in /*) ;; *) printf '%s\n' '--prefix must be absolute' >&2; exit 1 ;; esac
  install_root="$prefix/share/mira"
  bin_dir="$prefix/bin"
  no_service=true
fi
if [ "$update" = true ] && [ -f "$install_root/no-service" ]; then no_service=true; fi
if [ "$version" = latest ]; then
  release_url=$(curl --fail --silent --show-error --location --output /dev/null --write-out '%{url_effective}' https://github.com/ssine/mira/releases/latest)
  version=${release_url##*/}
fi
version=${version#v}
printf '%s\n' "$version" | awk '/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ { valid=1 } END { exit !valid }' || { printf '%s\n' 'Expected major.minor.patch' >&2; exit 1; }

if [ -z "$server" ] && [ "$update" = false ]; then
  printf 'Mira Server URL (for example https://mira.ssine.cc): ' >/dev/tty
  IFS= read -r server </dev/tty
fi
if [ "$update" = true ] && [ ! -x "$bin_dir/mira" ]; then
  printf '%s\n' 'No Mira installation found. Run the installer with --server first.' >&2; exit 1
fi

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
asset="mira_${version}_linux_${architecture}.tar.gz"
base_url="https://github.com/ssine/mira/releases/download/v${version}"
download() {
  if [ -n "$release_directory" ]; then cp "$release_directory/$1" "$stage/$1";
  else curl --fail --silent --show-error --location --retry 3 "$base_url/$1" --output "$stage/$1"; fi
}
verify() {
  expected=$(awk -v name="$1" '$2 == name { print $1 }' "$stage/SHA256SUMS")
  actual=$(sha256sum "$stage/$1" | awk '{print $1}')
  [ -n "$expected" ] && [ "$actual" = "$expected" ] || { printf 'Checksum verification failed: %s\n' "$1" >&2; exit 1; }
}
printf 'Downloading Mira %s for Linux %s...\n' "$version" "$architecture"
download SHA256SUMS
download "$asset"
download install.sh
verify "$asset"
verify install.sh
tar -xzf "$stage/$asset" -C "$stage"
package_dir="$stage/mira_${version}_linux_${architecture}"
[ -x "$package_dir/mira" ] && [ -x "$package_dir/mira-node" ] || { printf '%s\n' 'Release archive is incomplete' >&2; exit 1; }

mkdir -p "$install_root/versions" "$bin_dir"
for name in mira mira-node; do
  if [ -e "$bin_dir/$name" ] || [ -L "$bin_dir/$name" ]; then
    case "$(readlink "$bin_dir/$name" 2>/dev/null || true)" in "$install_root"/*) ;; *) printf 'Refusing to replace an unrelated %s\n' "$bin_dir/$name" >&2; exit 1 ;; esac
  fi
done
if [ "$no_service" = false ] && [ -e "$service_file" ] && ! grep -q '^# Managed by Mira installer$' "$service_file"; then
  printf 'Refusing to replace an independently managed service: %s\n' "$service_file" >&2; exit 1
fi
if [ -n "$server" ]; then "$package_dir/mira" setup --server "$server"; fi
if [ -d "$install_root/versions/$version" ]; then
  # Version directories are immutable. Re-installing an identical release is safe.
  cmp "$package_dir/mira-node" "$install_root/versions/$version/mira-node" >/dev/null || { printf '%s\n' 'This version is already installed with different contents; refusing to overwrite it.' >&2; exit 1; }
  cmp "$package_dir/mira" "$install_root/versions/$version/mira" >/dev/null || { printf '%s\n' 'Installed CLI contents differ; refusing to overwrite.' >&2; exit 1; }
else
  mv "$package_dir" "$install_root/versions/$version"
fi
previous=$(readlink "$bin_dir/mira-node" 2>/dev/null || true)
ln -sfn "$install_root/versions/$version/mira" "$bin_dir/mira"
ln -sfn "$install_root/versions/$version/mira-node" "$bin_dir/mira-node"
cp "$stage/install.sh" "$install_root/.install.sh.new"
chmod 700 "$install_root/.install.sh.new"
mv -f "$install_root/.install.sh.new" "$install_root/install.sh"
if [ "$no_service" = true ]; then printf '%s\n' 'No automatic service' > "$install_root/no-service"; fi

if [ "$no_service" = false ] && command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
  mkdir -p "$service_dir"
  printf '%s\n' '# Managed by Mira installer' '[Unit]' 'Description=Mira Node' 'After=network-online.target' '' '[Service]' "ExecStart=$bin_dir/mira-node" 'Restart=on-failure' 'RestartSec=3' '' '[Install]' 'WantedBy=default.target' > "$service_file"
  systemctl --user daemon-reload
  systemctl --user enable mira-node.service >/dev/null
  if ! systemctl --user restart mira-node.service; then
    if [ -n "$previous" ]; then
      ln -sfn "$previous" "$bin_dir/mira-node"
      ln -sfn "$(dirname "$previous")/mira" "$bin_dir/mira"
      systemctl --user restart mira-node.service || true
    fi
    printf '%s\n' 'Node service restart failed; the previous binary was restored where available.' >&2; exit 1
  fi
  identity_path=${MIRA_IDENTITY_FILE:-"$HOME/.config/mira/identity.json"}
  attempt=0
  while [ ! -f "$identity_path" ] && [ "$attempt" -lt 15 ]; do sleep 1; attempt=$((attempt + 1)); done
  "$bin_dir/mira" status
  printf '\nMira %s installed. Open your Server website to approve a new Node.\n' "$version"
  printf '%s\n' 'The user service starts at login. For startup before login, an administrator can enable user lingering.'
else
  printf '\nMira %s installed. Start the Node with:\n  %s/mira-node\n' "$version" "$bin_dir"
  printf '%s\n' 'No user systemd service was installed. Use your NAS service manager for persistent startup.'
fi
printf '\nCheck connection: %s/mira status\nUpdate later: %s/mira update\n' "$bin_dir" "$bin_dir"
case ":$PATH:" in *":$bin_dir:"*) ;; *) printf 'Add %s to PATH to use mira from any terminal.\n' "$bin_dir" ;; esac
