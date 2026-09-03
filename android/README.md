# Mira Android

The Android implementation is one deployable APK with two internal components:

- `app/` is the Android application module. It owns the Activity, foreground service,
  AccessibilityService, MediaProjection permission flow, boot receiver, configuration UI and
  native process lifecycle.
- `library/` is the Go data plane embedded into the APK. It owns the Mira control protocol,
  reverse WebSocket, heartbeat, status, file, process and screen capability dispatch.

Despite the directory name, `library/` currently builds an Android ARM64 executable rather than an
AAR or JNI-linked shared library. The application packages it as a native asset and starts it as a
child process. This separation is intentional: the same executable can run under the application
UID or under a root UID granted by KernelSU, Magisk or APatch.

In non-root mode, Android framework operations flow through a token-protected loopback bridge:

```text
Mira Control Server
        |
        | WebSocket
        v
Go data plane (app UID)
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

Build from this directory with Android SDK 35, JDK 17+, Gradle 8.13+ and Go 1.23+:

```bash
ANDROID_HOME=/path/to/android-sdk gradle :app:assembleDebug
```

The Gradle `preBuild` dependency compiles `library/` automatically and includes the resulting ARM64
binary in the APK. Build outputs under `app/build/`, `build/`, `.gradle/` and `library/dist/` are
ignored by Git.
