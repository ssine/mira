# Mira SSH v1 (0.10.0)

## Scope and trust

SSH is an additional **real SSH-2** transport, not a translation into JSON capability calls.
`mira ssh`, `mira scp` and `mira sftp` include the client. No external `ssh` installation,
port 22, system sshd, Tailscale, extra password, or extra APK is required.
It does not replace Mira's Node control channel or App Server protocol.

All approved Nodes retain the existing mutual-trust policy. An administrator manages enrollment
and revocation, but an administrator browser session alone is not an SSH client identity.
The only SSH username is `mira`: commands execute as the **target Node's OS user**. This is not
an OS-account login service and does not grant root. Android runs as app UID or the already
authorized root identity. Worker process separation is fault isolation, **not privilege separation**.

The Server is the trusted public-key authority and connection coordinator. SSH encrypts the
data end-to-end between CLI and worker; the relay does not decrypt terminal/file contents.
This does not protect against a compromised key authority, nor against an already trusted Node
executing commands to access another Node's files. Audit records contain session metadata only,
not commands, output, filenames, private keys or full Node credentials.

## Topology

```text
mira CLI on approved Node A
  -- authenticated HTTPS: discover B, publish A's keys, request session --> Server + PostgreSQL
  -- dedicated outbound WSS: SSH bytes --> Server relay <-- dedicated outbound WSS -- Node B
                                                                  Node B supervisor
                                                                    | private stdio pipes
                                                                    + same binary: --internal-ssh-worker
                                                                        SSH handshake/session/SFTP
                                                                        native PTY or child process
```

One worker per SSH connection, up to four SSH `session` channels per worker. Worker bootstrap is
passed through an anonymous pipe, not argv or a file. Only the supervisor has a network connection
to the relay. The worker has no TCP listening port and cannot take over the control channel.

## Credentials and key continuity

Each approved Node's existing protected credential contains a 32-byte random secret and UUID.
Generate distinct Ed25519 host/client seeds with HKDF-SHA256:

- IKM: decoded Node credential secret (not the printable bearer token).
- Salt: lowercase ASCII credential UUID.
- Info: `mira/ssh/v1/host` or `mira/ssh/v1/client`.
- Output: 32 bytes, used as an Ed25519 seed.

This is domain-separated derivation, not using a bearer token itself as a signing key. Reinstalling
or switching Android privilege mode while preserving identity preserves both keys. A newly approved
credential rotates both. No additional private-key files are necessary; backing up the identity
also backs up these keys. Never export private seeds to Server/database/logs.

Database migration 9 adds `mira_node_ssh_keys(credential_id, host_key, client_key, created_at)`.
Keys are immutable for a credential. Registration rejects silent key replacement. Revoking the
Node revokes its credential and consequently both keys; historical public keys may remain stored.
CLI verifies the exact target host public key from the authenticated Server response. Worker
requires the exact caller public key supplied over its authenticated control connection, username
`mira`, and SSH proof of possession. Neither side uses password auth or skips host-key verification.

## HTTP API

All endpoints require an approved Node bearer token. URLs never contain credentials.

`POST /v1/nodes/{ownNodeId}/ssh/keys`

```json
{"hostKey":"ssh-ed25519 <base64>","clientKey":"ssh-ed25519 <different-base64>"}
```

Only canonical comment-free Ed25519 public keys are accepted. Credential ID is taken from the
authenticated principal, not the request. Returns `200 {"status":"registered"}` idempotently;
`400` invalid keys, `403` wrong/revoked identity, `409` an existing key differs. An older Server's
`404` lets a new Node continue providing pre-SSH capabilities during rolling updates.

`POST /v1/nodes/{targetNodeId}/ssh/sessions` (no body)

Caller must first publish its keys; target must have published keys and an online control channel.
Returns `201`:

```json
{"sessionId":"UUID","hostKey":"ssh-ed25519 <base64>","username":"mira","protocolVersion":1}
```

Returns `409` for missing keys/offline target, `429` for concurrency limits. The Server sends:

```json
{"type":"ssh.open","sessionId":"UUID","sourceNodeId":"UUID","clientPublicKey":"ssh-ed25519 <base64>"}
```

Target starts its worker and opens the target-side WSS. Caller opens source-side WSS. Session IDs
are routing identifiers, **not bearer credentials**. Each side must authenticate its precise Node
and credential, and can claim that side once only.

## Dedicated WebSocket transport

- Paths: `/v1/ssh/sessions/{sessionId}/source` and `/target`.
- Subprotocols: `mira-ssh-v1`, `auth.<base64url(full-node-token)>` (same credential framing as control).
- Binary messages only, maximum 64 KiB per message; message boundaries have no SSH meaning.
- No compression. Node/CLI split writes into bounded frames. Server uses bounded streaming pipes
  with backpressure in both directions, not an unbounded application queue.
