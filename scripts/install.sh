#!/usr/bin/env sh
# Mira Linux installer: user systemd, OpenWrt procd, or portable binaries.
set -eu

server=""
version=latest
update=false
service_mode=""
release_directory=""
prefix=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --server) server=$2; shift 2 ;;
    --version) version=$2; shift 2 ;;
    --update) update=true; shift ;;
    --no-service) service_mode=none; shift ;;
    --service) service_mode=$2; shift 2 ;;
    --release-directory) release_directory=$2; shift 2 ;;
    --prefix) prefix=$2; shift 2 ;;
    --help) printf '%s\n' 'Usage: install.sh [--server URL] [--version VERSION] [--update] [--prefix DIR] [--service auto|systemd|procd|none] [--no-service]'; exit 0 ;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 2 ;;
  esac
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
fi
legacy_update=false
if [ -z "$service_mode" ] && [ "$update" = true ]; then
  if [ -f "$install_root/no-service" ]; then service_mode=none;
  elif [ -f "$install_root/service-manager" ]; then service_mode=$(cat "$install_root/service-manager");
  else legacy_update=true; fi
fi
# Preserve the original portable-prefix default; opt in with --service procd.
if [ -z "$service_mode" ]; then
  if [ -n "$prefix" ]; then service_mode=none; else service_mode=auto; fi
fi
case "$service_mode" in auto|systemd|procd|none) ;; *) printf '%s\n' 'Expected --service auto, systemd, procd or none' >&2; exit 2 ;; esac
openwrt=false
if [ -f /etc/openwrt_release ]; then openwrt=true; fi
if [ "$service_mode" = auto ]; then
  if [ "$openwrt" = true ] && [ -f /etc/rc.common ] && command -v procd >/dev/null 2>&1; then
    # Older installers could not manage procd. Do not start a second Node over
    # an existing manually supervised installation during an ordinary update.
    if [ "$legacy_update" = true ]; then service_mode=none; else service_mode=procd; fi
  elif command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then service_mode=systemd;
  else service_mode=none; fi
fi
if [ "$service_mode" = procd ]; then
  [ "$(id -u)" = 0 ] && [ -f /etc/rc.common ] && command -v procd >/dev/null 2>&1 || {
    printf '%s\n' 'OpenWrt procd service installation requires root and /etc/rc.common; use --no-service for portable binaries.' >&2; exit 1;
  }
  service_dir=/etc/init.d
  service_file="$service_dir/mira-node"
  # The generated script embeds paths as shell literals and an ownership comment.
  case "$install_root$HOME${MIRA_IDENTITY_FILE:-}${MIRA_NODE_CONFIG_FILE:-}" in
    *'
'*) printf '%s\n' 'Service paths must not contain newlines' >&2; exit 1 ;;
  esac
elif [ "$service_mode" = systemd ]; then
  command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1 || {
    printf '%s\n' 'The current user has no available systemd service manager.' >&2; exit 1;
  }
fi
if [ "$service_mode" != none ] && { [ -e "$service_file" ] || [ -L "$service_file" ]; }; then
  if [ -L "$service_file" ] || ! grep -q '^# Managed by Mira installer$' "$service_file"; then
    printf 'Refusing to replace an independently managed service: %s\n' "$service_file" >&2; exit 1
  fi
  if [ "$service_mode" = procd ] && ! grep -Fqx "# Mira install root: $install_root" "$service_file"; then
    printf 'The procd service belongs to another Mira installation: %s\n' "$service_file" >&2; exit 1
  fi
fi

