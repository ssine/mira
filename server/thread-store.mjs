import crypto from "node:crypto";
import {
  currentHead,
  historicalManifest,
  lockScope,
  beginReceipt,
  persistState,
  persistBoundaries,
  publishReceipt,
  rebuildStateEntries,
} from "./storage-rows.mjs";

const eventFormatVersion = 1;

function objectOrEmpty(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value
    : {};
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
  const result = await client.query(
    "SELECT thread_id FROM mira_thread_actions WHERE store_id=$1 AND action='delete' AND thread_id=ANY($2::text[]) LIMIT 1",
    [storeId, threadIds],
  );
  if (result.rowCount)
    throw Object.assign(new Error("此会话已永久删除，不能继续写入或恢复。"), {
      statusCode: 410,
      code: "thread_deleted",
    });
}

function snapshotThreadIds(snapshot) {
  return Object.entries(objectOrEmpty(snapshot)).flatMap(([key, value]) =>
    key === "rollout_paths"
      ? Object.values(objectOrEmpty(value))
      : Object.keys(objectOrEmpty(value)),
  );
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
  if (
    !Array.isArray(previous) ||
    !Array.isArray(next) ||
    previous.length > next.length
  ) {
    return false;
  }
  return previous.every((item, index) => jsonEqual(item, next[index]));
}

function payloadHash(payload) {
  return crypto
    .createHash("sha256")
    .update(stableStringify(payload))
    .digest("hex");
}

