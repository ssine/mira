import crypto from "node:crypto";

const object = (value) =>
  value !== null && typeof value === "object" && !Array.isArray(value);
const stable = (value) =>
  JSON.stringify(value, function (key, item) {
    return object(item)
      ? Object.fromEntries(
          Object.keys(item)
            .sort()
            .map((name) => [name, item[name]]),
        )
      : item;
  });
export const requestDigest = (value) =>
  crypto.createHash("sha256").update(stable(value)).digest("hex");
const equal = (left, right) => stable(left) === stable(right);
const ownValue = (value, key) =>
  object(value) && Object.hasOwn(value, key) ? value[key] : undefined;
const scopeThread = (field, key, value) =>
  field === "rollout_paths" ? (typeof value === "string" ? value : null) : key;

export function stateFromEntries(entries) {
  const state = {};
  for (const row of entries)
    if (row.is_root)
      Object.defineProperty(state, row.field, {
        value: row.value,
        enumerable: true,
        writable: true,
        configurable: true,
      });
  for (const row of entries)
    if (!row.is_root) {
      if (!object(state[row.field])) state[row.field] = {};
      Object.defineProperty(state[row.field], row.entry_key, {
        value: row.value,
        enumerable: true,
        writable: true,
        configurable: true,
      });
    }
  return state;
}

function entriesForState(state) {
  const entries = new Map();
  for (const [field, value] of Object.entries(state)) {
    entries.set(JSON.stringify([field, true, ""]), {
      field,
      entry_key: "",
      is_root: true,
      thread_id: null,
      value: object(value) ? {} : value,
    });
    if (object(value))
      for (const [key, entry] of Object.entries(value)) {
        entries.set(JSON.stringify([field, false, key]), {
          field,
          entry_key: key,
          is_root: false,
          thread_id: scopeThread(field, key, entry),
          value: entry,
        });
      }
  }
  return entries;
}

export async function lockScope(client, storeId, threadIds = null) {
  // Shared store gate permits independent threads; whole-state compatibility
  // writes take it exclusively. Consistent lock order prevents multi-thread deadlocks.
  const gate = JSON.stringify(["mira-store", storeId]);
  await client.query(
    `SELECT pg_advisory_xact_lock${threadIds === null ? "" : "_shared"}(hashtextextended($1,0))`,
    [gate],
  );
  if (threadIds !== null)
    for (const id of [...new Set(threadIds)].sort()) {
      await client.query(
        "SELECT pg_advisory_xact_lock(hashtextextended($1,0))",
        [JSON.stringify(["mira-thread", storeId, id])],
      );
    }
  const existing = await client.query(
    "SELECT version FROM codex_store_heads WHERE store_id=$1",
    [storeId],
  );
  if (!existing.rowCount)
    await client.query(
      "INSERT INTO codex_store_heads(store_id,version) VALUES($1,0) ON CONFLICT DO NOTHING",
      [storeId],
    );
}

export async function currentHead(client, storeId, threadIds = null) {
  // One statement gives metadata, boundaries and version one MVCC snapshot.
  const result = await client.query(
    `SELECT h.version::text,h.history_floor::text,h.updated_at,
    COALESCE((SELECT json_agg(s) FROM codex_store_state_entries s WHERE s.store_id=h.store_id
      AND ($2::text[] IS NULL OR s.is_root OR s.thread_id=ANY($2))), '[]'::json) AS entries,
    COALESCE((SELECT json_object_agg(thread_id,json_build_object('generation',active_generation,'itemCount',item_count))
      FROM codex_thread_projections p WHERE p.store_id=h.store_id AND ($2::text[] IS NULL OR p.thread_id=ANY($2))), '{}'::json) AS manifest
    FROM codex_store_heads h WHERE h.store_id=$1`,
    [storeId, threadIds],
  );
  const row = result.rows[0];
  if (!row)
    return {
      version: 0,
      historyFloor: 0,
      state: {},
      historyManifest: {},
      updatedAt: null,
    };
  return {
    version: Number(row.version),
    historyFloor: Number(row.history_floor),
    state: stateFromEntries(row.entries),
    historyManifest: row.manifest,
    updatedAt: row.updated_at.toISOString(),
  };
}

