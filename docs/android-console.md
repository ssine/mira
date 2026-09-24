# Android console and completion notifications

The Mira APK opens the existing Server console in Android System WebView. Enter the
Server's HTTPS origin in **设备设置**, start/approve the Node, then log into the Web
console as administrator. The app uses its own persistent cookie store; Edge/PWA
sessions are not imported and the Node credential does not grant administrator login.
The existing native settings screen remains available even when Server is offline.
The first version uses the one Server configured for this Android Node.

In the conversation view, open the sidebar's **Mira** menu for **设备设置**.
Other pages have a compact settings icon. No native toolbar occupies the reading
surface; connection failures expose retry and device settings even when the page
cannot load. Older Servers retain a compact native settings fallback.

The Android reading surface extends beneath the transparent status bar with a
light, theme-aware fade confined to the system status bar. The navigation row and
conversation text below the status bar remain unobscured. Native window insets keep controls below system icons and keep
the composer clear of navigation gestures, cutouts and the keyboard, including
after folding or rotating. Light/dark appearance also updates system bar icons.
Reduced-transparency and forced-color preferences use a solid top surface.

The online Web assets come from that Server. Compatible frontend changes arrive
with Server updates; changes to Android integration require an APK update. There is
no local conversation database, embedded model runtime, or offline conversation mode.

## Native notifications

Use **开启完成通知** inside the app after signing in. Android must allow the app's
notification permission and **对话完成** channel. Allow background activity and
startup in the phone's system settings if its firmware restricts Mira. The existing
foreground Node maintains the outbound connection even when WebView is closed.
No Firebase/Google push registration is involved. Force-stop, OEM process killing,
Doze and lack of network can delay delivery; this is not a platform push wakeup service.

A successful root Codex turn creates a PostgreSQL delivery in the same transaction
as the completion history. Imports, replacement histories, failures, cancellation
and subagent completions do not notify. Notifications contain the conversation title
and an ID, never the final answer. Native and browser opt-ins are independent.

The Server sends `notification.deliver` on the existing authenticated Node channel.
It contains a request ID and bounded params: delivery UUID, thread UUID, title and
expiry (Unix milliseconds). This is an internal Server event, not a device capability
or an Agent dynamic tool. The receiving Node passes the configured Server URL and
payload to its protected Android bridge. Android validates scope, consent, permission
and expiry; it posts an explicit Activity notification and replies with the ordinary
correlated `response` frame, `ok: true`, `result: {accepted: true}`. Only the targeted
Node can acknowledge it. Older Nodes ignore this extension; existing control traffic
and protocol version 1 are unchanged.

Queue entries expire after 24 hours and retry after 30 seconds without holding SQL
locks during delivery. At most 128 unfinished events per device are retained; excess
oldest events are marked finished. IDs remain stable across retry/restart. Android
keeps up to 512 recent delivery IDs until expiry and reuses the same system notification
tag if interrupted between posting and saving its acknowledgement. This bounds local
state and prevents ordinary retries from alerting twice; Android notification display
and the local dedup write cannot be one atomic transaction.

Administrator session expiry/logout, device revocation and disabling the subscription
stop future delivery and remove pending entries. Explicit app logout also disables
local delivery and clears completion notifications. The generic foreground-service
notification is separate. A newly logged-in session does not inherit the preceding
session's queue.

Notification clicks explicitly open Mira's own Activity, including cold launch.
Warm navigation uses the shared frontend handler to save text/attachment drafts.
If an editor operation or modal is busy, a visible prompt waits for the user to retry.
The first version opens notifications only for the Node's configured Server; switching
that Server does not reuse another Server's cookies, opt-in or queued navigation.

## WebView boundary

Only the exact configured HTTPS origin's main frame receives an AndroidX WebKit
WebMessage listener. The bridge exposes notification context/permission/consent,
navigation acknowledgement, system-bar appearance/insets, the device settings
Activity, and user-selected streamed downloads. It does not expose
Node tokens, passwords, arbitrary files/processes or the Go loopback bridge. External
links go to another app without the listener; untrusted navigation and mixed content
are blocked. Release WebView debugging is disabled. TLS errors are never bypassed.

Uploads use Android's document picker. HTTPS downloads remain same-origin across
redirects. Blob downloads stream acknowledged 48 KiB chunks to a user-selected document;
there is no total-size limit or whole-file JavaScript/native bridge buffer. Downloads
show progress and allow cancellation. Leaving/destroying the Activity can cancel an
in-progress download. Ordinary Web/PWA behavior continues to use Web Push.
