import fs from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";
import { Pool } from "pg";

import { appendAudit, AuthService } from "./auth.mjs";
import { CapabilityService } from "./capability-service.mjs";
import { currentSchemaVersion, initializeDatabase } from "./db.mjs";
import { dispatchDynamicTool, dynamicToolSpecs } from "./dynamic-tools.mjs";
import { NodeChannel } from "./node-channel.mjs";
import { SSHRelay } from "./ssh-relay.mjs";
import {
  approveEnrollment, createEnrollment, getEnrollment, listEnrollments, rejectEnrollment,
} from "./node-enrollment.mjs";
import {
  getNode, heartbeatNode, listNodes, registerNode, revokeNode, setDesiredAppServer,
} from "./node-registry.mjs";
import {
  commitDelta, getSnapshot, getStoreHead, getThreadHistory, listStoreEvents,
  listThreadEvents, putSnapshot, rebuildSnapshot, seedLegacySnapshots,
} from "./thread-store.mjs";

const listenHost = process.env.LISTEN_HOST ?? "127.0.0.1";
const listenPort = Number.parseInt(process.env.LISTEN_PORT ?? "8787", 10);
const databaseUrl = process.env.DATABASE_URL ?? "postgresql://mira:mira-local@127.0.0.1:55432/mira";
const serverDirectory = path.dirname(fileURLToPath(import.meta.url));
const serverPackage = JSON.parse(await fs.readFile(path.join(serverDirectory, "package.json"), "utf8"));
const publicDirectory = path.join(serverDirectory, "public");
const staticAssets = new Map([
  ["/", [path.join(publicDirectory, "index.html"), "text/html; charset=utf-8"]],
  ["/app.js", [path.join(publicDirectory, "app.js"), "text/javascript; charset=utf-8"]],
  ["/styles.css", [path.join(publicDirectory, "styles.css"), "text/css; charset=utf-8"]],
  ["/vendor/xterm.js", [path.join(serverDirectory, "node_modules/@xterm/xterm/lib/xterm.mjs"), "text/javascript; charset=utf-8"]],
  ["/vendor/xterm-addon-fit.js", [path.join(serverDirectory, "node_modules/@xterm/addon-fit/lib/addon-fit.mjs"), "text/javascript; charset=utf-8"]],
  ["/vendor/xterm.css", [path.join(serverDirectory, "node_modules/@xterm/xterm/css/xterm.css"), "text/css; charset=utf-8"]],
]);
const maxBodyBytes = 64 * 1024 * 1024;
const pool = new Pool({ connectionString: databaseUrl, max: 10 });

await initializeDatabase(pool);
const importedLegacyStoreCount = await seedLegacySnapshots(pool);
const authService = new AuthService({
  pool,
  secureCookies: process.env.MIRA_SECURE_COOKIES !== "false",
});
const authState = await authService.initialize();

function sendJson(response, status, value, headers = {}) {
  const payload = Buffer.from(JSON.stringify(value));
  response.writeHead(status, {
    "content-type": "application/json; charset=utf-8",
    "content-length": payload.length,
    "cache-control": "no-store",
    ...headers,
  });
  response.end(payload);
}

function errorJson(response, status, error, code) {
  sendJson(response, status, { error, code });
}

async function authorize(request, response, actorType, options = {}) {
  const principal = await authService.authenticate(request, options.clientType ?? null);
  if (!principal) {
    errorJson(response, 401, "authentication required", "authentication_required");
    return null;
  }
  if (principal.revoked) {
    errorJson(response, 403, "Node credential is revoked", "node_revoked");
    return null;
  }
  if (!authService.permits(principal, actorType)) {
    errorJson(response, 403, "permission denied", "permission_denied");
    return null;
  }
  if (options.nodeId && (principal.kind !== "node" || principal.nodeId !== options.nodeId)) {
    errorJson(response, 403, "Node identity does not match route", "node_identity_mismatch");
    return null;
  }
  if (options.csrf !== false && !["GET", "HEAD", "OPTIONS"].includes(request.method) &&
      !authService.validCsrf(request, principal)) {
    errorJson(response, 403, "invalid CSRF token", "invalid_csrf");
    return null;
  }
  return principal;
}

async function readJson(request) {
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > maxBodyBytes) {
      const error = new Error("request body exceeds 64 MiB");
      error.statusCode = 413;
      error.code = "body_too_large";
      throw error;
    }
    chunks.push(chunk);
  }
  if (chunks.length === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    const error = new Error("request body must be valid JSON");
    error.statusCode = 400;
    error.code = "invalid_json";
    throw error;
  }
}

function safeStoreId(value) {
  const decoded = decodeURIComponent(value);
  return /^[a-zA-Z0-9._-]{1,128}$/.test(decoded) ? decoded : null;
}

