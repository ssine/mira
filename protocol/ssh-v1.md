# Mira SSH v1 — embedded OpenSSH

Mira embeds OpenSSH and the shared Go Node runtime in one linked executable.
There is no alternate Go SSH/SFTP implementation, extracted executable payload,
system sshd dependency, system SSH config mutation, extra password or public port 22.
`mira ssh`, `mira scp` and `mira sftp` are CLI commands, not dynamic tools.

## Identity and transport

```text
approved Node A: Mira CLI → embedded ssh → Mira ssh-proxy
                                             │ outbound WSS
                                   Mira Server byte relay
                                             │ outbound WSS
approved Node B: supervisor → private worker → embedded sshd
                                                 ├─ shell / PTY
                                                 └─ native SFTP
```

Each connection has an independently supervised worker. Linux and Android use
sshd inetd stdio. Win32 OpenSSH uses a random **127.0.0.1-only** listener per worker
because its pipe-based inetd path did not pass validation. It is not LAN-accessible.
The Server coordinates identity and relays encrypted bytes; it does not decrypt
file/terminal contents or log into an operating-system SSH service.

Both ends use the existing approved Node credential. HKDF-SHA256 derives distinct
Ed25519 seeds: IKM is the decoded 32-byte secret, salt the lowercase credential UUID,
info `mira/ssh/v1/host` or `mira/ssh/v1/client`, output 32 bytes. These domains must
not change when updating or switching Android app/root mode. PostgreSQL stores
immutable public keys only (migration 9). Keys rotate with the Node credential.

The CLI pins the exact target host key returned by the authenticated Server. The
target accepts only the exact approved caller public key and proof of possession.
The SSH account is the **target Node's current OS account**, not a selectable user.
Public-key-only authentication and StrictModes stay enabled. No password/PAM login,
user RC or X11 is enabled. The approved Nodes remain mutually trusted; this is not
a multiuser account-login service or a privilege sandbox. A compromised Server key
authority or approved Node remains in the trust boundary.

Mira generates owner-private temporary keys/config per invocation/session. The
durable source of those keys remains the protected Node identity. Temporary files
are removed on orderly completion, but a hard crash can leave private session state;
do not describe this as secret-free disk storage. Secrets never enter argv, URLs,
Server audit records or the public-key registry.

## HTTP and WebSocket API

All SSH HTTP endpoints require an approved **Node** bearer credential, not an admin cookie.

| Method / path | Purpose |
| --- | --- |
| `POST /v1/nodes/{ownNodeId}/ssh/keys` | Register `{hostKey, clientKey}` idempotently; reject changes for the same credential |
| `GET /v1/nodes/{targetNodeId}/ssh/keys` | Return `{hostKey, username, protocolVersion: 1, backend}` without creating a session |
| `POST /v1/nodes/{targetNodeId}/ssh/sessions` | Create `{sessionId, hostKey, username, protocolVersion: 1}` and send `ssh.open` to the target |

Registration rejects invalid/noncanonical/non-Ed25519 or overlapping keys (`400`),
wrong/revoked credentials (`403`) and silent key changes (`409`). Session creation
requires registered keys and an online target; unavailable targets return `409`,
capacity exhaustion `429`. Server validates the reported OS account before dispatch.

`ssh.open` contains `{sessionId, sourceNodeId, clientPublicKey}`. Target and source
connect to `/v1/ssh/sessions/{sessionId}/target` and `/source`, with subprotocols
`mira-ssh-v1` and `auth.<base64url(full-node-token)>`. Session IDs route streams;
they are not bearer credentials. Each side authenticates its exact Node/credential
and can attach once. The worker bootstrap arrives over a private anonymous pipe.

