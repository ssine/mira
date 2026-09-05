import crypto from "node:crypto";

const eventFormatVersion = 1;
const storeWriteTails = new Map();

async function withStoreWriteQueue(storeId, operation) {
  const previous = storeWriteTails.get(storeId) ?? Promise.resolve();
  let release;
  const current = new Promise((resolve) => {
    release = resolve;
  });
  storeWriteTails.set(storeId, current);
  await previous;
  try {
    return await operation();
  } finally {
    release();
    if (storeWriteTails.get(storeId) === current) storeWriteTails.delete(storeId);
  }
}

function objectOrEmpty(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value : {};
}

function historiesFromSnapshot(snapshot) {
  return objectOrEmpty(snapshot?.histories);
}

function stateFromSnapshot(snapshot) {
  const state = { ...objectOrEmpty(snapshot) };
  delete state.histories;
  return state;
}

export async function assertThreadsNotDeleted(client, storeId, threadIds) {
  if (!threadIds.length) return;
  const result = await client.query("SELECT thread_id FROM mira_thread_actions WHERE store_id=$1 AND action='delete' AND thread_id=ANY($2::text[]) LIMIT 1", [storeId, threadIds]);
  if (result.rowCount) throw Object.assign(new Error("此会话已永久删除，不能继续写入或恢复。"), { statusCode: 410, code: "thread_deleted" });
}

function snapshotThreadIds(snapshot) {
  return Object.entries(objectOrEmpty(snapshot)).flatMap(([key, value]) =>
    key === "rollout_paths" ? Object.values(objectOrEmpty(value)) : Object.keys(objectOrEmpty(value)));
}

function canonicalJson(value) {
  if (Array.isArray(value)) return value.map(canonicalJson);
  if (value !== null && typeof value === "object") {
    return Object.fromEntries(
      Object.keys(value)
        .sort()
        .map((key) => [key, canonicalJson(value[key])]),
    );
  }
  return value;
}

function stableStringify(value) {
  return JSON.stringify(canonicalJson(value));
}

function jsonEqual(left, right) {
  return stableStringify(left) === stableStringify(right);
}

function isPrefix(previous, next) {
  if (!Array.isArray(previous) || !Array.isArray(next) || previous.length > next.length) {
    return false;
  }
  return previous.every((item, index) => jsonEqual(item, next[index]));
}

function payloadHash(payload) {
  return crypto.createHash("sha256").update(stableStringify(payload)).digest("hex");
}

async function acquireStoreWriteLock(client, storeId) {
  await client.query(
    `INSERT INTO codex_store_heads (store_id, version) VALUES ($1, 0)
     ON CONFLICT (store_id) DO NOTHING`,
    [storeId],
  );
  const result = await client.query(
    `UPDATE codex_store_heads SET version = version, updated_at = updated_at
     WHERE store_id = $1 RETURNING version::text`,
    [storeId],
  );
  return Number.parseInt(result.rows[0].version, 10);
}

async function advanceStoreHead(client, storeId, expectedVersion) {
  const result = await client.query(
    `UPDATE codex_store_heads SET version = version + 1, updated_at = NOW()
     WHERE store_id = $1 AND version = $2 RETURNING version::text`,
    [storeId, expectedVersion],
  );
  if (result.rowCount !== 1) throw new Error(`store head changed while committing ${storeId}`);
  return Number.parseInt(result.rows[0].version, 10);
}

function buildHistoryPlan(previousSnapshot, nextSnapshot, previousManifest = {}) {
  const previousHistories = historiesFromSnapshot(previousSnapshot);
  const nextHistories = historiesFromSnapshot(nextSnapshot);
  const manifest = {};
  const appends = [];

  for (const [threadId, nextHistoryValue] of Object.entries(nextHistories)) {
    if (!Array.isArray(nextHistoryValue)) {
      throw new Error(`history for thread ${threadId} must be an array`);
    }
    const previousHistory = previousHistories[threadId];
    const previousEntry = objectOrEmpty(previousManifest[threadId]);
    const previousGeneration = Number.isSafeInteger(previousEntry.generation)
      ? previousEntry.generation
      : 0;
    const appendOnly = Array.isArray(previousHistory) && isPrefix(previousHistory, nextHistoryValue);
    const generation = appendOnly ? Math.max(previousGeneration, 1) : previousGeneration + 1;
    const startIndex = appendOnly ? previousHistory.length : 0;

    manifest[threadId] = { generation, itemCount: nextHistoryValue.length };
    for (let index = startIndex; index < nextHistoryValue.length; index += 1) {
      appends.push({
        threadId,
        generation,
        itemSeq: index + 1,
        payload: nextHistoryValue[index],
      });
    }
  }

  return { manifest, appends };
}

function projectionForThread(snapshot, threadId, manifestEntry, eventSeq) {
  const state = stateFromSnapshot(snapshot);
  const created = objectOrEmpty(state.created_threads?.[threadId]);
  const metadata = objectOrEmpty(state.metadata_updates?.[threadId]);
  return {
    threadId,
    activeGeneration: manifestEntry.generation,
    itemCount: manifestEntry.itemCount,
    parentThreadId: created.parent_thread_id ?? null,
    sourceKind: created.source ?? metadata.source ?? null,
    title: metadata.title ?? null,
    cwd: metadata.cwd ?? created.metadata?.cwd ?? null,
    state: {
      createdThread: Object.keys(created).length > 0 ? created : null,
      metadata: Object.keys(metadata).length > 0 ? metadata : null,
      name: state.names?.[threadId] ?? null,
      section: state.sections?.[threadId] ?? null,
    },
    throughEventSeq: eventSeq,
  };
}