function boundedInteger(value, fallback, minimum, maximum) {
  const parsed = Number.parseInt(value ?? "", 10);
  return Number.isSafeInteger(parsed) && parsed >= minimum && parsed <= maximum ? parsed : fallback;
}

async function servePublic(response, relativePath) {
  const asset = staticAssets.get(relativePath);
  if (!asset) return false;
  try {
    const payload = await fs.readFile(asset[0]);
    response.writeHead(200, {
      "content-type": asset[1], "content-length": payload.length,
      "cache-control": "no-cache",
      "content-security-policy": "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
      "x-content-type-options": "nosniff", "referrer-policy": "no-referrer",
    });
    response.end(payload);
    return true;
  } catch (error) {
    if (error.code === "ENOENT") return false;
    throw error;
  }
}

async function route(request, response) {
  const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
  if (request.method === "GET" && await servePublic(response, url.pathname)) return;

  if (request.method === "GET" && url.pathname === "/healthz") {
    await pool.query("SELECT 1");
    sendJson(response, 200, {
      status: "ok", backend: "postgresql", databaseIsSourceOfTruth: true,
    version: serverPackage.version, schemaVersion: currentSchemaVersion(), adminConfigured: authState.adminConfigured,
    });
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/auth/config") {
    sendJson(response, 200, {
      adminConfigured: authState.adminConfigured, nodeEnrollment: true,
      nodeApprovalRequired: true, passwordSession: true,
      identities: ["admin", "node"],
    });
    return;
  }
  if (request.method === "POST" && url.pathname === "/v1/admin/login") {
    const body = await readJson(request);
    const login = await authService.login(request, body.username, body.password);
    if (!login) {
      await appendAudit(pool, {
        action: "admin.login.failed", request, success: false, errorCode: "invalid_credentials",
        metadata: { username: typeof body.username === "string" ? body.username.slice(0, 128) : null },
      });
      errorJson(response, 401, "invalid username or password", "invalid_credentials");
      return;
    }
    await appendAudit(pool, { action: "admin.login.succeeded", principal: login.principal, request });
    sendJson(response, 200, {
      user: { username: login.principal.username }, csrfToken: login.csrfToken,
      expiresAt: login.principal.expiresAt.toISOString(),
    }, { "set-cookie": login.cookie });
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/admin/session") {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const csrfToken = await authService.refreshCsrf(principal);
    sendJson(response, 200, { user: { username: principal.username }, csrfToken, expiresAt: principal.expiresAt.toISOString() });
    return;
  }
  if (request.method === "POST" && url.pathname === "/v1/admin/logout") {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const cookie = await authService.logout(principal);
    await appendAudit(pool, { action: "admin.logout", principal, request });
    sendJson(response, 200, { status: "logged_out" }, { "set-cookie": cookie });
    return;
  }

  if (request.method === "POST" && url.pathname === "/v1/node-enrollments") {
    const result = await createEnrollment(pool, request, await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  let match = url.pathname.match(/^\/v1\/node-enrollments\/([0-9a-f-]{36})$/i);
  if (request.method === "GET" && match) {
    const result = await getEnrollment(pool, request, match[1]);
    sendJson(response, result.status, result.body);
    return;
  }

  if (request.method === "GET" && url.pathname === "/v1/admin/enrollments") {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const status = url.searchParams.get("status");
    const allowed = new Set(["pending", "approved", "rejected", "expired"]);
    if (status !== null && !allowed.has(status)) {
      errorJson(response, 400, "invalid enrollment status", "invalid_request");
      return;
    }
    sendJson(response, 200, { data: await listEnrollments(pool, status) });
    return;
  }
  match = url.pathname.match(/^\/v1\/admin\/enrollments\/([0-9a-f-]{36})\/(approve|reject)$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const body = await readJson(request);
    const result = match[2] === "approve"
      ? await approveEnrollment(pool, request, principal, match[1], body)
      : await rejectEnrollment(pool, request, principal, match[1], body);
    sendJson(response, result.status, result.body);
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/admin/audit-events") {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const limit = boundedInteger(url.searchParams.get("limit"), 100, 1, 500);
    const events = await pool.query(
      `SELECT audit_event_id, action, actor_type, actor_admin_id, actor_node_id,
              client_type, target_node_id, thread_id, request_id, success,
              error_code, request_address, metadata, created_at
       FROM mira_audit_events ORDER BY audit_event_id DESC LIMIT $1`, [limit],
    );
    sendJson(response, 200, { data: events.rows.map((row) => ({
      eventId: Number(row.audit_event_id), action: row.action, actorType: row.actor_type,
      actorAdminId: row.actor_admin_id, actorNodeId: row.actor_node_id,
      clientType: row.client_type, targetNodeId: row.target_node_id,
      threadId: row.thread_id, requestId: row.request_id, success: row.success,
      errorCode: row.error_code, requestAddress: row.request_address,
      metadata: row.metadata, createdAt: row.created_at.toISOString(),
    })) });
    return;
  }

  if (request.method === "GET" && url.pathname === "/v1/capabilities") {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    sendJson(response, 200, {
      storageModel: "postgresql-event-log", eventFormatVersion: 1, adapterProtocolVersion: 2,
      snapshotProjection: true, nodeRegistry: true, nodeCapabilityChannel: true,
      appServerProxy: true, dynamicTools: true, androidNodeApp: true,
      imageToolResults: true, databaseIsSourceOfTruth: true,
      authenticationVersion: 1, identities: ["admin", "node"], nodeApprovalRequired: true,
      websocketQueryCredentials: false,
    });
    return;
  }
  if (request.method === "POST" && url.pathname === "/v1/nodes/register") {
    const principal = await authorize(request, response, "node", { csrf: false, clientType: "node" });
    if (!principal) return;
    const result = await registerNode(pool, principal.nodeId, await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/nodes") {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    const includeRevoked = principal.kind === "admin" && url.searchParams.get("includeRevoked") === "true";
    sendJson(response, 200, { data: await listNodes(pool, { includeRevoked }) });
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})$/i);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    const node = await getNode(pool, match[1], { includeRevoked: principal.kind === "admin" });
    if (!node) errorJson(response, 404, "Node not found", "not_found");
    else sendJson(response, 200, node);
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/dynamic-tools") {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    sendJson(response, 200, { dynamicTools: dynamicToolSpecs() });
    return;
  }
  if (request.method === "POST" && url.pathname === "/v1/dynamic-tools/call") {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const body = await readJson(request);
    if (typeof body.tool !== "string" || body.arguments === null || typeof body.arguments !== "object") {
      errorJson(response, 400, "tool and arguments are required", "invalid_request");
      return;
    }
    const result = await dispatchDynamicTool(capabilityService, principal, body.tool, body.arguments, {
      request, requestId: request.headers["x-request-id"] ?? null,
      threadId: request.headers["x-mira-thread-id"] ?? null,
      timeoutMs: body.timeoutMs,
    });
    sendJson(response, 200, { result });
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/heartbeat$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "node", { csrf: false, nodeId: match[1], clientType: "node" });
    if (!principal) return;
    const result = await heartbeatNode(pool, match[1], await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/desired-app-server$/i);
  if (request.method === "PUT" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    const result = await setDesiredAppServer(pool, match[1], await readJson(request));
    if (result.status === 200) await appendAudit(pool, {
      action: "app_server.desired.updated", principal, targetNodeId: match[1], request,
      metadata: { running: result.body.desiredAppServer.running },
    });
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/invoke$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    const body = await readJson(request);
    const result = await capabilityService.invoke(principal, match[1], body.capability, body.params ?? {}, {
      request, requestId: request.headers["x-request-id"] ?? null,
      threadId: request.headers["x-mira-thread-id"] ?? null, timeoutMs: body.timeoutMs,
    });
    sendJson(response, 200, { result });
    return;
  }
  match = url.pathname.match(/^\/v1\/admin\/nodes\/([0-9a-f-]{36})\/revoke$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const body = await readJson(request);
    const reason = typeof body.reason === "string" ? body.reason.slice(0, 2_000) : null;
    const result = await revokeNode(pool, request, principal, match[1], reason);
    if (result.status === 200) nodeChannel.disconnectNode(match[1], "Node authorization revoked");
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/admin\/nodes\/([0-9a-f-]{36})\/restore$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const body = await readJson(request);
    if (typeof body.enrollmentId !== "string") {
      errorJson(response, 409, "restore requires a fresh pending enrollmentId so the credential is rotated", "fresh_enrollment_required");
      return;
    }
    const binding = await pool.query(
      `SELECT requests.enrollment_id FROM mira_node_enrollment_requests requests
       JOIN codex_nodes nodes ON nodes.node_key = requests.node_key
       WHERE requests.enrollment_id = $1 AND nodes.node_id = $2
         AND requests.status = 'pending' AND nodes.approval_status = 'revoked'`,
      [body.enrollmentId, match[1]],
    );
    if (binding.rowCount === 0) {
      errorJson(response, 409, "fresh pending enrollment does not match revoked Node", "enrollment_conflict");
      return;
    }
    const result = await approveEnrollment(pool, request, principal, body.enrollmentId, body);
    sendJson(response, result.status, result.body);
    return;
  }

  match = url.pathname.match(/^\/v1\/stores\/([^/]+)$/);
  if (match) {
    const storeId = safeStoreId(match[1]);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    if (request.method === "GET") {
      const principal = await authorize(request, response, "trusted", { clientType: "codex" });
      if (!principal) return;
      sendJson(response, 200, await getSnapshot(pool, storeId)); return;
    }
    if (request.method === "PUT") {
      const principal = await authorize(request, response, "trusted", { clientType: "codex" });
      if (!principal) return;
      const result = await putSnapshot(pool, storeId, await readJson(request), request.headers);
      sendJson(response, result.status, result.body); return;
    }
  }
  match = url.pathname.match(/^\/v2\/stores\/([^/]+)$/);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    sendJson(response, 200, await getStoreHead(pool, storeId)); return;
  }
  match = url.pathname.match(/^\/v2\/stores\/([^/]+)\/threads\/([^/]+)\/history$/);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    const threadId = decodeURIComponent(match[2]);
    const generationValue = url.searchParams.get("generation");
    const generation = generationValue === null ? null : boundedInteger(generationValue, null, 1, Number.MAX_SAFE_INTEGER);
    const throughValue = url.searchParams.get("throughVersion");
    const throughVersion = throughValue === null ? null : boundedInteger(throughValue, null, 0, Number.MAX_SAFE_INTEGER);
    if (!storeId || threadId.length === 0 || threadId.length > 256 ||
        (generationValue !== null && generation === null) || (throughValue !== null && throughVersion === null)) {
      errorJson(response, 400, "invalid store, thread id, generation, or version", "invalid_request"); return;
    }
    const result = await getThreadHistory(pool, storeId, threadId, generation, throughVersion);
    sendJson(response, result.status, result.body); return;
  }
  match = url.pathname.match(/^\/v2\/stores\/([^/]+)\/commits$/);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await commitDelta(pool, storeId, await readJson(request), request.headers);
    sendJson(response, result.status, result.body); return;
  }
  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/events$/);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const after = boundedInteger(url.searchParams.get("after"), 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = boundedInteger(url.searchParams.get("limit"), 100, 1, 1000);
    sendJson(response, 200, { data: await listStoreEvents(pool, storeId, after, limit) }); return;
  }
  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/threads\/([^/]+)\/events$/);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    const threadId = decodeURIComponent(match[2]);
    const generationValue = url.searchParams.get("generation");
    const generation = generationValue === null ? null : boundedInteger(generationValue, null, 1, Number.MAX_SAFE_INTEGER);
    if (!storeId || threadId.length === 0 || threadId.length > 256 || (generationValue !== null && generation === null)) {
      errorJson(response, 400, "invalid store or thread id", "invalid_request"); return;
    }
    const after = boundedInteger(url.searchParams.get("after"), 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = boundedInteger(url.searchParams.get("limit"), 100, 1, 1000);
    sendJson(response, 200, { data: await listThreadEvents(pool, storeId, threadId, generation, after, limit) }); return;
  }
  match = url.pathname.match(/^\/v1\/stores\/([^/]+)\/rebuild$/);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    const storeId = safeStoreId(match[1]);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await rebuildSnapshot(pool, storeId);
    sendJson(response, result.status, result.body); return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/ssh\/(keys|sessions)$/i);
  if (match && request.method === "POST") {
    const principal = await authorize(request, response, "node", { csrf: false, clientType: "ssh",
      ...(match[2] === "keys" ? { nodeId: match[1] } : {}) });
    if (!principal) return;
    const result = match[2] === "keys"
      ? await sshRelay.publish(principal, await readJson(request))
      : await sshRelay.create(principal, match[1], request);
    sendJson(response, result.status, result.body); return;
  }
  errorJson(response, 404, "not found", "not_found");
}

