#!/usr/bin/env bash
set -euo pipefail
work=$PWD
apk info -v > builder-packages.txt
printf '%s  %s\n' b28c91532a8b65a1f983b4c28b7488174e4a01008e29ce8e69bd789f28bc2a89 openssl-3.5.5.tar.gz | sha256sum -c -
printf '%s  %s\n' d44d28a839ea9daf969cc69150fde59910b2b39361dad81a3bd6cbd19218db11 openssh-10.5p1.tar.gz | sha256sum -c -
tar xzf openssl-3.5.5.tar.gz
tar xzf openssh-10.5p1.tar.gz
mkdir crypto build
case $(uname -m) in x86_64) ssl_target=linux-x86_64 ;; aarch64) ssl_target=linux-aarch64 ;; *) exit 1 ;; esac
(cd crypto && ../openssl-3.5.5/Configure "$ssl_target" no-shared no-tests no-module --prefix="$work/crypto/install" --libdir=lib > configure.log 2>&1 && make -j"$MIRA_OPENSSH_JOBS" build_libs > build.log 2>&1 && make install_dev > install.log 2>&1)
(cd build && CC=gcc CFLAGS='-O2 -fPIC -fstack-protector-strong' LDFLAGS=-static-pie ../openssh-10.5p1/configure \
  --with-ssl-dir="$work/crypto/install" --prefix=/nonexistent/mira-openssh --without-pam --without-security-key-builtin \
  --disable-lastlog --disable-utmp --disable-wtmp --disable-utmpx --disable-wtmpx --disable-libutil --disable-etc-default-login \
  --with-privsep-user=nobody > configure.log 2>&1 && make -j8 ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen > build.log 2>&1)
while IFS='|' read -r role inputs; do
  (cd build && ld -r -o "$work/combined/$role.raw.o" $inputs -L. -Lopenbsd-compat -lssh -lopenbsd-compat)
  objcopy --redefine-sym "main=openssh_${role//-/_}_main" "combined/$role.raw.o" "combined/$role.renamed.o"
  objcopy --keep-global-symbol="openssh_${role//-/_}_main" "combined/$role.renamed.o" "combined/$role.o"
done < <(make -s -C build -f Makefile -f ../objects.mk mira-objects)
PATH=/opt/go/bin:$PATH CGO_ENABLED=1 CC=gcc /opt/go/bin/go -C node build -buildmode=c-archive -tags=netgo,osusergo -ldflags="${MIRA_OPENSSH_METADATA:?}" -o "$work/node.a" ./cmd/mira-node > go-build.log 2>&1
objcopy --rename-section .init_array=mira_go_init,alloc,load,data,contents node.a node-lazy.a
objects=();for role in ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen;do objects+=("combined/$role.o");done
gcc -O2 -static-pie -Wl,-T,go-init.ld -Wl,-z,relro,-z,now,-z,noexecstack -o bin/mira-node dispatcher.c \
  "${objects[@]}" node-lazy.a crypto/install/lib/libcrypto.a -lz -lpthread -ldl -lm > link.log 2>&1
strip bin/mira-node
for role in mira ssh sshd sshd-session sshd-auth scp sftp sftp-server ssh-keygen;do ln -s mira-node "bin/$role";done
readelf -lWd bin/mira-node > elf-report.txt
if grep -Eq 'INTERP|NEEDED' elf-report.txt;then echo 'Unexpected runtime library/loader dependency' >&2;exit 1;fi
bin/mira-node --mira-dispatch-probe
bin/mira-node --version
bin/ssh -V
sha256sum bin/mira-node
