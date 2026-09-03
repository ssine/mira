import crypto from "node:crypto";
import { WebSocket, WebSocketServer } from "ws";

import {
  dispatchDynamicTool,
  dynamicToolContentItems,
  dynamicToolNamespace,
  dynamicToolSpecs,
} from "./dynamic-tools.mjs";
import { setNodeChannelStatus } from "./node-registry.mjs";

function jsonMessage(data) {
  return JSON.parse(Buffer.isBuffer(data) ? data.toString("utf8") : String(data));
}

function tokenProtocol(token) {
  return `auth.${Buffer.from(token).toString("base64url")}`;
}

function mergeDynamicTools(existing) {
  const tools = Array.isArray(existing) ? existing : [];
  return [
    ...tools.filter((tool) => tool?.name !== dynamicToolNamespace),
    ...dynamicToolSpecs(),
  ];
}

export class NodeChannel {
  constructor({ server, pool, authToken }) {
    this.pool = pool;
    this.authToken = authToken;
    this.nodes = new Map();
    this.pending = new Map();
    this.proxies = new Map();
    this.wss = new WebSocketServer({
      noServer: true,
      maxPayload: 16 * 1024 * 1024,
      handleProtocols: (protocols) =>
        protocols.has("codex-node-v1") ? "codex-node-v1" : protocols.values().next().value,
    });
    server.on("upgrade", (request, socket, head) => this.upgrade(request, socket, head));
  }

  authorized(request, url, nodeConnection) {
    if (request.headers.authorization === `Bearer ${this.authToken}`) return true;
    if (!nodeConnection && url.searchParams.get("access_token") === this.authToken) return true;
    const protocols = String(request.headers["sec-websocket-protocol"] ?? "")
      .split(",")
      .map((value) => value.trim());
    return nodeConnection && protocols.includes(tokenProtocol(this.authToken));
  }

  upgrade(request, socket, head) {
    const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "localhost"}`);
    const nodeMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/connect$/i);
    const proxyMatch = url.pathname.match(/^\/v1\/nodes\/([0-9a-f-]{36})\/app-server$/i);
    if (!nodeMatch && !proxyMatch) {
      socket.destroy();
      return;
    }
    if (!this.authorized(request, url, Boolean(nodeMatch))) {
      socket.write("HTTP/1.1 401 Unauthorized\r\nConnection: close\r\n\r\n");
      socket.destroy();
      return;
    }
    this.wss.handleUpgrade(request, socket, head, (ws) => {
      if (nodeMatch) this.attachNode(nodeMatch[1], ws);
      else this.attachProxy(proxyMatch[1], ws);
    });
  }

  attachNode(nodeId, ws) {
    const previous = this.nodes.get(nodeId);
    if (previous?.readyState === WebSocket.OPEN) previous.close(1012, "replaced");
    this.nodes.set(nodeId, ws);
    void setNodeChannelStatus(this.pool, nodeId, {
      connected: true,
      connectedAt: new Date().toISOString(),
      protocolVersion: 1,
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
        connected: false,
        disconnectedAt: new Date().toISOString(),
        protocolVersion: 1,
      });
      for (const [requestId, pending] of this.pending) {
        if (pending.nodeId === nodeId) {
          clearTimeout(pending.timeout);
          pending.reject(new Error(`node ${nodeId} disconnected`));
          this.pending.delete(requestId);
        }
      }
      for (const [sessionId, proxy] of this.proxies) {
        if (proxy.nodeId === nodeId) {
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
      else pending.reject(new Error(message.error?.message ?? "node request failed"));
      return;
    }
    if (message.type === "appserver.message") {
      const proxy = this.proxies.get(message.sessionId);
      if (!proxy || proxy.nodeId !== nodeId) return;
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
    if (
      message.method === "item/tool/call" &&
      message.params?.namespace === dynamicToolNamespace &&
      message.id !== undefined
    ) {
      try {
        const result = await dispatchDynamicTool(
          this,
          this.pool,
          message.params.tool,
          message.params.arguments,
        );
        this.trySendToNode(proxy.nodeId, {
          type: "appserver.message",
          sessionId: proxy.sessionId,
          payload: JSON.stringify({
            id: message.id,
            result: {
              contentItems: dynamicToolContentItems(message.params.tool, result),
              success: true,
            },
          }),
        });
      } catch (error) {
        this.trySendToNode(proxy.nodeId, {
          type: "appserver.message",
          sessionId: proxy.sessionId,
          payload: JSON.stringify({
            id: message.id,
            result: {
              contentItems: [{ type: "inputText", text: error.message }],
              success: false,
            },
          }),
        });
      }
      return;
    }
    if (proxy.ws.readyState === WebSocket.OPEN) proxy.ws.send(payload);
  }

  attachProxy(nodeId, ws) {
    if (!this.nodes.has(nodeId)) {
      ws.close(1013, "node capability channel is offline");
      return;
    }
    const sessionId = crypto.randomUUID();
    const proxy = { nodeId, sessionId, ws };
    this.proxies.set(sessionId, proxy);
    if (!this.trySendToNode(nodeId, { type: "appserver.open", sessionId })) {
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
        payload = JSON.stringify(message);
      } catch {
        // App-server will report malformed JSON-RPC payloads.
      }
      if (!this.trySendToNode(nodeId, { type: "appserver.message", sessionId, payload })) {
        ws.close(1011, "node disconnected");
      }
    });
    ws.on("close", () => {
      this.proxies.delete(sessionId);
      this.trySendToNode(nodeId, { type: "appserver.close", sessionId });
    });
  }

  sendToNode(nodeId, message) {
    const ws = this.nodes.get(nodeId);
    if (!ws || ws.readyState !== WebSocket.OPEN) throw new Error(`node ${nodeId} is offline`);
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

  invoke(nodeId, capability, params, timeoutMs = 30_000) {
    const requestId = crypto.randomUUID();
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => {
        this.pending.delete(requestId);
        reject(new Error(`node ${capability} request timed out`));
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

  close() {
    for (const ws of this.nodes.values()) ws.close(1001, "server shutting down");
    for (const proxy of this.proxies.values()) proxy.ws.close(1001, "server shutting down");
    this.wss.close();
  }
}