const server = http.createServer(async (request, response) => {
  try {
    await route(request, response);
  } catch (error) {
    if (!response.headersSent) {
      const status = Number.isInteger(error.statusCode) ? error.statusCode : 500;
      if (status === 500) console.error(error);
      errorJson(response, status, status === 500 ? "internal server error" : error.message,
        status === 500 ? "internal_error" : (error.code ?? "request_failed"));
    }
  }
});
const nodeChannel = new NodeChannel({ server, pool, authService });
const sshRelay = new SSHRelay({ pool, authService, nodeChannel });
nodeChannel.sshRelay = sshRelay;
const capabilityService = new CapabilityService({ pool, nodeChannel });
nodeChannel.setCapabilityService(capabilityService);

server.listen(listenPort, listenHost, () => {
  console.log(`Mira Server listening on http://${listenHost}:${listenPort}; imported ${importedLegacyStoreCount} legacy store(s)`);
  if (!authState.adminConfigured) {
    console.warn("No Mira administrator is configured. Run: npm run admin -- set-password admin");
  }
});

let stopping = false;
async function shutdown(signal) {
  if (stopping) return;
  stopping = true;
  console.log(`received ${signal}; shutting down`);
  nodeChannel.close();
  server.close(async () => {
    await pool.end();
    process.exit(0);
  });
}
process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));
