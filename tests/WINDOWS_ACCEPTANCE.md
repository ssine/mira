# Windows and release acceptance — 0.9.x

0.9.1 follow-up: the legacy current-user task exposed MSIX AppData/HKCU virtualization when launched
from the packaged Codex app. Current Windows installs use `%USERPROFILE%\.mira` for state and one
Windows Service for Supervisor ownership. Existing device identity was retained during the migration.

0.9.2 follow-up: domain enrollment exposed shared TLS configuration between net/http and Gorilla.
The reverse-channel dialer now clones the configuration and advertises HTTP/1.1 only. The automated
regression test first negotiates HTTP/2 with a TLS test server and then requires a successful WSS
hello exchange; it runs on Linux and Windows.

0.9.3 follow-up: the legacy Windows login task found a Server reconnect race: the old
socket's delayed close marked its replacement offline despite fresh heartbeats. Stale sockets can
no longer change successor state or consume its responses, old work is rejected at replacement,
and per-Node status writes are serialized. Regression tests cover delayed close, active requests,
App Server proxies and delayed database completion (`go test ./internal/miraserver/channel`).

Verified on native Windows 11 x64 from WSL → Windows PowerShell, not Windows binaries running
inside a Linux compatibility shim:

- enrollment/approval and authenticated reverse channel;
- shared CLI identity, protected credential ACL;
- Unicode file create/read/stat/list/move/remove and all visible drive roots;
- native system process enumeration/count, managed process start/poll/terminate;
- CPU/memory/disk/network telemetry;
- real ConPTY, PowerShell interactive input, VT output, Ctrl-C interruption;
- resize to 132 × 37, confirmed from inside the child console;
- official npm Codex executable discovery (`codex-cli 0.152.1`), App Server start/health/stop;
- no model inference or production Codex conversation was created by this test;
- system-service Supervisor installation, with unchanged identity/configuration across Supervisor updates;
- checksum failure refusal and retained old binary directory.

Reproduce the native Node acceptance against a **local test Server** configured with the usual
local test administrator password (never point these credentials at production):

```powershell
.\tests\windows_node_e2e.ps1 -BinaryDirectory C:\path\to\extracted\release -ServerUrl http://127.0.0.1:18787 -TestAppServer
```

Windows installation ownership, service command generation and rollback semantics are covered by
native Go tests in `node/internal/installation` and `node/internal/supervisor`. The cross-platform
`tests/installers_e2e.mjs` covers first Linux node/server bootstrap and explicitly verifies that the
removed bootstrap `--update` path cannot mutate an installation.

Windows native Go tests also run in GitHub CI. Linux Go tests, race detection, authentication and
Web console E2E passed locally. Android arm64 cross-build, debug APK, release APK and APK signature
verification passed; no Android device was connected for this release's new update-button UI test.
