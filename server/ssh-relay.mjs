import crypto from "node:crypto";
import { WebSocket, WebSocketServer, createWebSocketStream } from "ws";
import { appendAudit } from "./auth.mjs";

const maxSessions = 128;
const maxPerNode = 8;
const frameLimit = 64 * 1024;
const fail = (status, error) => ({ status, body: { error, code: "ssh_unavailable" } });

// Only canonical, comment-free Ed25519 public keys. No private material is accepted.
export function validSSHKey(value) {
  if (typeof value !== "string" || !/^ssh-ed25519 [A-Za-z0-9+/]+={0,2}$/.test(value)) return false;
  const raw = Buffer.from(value.slice(12), "base64");
  return raw.length === 51 && raw.readUInt32BE(0) === 11 &&
    raw.subarray(4, 15).toString() === "ssh-ed25519" && raw.readUInt32BE(15) === 32 &&
    raw.toString("base64") === value.slice(12);
}

export class SSHRelay {
  constructor({ pool, authService, nodeChannel }) {
    Object.assign(this, { pool, authService, nodeChannel });
    this.sessions = new Map();
    this.wss = new WebSocketServer({ noServer: true, maxPayload: frameLimit,
      perMessageDeflate: false, handleProtocols: protocols => protocols.has("mira-ssh-v1") ? "mira-ssh-v1" : false });
  }

  async keys(nodeId) {
    const result = await this.pool.query(`SELECT keys.host_key, keys.client_key, credentials.credential_id
      FROM mira_node_ssh_keys keys JOIN mira_node_credentials credentials USING (credential_id)
      JOIN codex_nodes nodes USING (node_id)
      WHERE nodes.node_id = $1 AND nodes.approval_status = 'approved' AND credentials.revoked_at IS NULL`, [nodeId]);
    return result.rows[0];
  }

  async publish(principal, body) {
    if (!body || !validSSHKey(body.hostKey) || !validSSHKey(body.clientKey) || body.hostKey === body.clientKey) {
      return fail(400, "two distinct Ed25519 public keys are required");
    }
    // Credential binding is derived from the authenticated principal, never the body.
    const result = await this.pool.query(`INSERT INTO mira_node_ssh_keys (credential_id, host_key, client_key)
      SELECT credential_id, $2, $3 FROM mira_node_credentials
      WHERE credential_id = $1 AND revoked_at IS NULL
      ON CONFLICT (credential_id) DO UPDATE SET host_key = mira_node_ssh_keys.host_key
      RETURNING host_key, client_key`, [principal.credentialId, body.hostKey, body.clientKey]);
    const saved = result.rows[0];
    if (!saved) return fail(403, "Node credential is no longer approved");
    if (saved.host_key !== body.hostKey || saved.client_key !== body.clientKey) return fail(409, "SSH key change requires a new Node credential");
    return { status: 200, body: { status: "registered" } };
  }

  async create(principal, targetNodeId, request) {
    const [source, target] = await Promise.all([this.keys(principal.nodeId), this.keys(targetNodeId)]);
    if (!source || source.credential_id !== principal.credentialId) return fail(409, "publish this Node's SSH public keys first");
    if (!target || !this.nodeChannel.isConnected(targetNodeId)) return fail(409, "target Node is offline or does not support SSH yet");
    const count = nodeId => [...this.sessions.values()].filter(s => s.sourceNodeId === nodeId || s.targetNodeId === nodeId).length;
    if (this.sessions.size >= maxSessions || count(principal.nodeId) >= maxPerNode || count(targetNodeId) >= maxPerNode) {
      return fail(429, "SSH session limit reached");
    }
    const sessionId = crypto.randomUUID();
    const session = { sessionId, sourceNodeId: principal.nodeId, targetNodeId,
      sourceCredentialId: source.credential_id, targetCredentialId: target.credential_id,
      principal, claimed: new Set(), sockets: {}, streams: {} };
    this.sessions.set(sessionId, session);
    session.timer = setTimeout(() => this.end(sessionId, "connect timeout"), 30_000);
    session.timer.unref();
    try {
      await appendAudit(this.pool, { action: "ssh.requested", principal, targetNodeId, request,
        metadata: { sessionId, protocolVersion: 1 } });
      // Re-check after async audit: revocation/disconnect may have removed this session.
      if (this.sessions.get(sessionId) !== session) return fail(409, "SSH session cancelled");
      this.nodeChannel.sendToNode(targetNodeId, { type: "ssh.open", sessionId,
        sourceNodeId: principal.nodeId, clientPublicKey: source.client_key });
      return { status: 201, body: { sessionId, hostKey: target.host_key, username: "mira", protocolVersion: 1 } };
    } catch (error) { this.end(sessionId, "open failed"); throw error; }
  }

  async upgrade(request, socket, head, sessionId, side) {
    const reject = status => { socket.end(`HTTP/1.1 ${status} Rejected\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`); };
    const protocols = String(request.headers["sec-websocket-protocol"] ?? "").split(",").map(v => v.trim());
    if (!protocols.includes("mira-ssh-v1")) return reject(400);
    const token = protocols.find(v => v.startsWith("auth."))?.slice(5);
    const principal = token ? await this.authService.authenticateNodeToken(Buffer.from(token, "base64url").toString(), "ssh") : null;
    if (!principal || principal.revoked) return reject(403);
    const session = this.sessions.get(sessionId);
    if (!session || principal.nodeId !== session[`${side}NodeId`] || principal.credentialId !== session[`${side}CredentialId`]) return reject(404);
    if (session.claimed.has(side)) return reject(409);
    session.claimed.add(side);
    this.wss.handleUpgrade(request, socket, head, ws => {
      session.sockets[side] = ws;
      const stream = createWebSocketStream(ws, { highWaterMark: frameLimit });
      session.streams[side] = stream;
      ws.on("message", (_data, binary) => { if (!binary) this.end(sessionId, "binary frames required"); });
      stream.on("error", () => this.end(sessionId, "transport error"));
      stream.on("close", () => this.end(sessionId, "transport closed"));
      ws.on("close", () => this.end(sessionId, "transport closed"));
      if (session.streams.source && session.streams.target) {
        clearTimeout(session.timer);
        session.streams.source.pipe(session.streams.target).pipe(session.streams.source);
      }
    });
  }

  end(sessionId, reason = "closed") {
    const session = this.sessions.get(sessionId);
    if (!session) return;
    this.sessions.delete(sessionId);
    clearTimeout(session.timer);
    for (const stream of Object.values(session.streams)) stream.destroy();
    for (const ws of Object.values(session.sockets)) ws.terminate();
    this.nodeChannel.trySendToNode(session.targetNodeId, { type: "ssh.close", sessionId });
    void appendAudit(this.pool, { action: "ssh.closed", principal: session.principal, targetNodeId: session.targetNodeId,
      metadata: { sessionId, reason } }).catch(() => {});
  }

  disconnectNode(nodeId) {
    for (const session of this.sessions.values()) {
      if (session.sourceNodeId === nodeId || session.targetNodeId === nodeId) this.end(session.sessionId, "Node disconnected or revoked");
    }
  }

  close() { for (const id of this.sessions.keys()) this.end(id, "Server stopping"); this.wss.close(); }
}
