#!/usr/bin/env bash
# Compile upstream C objects for the single root/app-capable Android image.
set -euo pipefail
source "$(dirname "$0")/../common/build-env.sh"
android_toolchain
work=${1:?Invoke the top-level OpenSSH builder}
[[ $work =~ ^/tmp/mira-openssh-android\.[A-Za-z0-9]+$ ]] || exit 2
export ANDROID_NDK_ROOT="$ndk"
export PATH="$toolchain:$PATH"
cd "$work"
cp "$cache/openssl-3.5.5.tar.gz" openssl.tar.gz
printf '%s  %s\n' b28c91532a8b65a1f983b4c28b7488174e4a01008e29ce8e69bd789f28bc2a89 openssl.tar.gz | sha256sum -c -
tar xzf openssl.tar.gz
mkdir crypto
(cd crypto && ../openssl-3.5.5/Configure android-arm64 -D__ANDROID_API__=26 no-shared no-tests --prefix="$work/crypto/install" > configure.log 2>&1 && make -j"$jobs" build_libs > build.log 2>&1 && make install_dev > install.log 2>&1)
cp "$cache/openssh-10.5p1.tar.gz" openssh.tar.gz
actual=$(openssl dgst -sha256 -binary openssh.tar.gz | openssl base64 -A)
[[ "$actual" == '1E0oqDnqna+WnMaRUP3lmRCys5Nh2tgaO9bL0ZIY2xE=' ]] || { echo 'OpenSSH checksum mismatch' >&2; exit 1; }
tar xzf openssh.tar.gz
patch -d openssh-10.5p1 -p1 < "$component/android/patches/resolver.patch"
patch -d openssh-10.5p1 -p1 < "$component/android/patches/app-identity.patch"
patch -d openssh-10.5p1 -p1 < "$component/android/patches/app-home.patch"
cp "$component/android/root-path.h" openssh-10.5p1/mira-root-path.h
patch --fuzz=0 -d openssh-10.5p1 -p1 < "$component/android/patches/root-state.patch"
mkdir build
cd build
CC="$toolchain/aarch64-linux-android26-clang" AR="$toolchain/llvm-ar" RANLIB="$toolchain/llvm-ranlib" ac_cv_func_bzero=yes CPPFLAGS=-DHAVE_ATTRIBUTE__SENTINEL__=1 LDFLAGS=-Wl,-z,max-page-size=16384 \
 ../openssh-10.5p1/configure --host=aarch64-linux-android --with-ssl-dir="$work/crypto/install" --prefix=/nonexistent/mira-openssh --without-pam --disable-lastlog --disable-utmp --disable-wtmp --disable-utmpx --disable-wtmpx --disable-pututline --disable-pututxline --disable-libutil --disable-etc-default-login --with-default-path=/system/bin --with-privsep-user=nobody > configure.log 2>&1
flags="-O2 -fPIC -fstack-protector-strong -include $component/android/bionic-shim.h"
make -j"$jobs" CFLAGS="$flags" CFLAGS_NOPIE="$flags" ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen > build.log 2>&1
mkdir licenses
cp ../openssh-10.5p1/LICENCE licenses/OpenSSH-LICENCE
cp ../openssl-3.5.5/LICENSE.txt licenses/OpenSSL-LICENSE.txt
echo "Compiled Android API-26 OpenSSH objects: $work/build"
