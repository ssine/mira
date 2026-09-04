import { appendAudit } from "./auth.mjs";
import { getNode, listNodes } from "./node-registry.mjs";

const actions = {
  status: new Set(["get"]),
  file: new Set(["roots", "stat", "list", "read", "write", "mkdir", "move", "remove"]),
  process: new Set(["count", "list", "start", "poll", "signal"]),
  pty: new Set(["list", "open", "write", "poll", "resize", "close"]),
  screen: new Set(["display", "screenshot", "hierarchy", "tap", "swipe", "key", "text"]),
  codexSessions: new Set(["list", "read", "resolve"]),
};

function serviceError(message, statusCode, code) {
  const error = new Error(message);
  error.statusCode = statusCode;
  error.code = code;
  return error;
}

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function validateText(value, name, { required = false, maximum = 32768 } = {}) {
  if (value === undefined && !required) return;
  if (typeof value !== "string" || (required && value.length === 0) || value.length > maximum || value.includes("\0")) {
    throw serviceError(`${name} is invalid`, 400, "invalid_request");
  }
}

function validateInteger(value, name, minimum, maximum) {
  if (value === undefined) return;
  if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
    throw serviceError(`${name} must be an integer between ${minimum} and ${maximum}`, 400, "invalid_request");
  }
}

function validateParams(capability, params) {
  if (!isRecord(params)) throw serviceError("params must be an object", 400, "invalid_request");
  if (capability === "status") return {};
  const allowed = actions[capability];
  if (!allowed || typeof params.action !== "string" || !allowed.has(params.action)) {
    throw serviceError(`unsupported ${capability} action`, 400, "invalid_request");
  }
  if (capability === "file") {
    if (params.action !== "roots") validateText(params.path, "path", { required: true });
    validateText(params.destination, "destination");
    validateText(params.content, "content", { maximum: 6 * 1024 * 1024 });
    validateInteger(params.offset, "offset", 0, Number.MAX_SAFE_INTEGER);
    validateInteger(params.length, "length", 1, 4 * 1024 * 1024);
  }
  if (capability === "process" || capability === "pty") {
    validateText(params.command, "command", { maximum: 4096 });
    validateText(params.cwd, "cwd");
    validateText(params.processId, "processId", { maximum: 256 });
    validateText(params.sessionId, "sessionId", { maximum: 256 });
    validateText(params.input, "input", { maximum: 1024 * 1024 });
    validateInteger(params.cursor, "cursor", 0, Number.MAX_SAFE_INTEGER);
    if (params.args !== undefined && (!Array.isArray(params.args) || params.args.length > 128 || params.args.some((item) => typeof item !== "string" || item.length > 32768))) {
      throw serviceError("args must contain at most 128 strings", 400, "invalid_request");
    }
    if (params.env !== undefined && (!isRecord(params.env) || Object.keys(params.env).length > 256 || Object.entries(params.env).some(([key, value]) => !/^[A-Za-z_][A-Za-z0-9_]*$/.test(key) || typeof value !== "string" || value.length > 32768))) {
      throw serviceError("env is invalid", 400, "invalid_request");
    }
    if (capability === "pty") {
      validateInteger(params.rows, "rows", 1, 500);
      validateInteger(params.cols, "cols", 1, 1000);
      if (params.action === "resize" && (params.rows === undefined || params.cols === undefined)) {
        throw serviceError("PTY resize requires rows and cols", 400, "invalid_request");
      }
    }
  }
  if (capability === "screen") {
    for (const name of ["x", "y", "startX", "startY", "endX", "endY"]) validateInteger(params[name], name, 0, 100000);
    validateInteger(params.durationMs, "durationMs", 1, 60000);
    validateText(params.text, "text", { maximum: 4096 });
  }
  if (capability === "codexSessions") {
    validateText(params.path, "path");
    validateInteger(params.cursor, "cursor", 0, Number.MAX_SAFE_INTEGER);
    validateInteger(params.limit, "limit", 1, 8 * 1024 * 1024);
    if (["read", "resolve"].includes(params.action) && typeof params.path !== "string") {
      throw serviceError("path is required", 400, "invalid_request");
    }
    if (params.action === "resolve" && !/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(params.rolloutId ?? "")) {
      throw serviceError("valid rolloutId is required", 400, "invalid_request");
    }
  }
  return params;
}

function advertised(node, capability) {
  if (capability === "status") return true;
  if (capability === "file") return node.capabilities?.files === true;
  if (capability === "process") return node.capabilities?.processes === true;
  if (capability === "pty") return node.capabilities?.pty === true;
  if (capability === "screen") return node.capabilities?.screen === true || node.capabilities?.input === true;
  if (capability === "codexSessions") return node.capabilities?.codexSessions === true;
  return false;
}

export class CapabilityService {
  constructor({ pool, nodeChannel }) {
    this.pool = pool;
    this.nodeChannel = nodeChannel;
  }

  async validateActor(actor) {
    if (!actor || actor.revoked || !["admin", "node"].includes(actor.kind)) {
      throw serviceError("approved Node or administrator identity required", 403, "actor_forbidden");
    }
    if (actor.kind === "node" && !await getNode(this.pool, actor.nodeId)) {
      throw serviceError("actor Node is revoked", 403, "actor_forbidden");
    }
  }

  async list(actor) {
    await this.validateActor(actor);
    return listNodes(this.pool, { includeRevoked: actor.kind === "admin" });
  }

  async invoke(actor, targetNodeId, capability, params = {}, context = {}) {
    await this.validateActor(actor);
    if (typeof targetNodeId !== "string") throw serviceError("target nodeId is required", 400, "invalid_request");
    if (!Object.hasOwn(actions, capability)) throw serviceError("unknown capability", 400, "invalid_request");
    const validated = validateParams(capability, params);
    const node = await getNode(this.pool, targetNodeId);
    if (!node) throw serviceError("approved target Node not found", 404, "not_found");
    if (!advertised(node, capability)) {
      throw serviceError(`target Node does not advertise ${capability}`, 409, "capability_unavailable");
    }
    if (!this.nodeChannel.isConnected(targetNodeId)) {
      throw serviceError("target Node is offline", 503, "node_offline");
    }
    const timeoutMs = Math.max(100, Math.min(Number(context.timeoutMs) || 30_000, 120_000));
    try {
      const result = await this.nodeChannel.invoke(targetNodeId, capability, validated, timeoutMs);
      await appendAudit(this.pool, {
        action: "capability.invoked", principal: actor, targetNodeId,
        threadId: context.threadId ?? null, requestId: context.requestId ?? null,
        request: context.request ?? null,
        metadata: { capability, action: validated.action ?? "get", ...(context.auditMetadata ?? {}) },
      });
      return result;
    } catch (error) {
      await appendAudit(this.pool, {
        action: "capability.failed", principal: actor, targetNodeId,
        threadId: context.threadId ?? null, requestId: context.requestId ?? null,
        request: context.request ?? null, success: false,
        errorCode: error.code ?? (error.statusCode === 504 ? "capability_timeout" : "node_error"),
        metadata: { capability, action: validated.action ?? "get", ...(context.auditMetadata ?? {}) },
      });
      throw error;
    }
  }
}
