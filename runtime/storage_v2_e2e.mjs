import crypto from "node:crypto";
import process from "node:process";

const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const token = process.env.CONTROL_SERVER_TOKEN ?? "local-poc-token";
const storeId = process.env.STORAGE_E2E_STORE_ID ?? `storage-v2-e2e-${process.pid}`;
const threadId = crypto.randomUUID();

async function request(pathname, options = {}) {
  const response = await fetch(`${controlUrl}${pathname}`, {
    ...options,
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      ...(options.headers ?? {}),
    },
  });
  const payload = await response.json();
  if (!response.ok) {
    throw new Error(`${options.method ?? "GET"} ${pathname}: ${response.status} ${JSON.stringify(payload)}`);
  }
  return payload;
}

async function commit(expectedVersion, stateChanges, historyChanges, operationId = crypto.randomUUID()) {
  return request(`/v2/stores/${storeId}/commits`, {
    method: "POST",
    headers: { "x-codex-operation-id": operationId, "x-codex-version": "storage-v2-e2e" },
    body: JSON.stringify({ expectedVersion, stateChanges, historyChanges }),
  });
}

try {
  const item1 = { payload: { type: "message", role: "user", content: "one" } };
  const item2 = { payload: { type: "message", role: "assistant", content: "two" } };
  const item3 = { payload: { type: "message", role: "assistant", content: "three" } };
  const initialState = [
    {
      path: ["created_threads", threadId],
      mode: "set",
      conflictPolicy: "compareAndSwap",
      expected: { exists: false },
      value: { source: "cli" },
    },
  ];
  const forbiddenLww = await fetch(`${controlUrl}/v2/stores/${storeId}/commits`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
      "x-codex-operation-id": crypto.randomUUID(),
    },
    body: JSON.stringify({
      expectedVersion: 0,
      stateChanges: [{ ...initialState[0], conflictPolicy: "lastWriteWins" }],
      historyChanges: [],
    }),
  });
  if (forbiddenLww.status !== 400) {
    throw new Error("Server accepted lastWriteWins for a protected state path");
  }
  const operation1 = crypto.randomUUID();
  const first = await commit(
    0,
    initialState,
    [{ threadId, mode: "append", expectedGeneration: 0, expectedItemCount: 0, items: [item1] }],
    operation1,
  );
  const duplicate = await commit(
    0,
    initialState,
    [{ threadId, mode: "append", expectedGeneration: 0, expectedItemCount: 0, items: [item1] }],
    operation1,
  );
  const versionOneHead = await request(`/v2/stores/${storeId}`);
  const [second, third] = await Promise.all([
    commit(
      1,
      [
        {
          path: ["names", threadId],
          mode: "set",
          conflictPolicy: "compareAndSwap",
          expected: { exists: false },
          value: "parallel-a",
        },
      ],
      [
        {
          threadId,
          mode: "append",
          expectedGeneration: 1,
          expectedItemCount: 1,
          items: [item2],
        },
      ],
    ),
    commit(
      1,
      [
        {
          path: ["sections", threadId],
          mode: "set",
          conflictPolicy: "compareAndSwap",
          expected: { exists: false },
          value: "parallel-b",
        },
      ],
      [
        {
          threadId,
          mode: "append",
          expectedGeneration: 1,
          expectedItemCount: 1,
          items: [item3],
        },
      ],
    ),
  ]);
  const replacement = await commit(3, [], [
    {
      threadId,
      mode: "replace",
      expectedGeneration: 1,
      expectedItemCount: 3,
      items: [item2],
    },
  ]);
  let currentVersion = replacement.version;
  for (let timestamp = 1; timestamp <= 8; timestamp += 1) {
    const advanced = await commit(currentVersion, [
      {
        path: ["metadata_updates", threadId, "updated_at"],
        mode: "set",
        conflictPolicy: "lastWriteWins",
        expected: { exists: false },
        value: timestamp,
      },
    ], []);
    currentVersion = advanced.version;
  }
  const head = await request(`/v2/stores/${storeId}`);
  const versionOneHistory = await request(
    `/v2/stores/${storeId}/threads/${threadId}/history?generation=1&throughVersion=${versionOneHead.version}`,
  );
  const history = await request(
    `/v2/stores/${storeId}/threads/${threadId}/history?generation=2`,
  );
  const compatibility = await request(`/v1/stores/${storeId}`);
  const events = await request(`/v1/stores/${storeId}/events?after=0&limit=100`);
  const eventVersions = events.data.map((event) => event.eventSeq);
  if (
    first.version !== 1 ||
    duplicate.duplicate !== true ||
    new Set([second.version, third.version]).size !== 2 ||
    Math.max(second.version, third.version) !== 3 ||
    [second, third].filter((commitResult) => commitResult.rebased).length !== 1 ||
    replacement.historyManifest[threadId].generation !== 2 ||
    head.version !== 12 ||
    head.state.names[threadId] !== "parallel-a" ||
    head.state.sections[threadId] !== "parallel-b" ||
    head.state.metadata_updates[threadId].updated_at !== 8 ||
    versionOneHistory.itemCount !== 1 ||
    history.itemCount !== 1 ||
    compatibility.snapshot.histories[threadId].length !== 1 ||
    eventVersions.join(",") !== "1,2,3,4,5,6,7,8,9,10,11,12"
  ) {
    throw new Error("fine-grained protocol assertions failed");
  }
  console.log(
    JSON.stringify({
      ok: true,
      storeId,
      versions: [first.version, second.version, third.version, replacement.version],
      idempotentReplay: duplicate.duplicate,
      concurrentDeltasRebased: true,
      historicalReadIsVersionConsistent: true,
      activeGeneration: replacement.historyManifest[threadId].generation,
      activeItemCount: history.itemCount,
      compatibilityProjection: true,
      numericVersionOrderingPastNine: true,
      serverOwnedConflictPolicy: true,
    }),
  );
} catch (error) {
  console.error(JSON.stringify({ ok: false, storeId, error: error.message }));
  process.exitCode = 1;
}
