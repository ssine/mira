# Mira Node for Android

Users should install the signed APK from [GitHub Releases](https://github.com/ssine/mira/releases/latest).
The app displays its unified Mira version and provides **Check for updates**. Android confirms
installation and verifies the existing signing identity; official upgrades retain app data and Node
credentials. Migrating from a debug-signed APK requires a one-time reinstall and enrollment.
See [INSTALL.md](../../INSTALL.md) for the complete installation and update flow.

The Android distribution of Mira Node is one deployable APK with two internal components:

- `app/` is the Android application module. It owns the Activity, foreground service,
  AccessibilityService, MediaProjection permission flow, boot receiver, configuration UI and
  native process lifecycle.
- `../internal/node/` is the shared Go data plane also used by Windows, WSL and Linux. It owns the
  Mira control protocol, reverse WebSocket, heartbeat, status, file and process capability dispatch.

The root `build-android.sh` cross-compiles the same `mira-node` command as an Android ARM64
executable. The application packages it as a native asset and starts it as a child process. This
separation is intentional: the same executable can run under the application UID or under a root
UID granted by KernelSU, Magisk or APatch.

In non-root mode, Android framework operations flow through a token-protected loopback bridge:

```text
Mira Control Server
        |
        | WebSocket
        v
Mira Node data plane (app UID)
        |
        | 127.0.0.1 + per-launch token
        v
Java app layer
        |
        +-- AccessibilityService
        +-- MediaProjection
```

In root mode, the Java service launches the same Go executable through `su`. File, process and
system screen operations then execute directly under the granted root identity.

The file browser starts at `/` in both modes. Android's application UID still receives permission
errors outside files made visible by Android grants; root mode can traverse the complete filesystem.

The APK does not need a shared Server token. Before its first request the embedded Node creates and
persists its own credential, submits only the credential hash with an enrollment request, and
reports the six-digit verification code in the app status. After an administrator approves the
request, the same credential becomes usable and the returned Node ID is atomically added to the
identity in the application's no-backup directory. `Reset identity and enroll again` deletes it;
the Server-side Node record remains revoked or historical until the administrator manages it.

Build from this directory with Android SDK 35, the NDK version pinned in `NDK_VERSION`,
JDK 17+, Gradle 8.13+, Node.js 22 and Go 1.26.6+:

```bash
sdkmanager "ndk;$(cat NDK_VERSION)"
ANDROID_HOME=/path/to/android-sdk gradle :app:assembleDebug
```

The Gradle `preBuild` dependency compiles the shared Go module automatically and includes the
resulting ARM64 binary in the APK. Build outputs under `app/build/`, `build/`, `.gradle/` and
`../dist/` are ignored by Git.

If `dl.google.com` fails during TLS negotiation, the pinned r27d NDK is also available from
Google's official `https://dl-ssl.google.com/android/repository/android-ndk-r27d-linux.zip`.
Verify the archive against the [official r27d release](https://github.com/android/ndk/releases/tag/r27d)
(Linux SHA1 `22105e410cf29afcf163760cc95522b9fb981121`, size 663956036 bytes), extract it, and
set `ANDROID_NDK_HOME` to that directory. Keep TLS verification enabled; changing DNS or disabling
certificate checks is not required for this fallback. The local incident was consistent with
domain/SNI-dependent network handling, not a missing NDK or missing CA certificate.

Android production builds must enable cgo and `netcgo`, using the NDK's API 26 compiler.
Android does not expose desktop-style `resolv.conf`; pure-Go DNS can try localhost:53 and fail
even while Java networking works. The NDK-linked system resolver follows Android networking in
both root and app mode. No hard-coded DNS server, Server IP or disabled TLS verification is used.
`scripts/check-android-build.mjs` rejects accidentally packaged pure-Go binaries. A `CGO_ENABLED=0`
Android cross-compile remains useful only as a shared-code compile check, not a deployable APK.

The Release workflow can be dispatched with `publish=false` to produce signed acceptance artifacts
without a tag/release; install those on a real device before tagging the release. Verify a domain
Server URL, not just an IP address, along with screen/file/process capabilities and in-place update.

Root-created Node identities inherit the app-private directory's owner and SELinux label, retaining
0600 permissions. Switching root/app mode must keep the same credential and approval; never solve
ownership failures by making credentials world-readable or disabling SELinux. A root launch also
repairs identities created by earlier builds without replacing their content.

Enable **Start after reboot or app upgrade** to opt into recovery. Some OEMs (including the tested
ColorOS device) block the package-replaced broadcast unless Auto-launch/background activity is
allowed in system app settings. Opening Mira resumes an opted-in node when the service is absent;
an explicit **Stop** remains stopped. The UI must not display a persisted "Connected" status when
the service no longer exists. **Android app settings** opens the platform configuration page.
This is not a promise that every OEM will keep an app alive indefinitely.