async function replaceProjections(client, storeId, snapshot, manifest, eventSeq) {
  await client.query("DELETE FROM codex_thread_projections WHERE store_id = $1", [storeId]);
  for (const [threadId, manifestEntry] of Object.entries(manifest)) {
    const projection = projectionForThread(snapshot, threadId, manifestEntry, eventSeq);
    await client.query(
      `INSERT INTO codex_thread_projections (
         store_id, thread_id, active_generation, item_count, parent_thread_id,
         source_kind, title, cwd, state, through_event_seq
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`,
      [
        storeId,
        projection.threadId,
        projection.activeGeneration,
        projection.itemCount,
        projection.parentThreadId,
        projection.sourceKind,
        projection.title,
        projection.cwd,
        JSON.stringify(projection.state),
        projection.throughEventSeq,
      ],
    );
  }
}

async function appendCanonicalEvent(
  client,
  { storeId, eventSeq, previousEventSeq, operationId, codexVersion, snapshot, previousSnapshot, previousManifest },
) {
  const { manifest, appends } = buildHistoryPlan(
    previousSnapshot,
    snapshot,
    previousManifest,
  );
  await client.query(
    `INSERT INTO codex_store_events (
       store_id, event_seq, previous_event_seq, operation_id,
       event_format_version, codex_version, state, history_manifest
     ) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb)`,
    [
      storeId,
      eventSeq,
      previousEventSeq,
      operationId,
      eventFormatVersion,
      codexVersion,
      JSON.stringify(stateFromSnapshot(snapshot)),
      JSON.stringify(manifest),
    ],
  );

  for (const item of appends) {
    await client.query(
      `INSERT INTO codex_thread_events (
         store_id, thread_id, generation, item_seq, store_event_seq,
         event_format_version, codex_version, payload, payload_sha256
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::json, $9)`,
      [
        storeId,
        item.threadId,
        item.generation,
        item.itemSeq,
        eventSeq,
        eventFormatVersion,
        codexVersion,
        JSON.stringify(item.payload),
        payloadHash(item.payload),
      ],
    );
  }

  await replaceProjections(client, storeId, snapshot, manifest, eventSeq);
  return { manifest, appendedItemCount: appends.length };
}

export async function seedLegacySnapshots(pool) {
  const result = await pool.query(
    `SELECT store_id, version::text, snapshot
     FROM codex_thread_store_snapshots snapshots
     WHERE NOT EXISTS (
       SELECT 1 FROM codex_store_events events WHERE events.store_id = snapshots.store_id
     )
     ORDER BY store_id`,
  );

  for (const row of result.rows) {
    const client = await pool.connect();
    try {
      await client.query("BEGIN");
      const eventSeq = Number.parseInt(row.version, 10);
      await appendCanonicalEvent(client, {
        storeId: row.store_id,
        eventSeq,
        previousEventSeq: 0,
        operationId: crypto.randomUUID(),
        codexVersion: "legacy-snapshot-import",
        snapshot: row.snapshot,
        previousSnapshot: {},
        previousManifest: {},
      });
      await client.query(
        `INSERT INTO codex_store_heads (store_id, version) VALUES ($1, $2)
         ON CONFLICT (store_id) DO UPDATE SET version = EXCLUDED.version, updated_at = NOW()`,
        [row.store_id, eventSeq],
      );
      await client.query("COMMIT");
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  }
  return result.rowCount;
}

export async function getSnapshot(pool, storeId) {
  const result = await pool.query(
    `SELECT version::text, snapshot, updated_at
     FROM codex_thread_store_snapshots WHERE store_id = $1`,
    [storeId],
  );
  const head = await getStoreHead(pool, storeId);
  if (head.version && Number(result.rows[0]?.version) !== head.version) {
    const current = await canonicalStore(pool, storeId, head.version);
    return { version: current.version, snapshot: { ...current.state, histories: current.histories }, updatedAt: current.updatedAt };
  }
  if (result.rowCount === 0) {
    return { version: 0, snapshot: null, updatedAt: null };
  }
  const row = result.rows[0];
  return {
    version: Number.parseInt(row.version, 10),
    snapshot: row.snapshot,
    updatedAt: row.updated_at.toISOString(),
  };
}

async function putSnapshotTransaction(pool, storeId, body, headers) {
  if (!Number.isSafeInteger(body.expectedVersion) || body.expectedVersion < 0) {
    return { status: 400, body: { error: "expectedVersion must be a non-negative integer" } };
  }
  if (body.snapshot === null || typeof body.snapshot !== "object" || Array.isArray(body.snapshot)) {
    return { status: 400, body: { error: "snapshot must be a JSON object" } };
  }
  const operationHeader = headers["x-codex-operation-id"];
  const operationId =
    typeof operationHeader === "string" && /^[0-9a-f-]{36}$/i.test(operationHeader)
      ? operationHeader
      : crypto.randomUUID();
  const codexVersion =
    typeof headers["x-codex-version"] === "string" ? headers["x-codex-version"] : null;

  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const lockedVersion = await acquireStoreWriteLock(client, storeId);
    let current = await client.query(
      `SELECT version::text, snapshot
       FROM codex_thread_store_snapshots WHERE store_id = $1 FOR UPDATE`,
      [storeId],
    );
    if (lockedVersion && Number(current.rows[0]?.version) !== lockedVersion) {
      const canonical = await canonicalStore(client, storeId, lockedVersion);
      current = { rowCount: 1, rows: [{ version: canonical.version, snapshot: { ...canonical.state, histories: canonical.histories } }] };
    }
    const currentVersion =
      current.rowCount === 0 ? 0 : Number.parseInt(current.rows[0].version, 10);
    if (lockedVersion !== currentVersion) {
      throw new Error(
        `store head ${lockedVersion} does not match snapshot version ${currentVersion}`,
      );
    }
    const duplicate = await client.query(
      `SELECT event_seq::text FROM codex_store_events
       WHERE store_id = $1 AND operation_id = $2`,
      [storeId, operationId],
    );
    if (duplicate.rowCount > 0) {
      await client.query("ROLLBACK");
      return {
        status: 200,
        body: {
          version: Number.parseInt(duplicate.rows[0].event_seq, 10),
          operationId,
          duplicate: true,
        },
      };
    }

    if (currentVersion !== body.expectedVersion) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: { error: "snapshot version conflict", currentVersion },
      };
    }

    await assertThreadsNotDeleted(client, storeId, snapshotThreadIds(body.snapshot));
    const latestEvent = await client.query(
      `SELECT history_manifest FROM codex_store_events
       WHERE store_id = $1 ORDER BY event_seq DESC LIMIT 1`,
      [storeId],
    );
    const previousManifest = latestEvent.rowCount === 0 ? {} : latestEvent.rows[0].history_manifest;
    const previousSnapshot = current.rowCount === 0 ? {} : current.rows[0].snapshot;
    const eventSeq = await advanceStoreHead(client, storeId, currentVersion);
    const appendResult = await appendCanonicalEvent(client, {
      storeId,
      eventSeq,
      previousEventSeq: currentVersion,
      operationId,
      codexVersion,
      snapshot: body.snapshot,
      previousSnapshot,
      previousManifest,
    });

    const updated = await client.query(
      `INSERT INTO codex_thread_store_snapshots (store_id, version, snapshot)
       VALUES ($1, $2, $3::json)
       ON CONFLICT (store_id) DO UPDATE SET
         version = EXCLUDED.version,
         snapshot = EXCLUDED.snapshot,
         updated_at = NOW()
       RETURNING updated_at`,
      [storeId, eventSeq, JSON.stringify(body.snapshot)],
    );
    await client.query("COMMIT");
    return {
      status: 200,
      body: {
        version: eventSeq,
        operationId,
        appendedItemCount: appendResult.appendedItemCount,
        updatedAt: updated.rows[0].updated_at.toISOString(),
      },
    };
  } catch (error) {
    await client.query("ROLLBACK");
    if (error.code === "23505") {
      const latest = await client.query(
        "SELECT COALESCE(MAX(event_seq), 0)::text AS version FROM codex_store_events WHERE store_id = $1",
        [storeId],
      );
      return {
        status: 409,
        body: {
          error: "snapshot commit raced with another writer",
          currentVersion: Number.parseInt(latest.rows[0].version, 10),
        },
      };
    }
    throw error;
  } finally {
    client.release();
  }
}

