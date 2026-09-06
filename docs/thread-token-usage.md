# Conversation token usage

Conversation list/detail responses expose `tokenUsage` with `inputTokens`, `cachedInputTokens` and
`outputTokens`. The source is the existing canonical `metadata_updates/<threadId>/token_usage`
projection, which Codex updates from `TokenCount.info.total_token_usage`. These are cumulative
provider-reported counters for that thread. Cached input is included in input, not added to it.
Counts follow upstream thread/fork history semantics; separate subagents are not summed into parents.

Older imported histories may not have a metadata projection. The read-only fallback selects the
latest usable canonical `event_msg/token_count` cumulative snapshot in the active generation and
through the thread's observed item count. It never adds cumulative snapshots or sums `last_token_usage`.
Missing fields are unknown, and real zero values remain zero. Out-of-range or malformed counts are
not coerced into numbers. No requests to a model or a running Node are needed.

Schema 21 adds a partial event lookup index only. Raw events stay untouched. The fallback validates
candidate structure in JavaScript because PostgreSQL JSON extraction can reject canonical JSON that
contains escaped NUL. Its cache is bounded and keyed by store, thread, generation and immutable item
count. Metadata updates bypass that cache, so updates without new history items still become visible.
All values can be reconstructed from existing canonical metadata/history after a projection rebuild.

The conversation details panel shows all three exact numbers. The sidebar's existing second line
shows a compact `125k in · 8k out` summary when the row has enough width; its hover text includes the
exact cached-input count. Narrow rows hide only the compact summary. Polling updates text in place,
keeps selection/status and row height stable, and rejects older generation/item-count responses.
Unknown input/output does not produce a misleading zero summary.

Validation: `npm run check --prefix server`, `tests/thread_token_usage_e2e.mjs` against a disposable
Server database, and `tests/thread_token_usage_browser.mjs` cover cumulative snapshots, imported
history, zero/unknown, raw NUL, subagents, generation replacement, metadata-only updates, projection
rebuilds, API authentication, responsive layout and live updates of both views.
