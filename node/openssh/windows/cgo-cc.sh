#!/usr/bin/env bash
set -euo pipefail
# Build-only existing container toolchain, no Windows compiler installation here.
for argument in "$@"; do
  if [[ $argument == -print-prog-name=ar || $argument == --print-prog-name=ar ]]; then
    echo "${MIRA_LLVM_BIN:-/usr/lib/llvm-18/bin}/llvm-ar"
    exit 0
  fi
done
if command -v x86_64-w64-mingw32-gcc >/dev/null; then exec x86_64-w64-mingw32-gcc "$@"; fi
exec docker run --rm -i --network=none \
  -v /tmp:/tmp -v "${MIRA_OPENSSH_GOROOT:?}:${MIRA_OPENSSH_GOROOT}:ro" -w "$(pwd -P)" \
  officecanon/qemu-mingw-zstd:fedora41-pinned x86_64-w64-mingw32-gcc "$@"
