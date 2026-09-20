# Conversation sidebar layout

The Mira menu in the conversation sidebar offers three layouts. This is a browser
preference, independent of screen orientation, device type and thread state.

| Preference | Effective layout |
| --- | --- |
| Auto (default) | Docked at a viewport width of 1100 CSS pixels or more; drawer below it |
| Docked | Docked at 720 CSS pixels or more; temporary drawer below it |
| Drawer | Overlays the conversation at every width |

The 720px minimum leaves 480px for the conversation beside the minimum 240px
sidebar. The effective sidebar width is clamped to the available space, up to
480px. Temporary clamping and drawer fallback preserve the user's preferred width
and layout. The menu explains when a docked preference temporarily uses a drawer.

The sidebar button controls visibility without changing the preference. Choosing
a layout from the menu keeps the sidebar visible. Resizing into an overlay closes
it; returning to a docked layout restores its last expanded/collapsed state in the
page. Changing width without changing the effective layout preserves visibility.
A fresh page starts with docked sidebars expanded and drawers closed.

Layout and width use `mira.sidebar.layout` and `mira.sidebar.width` in localStorage.
Unknown layout values fall back to Auto. Storage failures retain usable in-memory
controls, and a layout change in another tab is applied through the storage event.

CSS columns, the backdrop, resize handle, swipe gestures, navigation auto-close
and conversation read visibility all follow the effective layout. The right-hand
details panel keeps its own 1100px breakpoint: forcing a left drawer must not
reserve a phantom third column. Docked sidebars do not intercept the conversation's
gesture for opening details. Resizing or changing layout cancels an active drag.

`tests/sidebar_layout_browser.mjs` covers layout selection, geometry, persistence,
fallback/restore, manual collapse, width clamping, conversation navigation, right
details, keyboard use and unavailable storage. The existing shell, swipe and
token-usage browser regressions also exercise the shared controls.
