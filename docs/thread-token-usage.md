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
Unknown input/output does not produce a misleading zero summary. Sidebar prices use `· $0.75`
(`*` marks a partial estimate, with a full explanation on hover). Compact rows use `125k↑ 8k↓`.
The menu overlays the title with a matching fade, reserving no column. Desktop sidebar width is
resizable from 240 to 480 px, supports keyboard arrows/Home/End and persists as a local preference;
mobile retains its existing gesture drawer. Recency labels use only time today, weekday within seven
calendar days, and date for older conversations (including the year when different).

Validation: `npm run check --prefix server`, `tests/thread_token_usage_e2e.mjs` against a disposable
Server database, and `tests/thread_token_usage_browser.mjs` cover cumulative snapshots, imported
history, zero/unknown, raw NUL, subagents, generation replacement, metadata-only updates, projection
rebuilds, API authentication, responsive layout and live updates of both views.

## Model and API-equivalent cost

List/detail reads now expose `model` from canonical model metadata, with a bounded, batched fallback
through the latest `turn_context` or `thread_settings_applied` record. Schema 22 adds partial indexes
for this lookup and for cost events. Neither feature mutates canonical history; generation changes
invalidate the caches and projection rebuilds preserve their inputs.

The composer reads `config/read` with the project's cwd and every page of `model/list` on a short,
read-only App Server connection. It shows the effective configured default (or the catalog default
when no model is configured), including configured models absent from the catalog. Model fields alone
are cached by Node/cwd for five minutes. Browsing the picker never starts/resumes a thread. Its first
row is reserved for the full-width message input; attachment, model, advertised reasoning effort and
send/stop controls share a compact second row. Model and effort options use styled, keyboard-accessible
popover menus instead of platform-native selects. A model selection applies to `thread/start` and the
next `turn/start`; effort uses the new thread's config and `turn/start.effort`, which also updates later
turns. Neither changes the Node's saved config. Each compatible Node card owns the low-frequency
"刷新模型" action, which invalidates that Node's browser catalog and can prepare its stopped runtime.
Node/project changes discard stale responses.

`GET /v1/codex/threads/:id?storeId=personal&includeCost=1` opts into `costEstimate`; ordinary list reads
do not scan cost history. The sidebar requests the same opt-in detail endpoint only for visible rows,
with at most two concurrent reads, bounded browser caching, and a ten-second refresh floor for an
advancing thread. Unchanged history is not repeatedly fetched. The detail panel requests this while open and refreshes on usage/history
changes. Its server projection reads canonical events in pages of 256, coalesces identical requests,
and incrementally processes appended events in a bounded cache. Separate subagents retain separate
estimates. A fork remains its own thread but contains a copied history prefix so Codex can continue
with the same context. Its cost projection reads inherited cumulative counters only as a baseline and
starts pricing after the child-owned `thread_settings_applied` boundary appended by `thread/fork`.
The inherited prefix therefore contributes context/token totals but no estimated spend to the child.
The boundary is durable in the child and does not depend on the source thread continuing to exist.
An older or imported fork without that boundary reports unavailable instead of pricing copied history.
Transcript reads opt into the same cached projection and attach estimates only to assistant turns on
the requested page. The final assistant message in each completed turn shows that turn's compact
estimated cost beside its total elapsed time. Zero, partial and unavailable estimates remain distinct;
the tooltip states that the value uses Standard API prices rather than ChatGPT plan deductions.

The estimate uses the dated Standard USD prices in `server/model-pricing.mjs`, sourced from
https://developers.openai.com/api/docs/pricing and the corresponding model pages. It prices each new
`last_token_usage` snapshot against its recorded model context/settings, deduplicating repeated
cumulative snapshots. Ordinary input excludes cache reads/writes; reasoning output is already included
in output. Long-context multipliers use the individual request's input length, never cumulative thread
input. Integer nanodollars avoid per-request rounding loss. Missing history, unknown model prices and
metadata ahead of history produce partial/unavailable estimates, not invented zero costs.

This is equivalent API model-token spend at the stated current Standard price, not a historical invoice
or ChatGPT plan deduction. Tool fees, service tiers, regional pricing and other billing adjustments are
excluded. Token-count events do not identify the server-executed model. In runtime 0.153.1-mira.7,
`ModelReroute` notifications are transient and not persisted, so a temporary server reroute cannot be
reconstructed from history. `complete` means all recorded usage was priced on this basis, not that the
result is an actual bill. Manual model changes are retained in context/settings and priced separately.

The details panel shows a direct dollar amount, the sidebar separates its compact amount from the
token summary with a middle dot, and completed-turn footers show the per-turn amount; none adds an
approximation sign.

Additional validation: `thread_cost_test.mjs`, `thread_cost_e2e.mjs`, `thread_models_browser.mjs` and
`thread_token_usage_browser.mjs` cover pricing, model changes, cache writes, long-context thresholds,
cache/rebuild/generation behavior, authentication, mixed-model cost display, default/model selection,
project races and live updates. Browser execution uses simulated Node RPCs and sends no real model calls.