# OpenWrt often supplies these through BusyBox. Install only missing commands,
# plus trusted TLS roots and util-linux script (the Web/dynamic-tool PTY backend).
required_commands='curl diff cmp tar sha256sum readlink awk mktemp'
command_available() {
  command -v "$1" >/dev/null 2>&1 || return 1
  if [ "$1" = script ]; then
    case "$(script --version 2>/dev/null)" in *util-linux*) ;; *) return 1 ;; esac
  fi
  return 0
}
if [ "$openwrt" = true ]; then
  required_commands="$required_commands script"
  packages=""
  for program in $required_commands; do
    command_available "$program" && continue
    case "$program" in
      curl) package=curl ;; diff|cmp) package=diffutils ;; tar) package=tar ;;
      sha256sum) package=coreutils-sha256sum ;; readlink) package=coreutils-readlink ;;
      awk) package=gawk ;; mktemp) package=coreutils-mktemp ;; script) package=script-utils ;;
    esac
    case " $packages " in *" $package "*) ;; *) packages="$packages $package" ;; esac
  done
  if [ ! -s /etc/ssl/certs/ca-certificates.crt ]; then packages="$packages ca-bundle"; fi
  if [ -n "$packages" ]; then
    [ "$(id -u)" = 0 ] || { printf 'Missing OpenWrt dependencies:%s; rerun as root to install them.\n' "$packages" >&2; exit 1; }
    printf 'Installing missing OpenWrt dependencies:%s\n' "$packages"
    if command -v opkg >/dev/null 2>&1; then
      opkg update && opkg install $packages || { printf '%s\n' 'OpenWrt dependency installation failed. Check package feeds, connectivity and free space, then rerun.' >&2; exit 1; }
    elif command -v apk >/dev/null 2>&1; then
      apk update && apk add $packages || { printf '%s\n' 'OpenWrt dependency installation failed. Check package repositories, connectivity and free space, then rerun.' >&2; exit 1; }
    else
      printf 'No supported OpenWrt package manager found; required packages:%s\n' "$packages" >&2; exit 1
    fi
  fi
  [ -s /etc/ssl/certs/ca-certificates.crt ] || { printf '%s\n' 'OpenWrt CA certificate bundle is still missing.' >&2; exit 1; }
fi
for program in $required_commands; do
  command_available "$program" || { printf 'Required command missing or incompatible after dependency checks: %s\n' "$program" >&2; exit 1; }
done

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
[ -f "$package_dir/openssh.json" ] && [ "$("$package_dir/mira-node" --mira-openssh-build)" = MIRA_LINKED_OPENSSH_LINUX_STATIC_V1 ] || { printf '%s\n' 'Release has no embedded OpenSSH' >&2; exit 1; }
for role in mira ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen; do
  [ "$(readlink "$package_dir/$role")" = mira-node ] || { printf 'Invalid embedded role: %s\n' "$role" >&2; exit 1; }
done

