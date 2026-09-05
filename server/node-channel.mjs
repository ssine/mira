import crypto from "node:crypto";
import { WebSocket, WebSocketServer } from "ws";

import {
  dispatchDynamicTool,
  dynamicToolContentItems,
  dynamicToolNamespace,
  dynamicToolSpecs,
} from "./dynamic-tools.mjs";
import { getNode, setNodeChannelStatus } from "./node-registry.mjs";

function jsonMessage(data) {
  return JSON.parse(Buffer.isBuffer(data) ? data.toString("utf8") : String(data));
}

function protocolToken(request) {
  const encoded = String(request.headers["sec-websocket-protocol"] ?? "")
    .split(",").map((value) => value.trim())
    .find((value) => value.startsWith("auth."))?.slice(5);
  if (!encoded) return null;
  try {
    return Buffer.from(encoded, "base64url").toString("utf8");
  } catch {
    return null;
  }
}

function mergeDynamicTools(existing) {
  const tools = Array.isArray(existing) ? existing : [];
  return [...tools.filter((tool) => tool?.name !== dynamicToolNamespace), ...dynamicToolSpecs()];
}

function validNativeAbsolutePath(value, platform) {
  if (typeof value !== "string" || value.length === 0 || value.length > 4_096 || /[\u0000-\u001f\u007f]/.test(value)) {
    return false;
  }
  if (platform === "windows") return /^(?:[a-zA-Z]:[\\/]|\\\\)/.test(value);
  return value.startsWith("/");
}

function bundledMiraCLIPath(codexPath, platform) {
  if (typeof codexPath !== "string") return null;
  const match = codexPath.match(/^(.*[\\/])mira-codex-package[\\/]bin[\\/]codex(?:\.exe)?$/i);
  if (!match) return null;
  return `${match[1]}${platform === "windows" ? "mira.exe" : "mira"}`;
}

function targetMiraCLIPath(target) {
  const platform = target?.platform;
  const candidates = [
    target?.reportedAppServer?.miraCliPath,
    target?.machineStatus?.miraCliPath,
    bundledMiraCLIPath(target?.reportedAppServer?.codexPath, platform),
  ];
  return candidates.find((candidate) => validNativeAbsolutePath(candidate, platform)) ?? null;
}

function targetDefaultCwd(target) {
  const value = target?.desiredAppServer?.defaultCwd;
  return validNativeAbsolutePath(value, target?.platform) ? value : null;
}

function shellInvocation(path, platform) {
  const quoted = platform === "windows"
    ? `'${path.replaceAll("'", "''")}'`
    : `'${path.replaceAll("'", `'"'"'`)}'`;
  return platform === "windows" ? `& ${quoted}` : quoted;
}

function miraCLIInstructions(target) {
  const path = targetMiraCLIPath(target);
  if (!path) return null;
  const executable = shellInvocation(path, target.platform);
  return [
    "MIRA_CLI_INSTRUCTIONS_V1_BEGIN",
    "Mira node-to-node SSH access is available from this execution node.",
    `The Mira CLI absolute path is ${JSON.stringify(path)}. Invoke that exact path as a normal shell command; do not assume mira is on PATH.`,
    "SSH, SCP, and SFTP are CLI-only operations. They are not home_nodes dynamic tools and must not be modeled or invoked as dynamic tools.",
    `List approved nodes before selecting a target: ${executable} nodes list --json`,
    `Run a remote command: ${executable} ssh <node-id-or-exact-node-key> -- pwd`,
    `Open an interactive remote shell: ${executable} ssh -t <node-id-or-exact-node-key>`,
    `Upload one regular file: ${executable} scp <local-path> <node-id>::<absolute-remote-path>`,
    `Download one regular file: ${executable} scp <node-id>::<absolute-remote-path> <local-path>`,
    `Copy a directory with native SCP: ${executable} scp -rp <local-directory> <node-id>::<absolute-remote-directory>`,
    `Use SFTP interactively: ${executable} sftp <node-id>; batch: ${executable} sftp -b <commands-file> <node-id>. Native SCP/SFTP overwrite semantics apply.`,
    "Use a Node ID or exact nodeKey returned by nodes list; do not guess selectors. Never read or expose the Mira identity credential.",
    "MIRA_CLI_INSTRUCTIONS_V1_END",
  ].join("\n");
}

