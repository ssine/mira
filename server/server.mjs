import fs from "node:fs/promises";
import http from "node:http";
import path from "node:path";
import process from "node:process";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";
import { Pool } from "pg";

import { appendAudit, AuthService } from "./auth.mjs";
import { CapabilityService } from "./capability-service.mjs";
import { getCodexTranscript } from "./codex-transcript.mjs";
import {
  defaultStoreId, importCodexSession, listImportedThreads, normalizeImportedThreadHistoryModes,
  scanCodexSessions,
} from "./codex-session-import.mjs";
import { currentSchemaVersion, initializeDatabase } from "./db.mjs";
import { dispatchDynamicTool, dynamicToolSpecs } from "./dynamic-tools.mjs";
import { NodeChannel } from "./node-channel.mjs";
import { SSHRelay } from "./ssh-relay.mjs";
import { manageThread, nameForkThread, renameThread } from "./thread-management.mjs";
import { markThreadRead } from "./thread-read-state.mjs";
import { startThreadErasureWorker, threadErasureStatus } from "./thread-erasure.mjs";
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
const codexStoreEndpoint = (process.env.MIRA_CODEX_STORE_ENDPOINT ?? `http://${listenHost}:${listenPort}`).replace(/\/$/, "");
const serverDirectory = path.dirname(fileURLToPath(import.meta.url));
const serverPackage = JSON.parse(await fs.readFile(path.join(serverDirectory, "package.json"), "utf8"));
const publicDirectory = path.join(serverDirectory, "public");
const staticAssets = new Map([
  ["/", [path.join(publicDirectory, "index.html"), "text/html; charset=utf-8"]],
  ["/app.js", [path.join(publicDirectory, "app.js"), "text/javascript; charset=utf-8"]],
  ["/thread-title.js", [path.join(publicDirectory, "thread-title.js"), "text/javascript; charset=utf-8"]],
  ["/account-status.js", [path.join(publicDirectory, "account-status.js"), "text/javascript; charset=utf-8"]],
  ["/trace-activity.js", [path.join(publicDirectory, "trace-activity.js"), "text/javascript; charset=utf-8"]],
  ["/trace-images.js", [path.join(publicDirectory, "trace-images.js"), "text/javascript; charset=utf-8"]],
  ["/conversation-progress.js", [path.join(publicDirectory, "conversation-progress.js"), "text/javascript; charset=utf-8"]],
  ["/theme.js", [path.join(publicDirectory, "theme.js"), "text/javascript; charset=utf-8"]],
  ["/styles.css", [path.join(publicDirectory, "styles.css"), "text/css; charset=utf-8"]],
  ["/pwa.js", [path.join(publicDirectory, "pwa.js"), "text/javascript; charset=utf-8"]],
  ["/service-worker.js", [path.join(publicDirectory, "service-worker.js"), "text/javascript; charset=utf-8"]],
  ["/manifest.webmanifest", [path.join(publicDirectory, "manifest.webmanifest"), "application/manifest+json; charset=utf-8"]],
  ["/offline.html", [path.join(publicDirectory, "offline.html"), "text/html; charset=utf-8"]],
  ["/offline.css", [path.join(publicDirectory, "offline.css"), "text/css; charset=utf-8"]],
  ["/offline.js", [path.join(publicDirectory, "offline.js"), "text/javascript; charset=utf-8"]],
  ["/icons/mira.svg", [path.join(publicDirectory, "icons/mira.svg"), "image/svg+xml"]],
  ["/icons/mira-192.png", [path.join(publicDirectory, "icons/mira-192.png"), "image/png"]],
  ["/icons/mira-512.png", [path.join(publicDirectory, "icons/mira-512.png"), "image/png"]],
  ["/vendor/xterm.js", [path.join(serverDirectory, "node_modules/@xterm/xterm/lib/xterm.mjs"), "text/javascript; charset=utf-8"]],
  ["/vendor/xterm-addon-fit.js", [path.join(serverDirectory, "node_modules/@xterm/addon-fit/lib/addon-fit.mjs"), "text/javascript; charset=utf-8"]],
  ["/vendor/xterm.css", [path.join(serverDirectory, "node_modules/@xterm/xterm/css/xterm.css"), "text/css; charset=utf-8"]],
  ["/vendor/marked.js", [path.join(serverDirectory, "node_modules/marked/lib/marked.esm.js"), "text/javascript; charset=utf-8"]],
  ["/vendor/dompurify.js", [path.join(serverDirectory, "node_modules/dompurify/dist/purify.es.mjs"), "text/javascript; charset=utf-8"]],
]);
const maxBodyBytes = 64 * 1024 * 1024;
const pool = new Pool({ connectionString: databaseUrl, max: 10 });

