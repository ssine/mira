# Conversation index: outlines, search, message rail and timeline

Implemented by schema 48 (Mira 1.0.111). PostgreSQL canonical history stays the
only source of truth. The conversation index is a derived, rebuildable copy of
what the Web console displays, so that opening, searching and charting a
conversation never detoasts tool output.

## Why

In production the canonical `codex_thread_events` table held about 28 GB for
2,330 conversations (14.5 million records). One `ILIKE` scan over it took
2 minutes 26 seconds. Almost all of it is tool output, token counters,
compaction history and patches; user and assistant prose is roughly 150–200 MB.
Searching, loading a whole conversation, or building a timeline must therefore
read a display-sized copy, not raw history.

## Tables

| Table | Contents |
| --- | --- |
| `mira_conversation_records` | One slim JSON copy per displayed Codex record `(store, thread, generation, item_seq)`, with its persisted time. |
| `mira_conversation_messages` | Plain text of Codex `user_message`/`agent_message` events and Claude prompts/assistant text, for search. |
| `mira_conversation_index_state` | Per Codex thread: indexed generation, item count, projector version and carried counters. |
| `mira_claude_index_state` | Per Claude session root (`subpath=''`, durable events) and native subagent transcript (entries). |

`pg_trgm` is created when the database allows it, with a GIN index on message
text. Without the extension the migration still succeeds and search scans the
(small) message table sequentially. Mira never installs or upgrades PostgreSQL.

### Slim records

`slimCodexRecord` keeps exactly what `ProjectCodexTranscript` renders without
tool details, plus lifecycle and tool timing for the timeline:

- messages, reasoning text that the projector would show, compaction summaries
  and turn lifecycle records are kept (bounded to 1 MiB strings);
- shell and `apply_patch` calls keep their inputs (they shape collapsed activity
  summaries); other tool inputs and all tool outputs keep a 64-character prefix
  that preserves "has input/output" semantics; typed images keep their position
  with a placeholder (the image route reads the canonical record);
- token counters are not stored. The last input count before a compaction is
  carried into the compaction record, as the projector expects;
- session metadata, world state, `turn_context` instructions and encrypted
  reasoning are dropped.

The projector accepts explicit sparse sequences (`ProjectionOptions.Sequences`),
so outline entries keep canonical `item_seq` keys. A read-only comparison over the
400 most recently updated production conversations (550,290 records, 162,775
displayed entries) produced identical outlines from slim and complete history;
slim records were about 16% of the raw JSON (mostly displayed reasoning text).
Re-run it with `MIRA_INDEX_EQUIVALENCE_DATABASE_URL` (see
`conversation_index_equivalence_test.go`); the session is read-only.

Changing slimming or extraction rules requires a new `ConversationIndexVersion`;
every thread is then re-indexed from canonical history.

## Indexing

A background indexer in the Server polls every 10 seconds (continuously while
there is work), most recently updated conversations first. It reads canonical
rows in 64-record chunks, so one large record never multiplies memory, and
publishes up to 512 slim records per transaction. That transaction holds the
projection row `FOR SHARE` and the index state `FOR UPDATE`:

- permanent deletion deletes the projection and, in the same transaction, the
  index rows (also when a store commit drops a thread from the manifest); the
  indexer cannot publish after a deletion and never resurrects deleted prose;
- a generation change resets the thread's index; reads and search join the
  active generation, so old-generation rows are never shown;
- a gap in canonical history publishes only the contiguous prefix. Threads that
  cannot progress are parked for five minutes instead of starving others;
- orphaned index rows (for example after store-wide projection rebuilds) are
  removed every ten minutes.

Imported conversations use the restored original timestamps, as transcripts do.
After an upgrade the first backfill reads all canonical history once. Outline and
timeline reads index their own thread on demand (bounded to a few seconds) and
fall back to paged history while a very large conversation is still indexing.

## API

- `GET /v1/codex/threads/{id}/transcript?outline=1[&after=N&generation=G]` returns
  the whole conversation in the existing transcript shape (`trace`, `generation`,
  `itemCount`, `storeVersion`) with `outline: true` and no `nextCursor`. Tool
  entries have no bodies; their `toolDetail` page names its record range. With
  `after` and the same `generation` it returns only new entries and every entry
  of a turn touched since `after`. `409 outline_pending` means: use paged reads.
- `...transcript?tail=1&toolDetails=1&cursor=t2:G:END:S&from=F` reads tool
  details for one outline range (bounded to 2,000 records and 200 tools).
- `GET /v1/conversations/search?q=...&archived=include|exclude|only` matches every
  whitespace-separated term (case-insensitive substring, at most 8 terms) in
  titles and prose of Codex and Claude conversations, archived ones and
  subagents included. Each result names its root conversation and at most three
  hits with snippets. Statements are bounded to 15 seconds; one- and
  two-character terms cannot use trigrams.
- `GET /v1/codex/threads/{id}/timeline` and `GET /v1/claude/sessions/{id}/timeline`
  return one lane per agent (root and subagents) with turns, tool calls (kind
  `tool`, `wait` or `agent`) and message instants, in epoch milliseconds.
  Unfinished intervals end at their last evidence, never "now".

## Web console

- **Whole conversations.** Codex conversations open from the outline and keep it
  in the browser cache (see `web-client-cache.md`). Scrolling never pages older
  history; later reads ask only for records after the cached count. Tool bodies
  still load on expansion. Turn costs are read newest first in batches of 50.
- **Whole history on open.** After a conversation opens, the remaining history
  loads in the background without scrolling: Claude pages are read without
  painting and rendered once; paged Codex history (index still building) reads
  its older pages. Prose renders only near the viewport, so long histories stay
  cheap; the reader keeps the latest reply in view.
- **Message rail.** A vertical rail at the right edge of the reading surface has
  one tick per user message. It spans the reading area minus 10% above and
  below; ticks share that height (compact for short conversations, compressed
  for long ones, scrolling only below a 3 px pitch). Hover or focus previews
  the message; click or Enter scrolls to it below the floating header/timeline;
  pressing and dragging scrubs through messages like a scrollbar. The active
  tick is the last message starting at or above the top of the reading area.
  Hidden on phones and when a conversation has fewer than two messages.
- **Search.** A search box above the sidebar list replaces the list with results
  while a query is present. Clicking a hit opens the conversation and scrolls to
  that message (Codex by `item_seq`, Claude by event sequence).
- **Timeline.** A header button toggles a floating panel; it is collapsed for
  every conversation and reads nothing until expanded. Activity of the whole
  agent tree joins into segments across idle gaps of up to 10 minutes; longer
  gaps become a fixed-width break labelled with the idle time, so a resumed
  conversation continues right next to the previous segment. Inside segments
  time is to scale (shared scale, zoomable). Model phases are turn time not
  covered by tools, attributed by event boundaries, not provider latency. User
  message markers jump to the message; subagent lane labels open the subagent.

## Limits

- Tool outputs and commands are not searchable.
- Times are persisted (or restored import) times, not runtime spans; see
  `agent-timeline-design.md` for the planned execution observations.
- Parallel tools in one lane overlap rather than stacking into sub-tracks.