function buildHistoryPlan(
  previousSnapshot,
  nextSnapshot,
  previousManifest = {},
) {
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
    const appendOnly =
      Array.isArray(previousHistory) &&
      isPrefix(previousHistory, nextHistoryValue);
    const generation = appendOnly
      ? Math.max(previousGeneration, 1)
      : previousGeneration + 1;
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

async function replaceProjections(
  client,
  storeId,
  snapshot,
  manifest,
  eventSeq,
  affected = new Set(Object.keys(manifest)),
  metadataAffected = affected,
) {
  for (const [threadId, manifestEntry] of Object.entries(manifest)) {
    if (!affected.has(threadId)) continue;
    if (!metadataAffected.has(threadId)) {
      const updated = await client.query(
        `UPDATE codex_thread_projections SET active_generation=$3,item_count=$4,updated_at=NOW()
        WHERE store_id=$1 AND thread_id=$2`,
        [storeId, threadId, manifestEntry.generation, manifestEntry.itemCount],
      );
      if (updated.rowCount) continue;
    }
    const projection = projectionForThread(
      snapshot,
      threadId,
      manifestEntry,
      eventSeq,
    );
    await client.query(
      `INSERT INTO codex_thread_projections (
         store_id, thread_id, active_generation, item_count, parent_thread_id,
         source_kind, title, cwd, state, through_event_seq
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
       ON CONFLICT(store_id,thread_id) DO UPDATE SET active_generation=EXCLUDED.active_generation,
         item_count=EXCLUDED.item_count,parent_thread_id=EXCLUDED.parent_thread_id,source_kind=EXCLUDED.source_kind,
         title=EXCLUDED.title,cwd=EXCLUDED.cwd,state=EXCLUDED.state,through_event_seq=EXCLUDED.through_event_seq,updated_at=NOW()`,
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

export async function seedLegacySnapshots() {
  return 0;
}

export async function getStoreHead(pool, storeId, threadIds = null) {
  return currentHead(pool, storeId, threadIds);
}

async function canonicalStore(client, storeId) {
  const head = await currentHead(client, storeId);
  const histories = {};
  for (const [threadId, entry] of Object.entries(head.historyManifest)) {
    const rows = await client.query(
      `SELECT payload FROM codex_thread_events_versioned
      WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND store_event_seq<=$4 ORDER BY item_seq`,
      [storeId, threadId, entry.generation, head.version],
    );
    if (rows.rowCount !== entry.itemCount)
      throw new Error(`incomplete history for ${threadId}`);
    histories[threadId] = rows.rows.map((row) => row.payload);
  }
  return { ...head, histories };
}

export async function getSnapshot(pool, storeId) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY");
    const current = await canonicalStore(client, storeId);
    await client.query("COMMIT");
    return {
      version: current.version,
      snapshot: current.version
        ? { ...current.state, histories: current.histories }
        : null,
      updatedAt: current.updatedAt,
    };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

export async function putSnapshot(pool, storeId, body, headers) {
  if (
    !Number.isSafeInteger(body.expectedVersion) ||
    body.expectedVersion < 0 ||
    !body.snapshot ||
    typeof body.snapshot !== "object" ||
    Array.isArray(body.snapshot)
  )
    return { status: 400, body: { error: "invalid compatibility snapshot" } };
  const operationId = headers["x-codex-operation-id"] ?? crypto.randomUUID();
  const codexVersion = headers["x-codex-version"] ?? null;
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const duplicate = await beginReceipt(
      client,
      storeId,
      operationId,
      body,
      codexVersion,
    );
    if (duplicate) {
      await client.query("ROLLBACK");
      return duplicate;
    }
    await lockScope(client, storeId);
    const current = await canonicalStore(client, storeId);
    if (body.expectedVersion !== current.version) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: {
          error: "snapshot version conflict",
          currentVersion: current.version,
        },
      };
    }
    await assertThreadsNotDeleted(
      client,
      storeId,
      snapshotThreadIds(body.snapshot),
    );
    const { manifest, appends } = buildHistoryPlan(
      { ...current.state, histories: current.histories },
      body.snapshot,
      current.historyManifest,
    );
    await repairNewGenerations(
      client,
      storeId,
      current.historyManifest,
      manifest,
      appends,
    );
    const state = stateFromSnapshot(body.snapshot);
    const affected = await persistState(
      client,
      storeId,
      operationId,
      current.state,
      state,
    );
    for (const id of await persistBoundaries(
      client,
      storeId,
      operationId,
      current.historyManifest,
      manifest,
    ))
      affected.add(id);
    await insertHistoryAppends(
      client,
      storeId,
      operationId,
      codexVersion,
      appends,
    );
    await replaceProjections(client, storeId, state, manifest, 1, affected);
    const version = await publishReceipt(
      client,
      storeId,
      operationId,
      affected,
      appends.length,
    );
    await client.query("COMMIT");
    return {
      status: 200,
      body: {
        version,
        operationId,
        appendedItemCount: appends.length,
        updatedAt: new Date().toISOString(),
      },
    };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

async function repairNewGenerations(client, storeId, before, after, appends) {
  for (const [id, entry] of Object.entries(after))
    if (!before[id]) {
      const previous = await client.query(
        "SELECT COALESCE(MAX(generation),0)::text AS generation FROM codex_thread_events WHERE store_id=$1 AND thread_id=$2",
        [storeId, id],
      );
      const generations = await client.query(
        "SELECT COALESCE(MAX(generation),0)::text AS generation FROM codex_thread_revisions WHERE store_id=$1 AND thread_id=$2",
        [storeId, id],
      );
      const generation =
        Math.max(
          Number(previous.rows[0].generation),
          Number(generations.rows[0].generation),
        ) + 1;
      entry.generation = generation;
      for (const item of appends)
        if (item.threadId === id) item.generation = generation;
    }
}

export async function getThreadHistory(
  pool,
  storeId,
  threadId,
  generation,
  throughVersion,
) {
  const head = await pool.query(
    "SELECT version::text,history_floor::text FROM codex_store_heads WHERE store_id=$1",
    [storeId],
  );
  const version = throughVersion ?? Number(head.rows[0]?.version ?? 0);
  if (version < Number(head.rows[0]?.history_floor ?? 0))
    return {
      status: 410,
      body: {
        error: "requested version predates the storage migration",
        code: "store_version_retired",
      },
    };
  if (
    (
      await pool.query(
        "SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete'",
        [storeId, threadId],
      )
    ).rowCount
  )
    return {
      status: 404,
      body: {
        error: "thread history was permanently deleted",
        code: "thread_deleted",
      },
    };
  const manifest = await historicalManifest(pool, storeId, version, [threadId]);
  const entry = manifest[threadId];
  const selected = generation ?? entry?.generation;
  if (!selected || (throughVersion !== null && entry?.generation !== selected))
    return { status: 404, body: { error: "thread history not found" } };
  const rows = await pool.query(
    `SELECT payload FROM codex_thread_events_versioned WHERE store_id=$1 AND thread_id=$2 AND generation=$3
    AND store_event_seq<=$4 ORDER BY item_seq`,
    [storeId, threadId, selected, version],
  );
  if (entry?.generation === selected && rows.rowCount !== entry.itemCount)
    return { status: 409, body: { error: "thread history is incomplete" } };
  if (!rows.rowCount && entry?.generation !== selected)
    return { status: 404, body: { error: "thread history not found" } };
  return {
    status: 200,
    body: {
      threadId,
      generation: selected,
      itemCount: rows.rowCount,
      items: rows.rows.map((row) => row.payload),
    },
  };
}

function statePathValue(state, change) {
  let parent = state;
  for (let index = 0; index < change.path.length - 1; index += 1) {
    if (
      parent === null ||
      typeof parent !== "object" ||
      Array.isArray(parent)
    ) {
      return { exists: false, value: undefined };
    }
    if (!Object.hasOwn(parent, change.path[index]))
      return { exists: false, value: undefined };
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
    const next = Object.hasOwn(parent, key) ? parent[key] : undefined;
    if (next === null || typeof next !== "object" || Array.isArray(next)) {
      if (change.mode === "remove") return;
      Object.defineProperty(parent, key, {
        value: {},
        enumerable: true,
        writable: true,
        configurable: true,
      });
    }
    parent = parent[key];
  }
  const key = change.path.at(-1);
  if (change.mode === "remove") delete parent[key];
  else
    Object.defineProperty(parent, key, {
      value: change.value,
      enumerable: true,
      writable: true,
      configurable: true,
    });
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
  {
    storeId,
    threadId,
    generation,
    throughVersion,
    afterItemSeq = 0,
    limit = null,
  },
) {
  if (limit === 0) return [];
  const result = await client.query(
    `SELECT payload FROM codex_thread_events_versioned
     WHERE store_id = $1 AND thread_id = $2 AND generation = $3
       AND store_event_seq <= $4 AND item_seq > $5
     ORDER BY item_seq ASC
     LIMIT $6`,
    [
      storeId,
      threadId,
      generation,
      throughVersion,
      afterItemSeq,
      limit ?? 2_147_483_647,
    ],
  );
  return result.rows.map((row) => row.payload);
}

async function planHistoryDelta(
  client,
  storeId,
  throughVersion,
  entry,
  change,
) {
  const generation = entry?.generation ?? 0;
  const itemCount = entry?.itemCount ?? 0;
  const appendCanRebase =
    change.mode === "append" &&
    change.expectedGeneration === generation &&
    change.expectedItemCount <= itemCount;
  const exactMatch =
    change.expectedGeneration === generation &&
    change.expectedItemCount === itemCount;
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
      nextEntry: {
        generation: nextGeneration,
        itemCount: itemCount + appended.length,
      },
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

async function insertHistoryAppends(
  client,
  storeId,
  operationId,
  codexVersion,
  appends,
) {
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
        operationId,
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
         store_id, thread_id, generation, item_seq, operation_id,
         event_format_version, codex_version, payload, payload_sha256
       ) VALUES ${tuples.join(",")}`,
      parameters,
    );
  }
}

async function commitDeltaTransaction(
  pool,
  storeId,
  body,
  headers,
  requestIdentity = body,
) {
  if (!Number.isSafeInteger(body.expectedVersion) || body.expectedVersion < 0) {
    return {
      status: 400,
      body: { error: "expectedVersion must be a non-negative integer" },
    };
  }
  if (!Array.isArray(body.stateChanges) || body.stateChanges.length > 100_000) {
    return { status: 400, body: { error: "stateChanges must be an array" } };
  }
  if (
    !Array.isArray(body.historyChanges) ||
    body.historyChanges.length > 10_000
  ) {
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
        (segment) =>
          typeof segment !== "string" ||
          segment.length === 0 ||
          segment.length > 256,
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
    typeof operationHeader === "string" &&
    /^[0-9a-f-]{36}$/i.test(operationHeader)
      ? operationHeader
      : crypto.randomUUID();
  const codexVersion =
    typeof headers["x-codex-version"] === "string"
      ? headers["x-codex-version"]
      : null;

  const knownFields = new Set([
    "created_threads",
    "metadata_updates",
    "names",
    "sections",
    "section_positions",
    "section_entered_at",
  ]);
  const scoped = body.stateChanges.every(
    (change) => change.path.length >= 2 && knownFields.has(change.path[0]),
  );
  const ids = [
    ...new Set([
      ...threadIds,
      ...body.stateChanges.map((change) => change.path[1]).filter(Boolean),
    ]),
  ];
  const compactScope = headers["x-mira-thread-scope"];
  if (compactScope && (!scoped || ids.some((id) => id !== compactScope)))
    return { status: 400, body: { error: "invalid thread-scoped commit" } };
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const duplicate = await beginReceipt(
      client,
      storeId,
      operationId,
      requestIdentity,
      codexVersion,
    );
    if (duplicate) {
      if (compactScope && duplicate.body.historyManifest)
        duplicate.body.historyManifest = Object.fromEntries(
          Object.entries(duplicate.body.historyManifest).filter(
            ([id]) => id === compactScope,
          ),
        );
      await client.query("ROLLBACK");
      return duplicate;
    }
    await lockScope(client, storeId, scoped ? ids : null);
    await assertThreadsNotDeleted(
      client,
      storeId,
      [
        ...body.historyChanges
          .filter((change) => change.mode !== "delete")
          .map((change) => change.threadId),
        ...body.stateChanges
          .filter((change) => change.mode !== "remove")
          .flatMap((change) =>
            change.path.length > 1
              ? change.path[0] === "rollout_paths"
                ? [change.value]
                : [change.path[1]]
              : snapshotThreadIds({ [change.path[0]]: change.value }),
          ),
      ].filter((id) => typeof id === "string"),
    );
    const current = await currentHead(client, storeId, scoped ? ids : null);
    if (body.expectedVersion > current.version) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: {
          error: "expected version is ahead of store",
          currentVersion: current.version,
        },
      };
    }
    if (body.expectedVersion < current.historyFloor) {
      await client.query("ROLLBACK");
      return {
        status: 409,
        body: {
          error: "reload after storage migration",
          code: "store_version_retired",
          currentVersion: current.version,
        },
      };
    }
    const state = structuredClone(current.state);
    for (const change of body.stateChanges) {
      const actual = statePathValue(state, change),
        desired = desiredStateValue(change);
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
    const manifest = structuredClone(current.historyManifest),
      appends = [];
    let changed = false;
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
        changed = true;
        if (plan.nextEntry === null) delete manifest[change.threadId];
        else manifest[change.threadId] = plan.nextEntry;
        appends.push(...plan.appends);
      }
    }
    await repairNewGenerations(
      client,
      storeId,
      current.historyManifest,
      manifest,
      appends,
    );
    const affected = await persistState(
      client,
      storeId,
      operationId,
      current.state,
      state,
    );
    const metadataAffected = new Set(affected);
    for (const id of await persistBoundaries(
      client,
      storeId,
      operationId,
      current.historyManifest,
      manifest,
    ))
      affected.add(id);
    await insertHistoryAppends(
      client,
      storeId,
      operationId,
      codexVersion,
      appends,
    );
    await replaceProjections(
      client,
      storeId,
      state,
      manifest,
      1,
      affected,
      metadataAffected,
    );
    const noChange = !changed && jsonEqual(state, current.state);
    const version = await publishReceipt(
      client,
      storeId,
      operationId,
      affected,
      appends.length,
      noChange ? current.version : null,
    );
    await client.query("COMMIT");
    const responseManifest = compactScope
      ? manifest
      : await historicalManifest(client, storeId, version);
    return {
      status: 200,
      body: {
        version,
        operationId,
        rebased: version - (noChange ? 0 : 1) !== body.expectedVersion,
        ...(noChange ? { noChange: true } : {}),
        historyManifest: responseManifest,
        appendedItemCount: appends.length,
        updatedAt: new Date().toISOString(),
      },
    };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}

export async function commitDelta(
  pool,
  storeId,
  body,
  headers,
  requestIdentity = body,
) {
  return commitDeltaTransaction(pool, storeId, body, headers, requestIdentity);
}

// Import already-staged history without materializing a whole thread/store in
// JavaScript. This uses the same canonical event, generation and writer lock
// rules as commitDelta. The one transaction makes cancellation all-or-nothing.
export async function commitImportedHistory(
  pool,
  storeId,
  value,
  context = {},
) {
  return (async () => {
    const client = await pool.connect();
    const {
      threadId,
      importId,
      count,
      created,
      metadata,
      normalize,
      codexVersion,
    } = value;
    const { signal, onProgress = () => {} } = context;
    const check = () => signal?.throwIfAborted();
    const batchSize = 100;
    try {
      await client.query("BEGIN");
      check();
      await lockScope(client, storeId, [threadId]);
      await assertThreadsNotDeleted(client, storeId, [threadId]);
      const head = await getStoreHead(client, storeId, [threadId]);
      const existing = head.historyManifest[threadId];
      const previousCount = existing?.itemCount ?? 0;
      let replace = false;
      const segments = value.segments ?? [
        { importId, firstLine: 1, count, boundary: null },
      ];
      for (const [index, segment] of segments.entries()) {
        check();
        await client.query(
          `INSERT INTO mira_codex_session_import_segments
          (import_id,segment_index,source_import_id,first_line_seq,item_count,end_position)
          VALUES ($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT DO NOTHING`,
          [
            importId,
            index,
            segment.importId,
            segment.firstLine,
            segment.count,
            JSON.stringify(segment.boundary),
          ],
        );
      }
      const saved = await client.query(
        `SELECT source_import_id AS "importId",first_line_seq::text AS "firstLine",
        item_count::text AS count,end_position AS boundary FROM mira_codex_session_import_segments WHERE import_id=$1 ORDER BY segment_index`,
        [importId],
      );
      if (
        !jsonEqual(
          saved.rows.map((s) => ({
            ...s,
            firstLine: Number(s.firstLine),
            count: Number(s.count),
          })),
          segments,
        )
      ) {
        throw Object.assign(
          new Error("同一源会话的祖先历史发生变化，未覆盖已保存的引用关系"),
          { code: "history_diverged", statusCode: 409 },
        );
      }
      const sourceBatch = async (after, limit = batchSize) => {
        const rows = [];
        let start = 0;
        for (const segment of segments) {
          const skip = Math.max(0, after - start);
          start += segment.count;
          if (skip >= segment.count) continue;
          const take = Math.min(limit - rows.length, segment.count - skip);
          rows.push(
            ...(
              await client.query(
                `SELECT raw_record FROM mira_codex_session_import_records
            WHERE import_id=$1 AND line_seq >= $2 AND line_seq < $3 ORDER BY line_seq`,
                [
                  segment.importId,
                  segment.firstLine + skip,
                  segment.firstLine + skip + take,
                ],
              )
            ).rows,
          );
          if (rows.length === limit) break;
        }
        return rows;
      };
      const existingBatch = async (after, limit = batchSize) =>
        (
          await client.query(
            `SELECT payload FROM codex_thread_events_versioned
         WHERE store_id=$1 AND thread_id=$2 AND generation=$3 AND item_seq>$4
         AND store_event_seq<=$5 ORDER BY item_seq LIMIT $6`,
            [
              storeId,
              threadId,
              existing?.generation ?? 0,
              after,
              head.version,
              limit,
            ],
          )
        ).rows;
      for (
        let offset = 0;
        offset < Math.min(count, previousCount);
        offset += batchSize
      ) {
        check();
        const limit = Math.min(
          batchSize,
          count - offset,
          previousCount - offset,
        );
        const source = await sourceBatch(offset, limit);
        const target = await existingBatch(offset, limit);
        if (source.length !== limit || target.length !== limit)
          throw new Error("Incomplete canonical import history");
        for (let index = 0; index < limit; index++) {
          const desired = normalize(source[index].raw_record);
          if (!jsonEqual(normalize(target[index].payload), desired)) {
            throw Object.assign(
              new Error(
                "本地会话与数据库历史已分叉；源记录已保留，未覆盖现有会话",
              ),
              { code: "history_diverged", statusCode: 409 },
            );
          }
          if (!jsonEqual(target[index].payload, desired)) replace = true;
        }
        onProgress({
          phase: "validating",
          records: offset + limit,
          totalRecords: Math.min(count, previousCount),
        });
      }
      const state = structuredClone(head.state);
      state.created_threads ??= {};
      state.metadata_updates ??= {};
      if (!state.created_threads[threadId])
        state.created_threads[threadId] = created;
      else state.created_threads[threadId].history_mode = "legacy";
      state.metadata_updates[threadId] ??= metadata;
      if (
        existing &&
        previousCount >= count &&
        !replace &&
        jsonEqual(state, head.state)
      ) {
        check();
        await client.query(
          "UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1",
          [importId, head.version],
        );
        await client.query("COMMIT");
        return { version: head.version, noChange: true };
      }
      const manifest = structuredClone(head.historyManifest);
      const generation = existing ? existing.generation + (replace ? 1 : 0) : 1;
      const total = Math.max(count, previousCount);
      manifest[threadId] = { generation, itemCount: total };
      const operationId = crypto.randomUUID();
      await beginReceipt(
        client,
        storeId,
        operationId,
        { importId, threadId, count },
        codexVersion,
      );
      if (!existing) {
        await repairNewGenerations(
          client,
          storeId,
          head.historyManifest,
          manifest,
          [],
        );
      }
      const writeGeneration = manifest[threadId].generation;
      const affected = await persistState(
        client,
        storeId,
        operationId,
        head.state,
        state,
      );
      for (const id of await persistBoundaries(
        client,
        storeId,
        operationId,
        head.historyManifest,
        manifest,
      ))
        affected.add(id);
      for (let offset = replace ? 0 : previousCount; offset < total; ) {
        check();
        const fromSource = offset < count;
        const limit = Math.min(
          batchSize,
          (fromSource ? count : total) - offset,
        );
        const rows = fromSource
          ? await sourceBatch(offset, limit)
          : await existingBatch(offset, limit);
        if (rows.length !== limit)
          throw new Error("Incomplete staged import history");
        const parameters = [];
        const tuples = [];
        for (const row of rows) {
          check();
          const payload = normalize(fromSource ? row.raw_record : row.payload);
          const base = parameters.length;
          parameters.push(
            storeId,
            threadId,
            writeGeneration,
            ++offset,
            operationId,
            eventFormatVersion,
            codexVersion,
            JSON.stringify(payload),
            payloadHash(payload),
          );
          tuples.push(
            `($${base + 1},$${base + 2},$${base + 3},$${base + 4},$${base + 5},$${base + 6},$${base + 7},$${base + 8}::json,$${base + 9})`,
          );
        }
        await client.query(
          `INSERT INTO codex_thread_events
          (store_id,thread_id,generation,item_seq,operation_id,event_format_version,codex_version,payload,payload_sha256)
          VALUES ${tuples.join(",")}`,
          parameters,
        );
        onProgress({
          phase: "publishing",
          records: offset,
          totalRecords: total,
        });
      }
      await replaceProjections(client, storeId, state, manifest, 1, affected);
      check();
      const version = await publishReceipt(
        client,
        storeId,
        operationId,
        affected,
        total - (replace ? 0 : previousCount),
      );
      await client.query(
        "UPDATE mira_codex_session_imports SET status='imported',store_event_seq=$2,error_code=NULL,updated_at=NOW() WHERE import_id=$1",
        [importId, version],
      );
      check();
      await client.query("COMMIT");
      return { version };
    } catch (error) {
      await client.query("ROLLBACK");
      throw error;
    } finally {
      client.release();
    }
  })();
}

export async function listStoreEvents(pool, storeId, after, limit) {
  const rows = await pool.query(
    `SELECT event_seq::text,previous_event_seq::text,operation_id,event_format_version,codex_version,created_at
    FROM codex_store_events WHERE store_id=$1 AND event_seq>$2 ORDER BY event_seq LIMIT $3`,
    [storeId, after, limit],
  );
  const result = [];
  for (const row of rows.rows)
    result.push({
      eventSeq: Number(row.event_seq),
      previousEventSeq: Number(row.previous_event_seq),
      operationId: row.operation_id,
      eventFormatVersion: row.event_format_version,
      codexVersion: row.codex_version,
      createdAt: row.created_at.toISOString(),
      historyManifest: await historicalManifest(
        pool,
        storeId,
        Number(row.event_seq),
      ),
    });
  return result;
}

export async function listThreadEvents(
  pool,
  storeId,
  threadId,
  generation,
  after,
  limit,
) {
  const params = [storeId, threadId, after, limit];
  const generationClause = generation === null ? "" : "AND generation = $5";
  if (generation !== null) params.push(generation);
  const result = await pool.query(
    `SELECT events.generation::text, events.item_seq::text, events.store_event_seq::text,
            events.event_format_version, events.codex_version, events.payload,
            events.payload_sha256, events.created_at
     FROM codex_thread_events_versioned AS events
     WHERE events.store_id = $1 AND events.thread_id = $2 AND events.item_seq > $3
       AND NOT EXISTS(SELECT 1 FROM mira_thread_actions WHERE store_id=$1 AND thread_id=$2 AND action='delete')
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
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await lockScope(client, storeId);
    const changes = await client.query(
      `SELECT s.path,s.mode,s.value FROM codex_store_state_changes s JOIN codex_store_events c USING(store_id,operation_id)
      WHERE s.store_id=$1 AND c.event_seq IS NOT NULL AND (s.thread_id IS NULL OR NOT EXISTS(
        SELECT 1 FROM mira_thread_actions a WHERE a.store_id=s.store_id AND a.thread_id=s.thread_id AND a.action='delete')) ORDER BY c.event_seq,s.change_seq`,
      [storeId],
    );
    const state = {};
    for (const change of changes.rows) applyStateChange(state, change);
    const head = await currentHead(client, storeId);
    if (!head.version) {
      await client.query("ROLLBACK");
      return { status: 404, body: { error: "store has no canonical events" } };
    }
    const manifest = await historicalManifest(client, storeId, head.version);
    // Full rebuild is an explicit maintenance operation, never a normal write.
    await client.query(
      "DELETE FROM codex_thread_projections WHERE store_id=$1",
      [storeId],
    );
    await rebuildStateEntries(client, storeId, state);
    await replaceProjections(client, storeId, state, manifest, head.version);
    await client.query("COMMIT");
    return {
      status: 200,
      body: { storeId, version: head.version, rebuilt: true },
    };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}
