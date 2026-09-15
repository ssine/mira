# Web conversation pagination

The Web sidebar loads root conversations separately from Codex subagents. A root
is a thread with no parent in the current archive filter; missing parents and
cycles cannot hide a conversation. Ordinary forks remain roots. A complete,
lightweight project directory keeps old projects reachable even when their
conversations are not in the first page.

`GET /v1/codex/threads?storeId=personal&view=roots&limit=50&archived=0`
returns `{paged, data, projects, total, nextCursor}`. Limits are 1–100 (default 50).
The legacy endpoint without `view` retains its existing contract.

- `view=roots`: optionally filter by the opaque `projectKey` from `projects`.
  Each project has `key`, `nodeId`, `cwd`, and a root conversation `count`.
- `view=children&parentThreadId=…`: paginate direct children only. Grandchildren
  are requested on their own expansion. Returned summaries include `childCount`,
  `subagentCount`, and `listRoot`; counts follow the current archive filter.
- `view=path&threadId=…`: return the selected thread and ancestors, at most
  `limit` rows. Continue from `nextParentId` for deeper trees.
- `view=refresh&id=…&id=…`: refresh at most 100 known IDs. `removed` identifies
  rows that were deleted or left the archive filter.
- `head=1` on roots/children reads the current first page without allocating or
  advancing a cursor. It discovers newly created conversations during polling.

Cursors retain ordered IDs, so concurrent recency changes do not skip older
conversations. They are scoped to store, view, project/parent and archive filter.
The process-local LRU has a 32 MiB budget, 128 entries and a 15-minute idle TTL.
Expiry/eviction returns HTTP 410 `thread_cursor_expired`; the Web restarts that
page stream while retaining and deduplicating already loaded rows. A single
snapshot exceeding the budget returns HTTP 503 `thread_page_busy`. Server
restarts may expire cursors; history remains in PostgreSQL.

Navigation reads IDs and project/parent/recency metadata across the selected
store, with a short (3-second), bounded directory cache. It does not enrich every
thread from history. Only the requested page receives lifecycle, read state and
usage summaries. Archive/delete operations invalidate the directory immediately.
Descendant activity advances root recency even when descendants are not loaded.
This is still a linear directory scan when its cache misses; it is not a constant
cost query for arbitrarily large stores.

The Web initially loads 50 recent roots and merges later project/child pages.
Filter epochs discard old responses. Collapsed children have no descendant DOM.
Polling refreshes the current conversation and visible sidebar rows, with root
head discovery every 10 seconds and at most two expanded child-head checks per
poll. It does not repeatedly reload all previously visited pages. Structural
renders retain disclosure state, focus and sidebar scroll position.

Token and cost statistics are independent of pagination: a parent's totals include
all descendant threads, including archived children, and exclude copied pre-fork
usage. They are fetched for visible rows on demand with bounded concurrency. The
child disclosure's activity text explicitly describes expanded rows; it does not
present a partially loaded status count as a whole-tree total. See
[Token accounting](thread-token-usage.md).

Regression coverage: `thread_pages_test.mjs`, `thread_pages_browser.mjs`, and
`views/thread_pages_test.go` (PostgreSQL tests use
`MIRA_VIEWS_TEST_DATABASE_URL`).
