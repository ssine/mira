#!/usr/bin/env bash
# One entry point for supported linked Node images. Never installs a service.
set -euo pipefail
component=$(cd "$(dirname "$0")" && pwd)
case ${1:-} in
  linux) exec bash "$component/linux/build.sh" ;;
  windows) exec bash "$component/windows/build.sh" ;;
  android) exec bash "$component/android/build.sh" ;;
  *) echo "Usage: bash node/openssh/build.sh {linux|windows|android}" >&2; exit 2 ;;
esac