mkdir -p "$install_root/versions" "$bin_dir"
for name in mira mira-node; do
  if [ -e "$bin_dir/$name" ] || [ -L "$bin_dir/$name" ]; then
    case "$(readlink "$bin_dir/$name" 2>/dev/null || true)" in "$install_root"/*) ;; *) printf 'Refusing to replace an unrelated %s\n' "$bin_dir/$name" >&2; exit 1 ;; esac
  fi
done
if [ -n "$server" ]; then
  if [ -n "${MIRA_NODE_CONFIG_FILE:-}" ]; then "$package_dir/mira" setup --server "$server" --config "$MIRA_NODE_CONFIG_FILE";
  else "$package_dir/mira" setup --server "$server"; fi
fi
if [ -d "$install_root/versions/$version" ]; then
  # Version directories are immutable. Re-installing an identical release is safe.
  cmp "$package_dir/mira-node" "$install_root/versions/$version/mira-node" >/dev/null || { printf '%s\n' 'This version is already installed with different contents; refusing to overwrite it.' >&2; exit 1; }
  cmp "$package_dir/mira" "$install_root/versions/$version/mira" >/dev/null || { printf '%s\n' 'Installed CLI contents differ; refusing to overwrite.' >&2; exit 1; }
  if [ -d "$package_dir/mira-codex-package" ]; then
    diff -qr "$package_dir/mira-codex-package" "$install_root/versions/$version/mira-codex-package" >/dev/null 2>&1 || { printf '%s\n' 'Installed Mira Codex package contents differ; refusing to overwrite.' >&2; exit 1; }
  fi
else
  mv "$package_dir" "$install_root/versions/$version"
fi
previous=$(readlink "$bin_dir/mira-node" 2>/dev/null || true)
ln -sfn "$install_root/versions/$version/mira" "$bin_dir/mira"
ln -sfn "$install_root/versions/$version/mira-node" "$bin_dir/mira-node"
cp "$stage/install.sh" "$install_root/.install.sh.new"
chmod 700 "$install_root/.install.sh.new"
mv -f "$install_root/.install.sh.new" "$install_root/install.sh"
if [ "$service_mode" = systemd ]; then
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
elif [ "$service_mode" = procd ]; then
  shell_literal() {
    printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
  }
  had_service=false
  was_enabled=false
  was_running=false
  if [ -f "$service_file" ]; then
    had_service=true
    "$service_file" enabled && was_enabled=true
    "$service_file" running >/dev/null 2>&1 && was_running=true
  else
    identity_path=${MIRA_IDENTITY_FILE:-"$HOME/.config/mira/identity.json"}
    config_path=${MIRA_NODE_CONFIG_FILE:-"$(dirname "$identity_path")/node.json"}
    cat > "$stage/mira-node.init" <<EOF
#!/bin/sh /etc/rc.common
# Managed by Mira installer
# Mira install root: $install_root
START=99
STOP=10
USE_PROCD=1

start_service() {
  procd_open_instance
  procd_set_param command $(shell_literal "$bin_dir/mira-node")
  procd_set_param env $(shell_literal "HOME=$HOME") $(shell_literal "MIRA_IDENTITY_FILE=$identity_path") $(shell_literal "MIRA_NODE_CONFIG_FILE=$config_path") $(shell_literal "PATH=$bin_dir:/usr/sbin:/usr/bin:/sbin:/bin")
  procd_set_param respawn 3600 5 5
  procd_set_param term_timeout 30
  procd_set_param stdout 1
  procd_set_param stderr 1
  procd_close_instance
}
EOF
    # Keep the existing service/environment on upgrades; only its version link changes.
    mkdir -p "$service_dir"
    cp "$stage/mira-node.init" "$service_file"
    chmod 755 "$service_file"
  fi
  start_procd() {
    if [ "$was_running" = true ] && [ "$previous" = "$install_root/versions/$version/mira-node" ]; then
      # An identical reinstall need not interrupt sessions in an already running Node.
      "$service_file" enable
      return $?
    fi
    "$service_file" enable && "$service_file" restart || return 1
    attempt=0
    while [ "$attempt" -lt 10 ]; do
      sleep 1
      "$service_file" running >/dev/null 2>&1 && return 0
      attempt=$((attempt + 1))
    done
    return 1
  }
  if ! start_procd; then
    "$service_file" stop || true
    if [ -n "$previous" ]; then
      ln -sfn "$previous" "$bin_dir/mira-node"
      ln -sfn "$(dirname "$previous")/mira" "$bin_dir/mira"
      if [ "$was_running" = true ]; then "$service_file" restart || true; fi
    fi
    if [ "$was_enabled" = false ]; then "$service_file" disable || true; fi
    if [ "$had_service" = false ]; then rm -f "$service_file"; fi
    printf '%s\n' 'Node procd startup failed; the previous binary/service was restored where available. Check logread -e mira-node.' >&2
    exit 1
  fi
  "$bin_dir/mira" status
  printf '\nMira %s installed with OpenWrt procd. Open your Server website to approve a new Node.\n' "$version"
  printf '%s\n' 'Starts automatically on boot. Service: /etc/init.d/mira-node {start|stop|restart|status}. Logs: logread -e mira-node.'
else
  printf '\nMira %s installed. Start the Node with:\n  %s/mira-node\n' "$version" "$bin_dir"
  printf '%s\n' 'No automatic service was installed. Use your device service manager for persistent startup.'
fi
if [ "$service_mode" = none ]; then
  printf '%s\n' 'No automatic service' > "$install_root/no-service"
else
  printf '%s\n' "$service_mode" > "$install_root/service-manager"
  rm -f "$install_root/no-service"
fi
printf '\nCheck connection: %s/mira status\nUpdate later: %s/mira update\n' "$bin_dir" "$bin_dir"
case ":$PATH:" in *":$bin_dir:"*) ;; *) printf 'Add %s to PATH to use mira from any terminal.\n' "$bin_dir" ;; esac