export async function putSnapshot(pool, storeId, body, headers) {
  return withStoreWriteQueue(storeId, () => putSnapshotTransaction(pool, storeId, body, headers));
}

async function canonicalStore(client, storeId, exactVersion = null) {
  if (exactVersion === 0) {
    return { version: 0, state: {}, manifest: {}, histories: {}, updatedAt: null };
  }
  const latest = await client.query(
    `SELECT events.event_seq::text, events.state, events.history_manifest, events.created_at
     FROM codex_store_events AS events
     WHERE events.store_id = $1 AND ($2::bigint IS NULL OR events.event_seq = $2)
     ORDER BY events.event_seq DESC LIMIT 1`,
    [storeId, exactVersion],
  );
  if (latest.rowCount === 0) {
    if (exactVersion !== null) {
      const error = new Error(`store head ${exactVersion} is not committed yet for ${storeId}`);
      error.code = "STORE_HEAD_NOT_READY";
      throw error;
    }
    return { version: 0, state: {}, manifest: {}, histories: {}, updatedAt: null };
  }
  const row = latest.rows[0];
  const manifest = objectOrEmpty(row.history_manifest);
  const histories = {};
  for (const [threadId, entry] of Object.entries(manifest)) {
    const items = await client.query(
      `SELECT payload FROM codex_thread_events
       WHERE store_id = $1 AND thread_id = $2 AND generation = $3
         AND store_event_seq <= $4
       ORDER BY item_seq ASC`,
      [storeId, threadId, entry.generation, row.event_seq],
    );
    if (items.rowCount !== entry.itemCount) {
      throw new Error(
        `canonical history ${threadId} expected ${entry.itemCount} items, found ${items.rowCount}`,
      );
    }
    histories[threadId] = items.rows.map((item) => item.payload);
  }
  return {
    version: Number.parseInt(row.event_seq, 10),
    state: row.state,
    manifest,
    histories,
    updatedAt: row.created_at.toISOString(),
  };
}

export async function getStoreHead(pool, storeId) {
  const latest = await pool.query(
    `SELECT heads.version::text, events.state, events.history_manifest, events.created_at
     FROM codex_store_heads AS heads
     LEFT JOIN codex_store_events AS events
       ON events.store_id = heads.store_id AND events.event_seq = heads.version
     WHERE heads.store_id = $1`,
    [storeId],
  );
  if (latest.rowCount === 0) {
    return { version: 0, state: {}, historyManifest: {}, updatedAt: null };
  }
  const row = latest.rows[0];
  const version = Number.parseInt(row.version, 10);
  if (version === 0) {
    return { version: 0, state: {}, historyManifest: {}, updatedAt: null };
  }
  if (row.state === null) {
    throw new Error(`store head ${version} is missing its canonical event`);
  }
  return {
    version,
    state: row.state,
    historyManifest: row.history_manifest,
    updatedAt: row.created_at.toISOString(),
  };
}

