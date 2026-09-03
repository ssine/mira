import http from "node:http";
import process from "node:process";
import { Pool } from "pg";

import { currentSchemaVersion, initializeDatabase } from "./db.mjs";
import { dispatchDynamicTool, dynamicToolSpecs } from "./dynamic-tools.mjs";
import { NodeChannel } from "./node-channel.mjs";
import {
  heartbeatNode,
  listNodes,
  registerNode,
  setDesiredAppServer,
} from "./node-registry.mjs";
import {
  commitDelta,
  getSnapshot,
  getStoreHead,
  getThreadHistory,
  listStoreEvents,
  listThreadEvents,
  putSnapshot,
  rebuildSnapshot,
  seedLegacySnapshots,
} from "./thread-store.mjs";

const listenHost = process.env.LISTEN_HOST ?? "127.0.0.1";
const listenPort = Number.parseInt(process.env.LISTEN_PORT ?? "8787", 10);
const databaseUrl =
  process.env.DATABASE_URL ??
  "postgresql://mira:mira-local@127.0.0.1:55432/mira";
const authToken = process.env.THREAD_STORE_TOKEN ?? "local-poc-token";
const maxBodyBytes = 64 * 1024 * 1024;

const pool = new Pool({ connectionString: databaseUrl, max: 10 });
await initializeDatabase(pool);
const importedLegacyStoreCount = await seedLegacySnapshots(pool);

function sendJson(response, status, value) {
  const payload = Buffer.from(JSON.stringify(value));
  response.writeHead(status, {
    "content-type": "application/json",
    "content-length": payload.length,
  });
  response.end(payload);
}

function authorized(request) {
  return request.headers.authorization === `Bearer ${authToken}`;
}

async function readJson(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > maxBodyBytes) {
      throw new Error("request body exceeds 64 MiB");
    }
    chunks.push(chunk);
  }
  if (chunks.length === 0) return {};
  return JSON.parse(Buffer.concat(chunks).toString("utf8"));
}

function safeStoreId(value) {
  const decoded = decodeURIComponent(value);
  return /^[a-zA-Z0-9._-]{1,128}$/.test(decoded) ? decoded : null;
}

function boundedInteger(value, fallback, minimum, maximum) {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isSafeInteger(parsed) && parsed >= minimum && parsed <= maximum
    ? parsed
    : fallback;
}

