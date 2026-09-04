# Consolidation acceptance

The component is now the only source/build/release backend. This records local
candidate acceptance, **not** a published release or production deployment.

## Verified

- Go tests, including race checks, Windows/Android compile checks, JavaScript
  checks, Node-channel/SSH-relay tests and unified-version checks.
- Fresh Linux amd64 static PIE build. No ELF INTERP/NEEDED entries. The same
  image passed real Mira enrollment/relay, PTY, SCP/SFTP and reverse CLI on Alpine
  3.22.2, Ubuntu 24.04 and Rocky 9. WSL additionally exercised recursive copies,
  ControlPersist, TCP forwarding, concurrency, exit/stderr and revocation/reaping.
- Windows amd64 full native rebuild with discovered Visual Studio 2022/SDK,
  static LibreSSL/zlib/libfido2/libcbor and one linked PE. Import table contains
  only Windows system DLLs. All role names are NTFS hard links to that image.
  Tests exercised default sibling discovery, RSA/ECDSA keygen, AES-GCM/compression,
  PTY, four concurrent connections, binary SCP/SFTP, TCP forwarding, Windows ↔
  Android, Windows → Linux/self, and revocation of a remote PowerShell child.
- Actual Android app source built and installed under an independent debug ID.
  App → root → app preserved one approved identity and app-owned 0600 credential.
  SSH/PTY/exit, binary SCP/SFTP, concurrency, outbound CLI, file/process/status,
  root screenshot and revocation/reaping passed. APK reinstall preserved identity
  and retargeted role links. No system SSH service or root module was installed.
- Linux and Windows candidate archives contain one executable image, not a copy
  per role. Per-user install/update tests retained identity/config and the previous
  version, and rejected a corrupt download. No-service install paths were tested;
  this run did not replace production systemd/Scheduled Task services.

The final combined device test used temporary Mira identities and a disposable
PostgreSQL database. ADB only installed/configured/inspected the test APK and
provided a temporary development loopback mapping. SSH traffic itself used Mira's
identity and relay. This is not a new production HTTPS/ingress acceptance claim.

## Candidate image fingerprints

These identify tested development images, not published binaries:

```text
Linux amd64:   27cd5d9f60c8866797d62f3aad96c9ea1d8a9a20c7d46bf2b79b4edac1122579
Windows amd64: 90e21c2e30edbf37fb73fb2e0b0a11c75aea8b62ce105195fa6f4d89428306ef
Android arm64: a9489e050c6933a79ee14514c9c1259592de22a1c6cdf94029b9403b106c97ec
```

## Still separate from this acceptance

- CI/Release workflows were updated but not dispatched/published in this task.
  Linux arm64 has a native build lane, not local hardware acceptance here.
- Windows LLD still reports two Go cgo `__acrt_iob_func` import-to-static warnings;
  no extra CRT DLL appears in the accepted import table.
- Hardware FIDO/PKCS#11, every OpenSSH option, Windows multiplexing, arbitrary
  kernels/OS versions, background Android reliability and long-running crash/fault
  soak tests are not implied by these results.
- The Android build retains OS SELinux and upstream root pre-auth chroot/demotion,
  but does not enable an additional OpenSSH seccomp sandbox.
- Before publication, choose a new unified SemVer, execute the full release lane
  (including Linux arm64 and signed APK), and accept those exact artifacts. Do not
  overwrite an existing immutable release version with these candidate contents.