await initializeDatabase(pool);
const importedLegacyStoreCount = await seedLegacySnapshots(pool);
const normalizedImportedThreadCount = await normalizeImportedThreadHistoryModes(pool);
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

async function servePublic(request, response, relativePath) {
  const asset = staticAssets.get(relativePath);
  if (!asset) return false;
  try {
    let payload = await fs.readFile(asset[0]);
    if (relativePath === "/service-worker.js") {
      const files = await Promise.all(["offline.html", "offline.css", "offline.js", "icons/mira.svg"].map((file) => fs.readFile(path.join(publicDirectory, file))));
      const hash = createHash("sha256").update(payload);
      for (const file of files) hash.update(file);
      payload = Buffer.from(payload.toString().replace("__MIRA_OFFLINE_VERSION__", hash.digest("hex").slice(0, 20)));
    }
    const etag = `"${createHash("sha256").update(payload).digest("hex")}"`;
    const headers = {
      "content-type": asset[1],
      "cache-control": "no-cache",
      "etag": etag,
      "content-security-policy": "default-src 'self'; script-src 'self'; worker-src 'self'; manifest-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; frame-src 'self' blob:; connect-src 'self' ws: wss:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
      "x-content-type-options": "nosniff", "referrer-policy": "no-referrer",
    };
    if (relativePath === "/service-worker.js") headers["service-worker-allowed"] = "/";
    const unchanged = request.headers["if-none-match"]?.split(",").some((value) => value.trim().replace(/^W\//, "") === etag || value.trim() === "*");
    response.writeHead(unchanged ? 304 : 200, { ...headers, ...(!unchanged ? { "content-length": payload.length } : {}) });
    response.end(unchanged || request.method === "HEAD" ? undefined : payload);
    return true;
  } catch (error) {
    if (error.code === "ENOENT") return false;
    throw error;
  }
}

async function route(request, response) {
  const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
  if (["GET", "HEAD"].includes(request.method) && await servePublic(request, response, url.pathname)) return;

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
    const nodes = await listNodes(pool, { includeRevoked });
    sendJson(response, 200, { data: nodes.map(node => ({ ...node, sshSessionCount: sshRelay.sessionCount(node.nodeId) })) });
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})$/i);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "trusted", { clientType: "cli" });
    if (!principal) return;
    const node = await getNode(pool, match[1], { includeRevoked: principal.kind === "admin" });
    if (!node) errorJson(response, 404, "Node not found", "not_found");
    else sendJson(response, 200, { ...node, sshSessionCount: sshRelay.sessionCount(node.nodeId) });
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/dynamic-tools") {
    const principal = await authorize(request, response, "trusted", { clientType: "codex" });
    if (!principal) return;
    sendJson(response, 200, { dynamicTools: dynamicToolSpecs() });
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/codex-sessions$/i);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const result = await scanCodexSessions(pool, capabilityService, principal, match[1], request);
    sendJson(response, 200, result);
    return;
  }
  match = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/codex-session-imports$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const body = await readJson(request);
    const controller = new AbortController();
    const abort = () => { if (!response.writableEnded) controller.abort(); };
    response.on("close", abort);
    const streaming = request.headers.accept?.includes("application/x-ndjson");
    let heartbeat;
    const emit = (event) => { if (!response.destroyed && !response.writableEnded) response.write(`${JSON.stringify(event)}\n`); };
    if (streaming) {
      response.writeHead(200, { "content-type": "application/x-ndjson", "cache-control": "no-store", "x-accel-buffering": "no" });
      response.flushHeaders();
      heartbeat = setInterval(() => emit({ type: "heartbeat" }), 5000);
    }
    try {
      const result = await importCodexSession(pool, capabilityService, principal, match[1], body, request, {
        signal: controller.signal,
        onProgress: streaming ? (progress) => emit({ type: "progress", ...progress }) : undefined,
      });
      if (streaming) { emit({ type: result.status === 200 ? "complete" : "error", ...result.body }); response.end(); }
      else if (!response.destroyed) sendJson(response, result.status, result.body);
    } catch (error) {
      if (streaming) { emit({ type: "error", error: error.message, code: error.code ?? "import_failed" }); response.end(); }
      else if (!response.destroyed) sendJson(response, error.statusCode ?? 500, { error: error.message, code: error.code ?? "import_failed" });
    } finally {
      clearInterval(heartbeat);
      response.removeListener("close", abort);
    }
    return;
  }
  if (request.method === "GET" && url.pathname === "/v1/codex/threads") {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = url.searchParams.get("storeId") ?? defaultStoreId;
    const limit = boundedInteger(url.searchParams.get("limit"), 200, 1, 500);
    const [data, cleanup] = await Promise.all([
      listImportedThreads(pool, storeId, limit, null, url.searchParams.get("archived") === "1"),
      threadErasureStatus(pool, storeId),
    ]);
    sendJson(response, 200, { storeId, data, cleanup });
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/threads\/([0-9a-f-]{36})(?:\/(archive|restore))?$/i);
  if (match && ((request.method === "DELETE" && !match[2]) || (request.method === "POST" && match[2]))) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = safeStoreId(url.searchParams.get("storeId") ?? defaultStoreId);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await manageThread(pool, storeId, match[1], match[2] ?? "delete", await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/threads\/([0-9a-f-]{36})\/read$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = safeStoreId(url.searchParams.get("storeId") ?? defaultStoreId);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await markThreadRead(pool, storeId, match[1], await readJson(request));
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/threads\/([0-9a-f-]{36})\/fork-title$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = safeStoreId(url.searchParams.get("storeId") ?? defaultStoreId);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await nameForkThread(pool, storeId, match[1], await readJson(request));
    if (result.status !== 200) { sendJson(response, result.status, result.body); return; }
    const [thread] = await listImportedThreads(pool, storeId, 1, match[1]);
    if (!thread) errorJson(response, 404, "分支会话不存在或已删除", "not_found");
    else sendJson(response, 200, thread);
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/threads\/([0-9a-f-]{36})$/i);
  if (request.method === "PATCH" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = safeStoreId(url.searchParams.get("storeId") ?? defaultStoreId);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const result = await renameThread(pool, storeId, match[1], await readJson(request));
    if (result.status !== 200) { sendJson(response, result.status, result.body); return; }
    const [thread] = await listImportedThreads(pool, storeId, 1, match[1]);
    if (!thread) errorJson(response, 404, "会话不存在或已不可访问", "not_found");
    else sendJson(response, 200, thread);
    return;
  }
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = url.searchParams.get("storeId") ?? defaultStoreId;
    const [thread] = await listImportedThreads(pool, storeId, 1, match[1]);
    if (!thread) errorJson(response, 404, "会话不存在或已不可访问", "not_found");
    else sendJson(response, 200, thread);
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/threads\/([0-9a-f-]{36})\/transcript$/i);
  if (request.method === "GET" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const storeId = safeStoreId(url.searchParams.get("storeId") ?? defaultStoreId);
    const cursorValue = url.searchParams.get("cursor");
    const tail = url.searchParams.get("tail") === "1";
    const cursor = tail ? cursorValue : cursorValue === null || !/^\d+$/.test(cursorValue)
      ? null
      : boundedInteger(cursorValue, null, 0, Number.MAX_SAFE_INTEGER);
    const limit = boundedInteger(url.searchParams.get("limit"), 60, 10, 200);
    if (!storeId || (cursorValue !== null && cursor === null)) {
      errorJson(response, 400, "invalid store id or transcript cursor", "invalid_request"); return;
    }
    const result = await getCodexTranscript(pool, storeId, match[1], { cursor, limit, tail });
    sendJson(response, result.status, result.body);
    return;
  }
  match = url.pathname.match(/^\/v1\/codex\/runtimes\/([0-9a-f-]{36})\/(start|stop)$/i);
  if (request.method === "POST" && match) {
    const principal = await authorize(request, response, "admin");
    if (!principal) return;
    const body = await readJson(request);
    const node = await getNode(pool, match[1]);
    if (!node) { errorJson(response, 404, "approved node not found", "not_found"); return; }
    if (node.capabilities?.appServer !== true) {
      errorJson(response, 409, "node cannot run Codex App Server", "capability_unavailable"); return;
    }
    const storeId = safeStoreId(body.storeId ?? defaultStoreId);
    if (!storeId) { errorJson(response, 400, "invalid store id", "invalid_request"); return; }
    const running = match[2] === "start";
    if (running) {
      const requestedPath = typeof body.codexPath === "string" ? body.codexPath : null;
      const compatible = requestedPath || node.capabilities?.codexRuntimeDownload === true || (node.codexInstallations ?? []).some((installation) =>
        installation.remoteThreadStoreSupported === true);
      if (!compatible) {
        errorJson(response, 409,
          "node has no Mira-compatible Codex with remote ThreadStore support",
          "compatible_codex_unavailable");
        return;
      }
    }
    const configOverrides = running ? [
      'experimental_thread_store.type="remote_http"',
      `experimental_thread_store.endpoint=${JSON.stringify(codexStoreEndpoint)}`,
      `experimental_thread_store.store_id=${JSON.stringify(storeId)}`,
    ] : [];
    const result = await setDesiredAppServer(pool, match[1], {
      running,
      listenUrl: node.desiredAppServer?.listenUrl ?? "ws://127.0.0.1:4510",
      codexPath: typeof body.codexPath === "string" ? body.codexPath : node.desiredAppServer?.codexPath,
      codexHome: typeof body.codexHome === "string" ? body.codexHome : node.desiredAppServer?.codexHome,
      configOverrides,
    });
    if (result.status === 200) await appendAudit(pool, {
      action: `codex_runtime.${running ? "started" : "stopped"}`, principal,
      targetNodeId: match[1], request, metadata: { storeId },
    });
    sendJson(response, result.status, result.body);
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
    const threadId = url.searchParams.get('threadId');
    if (threadId !== null && (!threadId || threadId.length > 256)) { errorJson(response,400,'invalid thread id','invalid_request'); return; }
    sendJson(response, 200, await getStoreHead(pool, storeId, threadId === null ? null : [threadId])); return;
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
  if (match && request.method === "GET" && match[2] === "keys") {
    const principal = await authorize(request, response, "node", { csrf: false, clientType: "ssh" });
    if (!principal) return;
    const result = await sshRelay.describe(match[1]);
    sendJson(response, result.status, result.body); return;
  }
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
const stopThreadErasureWorker = startThreadErasureWorker(pool);

server.listen(listenPort, listenHost, () => {
  console.log(`Mira Server listening on http://${listenHost}:${listenPort}; imported ${importedLegacyStoreCount} legacy store(s); normalized ${normalizedImportedThreadCount} imported thread(s)`);
  if (!authState.adminConfigured) {
    console.warn("No Mira administrator is configured. Run: npm run admin -- set-password admin");
  }
});

let stopping = false;
async function shutdown(signal) {
  if (stopping) return;
  stopping = true;
  console.log(`received ${signal}; shutting down`);
  const erasureStopped = stopThreadErasureWorker();
  nodeChannel.close();
  server.close(async () => {
    await erasureStopped;
    await pool.end();
    process.exit(0);
  });
}
process.on("SIGINT", () => void shutdown("SIGINT"));
process.on("SIGTERM", () => void shutdown("SIGTERM"));
