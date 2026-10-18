# ThreadStore read memory

The v2 head and history HTTP routes stream their existing JSON response shape.
They do not aggregate the whole store in PostgreSQL, decode every item into Go
objects, or marshal one complete response buffer. A read-only repeatable-read
transaction pins the head, state, manifest, generation and history count. A
concurrent commit or replacement cannot mix versions within a response.

Head reads enumerate field names and scan entries by the existing primary key.
Each field retains at most its root value while child entries are emitted; child
entries override root members without emitting duplicate JSON keys. Manifest
rows also stream. History reads validate the count before sending HTTP 200 and
then forward raw JSON items, preserving unknown fields, large integers and
escaped NUL. Account input-recovery metadata uses the same database snapshot.
The output buffer is 32 KiB; individual database values still require memory
proportional to their own size. This is not a hard per-response byte limit.

Late failures abort the HTTP transfer. Clients must receive a complete successful
response before adopting it. Cancellation and write failure close the rows and
transaction; pgx may discard a cancelled connection asynchronously. Each active
read uses one pool connection and normal socket backpressure. Slow readers keep
an MVCC snapshot open until completion/disconnect, so pool use and long-running
read transactions remain relevant operational metrics.

Schema 41 adds a generated `directory_timestamps` text array to thread projections.
Directory reads use the four small timestamp candidates instead of detoasting
the full state. Timestamp precedence and PostgreSQL date validation are unchanged.
Generation from `state` keeps writes by the previous Server compatible and makes
the field rebuildable. Applying the migration rewrites/locks the projection table
once; it does not rewrite canonical event history. Allow for that migration during
a normal unified Mira release, including its rollback compatibility window.

## Validation

Integration tests cover v1/v2-created data, scoped heads, unknown root fields,
overlapping root/child members, concurrent changes, historical generations,
incomplete/retired history, account recovery metadata and cancellation cleanup.
The account runtime E2E exercises native resume, account handoff, images and
subagent persistence against a disposable Server and synthetic loopback provider.

The opt-in capacity experiment uses 7,200 synthetic 32 KiB metadata entries and
128 one-MiB history records, with response bodies discarded in-process:

```sh
MIRA_STREAM_CAPACITY_TEST=1 \
MIRA_HISTORY_UPLOAD_TEST_DATABASE_URL=postgres://... \
go -C node test ./internal/miraserver -run '^TestStoreReadCapacity$' -count=1 -v
```

An October 2026 local run measured:

| Response | Materialized / streamed time | Materialized / streamed sampled Go heap peak |
| --- | --- | --- |
| 225.6 MiB head | 3.12 / 0.61 s | 1,516 / 13.4 MiB |
| 128.1 MiB history | 0.72 / 0.12 s | 392 / 24.4 MiB |

The heap baseline was about 6.3 MiB. Connections and Go encoder pools were cleared
between measurements, since the legacy aggregate can leave a large pgx receive
buffer behind. These are synthetic, single-request measurements, not production
RSS, concurrent p95 latency, or WAN throughput guarantees.

The v1 whole-store compatibility adapter and in-process materialization helpers
still exist. Native clients still download a full head for store-wide lists and a
full history for cold resume. Negotiated pagination, version-aware client caches
and incremental history fetching are separate changes. Streaming does not reduce
canonical image storage or move authoritative history to a client or tmpfs.
