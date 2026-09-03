# SSH v1 acceptance (0.10.0 candidate)

Verified on 2026-09-04. This is a candidate branch, not a production Server deployment.

| Environment | Executed checks | Result |
| --- | --- | --- |
| Linux/WSL | Two enrolled Nodes, dedicated reverse relay, independently spawned worker, shell/exec, native PTY, separate stderr/exit status, binary stdin and EOF | Passed |
| Linux/WSL | 9 MiB binary SFTP upload/download, byte equality, no-overwrite default, root policy, revocation closes live connection and reaps child | Passed |
| Standard OpenSSH client | Real SSH handshake, fixed host key, public-key auth and exec against Mira's Go server | Passed |
| Native Windows desktop | Built-in Windows CLI through local Server to native worker, ConPTY, exit code, 5 MiB SFTP, Windows → Linux exec | Passed |
| Native Windows desktop | 100 fast-exit ConPTY sessions retain their final output | Passed after output-handle lifetime fix; repeated in CI |
| Windows CI runner | Native unit tests, including SFTP using Windows short/long temporary-directory aliases | Passed after namespace fix |
| Android 15 arm64, real APK root mode | Actual UID 0, native PTY, 5 MiB binary SFTP roundtrip, APK's built-in client → Linux | Passed |
| Android 15 arm64, real APK app-only mode | Actual app UID/untrusted_app SELinux domain, same SSH/PTY/SFTP checks; `/data/system` access denied | Passed |
| Build/security | Native race tests, Go vet, Windows/Android cross-compilation, NDK/cgo/netcgo build, release archives and installer tests | Passed |
| Go vulnerability scan | Go 1.26.6, x/crypto 0.56.0, SFTP 1.13.11 | No reachable vulnerabilities found |

Android was upgraded in place with the existing release signing certificate. Its original Node
identity and Server URL were preserved. Each test started the **APK-packaged executable under the
actual current APK process UID**, using a new temporary identity and the isolated local test Server.
No shell-UID ADB process was substituted for app-UID testing. The original Auto/root setting was
restored after checking app-only mode. Temporary Node processes, directories and port forwards were
removed; the production Server, database and other installed Nodes were not upgraded.

For Android local acceptance, the desktop network blocked direct LAN access to the temporary WSL
listener. An ADB TCP reverse forward was used only to reach the local test Server. SSH still used
the real enrollment, Node control channel, dedicated WebSocket relay and worker. This proves local
APK protocol/capability behavior, not a new public-ingress deployment. The updated APK continued
connecting to the existing HTTPS domain for its normal non-SSH capabilities.

## Reproduce

```sh
go -C node test -race ./...
go -C node vet ./...
go -C node run golang.org/x/vuln/cmd/govulncheck@v1.7.0 ./...
node --test tests/node_channel_test.mjs tests/ssh_relay_test.mjs
docker compose up -d --wait postgres
node tests/ssh_e2e.mjs
```

`ssh_e2e.mjs` creates and drops its own database and two temporary Node identities. It does not
reuse production credentials. Optional `MIRA_TEST_DATABASE_URL` selects the local PostgreSQL
service; `MIRA_SSH_TEST_PORT` selects the temporary Server port. It listens on loopback by default.

For Windows through WSL, build `mira.exe` and `mira-node.exe` into a dedicated directory on a local
Windows drive, set `MIRA_SSH_WINDOWS_BIN` to its `/mnt/...` path, and run the same E2E. It runs
`windows_ssh_e2e.ps1`, creates a temporary native Node without changing the installed Windows service,
tests native worker/ConPTY/SFTP plus Windows → Linux SSH, and cleans up.

For Android, install the signed candidate APK and explicitly select root/app mode. A local module
supplied with `MIRA_SSH_DEVICE_TEST=/absolute/path/hook.mjs` receives `{url,nodes,cli,admin}` from the
isolated test harness. It can call `verifyAndroidSSHNode` from `android_ssh_e2e.mjs` with authenticated
access to the device's existing capability service and the temporary Server's reachable URL.
Provide secrets only from local protected state; never commit a device identity or administrator
password. The verifier creates its own app-private temporary identity, stops that Node before
cleanup, and does not replace the APK's normal identity. A failed stop retains test files for diagnosis.

Current non-goals: TCP/agent/X11 forwarding, multiuser OS login, recursive SCP, legacy SCP wire
protocol, automatic session recovery and a terminal UI inside the Android app. Existing JSON PTY
capability on Android is unchanged; Android's native PTY here is specifically the SSH path.
