#!/usr/bin/env bash
# WSL orchestration. CI may run build-native.ps1 and link.mjs in separate jobs.
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
fetch_sources windows
powershell=${MIRA_POWERSHELL:-/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe}
windows_temp=$($powershell -NoProfile -NonInteractive -Command '[IO.Path]::GetTempPath()' | tr -d '\r\n')
parent=$(wslpath -u "$windows_temp")
work="$parent/mira-openssh-windows.$(date +%s)-$$"
echo "Windows build workspace: $work"
$powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(wslpath -w "$component/windows/build-native.ps1")" \
  -Workspace "$(wslpath -w "$work")" -Cache "$(wslpath -w "$cache")"
node "$component/windows/prepare.mjs" "$work/source"
$powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(wslpath -w "$component/windows/build-native.ps1")" \
  -Workspace "$(wslpath -w "$work")" -Phase build
MIRA_OPENSSH_GO_OUTPUT="$work/go-objects" bash "$component/windows/build-go.sh"
node "$component/windows/combine.mjs" "$work/source" "$work/combined"
node "$component/windows/link.mjs" "$work"
$powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "$(wslpath -w "$component/windows/stage.ps1")" \
  -Image "$(wslpath -w "$work/mira-node.exe")" -Destination "$(wslpath -w "$work/bin")"
node "$component/manifest.mjs" "$work/bin" windows amd64 "$work"
if [[ -n ${MIRA_OPENSSH_OUTPUT:-} ]]; then cp -a "$work/bin" "$MIRA_OPENSSH_OUTPUT"; fi
echo "Windows single-image bundle: $work/bin"