function mergeDeveloperInstructions(existing, addition) {
  if (!addition) return existing;
  const current = typeof existing === "string"
    ? existing.replace(/\n?MIRA_CLI_INSTRUCTIONS_V1_BEGIN[\s\S]*?MIRA_CLI_INSTRUCTIONS_V1_END\n?/g, "\n").trim()
    : "";
  return current ? `${current}\n\n${addition}` : addition;
}

function rejectUpgrade(socket, status, label) {
  socket.write(`HTTP/1.1 ${status} ${label}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
  socket.destroy();
}

function channelError(message, statusCode, code) {
  const error = new Error(message);
  error.statusCode = statusCode;
  error.code = code;
  return error;
}

function safeStoreId(value) {
  if (value === null) return "personal";
  return typeof value === "string" && /^[a-zA-Z0-9._-]{1,128}$/.test(value) ? value : null;
}

function validClientRequestId(value) {
  return typeof value === "string" &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

function requestDigest(value) {
  return crypto.createHash("sha256").update(canonicalJSON(value)).digest("hex");
}

function proxyActorKey(caller) {
  return `${caller.kind}:${caller.subjectId ?? caller.nodeId ?? "unknown"}`;
}

export class NodeChannel {
  constructor({ server, pool, authService }) {
    this.pool = pool;
    this.authService = authService;
    this.capabilityService = null;
    this.nodes = new Map();
    this.statusWrites = new Map();
    this.pending = new Map();
    this.proxies = new Map();
    this.threadStarts = new Map();
    this.wss = new WebSocketServer({
      noServer: true,
      maxPayload: 16 * 1024 * 1024,
      handleProtocols: (protocols) => {
        if (protocols.has("mira-node-v1")) return "mira-node-v1";
        if (protocols.has("mira-client-v1")) return "mira-client-v1";
        return false;
      },
    });
    server.on("upgrade", (request, socket, head) => {
      void this.upgrade(request, socket, head).catch((error) => {
        console.error("websocket upgrade failed", error);
        socket.destroy();
      });
    });
  }

  setCapabilityService(service) {
    this.capabilityService = service;
  }

  isConnected(nodeId) {
    return this.nodes.get(nodeId)?.readyState === WebSocket.OPEN;
  }

  async nodeAuthorization(request, nodeId) {
    const token = protocolToken(request);
    const principal = token ? await this.authService.authenticateNodeToken(token, "node") : null;
    if (!principal) return { status: 401 };
    if (principal.revoked || principal.nodeId !== nodeId) return { status: 403 };
    return { status: 200, principal };
  }

  async proxyAuthorization(request, targetNodeId) {
    const token = protocolToken(request);
    const principal = token
      ? await this.authService.authenticateNodeToken(token, "app-server")
      : await this.authService.authenticate(request, "app-server");
    if (!principal) return { status: 401 };
    if (principal.revoked || !this.authService.permits(principal, "trusted")) return { status: 403 };
    const target = await getNode(this.pool, targetNodeId);
    if (!target) return { status: 404 };
    if (target.capabilities?.appServer !== true) return { status: 409 };
    return { status: 200, principal, target };
  }

  async upgrade(request, socket, head) {
    const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
    const sshMatch = url.pathname.match(/^\/v1\/ssh\/sessions\/([0-9a-f-]{36})\/(source|target)$/i);
    if (sshMatch && this.sshRelay) return this.sshRelay.upgrade(request, socket, head, sshMatch[1], sshMatch[2]);
    const nodeMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/connect$/i);
    const proxyMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/app-server$/i);
    if (!nodeMatch && !proxyMatch) {
      rejectUpgrade(socket, 404, "Not Found");
      return;
    }
    const storeId = proxyMatch ? safeStoreId(url.searchParams.get("storeId")) : null;
    if (proxyMatch && !storeId) {
      rejectUpgrade(socket, 400, "Bad Request");
      return;
    }
    // Durable credentials are never accepted from the URL/query string.
    const authorization = nodeMatch
      ? await this.nodeAuthorization(request, nodeMatch[1])
      : await this.proxyAuthorization(request, proxyMatch[1]);
    if (authorization.status !== 200) {
      const labels = { 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 409: "Conflict" };
      rejectUpgrade(socket, authorization.status, labels[authorization.status]);
      return;
    }
    this.wss.handleUpgrade(request, socket, head, (ws) => {
      if (nodeMatch) this.attachNode(nodeMatch[1], ws);
      else this.attachProxy(proxyMatch[1], ws, authorization.principal, storeId, authorization.target);
    });
  }

  attachNode(nodeId, ws) {
    const previous = this.nodes.get(nodeId);
    if (previous) this.rejectNodeWork(nodeId);
    this.nodes.set(nodeId, ws);
    if (previous?.readyState === WebSocket.OPEN) previous.close(1012, "replaced");
    this.writeNodeStatus(nodeId, {
      connected: true, connectedAt: new Date().toISOString(), protocolVersion: 1,
    });
    ws.on("message", (data) => {
      if (this.nodes.get(nodeId) !== ws) return;
      void this.handleNodeMessage(nodeId, ws, data).catch((error) => {
        console.error("node channel message failed", error);
        ws.close(1011, "node channel message failed");
      });
    });
    ws.on("close", () => {
      // A replaced connection can finish closing after its successor is live.
      // It must not mark the new channel offline or reject its in-flight work.
      if (this.nodes.get(nodeId) !== ws) return;
      this.nodes.delete(nodeId);
      this.writeNodeStatus(nodeId, {
        connected: false, disconnectedAt: new Date().toISOString(), protocolVersion: 1,
      });
      this.rejectNodeWork(nodeId);
    });
  }

  writeNodeStatus(nodeId, status) {
    // Serialize this Node's writes: pooled database connections may otherwise
    // commit a slow old disconnect after a newer connect. Remove settled queues.
    const write = (this.statusWrites.get(nodeId) ?? Promise.resolve())
      .then(() => setNodeChannelStatus(this.pool, nodeId, status))
      .catch((error) => console.error("node channel status update failed", error));
    this.statusWrites.set(nodeId, write);
    void write.then(() => {
      if (this.statusWrites.get(nodeId) === write) this.statusWrites.delete(nodeId);
    });
  }

  rejectNodeWork(nodeId) {
    this.sshRelay?.disconnectNode(nodeId);
    for (const [requestId, pending] of this.pending) {
      if (pending.nodeId === nodeId) {
        clearTimeout(pending.timeout);
        pending.reject(channelError("target Node disconnected", 503, "node_offline"));
        this.pending.delete(requestId);
      }
    }
    for (const [sessionId, proxy] of this.proxies) {
      if (proxy.targetNodeId === nodeId) {
        proxy.ws.close(1011, "node disconnected");
        proxy.clientClosed = true;
        if (proxy.idempotentThreadStarts.size > 0) {
          void this.abandonProxyThreadStarts(proxy).catch((error) => {
            console.error("failed to abandon disconnected thread/start", error);
            this.cleanupProxy(proxy);
          });
        } else {
          this.cleanupProxy(proxy);
        }
      }
    }
  }

  async handleNodeMessage(nodeId, ws, data) {
    let message;
    try {
      message = jsonMessage(data);
    } catch {
      ws.close(1007, "invalid JSON");
      return;
    }
    if (message.type === "response" && typeof message.requestId === "string") {
      const pending = this.pending.get(message.requestId);
      if (!pending || pending.nodeId !== nodeId) return;
      clearTimeout(pending.timeout);
      this.pending.delete(message.requestId);
      if (message.ok) pending.resolve(message.result);
      else pending.reject(channelError(message.error?.message ?? "Node request failed", 400, "node_request_failed"));
      return;
    }
    if (message.type === "appserver.message") {
      const proxy = this.proxies.get(message.sessionId);
      if (!proxy || proxy.targetNodeId !== nodeId) return;
      await this.forwardAppServerMessage(proxy, message.payload);
      return;
    }
    if (message.type === "appserver.error") {
      const proxy = this.proxies.get(message.sessionId);
      if (proxy) {
        proxy.ws.close(1011, String(message.error ?? "app-server tunnel failed"));
        proxy.clientClosed = true;
        if (proxy.idempotentThreadStarts.size > 0) {
          await this.abandonProxyThreadStarts(proxy);
        }
      }
      return;
    }
    if (message.type === "appserver.closed") {
      const proxy = this.proxies.get(message.sessionId);
      if (proxy) {
        proxy.ws.close(1000, "app-server closed");
        proxy.clientClosed = true;
        if (proxy.idempotentThreadStarts.size > 0) {
          await this.abandonProxyThreadStarts(proxy);
        }
      }
    }
  }

  threadStartKey(proxy, clientRequestId) {
    return `${proxy.storeId}\n${proxy.actorKey}\n${clientRequestId}`;
  }

  sendProxyResult(proxy, id, result) {
    if (proxy.ws.readyState === WebSocket.OPEN) {
      proxy.ws.send(JSON.stringify({ id, result }));
    }
  }

  sendProxyError(proxy, id, message, code = -32602) {
    if (proxy.ws.readyState === WebSocket.OPEN) {
      proxy.ws.send(JSON.stringify({ id, error: { code, message } }));
    }
  }

  async reserveThreadStart(proxy, message) {
    const clientRequestId = message.params?.miraRequestId;
    if (clientRequestId === undefined) return { action: "forward" };
    delete message.params.miraRequestId;
    if (message.id === undefined || !validClientRequestId(clientRequestId)) {
      this.sendProxyError(proxy, message.id ?? null, "miraRequestId must be a UUID on a thread/start request");
      return { action: "handled" };
    }
    // Keep released thread/start fingerprints stable; fork requests use a
    // separate method-qualified digest in the same durable creation ledger.
    const digest = requestDigest(message.method === "thread/fork" ? { method: message.method, params: message.params } : message.params);
    const key = this.threadStartKey(proxy, clientRequestId);
    const inserted = await this.pool.query(
      `INSERT INTO mira_appserver_thread_start_requests (
         store_id, actor_key, client_request_id, target_node_id, request_sha256, status
       ) VALUES ($1, $2, $3, $4, $5, 'pending')
       ON CONFLICT (store_id, actor_key, client_request_id) DO NOTHING
       RETURNING client_request_id`,
      [proxy.storeId, proxy.actorKey, clientRequestId, proxy.targetNodeId, digest],
    );
    if (inserted.rowCount > 0) {
      this.threadStarts.set(key, { owner: proxy, waiters: [] });
      proxy.idempotentThreadStarts.set(String(message.id), key);
      return { action: "forward" };
    }

    const existing = await this.pool.query(
      `SELECT request_sha256, status, thread_id, response
       FROM mira_appserver_thread_start_requests
       WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3`,
      [proxy.storeId, proxy.actorKey, clientRequestId],
    );
    const row = existing.rows[0];
    if (!row || row.request_sha256 !== digest) {
      this.sendProxyError(proxy, message.id, "miraRequestId was reused with different thread/start parameters");
      return { action: "handled" };
    }
    if (row.status === "completed") {
      this.bindProxyThread(proxy, row.thread_id);
      this.sendProxyResult(proxy, message.id, row.response);
      return { action: "handled" };
    }
    const active = this.threadStarts.get(key);
    if (row.status === "pending" && active) {
      active.waiters.push({ proxy, id: message.id });
      return { action: "handled" };
    }

    const claimed = await this.pool.query(
      `UPDATE mira_appserver_thread_start_requests
       SET target_node_id = $4, status = 'pending', thread_id = NULL, response = NULL, updated_at = NOW()
       WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3
         AND (status = 'failed' OR updated_at < NOW() - INTERVAL '30 seconds')
       RETURNING client_request_id`,
      [proxy.storeId, proxy.actorKey, clientRequestId, proxy.targetNodeId],
    );
    if (claimed.rowCount === 0) {
      this.sendProxyError(proxy, message.id, "thread/start with this miraRequestId is still in progress", -32001);
      return { action: "handled" };
    }
    this.threadStarts.set(key, { owner: proxy, waiters: [] });
    proxy.idempotentThreadStarts.set(String(message.id), key);
    return { action: "forward" };
  }

  async finishThreadStart(proxy, message) {
    if (message.id === undefined) return;
    const key = proxy.idempotentThreadStarts.get(String(message.id));
    if (!key) return;
    proxy.idempotentThreadStarts.delete(String(message.id));
    const [storeId, actorKey, clientRequestId] = key.split("\n");
    const threadId = message.error ? null : message.result?.thread?.id;
    if (typeof threadId === "string") {
      await this.pool.query(
        `UPDATE mira_appserver_thread_start_requests
         SET status = 'completed', thread_id = $4, response = $5::jsonb, updated_at = NOW()
         WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3`,
        [storeId, actorKey, clientRequestId, threadId, JSON.stringify(message.result)],
      );
    } else {
      await this.pool.query(
        `UPDATE mira_appserver_thread_start_requests
         SET status = 'failed', thread_id = NULL, response = NULL, updated_at = NOW()
         WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3`,
        [storeId, actorKey, clientRequestId],
      );
    }
    const active = this.threadStarts.get(key);
    this.threadStarts.delete(key);
    for (const waiter of active?.waiters ?? []) {
      if (typeof threadId === "string") this.bindProxyThread(waiter.proxy, threadId);
      const replay = { ...message, id: waiter.id };
      if (waiter.proxy.ws.readyState === WebSocket.OPEN) waiter.proxy.ws.send(JSON.stringify(replay));
    }
    if (proxy.clientClosed && proxy.idempotentThreadStarts.size === 0) this.cleanupProxy(proxy);
  }

  cleanupProxy(proxy) {
    if (this.proxies.get(proxy.sessionId) !== proxy) return;
    clearTimeout(proxy.detachTimer);
    this.proxies.delete(proxy.sessionId);
    this.trySendToNode(proxy.targetNodeId, { type: "appserver.close", sessionId: proxy.sessionId });
  }

  async abandonProxyThreadStarts(proxy) {
    for (const key of proxy.idempotentThreadStarts.values()) {
      const [storeId, actorKey, clientRequestId] = key.split("\n");
      await this.pool.query(
        `UPDATE mira_appserver_thread_start_requests
         SET status = 'failed', thread_id = NULL, response = NULL, updated_at = NOW()
         WHERE store_id = $1 AND actor_key = $2 AND client_request_id = $3 AND status = 'pending'`,
        [storeId, actorKey, clientRequestId],
      );
      const active = this.threadStarts.get(key);
      this.threadStarts.delete(key);
      for (const waiter of active?.waiters ?? []) {
        this.sendProxyError(waiter.proxy, waiter.id, "original thread/start connection was lost", -32002);
      }
    }
    proxy.idempotentThreadStarts.clear();
    this.cleanupProxy(proxy);
  }

  async forwardAppServerMessage(proxy, payload) {
    let message;
    try {
      message = JSON.parse(payload);
    } catch {
      if (proxy.ws.readyState === WebSocket.OPEN) proxy.ws.send(payload);
      return;
    }
    await this.finishThreadStart(proxy, message);
    const observedThreadId = message.params?.threadId ?? message.params?.thread?.id ?? null;
    if (typeof message.method === "string" && /^(?:thread|turn|item)\//.test(message.method) &&
        typeof observedThreadId === "string") {
      this.bindProxyThread(proxy, observedThreadId, false);
    }
    if (message.id !== undefined && proxy.threadRequestBindings.has(String(message.id))) {
      const requestedThreadId = proxy.threadRequestBindings.get(String(message.id));
      proxy.threadRequestBindings.delete(String(message.id));
      const threadId = message.error ? null : message.result?.thread?.id ?? requestedThreadId;
      if (typeof threadId === "string") this.bindProxyThread(proxy, threadId);
    }
    if (message.method === "item/tool/call" && message.params?.namespace === dynamicToolNamespace && message.id !== undefined) {
      const executionActor = {
        kind: "node", nodeId: proxy.targetNodeId, subjectId: null,
        clientType: "app-server", transport: "internal", revoked: false,
      };
      try {
        const result = await dispatchDynamicTool(
          this.capabilityService, executionActor, message.params.tool,
          message.params.arguments, {
            requestId: String(message.id),
            threadId: message.params?.threadId ?? proxy.threadId ?? null,
            auditMetadata: {
              source: "app-server",
              ...(typeof message.params?.parentThreadId === "string" ? { parentThreadId: message.params.parentThreadId } : {}),
              ...(typeof message.params?.subagentThreadId === "string" ? { subagentThreadId: message.params.subagentThreadId } : {}),
            },
          },
        );
        this.trySendToNode(proxy.targetNodeId, {
          type: "appserver.message", sessionId: proxy.sessionId,
          payload: JSON.stringify({ id: message.id, result: {
            contentItems: dynamicToolContentItems(message.params.tool, result), success: true,
          } }),
        });
      } catch (error) {
        this.trySendToNode(proxy.targetNodeId, {
          type: "appserver.message", sessionId: proxy.sessionId,
          payload: JSON.stringify({ id: message.id, result: {
            contentItems: [{ type: "inputText", text: error.message }], success: false,
          } }),
        });
      }
      return;
    }
    if (proxy.ws.readyState === WebSocket.OPEN) proxy.ws.send(payload);
  }

  bindProxyThread(proxy, threadId, primary = true) {
    if (primary || !proxy.threadId) proxy.threadId = threadId;
    proxy.boundThreadIds ??= new Set();
    if (proxy.boundThreadIds.has(threadId)) return;
    proxy.boundThreadIds.add(threadId);
    void this.pool.query(
      `INSERT INTO mira_codex_thread_runtimes (store_id, thread_id, node_id, bound_at)
       VALUES ($1, $2, $3, NOW())
       ON CONFLICT (store_id, thread_id) DO UPDATE SET
         node_id = EXCLUDED.node_id, bound_at = EXCLUDED.bound_at`,
      [proxy.storeId, threadId, proxy.targetNodeId],
    ).catch((error) => console.error("thread runtime binding failed", error));
  }

  async forwardProxyClientMessage(proxy, data) {
    let payload = Buffer.isBuffer(data) ? data.toString("utf8") : String(data);
    let message;
    try {
      message = JSON.parse(payload);
    } catch {
      if (!this.trySendToNode(proxy.targetNodeId, {
        type: "appserver.message", sessionId: proxy.sessionId, payload,
      })) proxy.ws.close(1011, "node disconnected");
      return;
    }
    if (message.method === "initialize") {
      message.params ??= {};
      message.params.capabilities ??= {};
      message.params.capabilities.experimentalApi = true;
    }
    if (["thread/start", "thread/resume", "thread/fork"].includes(message.method)) {
      message.params ??= {};
      if (["thread/start", "thread/fork"].includes(message.method) && message.params.miraRequestId !== undefined) {
        const reservation = await this.reserveThreadStart(proxy, message);
        if (reservation.action === "handled") return;
      }
      message.params.approvalPolicy ??= "never";
      message.params.sandbox ??= "danger-full-access";
      // Fork inherits the source tools; ThreadForkParams has no dynamicTools field.
      if (message.method !== "thread/fork") message.params.dynamicTools = mergeDynamicTools(message.params.dynamicTools);
      if (message.method === "thread/start" &&
          (typeof message.params.cwd !== "string" || message.params.cwd.trim() === "")) {
        message.params.cwd = targetDefaultCwd(proxy.target) ?? message.params.cwd;
      }
      message.params.developerInstructions = mergeDeveloperInstructions(
        message.params.developerInstructions,
        miraCLIInstructions(proxy.target),
      );
    }
    if (["thread/start", "thread/resume", "thread/fork", "turn/start"].includes(message.method) && message.id !== undefined) {
      proxy.threadRequestBindings.set(String(message.id),
        typeof message.params?.threadId === "string" ? message.params.threadId : null);
    }
    payload = JSON.stringify(message);
    if (!this.trySendToNode(proxy.targetNodeId, {
      type: "appserver.message", sessionId: proxy.sessionId, payload,
    })) proxy.ws.close(1011, "node disconnected");
  }

  attachProxy(targetNodeId, ws, caller, storeId = "personal", target = null) {
    if (!this.isConnected(targetNodeId)) {
      ws.close(1013, "node capability channel is offline");
      return;
    }
    const sessionId = crypto.randomUUID();
    const proxy = {
      targetNodeId, callerNodeId: caller.kind === "node" ? caller.nodeId : null,
      actorKey: proxyActorKey(caller), sessionId, ws, storeId, target, threadId: null,
      threadRequestBindings: new Map(), idempotentThreadStarts: new Map(), boundThreadIds: new Set(),
      clientClosed: false, detachTimer: null,
    };
    this.proxies.set(sessionId, proxy);
    if (!this.trySendToNode(targetNodeId, { type: "appserver.open", sessionId })) {
      this.proxies.delete(sessionId);
      ws.close(1013, "node capability channel is offline");
      return;
    }
    ws.on("message", (data) => {
      void this.forwardProxyClientMessage(proxy, data).catch((error) => {
        console.error("App Server client message failed", error);
        this.sendProxyError(proxy, null, "Mira could not process the App Server request", -32603);
      });
    });
    ws.on("close", () => {
      proxy.clientClosed = true;
      if (proxy.idempotentThreadStarts.size > 0) {
        proxy.detachTimer = setTimeout(() => {
          void this.abandonProxyThreadStarts(proxy).catch((error) => {
            console.error("failed to abandon detached thread/start", error);
            this.cleanupProxy(proxy);
          });
        }, 30_000);
        proxy.detachTimer.unref?.();
      } else {
        this.cleanupProxy(proxy);
      }
    });
  }

  sendToNode(nodeId, message) {
    const ws = this.nodes.get(nodeId);
    if (!ws || ws.readyState !== WebSocket.OPEN) throw channelError("target Node is offline", 503, "node_offline");
    ws.send(JSON.stringify(message));
  }

  trySendToNode(nodeId, message) {
    try {
      this.sendToNode(nodeId, message);
      return true;
    } catch {
      return false;
    }
  }

  async invoke(nodeId, capability, params, timeoutMs = 30_000) {
    const requestId = crypto.randomUUID();
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pending.delete(requestId);
        reject(channelError("capability request timed out", 504, "capability_timeout"));
      }, timeoutMs);
      this.pending.set(requestId, { nodeId, resolve, reject, timeout });
      try {
        this.sendToNode(nodeId, { type: "request", requestId, capability, params });
      } catch (error) {
        clearTimeout(timeout);
        this.pending.delete(requestId);
        reject(error);
      }
    });
  }

  disconnectNode(nodeId, reason = "revoked") {
    this.sshRelay?.disconnectNode(nodeId);
    const ws = this.nodes.get(nodeId);
    if (ws?.readyState === WebSocket.OPEN) ws.close(1008, reason);
    for (const [sessionId, proxy] of this.proxies) {
      if (proxy.targetNodeId === nodeId || proxy.callerNodeId === nodeId) {
        proxy.ws.close(1008, reason);
        proxy.clientClosed = true;
        if (proxy.idempotentThreadStarts.size > 0) {
          void this.abandonProxyThreadStarts(proxy).catch((error) => {
            console.error("failed to abandon revoked thread/start", error);
            this.cleanupProxy(proxy);
          });
        } else {
          this.cleanupProxy(proxy);
        }
      }
    }
  }

  close() {
    this.sshRelay?.close();
    for (const ws of this.nodes.values()) ws.close(1001, "server shutting down");
    for (const proxy of this.proxies.values()) {
      proxy.ws.close(1001, "server shutting down");
      proxy.clientClosed = true;
      if (proxy.idempotentThreadStarts.size > 0) {
        void this.abandonProxyThreadStarts(proxy).catch(() => this.cleanupProxy(proxy));
      } else {
        this.cleanupProxy(proxy);
      }
    }
    this.wss.close();
  }
}