- Bulk SSH bytes never enter the shared JSON control channel.
- At most 128 relay sessions globally and 8 involving any individual Node (including pending).
- Both sides must attach within 30 seconds. Worker SSH handshake is limited to 15 seconds.
- CLI sends SSH keepalives every 20 seconds, disconnecting after a 15-second reply timeout.
- Control replacement/disconnect, Node revocation, Server shutdown or data-stream failure closes
  the session. Server sends `{"type":"ssh.close","sessionId":"UUID"}` to the target.
- Closing transport cancels worker sessions and reaps children before forced termination.
  Windows supervisor also assigns the worker tree to a kill-on-close Job Object.
- No replay, reconnect-resume, durable session recovery or offline command queue. A failed upload
  may leave a partial destination file; retry deliberately, not automatically.

SSH owns channel EOF, separate stderr, exit status, resize and channel flow control. A WebSocket
closure terminates the whole SSH connection; it is not used as an individual channel EOF.

## Session features

- `shell`, `exec`, `pty-req`, `window-change`, `signal`, SFTP subsystem.
- Native Unix PTY and Windows ConPTY through `go-pty`; terminal modes through its SSH adapter.
- Unix commands use the Node's `$SHELL` (fallback `/bin/sh`); Android `/system/bin/sh`;
  Windows `%COMSPEC%` (fallback `cmd.exe`). An explicit exec string uses that shell's syntax.
- PTY sizes: 1–1000 columns, 1–500 rows. Session request payloads at most 32 KiB.
- SSH signals INT/TERM/KILL are accepted where supported; Ctrl-C in a PTY uses terminal input.
- Arbitrary environment requests, agent forwarding, X11, TCP forwarding, multiuser login and
  legacy SCP wire protocol are not supported in v1. `mira scp` deliberately uses SFTP.
- Not a full OpenSSH daemon. Mature libraries provide SSH cryptography/protocol and SFTP protocol;
  Mira implements identity, transport coordination and OS adapters.

## SFTP paths and bounds

SFTP follows the existing configured file roots and OS permissions. Defaults remain full local
filesystems; optional narrow roots are compatibility policy, not a security sandbox (shell/exec
has the Node user's full access). Paths resolving outside configured roots are rejected. Removing
or renaming a configured root is rejected. No rootless Android permission bypass is implied.

Unix/Android use absolute POSIX paths. Windows uses `/C:/Users/...`; `/` is a virtual volume list.
UNC, drive-relative and alternate-data-stream paths are rejected. Rootless Android retains normal
app/filesystem/SELinux restrictions even when a path starts at `/`.

Regular-file reads/writes stream without a total-file-size ceiling; no 4 MiB JSON cap. Up to 64
open file/directory handles per SFTP subsystem, at most 10,000 entries per directory listing.
Support: listing/stat, read/write, mkdir, remove/rmdir, non-overwriting rename, size/mode/time
updates. Append, ownership changes, symlink/hardlink creation and extended OpenSSH filesystem
operations are not implemented. One-shot stat does not consume a persistent handle slot.

## CLI

```sh
mira ssh homeserver
mira ssh homeserver -- 'uname -a'
mira ssh -t windows-node -- 'powershell.exe -NoLogo'
mira scp ./large.bin phone:/sdcard/Download/
mira scp phone:/sdcard/Download/large.bin ./copy.bin
mira scp --overwrite ./large.bin phone:/sdcard/Download/large.bin
mira sftp nas
mira sftp nas ls /srv
# Full nodeKeys can contain colons; use :: to separate such selectors from paths:
mira scp ./file 'android:phone::/sdcard/Download/file'
```

Global `--timeout` limits establishment, not session duration. `--json` is rejected for SSH stream
commands. `ssh -t` forces PTY; `-T` disables it. Without an exec command, an interactive local TTY
automatically requests a remote PTY, enters raw mode, restores the terminal on exit and forwards
resizes. Remote exit status becomes CLI exit status. Existing files are not overwritten by default.
Recursive copies and remote-to-remote operands are not yet supported.

The Node executable also supports `mira-node cli ...`. Android's APK-packaged executable can use
this entry point with `MIRA_IDENTITY_FILE` set to its protected APK identity path; it does not require
a separate CLI binary or Termux. This is a command-line entry point, not a new Android terminal UI.

Interactive SFTP: `pwd`, `ls`, `cd`, `stat`, `mkdir`, `rm`, `get`, `put`, `help`, `exit`.
Single/double quotes group paths containing spaces. The SFTP prompt is intentionally minimal.

## Validation and upgrades

Use `go -C node test -race ./internal/node -run SSH`, native Windows `go test`, and
`node tests/ssh_e2e.mjs` (isolated local database/Server/two approved Nodes).
The E2E checks binary stdin/EOF, stderr/exit, PTY, >4 MiB binary SFTP roundtrip, file policy,
revocation and child cleanup. Platform compile checks alone are not Android APK acceptance.

Upgrade Server first (append-only migration 9), then Node/CLI pairs. Old Nodes remain usable for
their existing capabilities; SSH needs the new target Node and caller CLI. No existing identity
or conversation data is deleted, and no Caddy port/SSH listener must be opened.