export async function historicalManifest(
  client,
  storeId,
  version,
  threadIds = null,
) {
  const result = await client.query(
    `SELECT DISTINCT ON(r.thread_id) r.thread_id,r.generation::text,r.item_count::text,r.active
    FROM codex_thread_revisions r JOIN codex_store_events c USING(store_id,operation_id)
    WHERE r.store_id=$1 AND c.event_seq<=$2 AND ($3::text[] IS NULL OR r.thread_id=ANY($3))
      AND NOT EXISTS(SELECT 1 FROM mira_thread_actions a WHERE a.store_id=r.store_id AND a.thread_id=r.thread_id AND a.action='delete')
    ORDER BY r.thread_id,c.event_seq DESC`,
    [storeId, version, threadIds],
  );
  return Object.fromEntries(
    result.rows
      .filter((row) => row.active)
      .map((row) => [
        row.thread_id,
        {
          generation: Number(row.generation),
          itemCount: Number(row.item_count),
        },
      ]),
  );
}

export async function beginReceipt(
  client,
  storeId,
  operationId,
  body,
  codexVersion,
) {
  const digest = requestDigest(body);
  const inserted = await client.query(
    `INSERT INTO codex_store_events(store_id,operation_id,request_sha256,codex_version)
    VALUES($1,$2,$3,$4) ON CONFLICT(store_id,operation_id) DO NOTHING RETURNING operation_id`,
    [storeId, operationId, digest, codexVersion],
  );
  if (inserted.rowCount) return null;
  const previous = (
    await client.query(
      "SELECT * FROM codex_store_events WHERE store_id=$1 AND operation_id=$2",
      [storeId, operationId],
    )
  ).rows[0];
  if (previous.request_sha256 !== digest)
    return {
      status: 409,
      body: {
        error: "operation UUID was already used for a different request",
        code: "operation_conflict",
      },
    };
  return {
    status: 200,
    body: {
      version: Number(previous.result_version),
      operationId,
      duplicate: true,
      rebased: true,
      historyManifest: await historicalManifest(
        client,
        storeId,
        Number(previous.result_version),
      ),
      appendedItemCount: Number(previous.appended_item_count),
      updatedAt: previous.created_at.toISOString(),
    },
  };
}

function changesBetween(before, after, path = [], output = []) {
  if (equal(before, after)) return output;
  if (object(before) && object(after)) {
    for (const key of new Set([...Object.keys(before), ...Object.keys(after)]))
      changesBetween(
        ownValue(before, key),
        ownValue(after, key),
        [...path, key],
        output,
      );
  } else if (object(after)) {
    output.push({ path, mode: "set", value: {} });
    for (const [key, value] of Object.entries(after))
      changesBetween(undefined, value, [...path, key], output);
  } else
    output.push({
      path,
      mode: after === undefined ? "remove" : "set",
      value: after,
    });
  return output;
}

export function stateDelta(before, after) {
  return changesBetween(before, after);
}

export async function rebuildStateEntries(client, storeId, state) {
  await client.query(
    "DELETE FROM codex_store_state_entries WHERE store_id=$1",
    [storeId],
  );
  for (const row of entriesForState(state).values())
    await client.query(
      `INSERT INTO codex_store_state_entries(store_id,field,is_root,entry_key,thread_id,value)
    VALUES($1,$2,$3,$4,$5,$6::jsonb)`,
      [
        storeId,
        row.field,
        row.is_root,
        row.entry_key,
        row.thread_id,
        JSON.stringify(row.value),
      ],
    );
}

