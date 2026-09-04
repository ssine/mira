#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
if [[ -n ${MIRA_OPENSSH_GO_OUTPUT:-} ]]; then
  [[ $MIRA_OPENSSH_GO_OUTPUT == /* && ! -e $MIRA_OPENSSH_GO_OUTPUT && ! -L $MIRA_OPENSSH_GO_OUTPUT ]] || { echo 'Go object output must be a fresh absolute directory' >&2; exit 1; }
fi
work=$(mktemp -d /tmp/mira-openssh-windows-go.XXXXXX)
snapshot_node "$work"
mkdir "$work/go-objects"
cp "$component/windows/cgo-cc.sh" "$work/cc"
chmod 700 "$work/cc"
echo "Windows Go archive workspace: $work"
export MIRA_OPENSSH_GOROOT=$(go -C "$work/node" env GOROOT)
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC="$work/cc" CGO_CFLAGS=-D__USE_MINGW_ANSI_STDIO=0 \
  go -C "$work/node" build -buildmode=c-archive -ldflags="-extar=${MIRA_LLVM_BIN:-/usr/lib/llvm-18/bin}/llvm-ar $metadata" -o "$work/node.a" ./cmd/mira-node > "$work/go-build.log" 2>&1
(cd "$work/go-objects" && ${MIRA_LLVM_BIN:-/usr/lib/llvm-18/bin}/llvm-ar x "$work/node.a")
# Unlike ELF, MSVC does not automatically execute GNU .ctors. Export the pointer
# itself and put it in read-only data; the C dispatcher calls it only for Node.
# LLVM 18 cannot add/rename COFF symbols/sections. GNU objcopy can, but adding
# the symbol and renaming its section must be separate passes.
for step in constructor lazy; do
  if [[ $step == constructor ]]; then
    options=(--add-symbol mira_go_constructor=.ctors:0,global)
    input="$work/go-objects/go.o"
  else
    options=(--rename-section '.ctors=.rdata$mirago,alloc,load,readonly,data,contents')
    input="$work/go-objects/go-constructor.obj"
  fi
  if command -v x86_64-w64-mingw32-objcopy >/dev/null; then
    x86_64-w64-mingw32-objcopy "${options[@]}" "$input" "$work/go-objects/go-$step.obj"
  else
    docker run --rm --network=none -v /tmp:/tmp officecanon/qemu-mingw-zstd:fedora41-pinned \
      x86_64-w64-mingw32-objcopy "${options[@]}" "$input" "$work/go-objects/go-$step.obj"
  fi
done
if [[ -n ${MIRA_OPENSSH_GO_OUTPUT:-} ]]; then cp -a "$work/go-objects" "$MIRA_OPENSSH_GO_OUTPUT"; fi
echo "Windows relocatable Go objects: $work/go-objects"
