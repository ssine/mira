#!/usr/bin/env bash
# Link Go and OpenSSH objects into one APK-ready executable.
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
android_toolchain
work=${1:?Invoke the top-level OpenSSH builder}
[[ $work =~ ^/tmp/mira-openssh-android\.[A-Za-z0-9]+$ ]] || exit 2
ssh_build="$work/build"
test -f "$ssh_build/libssh.a"
crypto=$(cd "$ssh_build/../crypto/install/lib" && pwd)
mkdir -p "$work/combined" "$work/go-link" "$work/bin"
CGO_ENABLED=1 GOOS=android GOARCH=arm64 CC="$cc" go -C "$work/node" build \
  -buildmode=c-shared -tags=netcgo -ldflags="-tmpdir=$work/go-link $metadata" \
  -o "$work/libnode-not-shipped.so" ./cmd/mira-node > "$work/go-build.log" 2>&1
# Reuse unlinked object files, never embed/extract a prelinked shared library.
mapfile -t go_objects < <(find "$work/go-link" -type f -name '*.o' | sort)
((${#go_objects[@]} > 0))
for object in "${go_objects[@]}"; do
  "$toolchain/llvm-objcopy" --rename-section .init_array=mira_go_init,alloc,load,data,contents "$object" "$work/combined/go-$(basename "$object")"
done
objects=()
while IFS='|' read -r role inputs; do
  name=${role//-/_}
  # Resolve each role's archive members before hiding the role-local globals.
  (cd "$ssh_build" && "$toolchain/ld.lld" -r -o "$work/combined/$role.raw.o" $inputs -L. -Lopenbsd-compat -lssh -lopenbsd-compat)
  "$toolchain/llvm-objcopy" --redefine-sym "main=openssh_${name}_main" "$work/combined/$role.raw.o" "$work/combined/$role.renamed.o"
  "$toolchain/llvm-objcopy" --keep-global-symbol="openssh_${name}_main" "$work/combined/$role.renamed.o" "$work/combined/$role.o"
  objects+=("$work/combined/$role.o")
done < <(make -s -C "$ssh_build" -f Makefile -f "$component/common/objects.mk" mira-objects)
"$cc" -O2 -fPIE -pie -Wl,-T,"$component/common/go-init.ld" \
  -Wl,-z,relro,-z,now,-z,noexecstack,-z,max-page-size=16384 \
  -o "$work/bin/mira-node" "$component/common/dispatcher-unix.c" \
  "${objects[@]}" "$work"/combined/go-*.o "$crypto/libcrypto.a" -lz -ldl -lm -llog
for role in mira ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen; do
  ln -s mira-node "$work/bin/$role"
done
"$toolchain/llvm-strip" "$work/bin/mira-node"
"$toolchain/llvm-readelf" -lWd "$work/bin/mira-node" > "$work/elf-report.txt"
sha256sum "$work/bin/mira-node"
node "$repo/scripts/check-android-build.mjs" "$work/bin/mira-node"
node "$component/android/check-image.mjs" "$work/bin/mira-node"
node "$component/manifest.mjs" "$work/bin" android arm64 "$work"
if [[ -n ${MIRA_OPENSSH_OUTPUT:-} ]]; then cp -a "$work/bin" "$MIRA_OPENSSH_OUTPUT"; fi
echo "Linked single executable and role aliases: $work/bin"