- Binary WSS frames, maximum 64 KiB; boundaries have no SSH protocol meaning.
- Bounded streaming/backpressure. Bulk data bypasses JSON control buffers.
- WSS compression is disabled; native SSH compression is available with `-C`.
- Both sides must attach within 30 seconds; SSH LoginGraceTime is 15 seconds.
- Relay defaults: 128 global connections, 32 involving one Node. Configure with
  `MIRA_SSH_MAX_SESSIONS` and `MIRA_SSH_MAX_SESSIONS_PER_NODE`.
- The Node also bounds independently supervised inbound workers. OpenSSH owns
  per-connection channels and its own limits; limits are not unbounded.
- Native keepalive options can be supplied with `-o`; do not assume the old Go
  client's keepalive behavior applies.
- Control disconnect/replacement, revocation, Server shutdown or data failure
  closes the stream. `ssh.close` cancels the worker. Windows Job Objects and Linux
  subreaper handling reap descendants. No transport replay or resumable session queue.

SSH owns EOF, stderr, exit status, PTY resize and channel flow control. A broken
upload may leave a partial file. Retry deliberately. Audit records retain session
metadata, not commands, file names, terminal output or private credentials.

## Filesystem and CLI semantics

Native SFTP has the Node OS user's filesystem scope. If `allowedRoots` is explicitly
narrowed, **SSH is rejected**, rather than silently bypassing that setting. Mira's
JSON file capabilities still honor the narrower roots. Rootless Android still has
Android permissions/SELinux restrictions; `/` does not imply root privileges.

```sh
mira ssh homeserver -- 'uname -a'
mira ssh -tt windows-node -- 'powershell.exe -NoLogo'
mira ssh -N -L 18080:127.0.0.1:8080 homeserver
mira ssh -M -S /tmp/mira-control -o ControlPersist=60 homeserver
mira scp -rp ./folder phone:/sdcard/Download/
mira scp phone:/sdcard/Download/file ./copy
mira sftp -b commands.sftp nas
mira scp ./file 'android:phone::/sdcard/Download/file'
```

Native SCP/SFTP overwrite, recursion, batch and metadata semantics apply; the old
`scp --overwrite` and `sftp <node> ls ...` forms are removed. Use interactive SFTP
or `sftp -b`. Windows remote paths use `/C:/Users/...`; Windows command strings use
the target shell's syntax. Native SSH options are accepted except Mira-owned
identity/account/host-key/config/ProxyCommand/endpoint settings. Combined flags
cannot bypass these restrictions. Transfers cannot address two different Mira
Nodes in a single SCP command; run from the source Node or stage locally.

The default shell/home come from the native OS account and OpenSSH platform port.
This is independent of managed Codex `defaultCwd`. `-t/-T`, exit status and terminal
mode/resize handling are native OpenSSH behavior. `--json` is not a stream format.
`mira-node cli ...` is the same embedded CLI entry point, also available inside the
Android image; no Termux or second Go binary is required.

TCP forwarding and Unix ControlPersist are tested. This does not promise every
OpenSSH extension, agent forwarding, PKCS#11/FIDO device, Windows multiplexing,
system `~/.ssh/config` integration or SSH ecosystem wrapper has been validated.

## Packaging and upgrades

Source/build/patch/test ownership is [node/openssh](../node/openssh/README.md).
All role names must resolve to the same running image. Desktop role links live
inside an immutable version directory; only Mira's two public launchers enter PATH.
Windows ZIP contains one PE; installation creates NTFS hard links. Android retargets
private role symlinks after an APK upgrade. No system sshd or system SSH config is changed.

Native package manifests include image SHA-256, platform, role list and pinned
source digests; dependency notices accompany releases and the APK. Packaging refuses
plain Go development binaries. A plain `go build` remains useful for common-node
development and cross-compilation, but advertises no usable embedded SSH.

Update Server first, then linked Node packages. The Server retains old v1 wire
metadata compatibility for rolling upgrades, not an old SSH implementation. Existing
identity/configuration and old version directories are retained. `mira update`
checks pending/active SSH at both endpoints along with process/PTY/App Server state;
this remains an advisory preflight, not an atomic distributed drain lock.
