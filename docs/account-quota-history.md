# Account quota history

Mira Server samples the Codex **weekly remaining percentage**, next reset timestamp and available
reset count every five minutes, even when no browser is open. This is subscription quota, not a
currency balance. History begins when schema 20 is deployed; there is no upstream historical backfill.

Only approved, online Nodes with an already running App Server are sampled. Two readers run at most
at once, each with a 20-second deadline. A dedicated reverse-channel tunnel sends only `initialize`,
`initialized`, `account/read` (`refreshToken: false`) and `account/rateLimits/read`. It does not start
a runtime/thread/turn, dispatch tools or process conversation broadcasts. Cancellation, revocation,
replacement and shutdown close the tunnel. Identity is checked again after reading limits; a switch
during sampling discards that sample.

Schema 20 adds `mira_account_quota_samples`. Observations are inserted with typed quota fields and
only the account type, email and plan name; credentials, upstream error text and raw account payloads
are never stored. Samples are scoped to Node plus a hash of the reported Codex home/binary. Different
Nodes remain separate even if the email matches, because App Server does not expose a reliable
workspace identity in the account response. Changing an installation selects a separate history.
Within an installation, earlier observations for other accounts become gaps in the current account's
chart; returning to an account restores its earlier observations.

The latest persisted timestamp enforces the five-minute interval across restarts. A PostgreSQL
advisory lock coalesces concurrent workers and a unique five-minute slot prevents duplicate writes.
Read failures create an observation without a quota; offline periods are not filled. Missing weekly
windows stay unknown and genuine zero values stay zero. Retained observations are not automatically
deleted. Chart views are rebuilt directly from these rows, independently of conversation persistence.

`GET /v1/nodes/:nodeId/account-history?range=24h|7d|30d` is administrator-only and reads PostgreSQL.
There is no browser write endpoint. Its bounded window contains at most 8,641 observations, including
gaps. Responses use `Cache-Control: no-store`; the account UI keeps a five-minute in-memory cache per
Node, installation, account and range, cleared on logout. Existing live account reads keep their own
five-minute cache.

The account details popover shows a height-limited step chart. A step indicates the new observed
value at the sample time, not an exact consumption/reset time. Failed samples and time gaps exceeding
2.5 sample intervals break the line. Clicking, hovering or using arrow/Home/End keys inspects a sample.
Timestamps use the browser's local timezone. A single sample is shown as a point, and empty history
never invents past data.

Validation: `npm run check --prefix server`, `tests/account_history_e2e.mjs` with a disposable Server
database, and `tests/account_sidebar_browser.mjs` cover quota normalization, read-only transport,
concurrent/restarted sampling, identity/runtime changes, history authorization, gaps, cache behavior,
keyboard/pointer interaction, mobile layout and light/dark themes.
