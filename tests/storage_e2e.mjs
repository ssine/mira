import crypto from "node:crypto";
import process from "node:process";

const controlUrl = (process.env.CONTROL_SERVER_URL ?? "http://127.0.0.1:8787").replace(/\/$/, "");
const token = process.env.MIRA_NODE_TOKEN ?? process.env.CONTROL_SERVER_TOKEN;
if (!token) throw new Error("MIRA_NODE_TOKEN is required");
const storeId = process.env.STORAGE_E2E_STORE_ID ?? `storage-e2e-${process.pid}`;
const threadId = "01a05ec2-3005-7c83-ad32-509c861ac343";

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

function snapshotHash(snapshot) {
  return crypto
    .createHash("sha256")
    .update(JSON.stringify(canonicalJson(snapshot)))
    .digest("hex");
}

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

async function put(expectedVersion, snapshot, operationId) {
  return request(`/v1/stores/${storeId}`, {
    method: "PUT",
    headers: {
      "x-codex-operation-id": operationId,
      "x-codex-version": "storage-e2e",
    },
    body: JSON.stringify({ expectedVersion, snapshot }),
  });
}

const item1 = { timestamp: "2026-09-02T00:00:00Z", payload: { type: "message", role: "user", content: "one" } };
const item2 = { timestamp: "2026-09-02T00:00:01Z", payload: { type: "message", role: "assistant", content: "two" } };
const item3 = { timestamp: "2026-09-02T00:00:02Z", payload: { type: "message", role: "user", content: "three" } };
const operation1 = crypto.randomUUID();
const operation2 = crypto.randomUUID();
const operation3 = crypto.randomUUID();

try {
  const snapshot1 = {
    created_threads: { [threadId]: { source: "cli" } },
    histories: { [threadId]: [item1, item2] },
  };
  const first = await put(0, snapshot1, operation1);
  if (first.version !== 1 || first.appendedItemCount !== 2) {
    throw new Error(`unexpected first append result: ${JSON.stringify(first)}`);
  }

  const snapshot2 = {
    created_threads: { [threadId]: { source: "cli" } },
    histories: { [threadId]: [item1, item2, item3] },
  };
  const second = await put(1, snapshot2, operation2);
  if (second.version !== 2 || second.appendedItemCount !== 1) {
    throw new Error(`unexpected second append result: ${JSON.stringify(second)}`);
  }
  const duplicate = await put(1, snapshot2, operation2);
  if (duplicate.version !== 2 || duplicate.duplicate !== true) {
    throw new Error(`idempotent replay was not recognized: ${JSON.stringify(duplicate)}`);
  }

  const reorderedSnapshot = {
    histories: {
      [threadId]: [
        { payload: { content: "one", role: "user", type: "message" }, timestamp: item1.timestamp },
        { payload: { content: "two", role: "assistant", type: "message" }, timestamp: item2.timestamp },
        { payload: { content: "three", role: "user", type: "message" }, timestamp: item3.timestamp },
      ],
    },
    created_threads: { [threadId]: { source: "cli" } },
  };
  const third = await put(2, reorderedSnapshot, operation3);
  if (third.version !== 3 || third.appendedItemCount !== 0) {
    throw new Error(`key reordering created false events: ${JSON.stringify(third)}`);
  }

  const before = await request(`/v1/stores/${storeId}`);
  const beforeHash = snapshotHash(before.snapshot);
  await request(`/v1/stores/${storeId}/rebuild`, { method: "POST", body: "{}" });
  const after = await request(`/v1/stores/${storeId}`);
  const afterHash = snapshotHash(after.snapshot);
  const storeEvents = await request(`/v1/stores/${storeId}/events?limit=10`);
  const threadEvents = await request(
    `/v1/stores/${storeId}/threads/${threadId}/events?generation=1&limit=10`,
  );
  if (beforeHash !== afterHash || threadEvents.data.length !== 3 || storeEvents.data.length !== 3) {
    throw new Error("canonical event rebuild did not reproduce the expected snapshot");
  }

  console.log(
    JSON.stringify({
      ok: true,
      storeId,
      versions: [first.version, second.version, third.version],
      idempotentReplay: true,
      falseAppendOnKeyReorder: false,
      canonicalThreadItemCount: threadEvents.data.length,
      rebuildHash: afterHash,
      rebuildMatched: beforeHash === afterHash,
    }),
  );
} catch (error) {
  console.error(JSON.stringify({ ok: false, storeId, error: error.message }));
  process.exitCode = 1;
}
