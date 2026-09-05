# Web conversation read state

The single administrator's read position is shared across Web clients in
`mira_thread_read_positions`, separately from Codex history and its rebuildable
projections. A position identifies a store, thread, generation and acknowledged
raw item count. Repeated or out-of-order acknowledgments advance monotonically
within a generation. Replaced histories require a new acknowledgment; permanent
deletion removes the position under the same thread lock.

Migration 19 establishes an already-read baseline for existing histories.
Subsequent messages, completed tool results and terminal turn/error events count
as updates. Title changes, token counts, heartbeats and retry notifications do
not. The latest qualifying position is derived from canonical records, cached by
generation/item count, and can be rebuilt without changing history. The index
only selects candidates: JavaScript validates their structure to retain support
for JSON records containing escaped NUL and unknown future event types.

`GET /v1/codex/threads` and the individual thread route include
`readState: { generation, latestItemSeq, readItemCount, unread }`.
`POST /v1/codex/threads/:threadId/read?storeId=personal` accepts
`{ generation, itemCount }` and returns the acknowledged `readItemCount`.
It requires administrator authentication and CSRF proof. It rejects future
counts and stale generations and does not change thread recency or store version.

The browser acknowledges only a rendered canonical snapshot after a short
foreground dwell at the bottom. Background tabs, old-message browsing, history
gaps, modal previews and the overlaid mobile sidebar do not consume updates.
Polling transfers read positions between devices without resuming Codex or
connecting to the execution Node. An unsuccessful acknowledgment remains unread
and can be retried; a later reply can never be acknowledged by an older snapshot.

List rows use an outline/left rule for selection, pale blue for running, pale
green for idle with unread updates, and heavier title text for unread content.
State text remains present independently of color. No list animation is needed.
