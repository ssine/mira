# Embedded OpenSSH component

This is the only Mira SSH implementation. It is a Node component, not a parallel
application or experiment. Go owns Mira identity, registration, reverse WSS and
worker supervision; upstream C owns SSH/SFTP, terminal sessions and transfers.
No Rust rewrite or custom SSH protocol implementation is needed.

```text
node/
  internal/ssh_*.go   Mira transport, key derivation, native adapter, supervision
  openssh/
    common/               C dispatch, lazy Go startup and object-link helpers
    linux/                static musl/OpenSSL builder
    windows/              MSVC compilation, static dependencies, COFF linking
    android/              Bionic/app/root patches and NDK linking
    tests/                real Mira relay and device regression tests
    sources.json          pinned upstream archive digests
    build.sh              platform build entry point
    manifest.mjs          artifact provenance and license notices
    stage-package.mjs     single-image release staging
    verify-package.mjs    reject incomplete or non-linked bundles
  android/            the only Android application source
```

The executable dispatches by its role name **before starting Go**. This avoids
forking OpenSSH out of an already multithreaded Go process. `mira` selects the Go
CLI; `mira-node` selects the Go service; SSH role links select native C entrypoints.
The Go bridge is a `.go.in` template injected only into native build snapshots.
Plain `go build`/cross-compilation remains a development check, without usable SSH.

## Build

From the repository root, populate Go dependencies with `go -C node mod download`.
Source downloads are verified against `sources.json`; set `MIRA_SOURCE_CACHE` to
reuse a cache. Changed upstream archive bytes fail closed. No production credentials
or system SSH configuration are involved.

```sh
bash node/openssh/build.sh linux
bash node/openssh/build.sh windows
bash node/openssh/build.sh android
```

- Linux: native amd64/arm64 host, Docker and Go. OpenSSL, zlib and musl are static;
  no ELF interpreter or dynamic library dependency. The normal OS still supplies
  the shell, accounts, CA certificates and network configuration. Static musl is
  not glibc NSS/LDAP/PAM plugin compatibility. ARM64 has a native CI build lane;
  do not infer real hardware acceptance from an amd64 container test.
- Windows: WSL orchestrates native Visual Studio 2022 C++/SDK plus LLVM 18 tools.
  CMake builds static LibreSSL/zlib/libfido2/libcbor with `/MT`; MSVC builds the C
  objects, LLD links their COFF with Go. Set `MIRA_LLD`/`MIRA_LLVM_BIN` if needed.
  `MIRA_WINDOWS_BUILD_TOOLS`/`MIRA_WINDOWS_SDK_VERSION` override discovery inside
  PowerShell. GNU MinGW is used for cgo; an existing container toolchain is a local
  fallback. CI separates Windows C compilation and Linux cross-linking jobs.
- Android: Linux/WSL, pinned NDK, Go and Node.js. One API-26+ ARM64 ELF, linked crypto,
  Android OS libc/libdl/libm/liblog/libz. Non-root and authorized root share one
  image. The extra OpenSSH seccomp sandbox is not enabled; Android SELinux stays on.

Set `MIRA_OPENSSH_OUTPUT=/absolute/new/bundle` to copy a finished bundle. Builders
retain their isolated temporary workspace and logs for diagnosis. They do not install
a service or replace existing Nodes. Source changes must go through these builders;
never set the Go `BundledOpenSSH` marker on an ordinary release executable.

Android's Gradle build calls `node/build-android.sh`, which builds this component by
default. `MIRA_ANDROID_OPENSSH_BUNDLE=/absolute/bundle` can reuse a verified build.
The one APK source can use the independent debug application ID described in
[Android README](../android/README.md); the old standalone test application is removed.

## Distribution and updates

`scripts/build-release.sh` requires complete verified bundles under
`node/openssh/out/{linux-amd64,linux-arm64,windows-amd64}` (or the corresponding
`MIRA_OPENSSH_LINUX_AMD64`, `MIRA_OPENSSH_LINUX_ARM64`, `MIRA_OPENSSH_WINDOWS_AMD64`
overrides). No implicit plain-Go fallback. `MIRA_RELEASE_TARGETS` can select an
explicit subset for local packaging tests; official Release builds require all.

The optional container Node consumes the same verified native bundle. Build it
from the repository root with `docker build -f node/Dockerfile .` after placing
the matching architecture in `node/openssh/out/`. It does not compile a second
plain-Go implementation or install a system SSH service.

Linux tarballs contain one executable and role symlinks. Windows ZIP contains one
PE, manifest and notices; the per-user installer creates NTFS hard links before
switching the version pointer. Link creation failure aborts installation. Public
PATH contains only Mira launchers, never replacements for system `ssh`/`sshd`.
Android uses private symlinks into its current APK native-library directory.
Identity/configuration live outside versioned binaries and survive updates.

The marker and manifest are build guards, not a substitute for signed distribution.
Release archive SHA-256 and Android signing verification remain in the install path.
Manifests include source digests and dependency notices. Upstream patch drift must
be reviewed; this is not a claim of bit-for-bit reproducibility across toolchains.

## Regression entry points

```sh
MIRA_TEST_LINUX_SINGLEFILE=/absolute/bundle/mira-node \
  node node/openssh/tests/e2e.mjs
```

The harness creates and drops a temporary PostgreSQL database and approved test
Nodes. `MIRA_TEST_DATABASE_URL` defaults to a **local disposable** database on port
55433. Never point it at production. Device hooks are explicitly selected with
`MIRA_OPENSSH_DEVICE_TEST`:

- `node/openssh/tests/linux-distros.mjs`: same image in Alpine, Ubuntu and Rocky.
- `node/openssh/tests/windows.mjs`: native Windows, PTY, full crypto, transfer,
  forwarding, bidirectional clients and revocation. Set `MIRA_TEST_WINDOWS_BIN`
  to one linked bundle (WSL path); it tests automatic sibling-role discovery.
- `node/openssh/tests/android.mjs`: actual APK app → root → app and optional Windows
  peer; requires explicit test device, test package and already-authorized root.
  Set `MIRA_TEST_ADB_SERIAL` and `MIRA_OPENSSH_TEST_PUBLIC_URL` for the fixture.

ADB is installation/configuration/inspection only, not the tested SSH channel.
Set `MIRA_TEST_WINDOWS_TRAY=1` with the Windows hook to run its Node in tray mode
while exercising the same linked-image SSH, ConPTY, transfer and revocation tests.
`tests/windows_tray_e2e.ps1 -Binary <image> -Installer scripts/install.ps1` separately
tests the installer launcher in an isolated user task, window close/reopen, duplicate
launch, log output and confirmed exit without replacing the installed Node task.
Tests exercise Mira enrollment, identities and reverse relay. See
[protocol/ssh-v1.md](../../protocol/ssh-v1.md) for boundaries and CLI changes.
