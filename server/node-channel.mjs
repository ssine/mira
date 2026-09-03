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

export class NodeChannel {
  constructor({ server, pool, authService }) {
    this.pool = pool;
    this.authService = authService;
    this.capabilityService = null;
    this.nodes = new Map();
    this.pending = new Map();
    this.proxies = new Map();
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
    return { status: 200, principal };
  }

  async upgrade(request, socket, head) {
    const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
    const nodeMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/connect$/i);
    const proxyMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/app-server$/i);
    if (!nodeMatch && !proxyMatch) {
      rejectUpgrade(socket, 404, "Not Found");
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
      else this.attachProxy(proxyMatch[1], ws, authorization.principal);
    });
  }

  attachNode(nodeId, ws) {
    const previous = this.nodes.get(nodeId);
    if (previous?.readyState === WebSocket.OPEN) previous.close(1012, "replaced");
    this.nodes.set(nodeId, ws);
    void setNodeChannelStatus(this.pool, nodeId, {
      connected: true, connectedAt: new Date().toISOString(), protocolVersion: 1,
    });
    ws.on("message", (data) => {
      void this.handleNodeMessage(nodeId, ws, data).catch((error) => {
        console.error("node channel message failed", error);
        ws.close(1011, "node channel message failed");
      });
    });
    ws.on("close", () => {
      if (this.nodes.get(nodeId) === ws) this.nodes.delete(nodeId);
      void setNodeChannelStatus(this.pool, nodeId, {
        connected: false, disconnectedAt: new Date().toISOString(), protocolVersion: 1,
      });
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
          this.proxies.delete(sessionId);
        }
      }
    });
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
      if (proxy) proxy.ws.close(1011, String(message.error ?? "app-server tunnel failed"));
      return;
    }
    if (message.type === "appserver.closed") {
      const proxy = this.proxies.get(message.sessionId);
      if (proxy) proxy.ws.close(1000, "app-server closed");
    }
  }

  async forwardAppServerMessage(proxy, payload) {
    let message;
    try {
      message = JSON.parse(payload);
    } catch {
      if (proxy.ws.readyState === WebSocket.OPEN) proxy.ws.send(payload);
      return;
    }
    if (message.id !== undefined && proxy.startRequestIds.has(String(message.id))) {
      proxy.startRequestIds.delete(String(message.id));
      if (typeof message.result?.thread?.id === "string") proxy.threadId = message.result.thread.id;
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

  attachProxy(targetNodeId, ws, caller) {
    if (!this.isConnected(targetNodeId)) {
      ws.close(1013, "node capability channel is offline");
      return;
    }
    const sessionId = crypto.randomUUID();
    const proxy = {
      targetNodeId, callerNodeId: caller.kind === "node" ? caller.nodeId : null,
      sessionId, ws, threadId: null, startRequestIds: new Set(),
    };
    this.proxies.set(sessionId, proxy);
    if (!this.trySendToNode(targetNodeId, { type: "appserver.open", sessionId })) {
      this.proxies.delete(sessionId);
      ws.close(1013, "node capability channel is offline");
      return;
    }
    ws.on("message", (data) => {
      let payload = Buffer.isBuffer(data) ? data.toString("utf8") : String(data);
      try {
        const message = JSON.parse(payload);
        if (message.method === "initialize") {
          message.params ??= {};
          message.params.capabilities ??= {};
          message.params.capabilities.experimentalApi = true;
        }
        if (message.method === "thread/start" || message.method === "thread/resume") {
          message.params ??= {};
          message.params.dynamicTools = mergeDynamicTools(message.params.dynamicTools);
        }
        if (message.method === "thread/start" && message.id !== undefined) {
          proxy.startRequestIds.add(String(message.id));
        }
        if (["thread/resume", "turn/start"].includes(message.method) && typeof message.params?.threadId === "string") {
          proxy.threadId = message.params.threadId;
        }
        payload = JSON.stringify(message);
      } catch {
        // The upstream App Server reports malformed JSON-RPC payloads.
      }
      if (!this.trySendToNode(targetNodeId, { type: "appserver.message", sessionId, payload })) {
        ws.close(1011, "node disconnected");
      }
    });
    ws.on("close", () => {
      this.proxies.delete(sessionId);
      this.trySendToNode(targetNodeId, { type: "appserver.close", sessionId });
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
    const ws = this.nodes.get(nodeId);
    if (ws?.readyState === WebSocket.OPEN) ws.close(1008, reason);
    for (const [sessionId, proxy] of this.proxies) {
      if (proxy.targetNodeId === nodeId || proxy.callerNodeId === nodeId) {
        proxy.ws.close(1008, reason);
        this.proxies.delete(sessionId);
      }
    }
  }

  close() {
    for (const ws of this.nodes.values()) ws.close(1001, "server shutting down");
    for (const proxy of this.proxies.values()) proxy.ws.close(1001, "server shutting down");
    this.wss.close();
  }
}