export async function getThreadHistory(pool, storeId, threadId, generation, throughVersion) {
  let selectedGeneration = generation;
  let expectedCount = null;
  const versionedManifest = await pool.query(
    `SELECT events.event_seq::text, events.history_manifest
     FROM codex_store_events AS events
     WHERE events.store_id = $1 AND ($2::bigint IS NULL OR events.event_seq <= $2)
     ORDER BY events.event_seq DESC LIMIT 1`,
    [storeId, throughVersion],
  );
  const entry = versionedManifest.rows[0]?.history_manifest?.[threadId];
  if (selectedGeneration === null) {
    if (!entry) return { status: 404, body: { error: "thread history not found" } };
    selectedGeneration = entry.generation;
  }
  if (entry?.generation === selectedGeneration) {
    expectedCount = entry.itemCount;
  } else if (throughVersion !== null) {
    return {
      status: 404,
      body: {
        error: "thread history generation is not active at requested version",
        throughVersion,
      },
    };
  }
  const result = await pool.query(
    `SELECT payload FROM codex_thread_events
     WHERE store_id = $1 AND thread_id = $2 AND generation = $3
       AND ($4::bigint IS NULL OR store_event_seq <= $4)
     ORDER BY item_seq ASC`,
    [storeId, threadId, selectedGeneration, throughVersion],
  );
  if (result.rowCount === 0 && expectedCount !== 0) {
    return { status: 404, body: { error: "thread history generation not found" } };
  }
  if (expectedCount !== null && result.rowCount !== expectedCount) {
    return {
      status: 409,
      body: {
        error: "thread history changed while reading the requested version",
        expectedItemCount: expectedCount,
        actualItemCount: result.rowCount,
      },
    };
  }
  return {
    status: 200,
    body: {
      threadId,
      generation: selectedGeneration,
      itemCount: result.rowCount,
      items: result.rows.map((row) => row.payload),
    },
  };
}

function statePathValue(state, change) {
  let parent = state;
  for (let index = 0; index < change.path.length - 1; index += 1) {
    if (parent === null || typeof parent !== "object" || Array.isArray(parent)) {
      return { exists: false, value: undefined };
    }
    parent = parent[change.path[index]];
  }
  if (parent === null || typeof parent !== "object" || Array.isArray(parent)) {
    return { exists: false, value: undefined };
  }
  const key = change.path.at(-1);
  return {
    exists: Object.hasOwn(parent, key),
    value: parent[key],
  };
}

function applyStateChange(state, change) {
  let parent = state;
  for (let index = 0; index < change.path.length - 1; index += 1) {
    const key = change.path[index];
    const next = parent[key];
    if (next === null || typeof next !== "object" || Array.isArray(next)) {
      if (change.mode === "remove") return;
      parent[key] = {};
    }
    parent = parent[key];
  }
  const key = change.path.at(-1);
  if (change.mode === "remove") delete parent[key];
  else parent[key] = change.value;
}

function desiredStateValue(change) {
  return change.mode === "remove"
    ? { exists: false, value: undefined }
    : { exists: true, value: change.value };
}

function requiredStateConflictPolicy(path) {
  if (
    path[0] === "metadata_updates" &&
    ["updated_at", "advance_recency_at", "token_usage"].includes(path[2])
  ) {
    return "lastWriteWins";
  }
  return "compareAndSwap";
}

function matchesStateValue(actual, expected) {
  return (
    actual.exists === expected.exists &&
    (!actual.exists || jsonEqual(actual.value, expected.value))
  );
}

async function activeHistoryItems(
  client,
  { storeId, threadId, generation, throughVersion, afterItemSeq = 0, limit = null },
) {
  if (limit === 0) return [];
  const result = await client.query(
    `SELECT payload FROM codex_thread_events
     WHERE store_id = $1 AND thread_id = $2 AND generation = $3
       AND store_event_seq <= $4 AND item_seq > $5
     ORDER BY item_seq ASC
     LIMIT $6`,
    [storeId, threadId, generation, throughVersion, afterItemSeq, limit ?? 2_147_483_647],
  );
  return result.rows.map((row) => row.payload);
}

