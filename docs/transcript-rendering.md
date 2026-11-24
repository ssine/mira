# Rendering long conversations

The Web console holds a whole conversation in the page (see
`conversation-index.md`). One Codex conversation measured in November 2026 had
41,000 records: 9,000 displayed items, 3,700 rows and 6,700 tool calls. Work per
read, scroll or streamed token must therefore not grow with its length, and a
message inside the reading area must never be empty.

## Reconciliation

`renderTranscript` runs on every background read of a running conversation.

- `mergeTranscriptItems` keeps the previous item object when a re-read record is
  unchanged. `card._miraItem` names the item a card shows; an identical object is
  skipped. A live notification resets it, so stored content wins again later.
- The desired rows are built in memory and `placeRows` touches only rows that
  differ. Cards, groups, expanded details, decoded images and mounted tool rows
  keep their elements. Nothing is detached and rebuilt.
- A streamed message whose stored record arrives under another key keeps its
  card; the old key remains an alias for late notifications.
- Footers and group summaries are written for changed turns and groups only.
- An outline tool range is one page whatever snapshot its cursor names. It is
  re-read when the tool gained records, not once per background read.

## Sections

Browsers lay out every sibling when one of them changes (about 10 ms for 3,700
rows on a desktop, several times that on a phone, repeated for each forced
layout). Rows above the tail are bundled into `.trace-section` elements of about
50 rows with layout containment; the last 120 rows stay flat and short
conversations have no sections. A row never leaves its section, and rows that are
in view or focused are not moved into one. Use `traceRows()` rather than
`#conversationTrace.children`. The scroller is a block: a nested flex container
would lay its whole content out twice.

## What is rendered

`ViewportRenderer` fills prose and tool bodies within half a screen of the
viewport. Near rows are found by binary search over current geometry in the same
frame as a scroll, resize, size change or reconciliation, before it paints. There
is deliberately no IntersectionObserver: its notifications arrive after the
paint, can be starved by other work, and disagree with geometry that the callback
itself changes; each left messages empty beside intact tool summaries.

All reads precede all writes. Code that prepares offscreen rows must not read
layout per row: one forced layout per card took 10 seconds to open the
conversation above on a desktop. Use the renderer's cached `width`.

## Copies and projections

- Transcripts above 1 MiB are rewritten to IndexedDB at most every 30 seconds
  while they grow; switching conversations or hiding the page stores the latest.
- `claudeTrace` is linear in the session's events, because a live repaint
  projects all of them.
- The timeline draws adjacent spans too narrow to tell apart as one element and
  redraws a growing span when it gains half a pixel.

`tests/long_conversation_browser.mjs` checks element identity across background
reads, sections, single tool-page loads, and that no painted frame leaves a
message in view empty, on a slowed CPU.
