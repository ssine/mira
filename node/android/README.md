# Mira Node for Android

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

Build from this directory with Android SDK 35, JDK 17+, Gradle 8.13+ and Go 1.23+:

```bash
ANDROID_HOME=/path/to/android-sdk gradle :app:assembleDebug
```

The Gradle `preBuild` dependency compiles the shared Go module automatically and includes the
resulting ARM64 binary in the APK. Build outputs under `app/build/`, `build/`, `.gradle/` and
`../dist/` are ignored by Git.