async function planHistoryDelta(client, storeId, throughVersion, entry, change) {
  const generation = entry?.generation ?? 0;
  const itemCount = entry?.itemCount ?? 0;
  const appendCanRebase =
    change.mode === "append" &&
    change.expectedGeneration === generation &&
    change.expectedItemCount <= itemCount;
  const exactMatch =
    change.expectedGeneration === generation && change.expectedItemCount === itemCount;
  if (!(appendCanRebase || exactMatch)) {
    return {
      conflict: {
        error: "thread generation conflict",
        threadId: change.threadId,
        currentGeneration: generation,
        currentItemCount: itemCount,
      },
    };
  }

  if (change.mode === "delete") {
    return { changed: entry !== undefined, nextEntry: null, appends: [] };
  }

  if (change.mode === "append") {
    const overlapLimit = Math.min(
      change.items.length,
      Math.max(0, itemCount - change.expectedItemCount),
    );
    const overlapping = entry
      ? await activeHistoryItems(client, {
          storeId,
          threadId: change.threadId,
          generation,
          throughVersion,
          afterItemSeq: change.expectedItemCount,
          limit: overlapLimit,
        })
      : [];
    if (overlapping.length !== overlapLimit) {
      throw new Error(
        `canonical history ${change.threadId} expected ${overlapLimit} overlapping items, found ${overlapping.length}`,
      );
    }
    let overlap = 0;
    while (
      overlap < overlapping.length &&
      jsonEqual(overlapping[overlap], change.items[overlap])
    ) {
      overlap += 1;
    }
    const appended = change.items.slice(overlap);
    const nextGeneration = entry ? generation : 1;
    return {
      // An empty append still creates a new empty history, matching the V1
      // snapshot semantics. An empty append to an existing history is a no-op.
      changed: entry === undefined || appended.length > 0,
      nextEntry: { generation: nextGeneration, itemCount: itemCount + appended.length },
      appends: appended.map((payload, index) => ({
        threadId: change.threadId,
        generation: nextGeneration,
        itemSeq: itemCount + index + 1,
        payload,
      })),
    };
  }

  const previous = entry
    ? await activeHistoryItems(client, {
        storeId,
        threadId: change.threadId,
        generation,
        throughVersion,
        limit: itemCount,
      })
    : undefined;
  if (previous && previous.length !== itemCount) {
    throw new Error(
      `canonical history ${change.threadId} expected ${itemCount} items, found ${previous.length}`,
    );
  }
  if (previous && jsonEqual(previous, change.items)) {
    return { changed: false, nextEntry: entry, appends: [] };
  }
  const appendOnly = previous !== undefined && isPrefix(previous, change.items);
  const nextGeneration = appendOnly ? Math.max(generation, 1) : generation + 1;
  const startIndex = appendOnly ? previous.length : 0;
  return {
    changed: true,
    nextEntry: { generation: nextGeneration, itemCount: change.items.length },
    appends: change.items.slice(startIndex).map((payload, index) => ({
      threadId: change.threadId,
      generation: nextGeneration,
      itemSeq: startIndex + index + 1,
      payload,
    })),
  };
}

async function insertHistoryAppends(client, storeId, eventSeq, codexVersion, appends) {
  const batchSize = 100;
  for (let offset = 0; offset < appends.length; offset += batchSize) {
    const parameters = [];
    const tuples = [];
    for (const item of appends.slice(offset, offset + batchSize)) {
      const base = parameters.length;
      parameters.push(
        storeId,
        item.threadId,
        item.generation,
        item.itemSeq,
        eventSeq,
        eventFormatVersion,
        codexVersion,
        JSON.stringify(item.payload),
        payloadHash(item.payload),
      );
      tuples.push(
        `($${base + 1},$${base + 2},$${base + 3},$${base + 4},$${base + 5},$${base + 6},$${base + 7},$${base + 8}::json,$${base + 9})`,
      );
    }
    await client.query(
      `INSERT INTO codex_thread_events (
         store_id, thread_id, generation, item_seq, store_event_seq,
         event_format_version, codex_version, payload, payload_sha256
       ) VALUES ${tuples.join(",")}`,
      parameters,
    );
  }
}

