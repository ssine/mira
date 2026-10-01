# Web client cache

The Web console renders the last known state immediately and revalidates it in the
background. PostgreSQL remains the only source of truth: every browser copy is
disposable, may be stale, and is replaced by the next successful Server read.

## Storage

`client-cache.js` stores copies in the IndexedDB database `mira-client-cache`:

- `snapshots` hold small whole-view values: the root conversation list (at most 400
  non-archived rows with pager projects), the Node list, model catalogs per Node and
  account (at most 24), cost estimates (at most 300) and residency observations.
- `entries` hold per-conversation transcripts, with `meta` recording each entry's
  size and last use. Entries are evicted least recently used first once the total
  exceeds 96 MB or 400 entries; a single entry larger than 16 MB is not stored.

Every record carries the Server version reported by `/healthz`. A different version
never reads it, so projection changes between releases cannot surface old shapes.
Writes are coalesced per key and flushed after one second, when switching
conversations, and when the page is hidden. A write may be a function evaluated at
flush time; it returns `undefined` to keep the previous copy when the current state
is inconsistent. Signing out clears the whole database. Storage failures (quota,
private browsing) only lose copies.

## What is shown from cache

- **Sidebar and Nodes.** After login the cached list, Node selectors, residency and
  costs render before the first request completes. Cached rows that the first roots
  read does not confirm are dropped. Cached rows never restore running state: the
  activity indicator waits for the Server. Opening a conversation whose row is
  still an unconfirmed copy reads its metadata (account, Node, title) first and
  falls back to the copy only when that read fails.
- **Transcripts.** A Codex transcript is saved only when it has no history gap and a
  known tail version; on reopen it is shown and then synchronized incrementally
  from the cached cursor. A generation change or an item gap larger than 600
  discards the copy. A Claude transcript is saved only while no turn is running; on
  reopen the cached rows are shown, and the first latest-page read keeps the older
  cached rows only when the new page overlaps them.
- **Models.** A cached catalog is used immediately and refreshed in the background
  after five minutes; explicit refresh always reads the Node.
- **Residency.** The cache circle keeps its last observation. After three minutes or
  a failed check it is dimmed (`data-stale`) with an explanatory tooltip instead of
  disappearing. Visible rows are rechecked every 60 seconds, the open conversation
  every 15 seconds.

## Request reduction

- Node reads are shared by concurrent callers and reused for 15 seconds during
  navigation and 5 seconds in dialogs and the account sidebar.
- Sidebar cost estimates for an unchanged usage snapshot are reused for five
  minutes; rows with the same generation but new usage wait at least 30 seconds.
  The open details panel still rechecks every 10 seconds because descendants can
  keep running under an unchanged parent.
- Push subscriptions are re-registered only when the endpoint changes or the last
  registration is older than 12 hours.
- Startup requests the administrator session and `/healthz` in parallel, and starts
  the thread list and Node reads without waiting for each other.

`tests/client_cache_browser.mjs` checks version isolation, lazy writes, oversized
entries, LRU eviction, removal and clearing against Chromium's IndexedDB.