async function route(request, response) {
  const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
  if (request.method === "GET" && url.pathname === "/healthz") {
    await pool.query("SELECT 1");
    sendJson(response, 200, {
      status: "ok",
      backend: "postgresql",
      databaseIsSourceOfTruth: true,
      schemaVersion: currentSchemaVersion(),
    });
    return;
  }

  if (!authorized(request)) {
    sendJson(response, 401, { error: "unauthorized" });
    return;
  }

  if (request.method === "GET" && url.pathname === "/v1/capabilities") {
    sendJson(response, 200, {
      storageModel: "postgresql-event-log",
      eventFormatVersion: 1,
      adapterProtocolVersion: 2,
      snapshotProjection: true,
      nodeRegistry: true,
      nodeCapabilityChannel: true,
      appServerProxy: true,
      dynamicTools: true,
      androidNodeApp: true,
      imageToolResults: true,
      databaseIsSourceOfTruth: true,
    });
    return;
  }

  if (request.method === "POST" && url.pathname === "/v1/nodes/register") {
    const result = await registerNode(pool, await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/nodes") {
    sendJson(response, 200, { data: await listNodes(pool) });
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/dynamic-tools") {
    sendJson(response, 200, { dynamicTools: dynamicToolSpecs() });
    return;
  }
  if (request.method === "POST" && url.pathname === "/v1/dynamic-tools/call") {
    const body = await readJson(request);
    if (typeof body.tool !== "string" || body.arguments === null || typeof body.arguments !== "object") {
      sendJson(response, 400, { error: "tool and arguments are required" });
      return;
    }
    sendJson(response, 200, {
      result: await dispatchDynamicTool(nodeChannel, pool, body.tool, body.arguments),
    });
    return;
  }

  let match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/heartbeat$/i);
  if (request.method === "POST" && match) {
    const result = await heartbeatNode(pool, match[1], await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/desired-app-server$/i);
  if (request.method === "PUT" && match) {
    const result = await setDesiredAppServer(pool, match[1], await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/invoke$/i);
  if (request.method === "POST" && match) {
    const body = await readJson(request);
    if (typeof body.capability !== "string" || body.params === null || typeof body.params !== "object") {
      sendJson(response, 400, { error: "capability and params are required" });
      return;
    }
    const timeoutMs = boundedInteger(body.timeoutMs, 30_000, 100, 120_000);
    sendJson(response, 200, {
      result: await nodeChannel.invoke(match[1], body.capability, body.params, timeoutMs),
    });
    return;
  }

  match = url.pathname.match(/^\/v1\/stores\/([^/]+)$/);
  if (match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) {
      sendJson(response, 400, { error: "invalid store id" });
      return;
    }
    if (request.method === "GET") {
      sendJson(response, 200, await getSnapshot(pool, storeId));
      return;
    }
    if (request.method === "PUT") {
      const result = await putSnapshot(pool, storeId, await readJson(request), request.headers);
      sendJson(response, result.status, result.body);
      return;
    }
  }

  match = url.pathname.match(/^\/v2\/stores\/([^/]+)$/);
  if (request.method === "GET" && match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) {
      sendJson(response, 400, { error: "invalid store id" });
      return;
    }
    sendJson(response, 200, await getStoreHead(pool, storeId));
    return;
  }

  match = url.pathname.match(/^\/v2\/stores\/([^/]+)\/threads\/([^/]+)\/history$/);
  if (request.method === "GET" && match) {
    const storeId = safeStoreId(match[1]);
    const threadId = decodeURIComponent(match[2]);
    const generationValue = url.searchParams.get("generation");
    const generation =
      generationValue === null
        ? null
        : boundedInteger(generationValue, null, 1, Number.MAX_SAFE_INTEGER);
    const throughVersionValue = url.searchParams.get("throughVersion");
    const throughVersion =
      throughVersionValue === null
        ? null
        : boundedInteger(throughVersionValue, null, 0, Number.MAX_SAFE_INTEGER);
    if (
      !storeId ||
      threadId.length === 0 ||
      threadId.length > 256 ||
      (generationValue !== null && generation === null) ||
      (throughVersionValue !== null && throughVersion === null)
    ) {
      sendJson(response, 400, { error: "invalid store, thread id, or generation" });
      return;
    }
    const result = await getThreadHistory(pool, storeId, threadId, generation, throughVersion);
    sendJson(response, result.status, result.body);
    return;
  }

  match = url.pathname.match(/^\/v2\/stores\/([^/]+)\/commits$/);
  if (request.method === "POST" && match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) {
      sendJson(response, 400, { error: "invalid store id" });
      return;
    }
    const result = await commitDelta(pool, storeId, await readJson(request), request.headers);
    sendJson(response, result.status, result.body);
    return;
  }

  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/events$/);
  if (request.method === "GET" && match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) {
      sendJson(response, 400, { error: "invalid store id" });
      return;
    }
    const after = boundedInteger(url.searchParams.get("after"), 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = boundedInteger(url.searchParams.get("limit"), 100, 1, 1000);
    sendJson(response, 200, { data: await listStoreEvents(pool, storeId, after, limit) });
    return;
  }

  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/threads\/([^/]+)\/events$/);
  if (request.method === "GET" && match) {
    const storeId = safeStoreId(match[1]);
    const threadId = decodeURIComponent(match[2]);
    if (!storeId || threadId.length === 0 || threadId.length > 256) {
      sendJson(response, 400, { error: "invalid store or thread id" });
      return;
    }
    const generationValue = url.searchParams.get("generation");
    const generation =
      generationValue === null
        ? null
        : boundedInteger(generationValue, null, 1, Number.MAX_SAFE_INTEGER);
    const after = boundedInteger(url.searchParams.get("after"), 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = boundedInteger(url.searchParams.get("limit"), 100, 1, 1000);
    sendJson(response, 200, {
      data: await listThreadEvents(pool, storeId, threadId, generation, after, limit),
    });
    return;
  }

  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/rebuild$/);
  if (request.method === "POST" && match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) {
      sendJson(response, 400, { error: "invalid store id" });
      return;
    }
    const result = await rebuildSnapshot(pool, storeId);
    sendJson(response, result.status, result.body);
    return;
  }

  sendJson(response, 404, { error: "not found" });
}

const server = http.createServer(async (request, response) => {
  try {
    await route(request, response);
  } catch (error) {
    console.error(error);
    if (!response.headersSent) sendJson(response, 500, { error: "internal server error" });
  }
});
const nodeChannel = new NodeChannel({ server, pool, authToken });

server.listen(listenPort, listenHost, () => {
  console.log(
    `codex control server listening on http://${listenHost}:${listenPort}; ` +
      `imported ${importedLegacyStoreCount} legacy store(s)`,
  );
});

async function shutdown(signal) {
  console.log(`received ${signal}; shutting down`);
  nodeChannel.close();
  server.close(async () => {
    await pool.end();
    process.exit(0);
  });
}

process.on("SIGINT", () => shutdown("SIGINT"));
process.on("SIGTERM", () => shutdown("SIGTERM"));
