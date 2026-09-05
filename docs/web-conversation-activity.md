# Web conversation activity

Conversation activity is derived from PostgreSQL's canonical rollout lifecycle
records. Schema 18 adds a partial lookup index; it adds no independent running
flag and never rewrites history. Existing CLI, App Server and subagent records
work immediately after migration, including after projection rebuilds.

The Server inspects a bounded tail of lifecycle candidates in the current
generation and item-count boundary. JavaScript validates the raw envelope so
unknown items and JSON containing escaped NUL remain safe. An immutable-boundary
cache retains at most 1,000 projections per pool. Node reachability is checked
afresh. An unfinished turn becomes uncertain when its Node is offline, its
managed runtime has restarted, or its imported history cannot establish current
execution. Uncertainty is not completion.

The Web combines durable activity with live App Server events. Late snapshots
cannot undo newer generation/count observations or resurrect a locally finished
turn. Losing the browser socket does not hide an ongoing task. If the central
Server cannot be checked for 20 seconds, a running indicator becomes uncertain.

Each visible conversation window checks the central list every two seconds while
its selected turn runs, otherwise every five seconds. Status icons update in
place without closing menus or rebuilding the sidebar. Changed history is merged
into the selected reader without resuming the thread or starting a runtime. A
window returning to the foreground checks immediately. Missed intervals spanning
multiple pages are read in bounded chunks with a resumable in-memory gap cursor.
This also covers CLI execution and conversations on different Nodes that have no
live subscription in that browser. App Server subscribers keep immediate live
updates; other windows display newly persisted output on their next check.

Run `tests/thread_activity_e2e.mjs` against a disposable PostgreSQL database,
`tests/thread_activity_browser.mjs` against a disposable Server, and the existing
`tests/trace_activity_browser.mjs` for live-event/reconnect/history regressions.
