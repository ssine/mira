# Windows and release acceptance — 0.9.0

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
- per-user Scheduled Task installation and upgrade, with unchanged identity/configuration;
- checksum failure refusal and retained old binary directory.

Reproduce the native Node acceptance against a **local test Server** configured with the usual
local test administrator password (never point these credentials at production):

```powershell
.\tests\windows_node_e2e.ps1 -BinaryDirectory C:\path\to\extracted\release -ServerUrl http://127.0.0.1:18787 -TestAppServer
```

On WSL, run `scripts/build-release.sh dist`, then
`MIRA_TEST_WINDOWS_SERVICE=1 node tests/installers_e2e.mjs`. The service test refuses to replace an
existing `MiraNode-<username>` task. Portable install tests do not require that flag. The test uses
a synthetic previous-version label, not a claim that a public 0.8.999 release exists.

Windows native Go tests also run in GitHub CI. Linux Go tests, race detection, authentication and
Web console E2E passed locally. Android arm64 cross-build, debug APK, release APK and APK signature
verification passed; no Android device was connected for this release's new update-button UI test.