export async function persistState(
  client,
  storeId,
  operationId,
  before,
  after,
) {
  const oldEntries = entriesForState(before),
    nextEntries = entriesForState(after);
  const affected = new Set();
  for (const key of new Set([...oldEntries.keys(), ...nextEntries.keys()])) {
    const old = oldEntries.get(key),
      next = nextEntries.get(key),
      row = next ?? old;
    if (old && next && equal(old.value, next.value)) continue;
    if (old?.thread_id) affected.add(old.thread_id);
    if (next?.thread_id) affected.add(next.thread_id);
    if (!next)
      await client.query(
        "DELETE FROM codex_store_state_entries WHERE store_id=$1 AND field=$2 AND is_root=$3 AND entry_key=$4",
        [storeId, row.field, row.is_root, row.entry_key],
      );
    else
      await client.query(
        `INSERT INTO codex_store_state_entries(store_id,field,is_root,entry_key,thread_id,value)
      VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(store_id,field,is_root,entry_key) DO UPDATE SET thread_id=EXCLUDED.thread_id,value=EXCLUDED.value
      WHERE codex_store_state_entries.value IS DISTINCT FROM EXCLUDED.value`,
        [
          storeId,
          row.field,
          row.is_root,
          row.entry_key,
          row.thread_id,
          JSON.stringify(row.value),
        ],
      );
  }
  const changes = changesBetween(before, after);
  // Root removals/replacements can contain many threads only on the exclusive
  // compatibility path. Its event contains no old object, and erasure scopes
  // each new value to its own thread.
  for (let index = 0; index < changes.length; index++) {
    const change = changes[index];
    const lookup = (state) => change.path.reduce(ownValue, state);
    const threadId =
      change.path.length >= 2
        ? scopeThread(
            change.path[0],
            change.path[1],
            lookup(after) ?? lookup(before),
          )
        : null;
    await client.query(
      `INSERT INTO codex_store_state_changes(store_id,operation_id,change_seq,thread_id,path,mode,value)
      VALUES($1,$2,$3,$4,$5,$6,$7::json)`,
      [
        storeId,
        operationId,
        index + 1,
        threadId,
        change.path,
        change.mode,
        change.mode === "set" ? JSON.stringify(change.value) : null,
      ],
    );
  }
  return affected;
}

export async function persistBoundaries(
  client,
  storeId,
  operationId,
  before,
  after,
) {
  const affected = new Set();
  for (const threadId of new Set([
    ...Object.keys(before),
    ...Object.keys(after),
  ])) {
    if (equal(before[threadId], after[threadId])) continue;
    affected.add(threadId);
    const entry = after[threadId] ?? before[threadId];
    await client.query(
      `INSERT INTO codex_thread_revisions(store_id,thread_id,operation_id,generation,item_count,active)
      VALUES($1,$2,$3,$4,$5,$6)`,
      [
        storeId,
        threadId,
        operationId,
        entry.generation,
        entry.itemCount,
        Boolean(after[threadId]),
      ],
    );
    if (!after[threadId])
      await client.query(
        "DELETE FROM codex_thread_projections WHERE store_id=$1 AND thread_id=$2",
        [storeId, threadId],
      );
  }
  return affected;
}

export async function publishReceipt(
  client,
  storeId,
  operationId,
  affected,
  appendedItemCount = 0,
  noChangeVersion = null,
) {
  if (noChangeVersion !== null) {
    await client.query(
      "UPDATE codex_store_events SET result_version=$3 WHERE store_id=$1 AND operation_id=$2",
      [storeId, operationId, noChangeVersion],
    );
    return noChangeVersion;
  }
  // This is the only globally serialized part. History and metadata are already
  // prepared using UUID references; nothing large runs under the publication lock.
  const row = (
    await client.query(
      "UPDATE codex_store_heads SET version=version+1,updated_at=NOW() WHERE store_id=$1 RETURNING version::text",
      [storeId],
    )
  ).rows[0];
  const version = Number(row.version);
  await client.query(
    `UPDATE codex_store_events SET event_seq=$3::bigint,previous_event_seq=$3::bigint-1,result_version=$3::bigint,appended_item_count=$4
    WHERE store_id=$1 AND operation_id=$2`,
    [storeId, operationId, version, appendedItemCount],
  );
  if (affected.size)
    await client.query(
      "UPDATE codex_thread_projections SET through_event_seq=$3 WHERE store_id=$1 AND thread_id=ANY($2)",
      [storeId, [...affected], version],
    );
  return version;
}