async function commitDeltaTransaction(pool, storeId, body, headers, raceAttempt = 0) {
  if (!Number.isSafeInteger(body.expectedVersion) || body.expectedVersion < 0) {
    return { status: 400, body: { error: "expectedVersion must be a non-negative integer" } };
  }
  if (!Array.isArray(body.stateChanges) || body.stateChanges.length > 100_000) {
    return { status: 400, body: { error: "stateChanges must be an array" } };
  }
  if (!Array.isArray(body.historyChanges) || body.historyChanges.length > 10_000) {
    return { status: 400, body: { error: "historyChanges must be an array" } };
  }
  const threadIds = new Set();
  const statePaths = new Set();
  for (const change of body.stateChanges) {
    const pathKey = JSON.stringify(change?.path ?? null);
    if (
      !change ||
      !Array.isArray(change.path) ||
      change.path.length === 0 ||
      change.path.length > 16 ||
      change.path[0] === "histories" ||
      change.path.some(
        (segment) => typeof segment !== "string" || segment.length === 0 || segment.length > 256,
      ) ||
      !["set", "remove"].includes(change.mode) ||
      !["compareAndSwap", "lastWriteWins"].includes(change.conflictPolicy) ||
      change.conflictPolicy !== requiredStateConflictPolicy(change.path) ||
      (change.mode === "set" && !Object.hasOwn(change, "value")) ||
      !change.expected ||
      typeof change.expected.exists !== "boolean" ||
      (change.expected.exists && !Object.hasOwn(change.expected, "value")) ||
      statePaths.has(pathKey)
    ) {
      return { status: 400, body: { error: "invalid state change" } };
    }
    statePaths.add(pathKey);
  }
  for (const change of body.historyChanges) {
    if (
      !change ||
      typeof change.threadId !== "string" ||
      change.threadId.length === 0 ||
      change.threadId.length > 256 ||
      threadIds.has(change.threadId) ||
      !["append", "replace", "delete"].includes(change.mode) ||
      !Number.isSafeInteger(change.expectedGeneration) ||
      change.expectedGeneration < 0 ||
      !Number.isSafeInteger(change.expectedItemCount) ||
      change.expectedItemCount < 0 ||
      (change.mode !== "delete" && !Array.isArray(change.items))
    ) {
      return { status: 400, body: { error: "invalid history change" } };
    }
    threadIds.add(change.threadId);
  }
  const operationHeader = headers["x-codex-operation-id"];
  const operationId =
    typeof operationHeader === "string" && /^[0-9a-f-]{36}$/i.test(operationHeader)
      ? operationHeader
      : crypto.randomUUID();
  const codexVersion =
    typeof headers["x-codex-version"] === "string" ? headers["x-codex-version"] : null;

  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const lockedVersion = await acquireStoreWriteLock(client, storeId);
    const duplicate = await client.query(
      `SELECT event_seq::text, history_manifest FROM codex_store_events
       WHERE store_id = $1 AND operation_id = $2`,
      [storeId, operationId],
    );
    if (duplicate.rowCount > 0) {
      await client.query("ROLLBACK");
      return {
        status: 200,
        body: {
          version: Number.parseInt(duplicate.rows[0].event_seq, 10),
          operationId,
          duplicate: true,
          rebased: true,
          historyManifest: duplicate.rows[0].history_manifest,
        },
      };
    }

    await assertThreadsNotDeleted(client, storeId, [
      ...body.historyChanges.filter(change => change.mode !== "delete").map(change => change.threadId),
      ...body.stateChanges.filter(change => change.mode !== "remove").flatMap(change =>
        change.path.length > 1 ? (change.path[0] === "rollout_paths" ? [change.value] : [change.path[1]])
          : snapshotThreadIds({ [change.path[0]]: change.value })),
    ].filter(value => typeof value === "string"));
    const current = await getStoreHead(client, storeId);
    if (current.version !== lockedVersion) {
      throw new Error(`store head ${lockedVersion} is missing its canonical event`);
    }
    const rebased = current.version !== body.expectedVersion;
    if (current.version < body.expectedVersion) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: { error: "expected version is ahead of store", currentVersion: current.version },
      };
    }
    const state = structuredClone(current.state);
    for (const change of body.stateChanges) {
      const actual = statePathValue(state, change);
      const desired = desiredStateValue(change);
      if (
        !matchesStateValue(actual, change.expected) &&
        !matchesStateValue(actual, desired) &&
        change.conflictPolicy !== "lastWriteWins"
      ) {
        await client.query("ROLLBACK");
        return {
          status: 409,
          body: {
            error: "state path conflict",
            path: change.path,
            currentExists: actual.exists,
          },
        };
      }
      applyStateChange(state, change);
    }
    const manifest = structuredClone(current.historyManifest);
    const appends = [];
    let historyChanged = false;
    for (const change of body.historyChanges) {
      const plan = await planHistoryDelta(
        client,
        storeId,
        current.version,
        current.historyManifest[change.threadId],
        change,
      );
      if (plan.conflict) {
        await client.query("ROLLBACK");
        return { status: 409, body: plan.conflict };
      }
      if (plan.changed) {
        historyChanged = true;
        if (plan.nextEntry === null) delete manifest[change.threadId];
        else manifest[change.threadId] = plan.nextEntry;
        appends.push(...plan.appends);
      }
    }
    if (!historyChanged && jsonEqual(state, current.state)) {
      await client.query("ROLLBACK");
      return {
        status: 200,
        body: {
          version: current.version,
          operationId,
          noChange: true,
          rebased,
          historyManifest: current.historyManifest,
          appendedItemCount: 0,
          updatedAt: current.updatedAt,
        },
      };
    }
    const eventSeq = await advanceStoreHead(client, storeId, current.version);
    if (process.env.DEBUG_STORE_COMMITS === "true") {
      console.log("remote store commit", {
        storeId,
        expectedVersion: body.expectedVersion,
        currentVersion: current.version,
        eventSeq,
        stateChangeCount: body.stateChanges.length,
        historyChanges: body.historyChanges.map((change) => ({
          threadId: change.threadId,
          mode: change.mode,
          expectedGeneration: change.expectedGeneration,
          expectedItemCount: change.expectedItemCount,
          itemCount: change.items?.length ?? 0,
        })),
      });
    }
    const inserted = await client.query(
      `INSERT INTO codex_store_events (
         store_id, event_seq, previous_event_seq, operation_id,
         event_format_version, codex_version, state, history_manifest
       ) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb)
       RETURNING created_at`,
      [
        storeId,
        eventSeq,
        current.version,
        operationId,
        eventFormatVersion,
        codexVersion,
        JSON.stringify(state),
        JSON.stringify(manifest),
      ],
    );
    await insertHistoryAppends(client, storeId, eventSeq, codexVersion, appends);
    await replaceProjections(client, storeId, state, manifest, eventSeq);
    // V1 snapshots are compatibility caches. Keeping a full materialized copy
    // here turns every streaming token into an O(total store size) rewrite.
    await client.query("DELETE FROM codex_thread_store_snapshots WHERE store_id = $1", [storeId]);
    await client.query("COMMIT");
    return {
      status: 200,
      body: {
        version: eventSeq,
        operationId,
        rebased,
        historyManifest: manifest,
        appendedItemCount: appends.length,
        updatedAt: inserted.rows[0].created_at.toISOString(),
      },
    };
  } catch (error) {
    await client.query("ROLLBACK");
    if (["23505", "STORE_HEAD_NOT_READY"].includes(error.code) && raceAttempt < 50) {
      return new Promise((resolve, reject) => {
        setTimeout(() => {
          commitDeltaTransaction(pool, storeId, body, headers, raceAttempt + 1).then(
            resolve,
            reject,
          );
        }, Math.min(100, 5 * (raceAttempt + 1)));
      });
    }
    if (error.code === "23505") {
      const latest = await client.query(
        "SELECT COALESCE(MAX(event_seq), 0)::text AS version FROM codex_store_events WHERE store_id = $1",
        [storeId],
      );
      return {
        status: 409,
        body: {
          error: "delta commit raced with another writer",
          currentVersion: Number.parseInt(latest.rows[0].version, 10),
        },
      };
    }
    throw error;
  } finally {
    client.release();
  }
}

