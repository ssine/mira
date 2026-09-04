# Codex activity presentation

The web console projects official Codex items into a readable activity trace. PostgreSQL's raw
rollout history remains authoritative; activity labels, statistics and headings are rebuildable UI
metadata, not a second event log. This feature does not require a database migration or a Codex fork.

`server/public/trace-activity.js` is shared by the browser's App Server event adapter and
`server/codex-transcript.mjs`. Keep its code browser-compatible and free of side effects.

- `commandExecution.commandActions` (live) and `CommandExecution.parsed_cmd` (rollout) describe read,
  search and directory-list operations. Unrecognized commands remain ordinary command activities;
  never execute, evaluate or heuristically interpret arbitrary code-mode JavaScript to produce labels.
- Preserve command output, cwd, exit code and duration in the expanded view. A failed, declined or
  interrupted operation must not be described as successfully completed.
- File updates have unified diffs. **For an added/deleted file, the official App Server `diff` field
  contains whole-file content**, not a unified diff. Rollout records use a path-keyed map with
  `content` or `unified_diff`. Both representations must produce the same line statistics.
- Render readable reasoning summaries as compact expandable headings. Empty summaries and raw
  reasoning do not create cards. Respect `summaryIndex` when assembling streamed summary sections.
- Consecutive tools in the same turn form a collapsed group with action counts and the latest running
  activity (or last completed activity). Expanding the group shows concise rows; expanding a row
  reveals its command output/diff. A code-mode wrapper and its nested commands may be distinct items.
- Deduplicate a materialized item and a model-facing call only when their **turn ID and call/item ID**
  match. Never discard all structured commands just because a turn contains another tool call.
- Scope live keys by thread, turn and item. Share these identities with projected history so replay
  cannot duplicate cards, and preserve expansion when replacing live data with authoritative history.
- Tool labels are plain text; narrative Markdown continues to be sanitized. New events must respect
  the user's scroll position. Keep the existing backwards pagination and do not shrink trace rows.

References: [App Server events](https://developers.openai.com/codex/app-server#events) and the pinned
Codex baseline's `app-server-protocol/src/protocol/item_builders.rs`.

## Validation

`npm run check --prefix server` covers cross-format metadata, paired tool records, nested commands,
failure states, diff statistics, summary handling and copy-button regressions.

For a real browser without starting a Mira service, enrolling a Node or making model calls:

```sh
node tests/trace_activity_browser.mjs
# Or supply an installed Playwright module and browser channel:
node tests/trace_activity_browser.mjs /absolute/path/to/playwright/index.mjs msedge
```

The browser test starts its own loopback fixture server and replaces bootstrap only in the HTTP
response, not in the application source. It exercises the actual page, event handler, renderer and
CSS at desktop and mobile widths, then closes both server and browser. Playwright is an optional test
dependency, not a runtime dependency. Set `MIRA_TRACE_SCREENSHOT` to save the final page if useful.
