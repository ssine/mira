import assert from "node:assert/strict";
import crypto from "node:crypto";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import {
  getStoreHead,
  getSnapshot,
  getThreadHistory,
  commitDelta,
  rebuildSnapshot,
  putSnapshot,
} from "../server/thread-store.mjs";

const base = process.env.MIRA_STORAGE_ROWS_TEST_DATABASE_URL;
if (!base)
  throw new Error(
    "MIRA_STORAGE_ROWS_TEST_DATABASE_URL must name a disposable development database",
  );
const owner = new pg.Pool({ connectionString: base });
const name = `storage_rows_${crypto.randomUUID().replaceAll("-", "")}`;
await owner.query(`CREATE DATABASE ${name}`);
const url = new URL(base);
url.pathname = `/${name}`;
const pool = new pg.Pool({ connectionString: url.toString() });
const store = "migration-fixture",
  a = crypto.randomUUID(),
  b = crypto.randomUUID();
const headers = () => ({ "x-codex-operation-id": crypto.randomUUID() });
try {
  await initializeDatabase(pool, { throughVersion: 15 });
  const state = {
    created_threads: {
      [a]: { source: "cli", metadata: { cwd: "/workspace" } },
      [b]: { source: "subagent", parent_thread_id: a },
    },
    names: { [a]: "latest title" },
    future_root: { [a]: { opaque: [1, null, { future: true }] } },
    scalar: false,
  };
  const manifest = {
    [a]: { generation: 1, itemCount: 1 },
    [b]: { generation: 1, itemCount: 1 },
  };
  for (let version = 1; version <= 3; version++)
    await pool.query(
      `INSERT INTO codex_store_events
    (store_id,event_seq,previous_event_seq,operation_id,event_format_version,state,history_manifest)
    VALUES($1,$2,$3,$4,1,$5::jsonb,$6::jsonb)`,
      [
        store,
        version,
        version - 1,
        crypto.randomUUID(),
        JSON.stringify({
          ...state,
          names: { [a]: version === 3 ? "latest title" : "old title" },
        }),
        JSON.stringify(manifest),
      ],
    );
  await pool.query(
    "INSERT INTO codex_store_heads(store_id,version) VALUES($1,3)",
    [store],
  );
  for (const [id, raw] of [
    [
      a,
      '{ "type": "future_item", "payload": {"nul":"\\u0000","surrogate":"\\ud800"} }',
    ],
    [b, '{"type":"child","unknown":true}'],
  ]) {
    await pool.query(
      `INSERT INTO codex_thread_events(store_id,thread_id,generation,item_seq,store_event_seq,event_format_version,payload,payload_sha256)
      VALUES($1,$2,1,1,1,1,$3::json,$4)`,
      [store, id, raw, crypto.createHash("sha256").update(raw).digest("hex")],
    );
    await pool.query(
      `INSERT INTO codex_thread_projections(store_id,thread_id,active_generation,item_count,state,through_event_seq)
      VALUES($1,$2,1,1,'{}',3)`,
      [store, id],
    );
  }
  const rawBefore = (
    await pool.query(
      "SELECT thread_id,payload::text,payload_sha256,created_at FROM codex_thread_events ORDER BY thread_id",
    )
  ).rows;
  await initializeDatabase(pool);
  const head = await getStoreHead(pool, store);
  assert.equal(head.version, 3);
  assert.equal(head.historyFloor, 3);
  assert.deepEqual(head.state, state);
  assert.deepEqual(head.historyManifest, manifest);
  assert.deepEqual(
    (
      await pool.query(
        "SELECT thread_id,payload::text,payload_sha256,created_at FROM codex_thread_events ORDER BY thread_id",
      )
    ).rows,
    rawBefore,
  );
  assert.equal(
    (await pool.query("SELECT count(*)::int AS n FROM codex_store_events"))
      .rows[0].n,
    1,
  );
  assert.equal(
    (
      await pool.query(
        "SELECT to_regclass('codex_thread_store_snapshots') AS old",
      )
    ).rows[0].old,
    null,
  );
  assert.equal((await getThreadHistory(pool, store, a, 1, 2)).status, 410);
  assert.equal(
    (await getThreadHistory(pool, store, a, 1, 3)).body.items[0].payload.nul,
    "\0",
  );
  await pool.query("DELETE FROM codex_store_state_entries WHERE store_id=$1", [
    store,
  ]);
  await pool.query("DELETE FROM codex_thread_projections WHERE store_id=$1", [
    store,
  ]);
  await rebuildSnapshot(pool, store);
  assert.deepEqual((await getStoreHead(pool, store)).state, state);
  assert.equal(
    (await getSnapshot(pool, store)).snapshot.histories[b][0].unknown,
    true,
  );
  const frozen = (
    await pool.query(
      "SELECT ctid::text,state::text FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2",
      [store, b],
    )
  ).rows[0];
  const changesBefore = (
    await pool.query("SELECT count(*)::int AS n FROM codex_store_state_changes")
  ).rows[0].n;
  const body = {
    expectedVersion: 3,
    stateChanges: [],
    historyChanges: [
      {
        threadId: a,
        mode: "append",
        expectedGeneration: 1,
        expectedItemCount: 1,
        items: [{ type: "new_item" }],
      },
    ],
  };
  const operation = headers();
  const duplicates = await Promise.all([
    commitDelta(pool, store, body, operation),
    commitDelta(pool, store, body, operation),
  ]);
  assert(duplicates.every((result) => result.status === 200));
  assert.equal(duplicates.filter((result) => result.body.duplicate).length, 1);
  assert.equal(
    (await getThreadHistory(pool, store, a, 1, null)).body.itemCount,
    2,
  );
  assert.deepEqual(
    (
      await pool.query(
        "SELECT ctid::text,state::text FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2",
        [store, b],
      )
    ).rows[0],
    frozen,
  );
  assert.equal(
    (
      await pool.query(
        "SELECT count(*)::int AS n FROM codex_store_state_changes",
      )
    ).rows[0].n,
    changesBefore,
  );
  assert.equal(
    (
      await commitDelta(
        pool,
        store,
        {
          ...body,
          historyChanges: [
            { ...body.historyChanges[0], items: [{ different: true }] },
          ],
        },
        operation,
      )
    ).status,
    409,
  );

  let started, release;
  const entered = new Promise((resolve) => {
    started = resolve;
  });
  const blocked = new Promise((resolve) => {
    release = resolve;
  });
  const slow = {
    query: (...args) => pool.query(...args),
    async connect() {
      const client = await pool.connect();
      return {
        release: () => client.release(),
        async query(sql, values) {
          const result = await client.query(sql, values);
          if (sql.includes("INSERT INTO codex_thread_events (")) {
            started();
            await blocked;
          }
          return result;
        },
      };
    },
  };
  const before = (await getStoreHead(pool, store)).version;
  const paused = commitDelta(
    slow,
    store,
    {
      expectedVersion: before,
      stateChanges: [],
      historyChanges: [
        {
          threadId: a,
          mode: "append",
          expectedGeneration: 1,
          expectedItemCount: 2,
          items: [{ paused: true }],
        },
      ],
    },
    headers(),
  );
  await entered;
  let timer;
  const other = await Promise.race([
    commitDelta(
      pool,
      store,
      {
        expectedVersion: before,
        stateChanges: [],
        historyChanges: [
          {
            threadId: b,
            mode: "append",
            expectedGeneration: 1,
            expectedItemCount: 1,
            items: [{ concurrent: true }],
          },
        ],
      },
      headers(),
    ),
    new Promise((_, reject) => {
      timer = setTimeout(
        () =>
          reject(
            new Error("unrelated thread blocked during raw history insertion"),
          ),
        3000,
      );
    }),
  ]).finally(() => {
    clearTimeout(timer);
    release();
  });
  const completed = await paused;
  assert.equal(other.status, 200);
  assert.equal(completed.status, 200);
  assert(other.body.version < completed.body.version);
  assert.equal(
    (await getThreadHistory(pool, store, a, 1, other.body.version)).body
      .itemCount,
    2,
  );
  assert.equal(
    (await getThreadHistory(pool, store, a, 1, completed.body.version)).body
      .itemCount,
    3,
  );
  const current = await getStoreHead(pool, store);
  const removed = await commitDelta(
    pool,
    store,
    {
      expectedVersion: current.version,
      stateChanges: [],
      historyChanges: [
        {
          threadId: a,
          mode: "delete",
          expectedGeneration: 1,
          expectedItemCount: 3,
        },
      ],
    },
    headers(),
  );
  const recreated = await commitDelta(
    pool,
    store,
    {
      expectedVersion: removed.body.version,
      stateChanges: [],
      historyChanges: [
        {
          threadId: a,
          mode: "append",
          expectedGeneration: 0,
          expectedItemCount: 0,
          items: [],
        },
      ],
    },
    headers(),
  );
  assert.equal(recreated.body.historyManifest[a].generation, 2);
  const unknown = JSON.parse(
    '{"__proto__":{"opaque":true},"constructor":null}',
  );
  let unknownCommit = await commitDelta(
    pool,
    store,
    {
      expectedVersion: recreated.body.version,
      stateChanges: [
        {
          path: ["created_threads", a, "future_fields"],
          mode: "set",
          conflictPolicy: "compareAndSwap",
          expected: { exists: false },
          value: unknown,
        },
      ],
      historyChanges: [],
    },
    headers(),
  );
  assert.equal(unknownCommit.status, 200);
  unknownCommit = await commitDelta(
    pool,
    store,
    {
      expectedVersion: unknownCommit.body.version,
      stateChanges: [
        {
          path: ["created_threads", a, "future_fields", "__proto__"],
          mode: "remove",
          conflictPolicy: "compareAndSwap",
          expected: { exists: true, value: { opaque: true } },
        },
      ],
      historyChanges: [],
    },
    headers(),
  );
  assert.equal(unknownCommit.status, 200);
  await rebuildSnapshot(pool, store);
  assert.deepEqual(
    (await getStoreHead(pool, store)).state.created_threads[a].future_fields,
    { constructor: null },
  );
  const compatibility = await getSnapshot(pool, store);
  assert.equal(
    (
      await putSnapshot(
        pool,
        store,
        {
          expectedVersion: compatibility.version,
          snapshot: compatibility.snapshot,
        },
        headers(),
      )
    ).status,
    200,
  );
  console.log(
    "PASS: schema 15 cutover preserves raw bytes/current state, removes old snapshots, rebuilds projections, rejects retired versions, deduplicates exact retries, updates only affected rows, publishes concurrent thread writes in order and isolates recreated generations",
  );
} finally {
  await pool.end();
  await owner.query(`DROP DATABASE ${name} WITH (FORCE)`);
  await owner.end();
}