export async function commitDelta(pool, storeId, body, headers) {
  return withStoreWriteQueue(storeId, () =>
    commitDeltaTransaction(pool, storeId, body, headers),
  );
}

// Import already-staged history without materializing a whole thread/store in
// JavaScript. This uses the same canonical event, generation and writer lock
// rules as commitDelta. The one transaction makes cancellation all-or-nothing.
export async function commitImportedHistory(pool, storeId, value, context = {}) {
  return withStoreWriteQueue(storeId, async () => {
    const client = await pool.connect();
    const { threadId, importId, count, created, metadata, normalize, codexVersion } = value;
    const { signal, onProgress = () => {} } = context;
    const check = () => signal?.throwIfAborted();
    const batchSize = 100;
    try {
      await client.query("BEGIN");
      check();
      await acquireStoreWriteLock(client, storeId);
      await assertThreadsNotDeleted(client, storeId, [threadId]);
      const head = await getStoreHead(client, storeId);
      const existing = head.historyManifest[threadId];
      const previousCount = existing?.itemCount ?? 0;
      let replace = false;
      const segments = value.segments ?? [{ importId, firstLine: 1, count, boundary: null }];
      for (const [index, segment] of segments.entries()) {
        check();
        await client.query(`INSERT INTO mira_codex_session_import_segments
          (import_id,segment_index,source_import_id,first_line_seq,item_count,end_position)
          VALUES ($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`,
        [importId, index, segment.importId, segment.firstLine, segment.count, JSON.stringify(segment.boundary)]);
      }
      const saved = await client.query(`SELECT source_import_id AS "importId",first_line_seq::text AS "firstLine",
        item_count::text AS count,end_position AS boundary FROM mira_codex_session_import_segments WHERE import_id=$1 ORDER BY segment_index`, [importId]);
      if (!jsonEqual(saved.rows.map((s) => ({ ...s, firstLine: Number(s.firstLine), count: Number(s.count) })), segments)) {
        throw Object.assign(new Error("同一源会话的祖先历史发生变化，未覆盖已保存的引用关系"), { code: "history_diverged", statusCode: 409 });
      }
      const sourceBatch = async (after, limit = batchSize) => {
        const rows = [];
        let start = 0;
        for (const segment of segments) {
          const skip = Math.max(0, after - start);
          start += segment.count;
          if (skip >= segment.count) continue;
          const take = Math.min(limit - rows.length, segment.count - skip);
          rows.push(...(await client.query(`SELECT raw_record FROM mira_codex_session_import_records
            WHERE import_id=$1 AND line_seq >= $2 AND line_seq < $3 ORDER BY line_seq`,
          [segment.importId, segment.firstLine + skip, segment.firstLine + skip + take])).rows);
          if (rows.length === limit) break;
        }
        return rows;
      };
      const existingBatch = async (after, limit = batchSize) => (await client.query(
        `SELECT payload FROM codex_thread_events
         WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4
         AND store_event_seq<=$5 ORDER BY item_seq LIMIT $6`,
        [storeId, threadId, existing?.generation ?? 0, after, head.version, limit])).rows;
      for (let offset = 0; offset < Math.min(count, previousCount); offset += batchSize) {
        check();
        const limit = Math.min(batchSize, count - offset, previousCount - offset);
        const source = await sourceBatch(offset, limit);
        const target = await existingBatch(offset, limit);
        if (source.length !== limit || target.length !== limit) throw new Error("Incomplete canonical import history");
        for (let index = 0; index < limit; index++) {
          const desired = normalize(source[index].raw_record);
          if (!jsonEqual(normalize(target[index].payload), desired)) {
            throw Object.assign(new Error("本地会话与数据库历史已分叉；源记录已保留，未覆盖现有会话"), { code: "history_diverged", statusCode: 409 });
          }
          if (!jsonEqual(target[index].payload, desired)) replace = true;
        }
        onProgress({ phase: "validating", records: offset + limit, totalRecords: Math.min(count, previousCount) });
      }
      const state = structuredClone(head.state);
      state.created_threads ??= {};
      state.metadata_updates ??= {};
      if (!state.created_threads[threadId]) state.created_threads[threadId] = created;
      else state.created_threads[threadId].history_mode = "legacy";
      state.metadata_updates[threadId] ??= metadata;
      if (existing && previousCount >= count && !replace && jsonEqual(state, head.state)) {
        check();
        await client.query("UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1", [importId, head.version]);
        await client.query("COMMIT");
        return { version: head.version, noChange: true };
      }
      const manifest = structuredClone(head.historyManifest);
      const generation = existing ? existing.generation + (replace ? 1 : 0) : 1;
      const total = Math.max(count, previousCount);
      manifest[threadId] = { generation, itemCount: total };
      const version = await advanceStoreHead(client, storeId, head.version);
      await client.query(`INSERT INTO codex_store_events
        (store_id,event_seq,previous_event_seq,operation_id,event_format_version,codex_version,state,history_manifest)
        VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb)`,
      [storeId, version, head.version, crypto.randomUUID(), eventFormatVersion, codexVersion, JSON.stringify(state), JSON.stringify(manifest)]);
      for (let offset = replace ? 0 : previousCount; offset < total;) {
        check();
        const fromSource = offset < count;
        const limit = Math.min(batchSize, (fromSource ? count : total) - offset);
        const rows = fromSource ? await sourceBatch(offset, limit) : await existingBatch(offset, limit);
        if (rows.length !== limit) throw new Error("Incomplete staged import history");
        const parameters = [];
        const tuples = [];
        for (const row of rows) {
          check();
          const payload = normalize(fromSource ? row.raw_record : row.payload);
          const base = parameters.length;
          parameters.push(storeId, threadId, generation, ++offset, version, eventFormatVersion, codexVersion, JSON.stringify(payload), payloadHash(payload));
          tuples.push(`($${base+1},$${base+2},$${base+3},$${base+4},$${base+5},$${base+6},$${base+7},$${base+8}::json,$${base+9})`);
        }
        await client.query(`INSERT INTO codex_thread_events
          (store_id,thread_id,generation,item_seq,store_event_seq,event_format_version,codex_version,payload,payload_sha256)
          VALUES ${tuples.join(",")}`, parameters);
        onProgress({ phase: "publishing", records: offset, totalRecords: total });
      }
      await replaceProjections(client, storeId, state, manifest, version);
      // Compatibility snapshots are rebuildable caches, not the source of
      // truth. V1 reads rebuild on demand; normal V2 reads use canonical rows.
      await client.query("DELETE FROM codex_thread_store_snapshots WHERE store_id=$1", [storeId]);
      await client.query("UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1", [importId, version]);
      check();
      await client.query("COMMIT");
      return { version };
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally { client.release(); }
  });
}

export async function listStoreEvents(pool, storeId, after, limit) {
  const result = await pool.query(
    `SELECT events.event_seq::text, events.previous_event_seq::text, events.operation_id,
            events.event_format_version, events.codex_version, events.history_manifest,
            events.created_at
     FROM codex_store_events AS events
     WHERE events.store_id = $1 AND events.event_seq > $2
     ORDER BY events.event_seq ASC LIMIT $3`,
    [storeId, after, limit],
  );
  return result.rows.map((row) => ({
    eventSeq: Number.parseInt(row.event_seq, 10),
    previousEventSeq: Number.parseInt(row.previous_event_seq, 10),
    operationId: row.operation_id,
    eventFormatVersion: row.event_format_version,
    codexVersion: row.codex_version,
    historyManifest: row.history_manifest,
    createdAt: row.created_at.toISOString(),
  }));
}

export async function listThreadEvents(pool, storeId, threadId, generation, after, limit) {
  const params = [storeId, threadId, after, limit];
  const generationClause = generation === null ? "" : "AND generation = $5";
  if (generation !== null) params.push(generation);
  const result = await pool.query(
    `SELECT events.generation::text, events.item_seq::text, events.store_event_seq::text,
            events.event_format_version, events.codex_version, events.payload,
            events.payload_sha256, events.created_at
     FROM codex_thread_events AS events
     WHERE events.store_id = $1 AND events.thread_id = $2 AND events.item_seq > $3
       ${generationClause}
     ORDER BY events.generation ASC, events.item_seq ASC LIMIT $4`,
    params,
  );
  return result.rows.map((row) => ({
    generation: Number.parseInt(row.generation, 10),
    itemSeq: Number.parseInt(row.item_seq, 10),
    storeEventSeq: Number.parseInt(row.store_event_seq, 10),
    eventFormatVersion: row.event_format_version,
    codexVersion: row.codex_version,
    payload: row.payload,
    payloadSha256: row.payload_sha256,
    createdAt: row.created_at.toISOString(),
  }));
}

export async function rebuildSnapshot(pool, storeId) {
  const latest = await pool.query(
    `SELECT events.event_seq::text, events.state, events.history_manifest
     FROM codex_store_events AS events WHERE events.store_id = $1
     ORDER BY events.event_seq DESC LIMIT 1`,
    [storeId],
  );
  if (latest.rowCount === 0) {
    return { status: 404, body: { error: "store has no canonical events" } };
  }
  const row = latest.rows[0];
  const eventSeq = Number.parseInt(row.event_seq, 10);
  const histories = {};
  for (const [threadId, entry] of Object.entries(objectOrEmpty(row.history_manifest))) {
    const result = await pool.query(
      `SELECT payload FROM codex_thread_events
       WHERE store_id = $1 AND thread_id = $2 AND generation = $3
         AND store_event_seq <= $4
       ORDER BY item_seq ASC`,
      [storeId, threadId, entry.generation, eventSeq],
    );
    if (result.rowCount !== entry.itemCount) {
      throw new Error(
        `cannot rebuild ${threadId}: expected ${entry.itemCount} items, found ${result.rowCount}`,
      );
    }
    histories[threadId] = result.rows.map((item) => item.payload);
  }
  const snapshot = { ...row.state, histories };
  await pool.query(
    `INSERT INTO codex_thread_store_snapshots (store_id, version, snapshot)
     VALUES ($1, $2, $3::json)
     ON CONFLICT (store_id) DO UPDATE SET
       version = EXCLUDED.version,
       snapshot = EXCLUDED.snapshot,
       updated_at = NOW()`,
    [storeId, eventSeq, JSON.stringify(snapshot)],
  );
  return { status: 200, body: { storeId, version: eventSeq, rebuilt: true } };
}
