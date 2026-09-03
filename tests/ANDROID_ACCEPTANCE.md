# Android APK acceptance

## Mira 0.9.4 — physical Android 15 / arm64 device

Tested on a OnePlus Ace 2 Pro with an existing KernelSU installation. Only Mira received root
authorization; the root manager and unrelated modules were retained. The old debug APK, Mira
runtime files and test data were removed before installing the official signing identity.

Signed candidate `a623df6` passed the following tests through the public HTTPS domain and the
Node's outbound WSS channel. ADB was used for installation and Android UI configuration, not for
the capability requests under test.

- Enrollment was approved after checking the verification code on the device and Server.
- Root mode: system status, memory, process count, filesystem roots, temporary Unicode file
  write/read/move/stat/remove, managed process start/poll/SIGTERM all passed.
- Screen: PNG signature and display size verified; a remote tap opened Mira's privilege selector,
  then a remote Back key dismissed it. UI hierarchy confirmed the actual result.
- App-only mode: domain reconnect, status, app-private file operations and managed processes
  passed with a non-root UID. Android exposed only the permitted process subset.
- Without Android screen grants, app mode returned `screen_capture_permission_required`.
  Listing `/data/system` returned permission denied, as expected.
- Root-created identity retained mode `0600`, inherited the app directory's UID/GID and complete
  SELinux label, and remained usable in app mode with the same Node ID and approval.
- In-place signed APK replacement retained configuration, Android UID and root authorization.
  One native Node process was observed after launch and mode switch.

The reusable test is `android_domain_e2e.mjs`, exporting `verifyAndroidDomainNode`. Supply an
authenticated `request(path, body?)`, HTTPS domain, explicitly approved node key and expected
version. Use `mode: "root", screen: true` with Mira foreground and the privilege selector visible;
use `mode: "app", screen: false` to test non-root core capabilities without screen grants. The
test creates a unique temporary directory and removes it in `finally`; never aim it at user files.

## Recovery limitation found during testing

ColorOS explicitly blocked `MY_PACKAGE_REPLACED` in its startup manager. A successful APK upgrade
therefore did not imply automatic service recovery. Allow Auto-launch/background activity in
Android settings where needed. Mira also resumes when opened if auto-start is enabled and the
user has not pressed Stop; absent services must not render a stale persisted Connected status.

Candidate `d5360ef` verified that fallback: after in-place upgrade, opening Mira alone reconnected
the same approved Node, and the full root capability suite passed again. Pressing Stop terminated
the native process; leaving and reopening the Activity kept the Node offline and displayed
Stopped by user. Save and start explicitly resumed it. Root capability calls also passed while
the Activity was in the background. ColorOS's per-app background battery restriction was relaxed
through its normal confirmation UI; this alone did not unblock the separate upgrade broadcast.

Not claimed by this run: reboot recovery, indefinite background survival, cellular/Wi-Fi handoff,
non-root Accessibility/MediaProjection end-to-end, all Android OEMs or all root providers.
