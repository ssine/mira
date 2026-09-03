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
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`,
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
    const current = await client.query(
      `SELECT version::text, snapshot
       FROM codex_thread_store_snapshots WHERE store_id = $1 FOR UPDATE`,
      [storeId],
    );
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
       VALUES ($1, $2, $3::jsonb)
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

    const current = await canonicalStore(client, storeId, lockedVersion);
    const rebased = current.version !== body.expectedVersion;
    if (current.version < body.expectedVersion) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: { error: "expected version is ahead of store", currentVersion: current.version },
      };
    }
    const histories = { ...current.histories };
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
    for (const change of body.historyChanges) {
      const entry = current.manifest[change.threadId];
      const generation = entry?.generation ?? 0;
      const itemCount = entry?.itemCount ?? 0;
      const appendCanRebase =
        change.mode === "append" &&
        change.expectedGeneration === generation &&
        change.expectedItemCount <= itemCount;
      const exactMatch =
        change.expectedGeneration === generation && change.expectedItemCount === itemCount;
      if (!(appendCanRebase || exactMatch)) {
        await client.query("ROLLBACK");
        return {
          status: 409,
          body: {
            error: "thread generation conflict",
            threadId: change.threadId,
            currentGeneration: generation,
            currentItemCount: itemCount,
          },
        };
      }
      if (change.mode === "delete") delete histories[change.threadId];
      else if (change.mode === "replace") histories[change.threadId] = change.items;
      else {
        const currentItems = histories[change.threadId] ?? [];
        let overlap = 0;
        while (
          overlap < change.items.length &&
          change.expectedItemCount + overlap < currentItems.length &&
          jsonEqual(currentItems[change.expectedItemCount + overlap], change.items[overlap])
        ) {
          overlap += 1;
        }
        histories[change.threadId] = [...currentItems, ...change.items.slice(overlap)];
      }
    }
    const snapshot = { ...state, histories };
    if (jsonEqual(snapshot, { ...current.state, histories: current.histories })) {
      await client.query("ROLLBACK");
      return {
        status: 200,
        body: {
          version: current.version,
          operationId,
          noChange: true,
          rebased,
          historyManifest: current.manifest,
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
    const appendResult = await appendCanonicalEvent(client, {
      storeId,
      eventSeq,
      previousEventSeq: current.version,
      operationId,
      codexVersion,
      snapshot,
      previousSnapshot: { ...current.state, histories: current.histories },
      previousManifest: current.manifest,
    });
    const updated = await client.query(
      `INSERT INTO codex_thread_store_snapshots (store_id, version, snapshot)
       VALUES ($1, $2, $3::jsonb)
       ON CONFLICT (store_id) DO UPDATE SET
         version = EXCLUDED.version,
         snapshot = EXCLUDED.snapshot,
         updated_at = NOW()
       RETURNING updated_at`,
      [storeId, eventSeq, JSON.stringify(snapshot)],
    );
    await client.query("COMMIT");
    return {
      status: 200,
      body: {
        version: eventSeq,
        operationId,
        rebased,
        historyManifest: appendResult.manifest,
        appendedItemCount: appendResult.appendedItemCount,
        updatedAt: updated.rows[0].updated_at.toISOString(),
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
     VALUES ($1, $2, $3::jsonb)
     ON CONFLICT (store_id) DO UPDATE SET
       version = EXCLUDED.version,
       snapshot = EXCLUDED.snapshot,
       updated_at = NOW()`,
    [storeId, eventSeq, JSON.stringify(snapshot)],
  );
  return { status: 200, body: { storeId, version: eventSeq, rebuilt: true } };
}
