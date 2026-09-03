import crypto from "node:crypto";
import { isIP } from "node:net";
import argon2 from "argon2";

const sessionLifetimeMs = 12 * 60 * 60 * 1000;
const clientTypes = new Set(["node", "cli", "codex", "app-server"]);

export function tokenHash(value) {
  return crypto.createHash("sha256").update(value).digest("hex");
}

export function nodeSecretHash(secret) {
  let bytes;
  try {
    bytes = Buffer.from(secret, "base64url");
  } catch {
    return null;
  }
  if (bytes.length !== 32 || bytes.toString("base64url") !== secret) return null;
  return crypto.createHash("sha256").update(bytes).digest("hex");
}

export function parseNodeToken(token) {
  if (typeof token !== "string") return null;
  const match = token.match(
    /^mira_node_([0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})_([A-Za-z0-9_-]{43})$/i,
  );
  if (!match) return null;
  const secretHash = nodeSecretHash(match[2]);
  return secretHash ? { credentialId: match[1].toLowerCase(), secret: match[2], secretHash } : null;
}

export function randomToken(prefix) {
  return `${prefix}_${crypto.randomBytes(32).toString("base64url")}`;
}

function validUsername(value) {
  return typeof value === "string" && /^[a-zA-Z0-9._@-]{1,128}$/.test(value);
}

export async function hashPassword(password) {
  if (typeof password !== "string" || password.length < 12 || password.length > 1024) {
    throw new Error("administrator password must contain between 12 and 1024 characters");
  }
  return argon2.hash(password.normalize("NFKC"), {
    type: argon2.argon2id,
    memoryCost: 19_456,
    timeCost: 2,
    parallelism: 1,
  });
}

export async function verifyPassword(password, encoded) {
  if (typeof password !== "string" || typeof encoded !== "string") return false;
  try {
    return await argon2.verify(encoded, password.normalize("NFKC"), {
      type: argon2.argon2id,
    });
  } catch {
    return false;
  }
}

function parseCookies(request) {
  const result = new Map();
  for (const part of String(request.headers.cookie ?? "").split(";")) {
    const separator = part.indexOf("=");
    if (separator < 1) continue;
    const name = part.slice(0, separator).trim();
    const value = part.slice(separator + 1).trim();
    if (name) result.set(name, value);
  }
  return result;
}

function bearerToken(request) {
  return String(request.headers.authorization ?? "").match(/^Bearer ([^\s]+)$/)?.[1] ?? null;
}

export function requestAddress(request) {
  if (process.env.MIRA_TRUST_PROXY_HEADERS === "true") {
    const forwarded = String(request.headers?.["x-forwarded-for"] ?? "")
      .split(",")
      .map((value) => value.trim())
      .filter(Boolean)
      .at(-1);
    if (forwarded && isIP(forwarded)) return forwarded;
  }
  return request.socket?.remoteAddress ?? null;
}

export async function appendAudit(
  pool,
  {
    action,
    principal = null,
    targetNodeId = null,
    threadId = null,
    requestId = null,
    request = null,
    success = true,
    errorCode = null,
    metadata = {},
  },
) {
  await pool.query(
    `INSERT INTO mira_audit_events (
       action, actor_type, actor_admin_id, actor_node_id, client_type,
       target_node_id, thread_id, request_id, success, error_code,
       request_address, metadata
     ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::jsonb)`,
    [
      action,
      principal?.kind ?? null,
      principal?.kind === "admin" ? principal.subjectId : null,
      principal?.kind === "node" ? principal.nodeId : null,
      principal?.clientType ?? null,
      targetNodeId,
      threadId,
      requestId,
      success,
      errorCode,
      request ? requestAddress(request) : null,
      JSON.stringify(metadata),
    ],
  );
}

function constantTimeHashEqual(left, right) {
  if (typeof left !== "string" || typeof right !== "string") return false;
  const leftBytes = Buffer.from(left, "hex");
  const rightBytes = Buffer.from(right, "hex");
  return (
    leftBytes.length === 32 &&
    rightBytes.length === 32 &&
    crypto.timingSafeEqual(leftBytes, rightBytes)
  );
}

export class AuthService {
  constructor({ pool, secureCookies = true }) {
    this.pool = pool;
    this.secureCookies = secureCookies;
    this.cookieName = secureCookies ? "__Host-mira_session" : "mira_session";
    this.loginFailures = new Map();
  }

  async initialize() {
    await this.pool.query(
      `UPDATE mira_admin_sessions SET revoked_at = NOW()
       WHERE revoked_at IS NULL AND expires_at <= NOW()`,
    );
    const count = await this.pool.query("SELECT COUNT(*)::integer AS count FROM mira_admin_users");
    return { adminConfigured: count.rows[0].count === 1 };
  }

  async authenticate(request, defaultClientType = null) {
    const bearer = bearerToken(request);
    if (bearer) {
      const requestedClientType = String(request.headers["x-mira-client-type"] ?? "");
      const selectedClientType = clientTypes.has(requestedClientType)
        ? requestedClientType
        : defaultClientType;
      return this.authenticateNodeToken(bearer, selectedClientType);
    }
    const sessionToken = parseCookies(request).get(this.cookieName);
    if (!sessionToken) return null;
    const result = await this.pool.query(
      `UPDATE mira_admin_sessions AS sessions SET last_seen_at = NOW()
       FROM mira_admin_users AS users
       WHERE sessions.token_hash = $1
         AND sessions.admin_user_id = users.admin_user_id
         AND sessions.revoked_at IS NULL
         AND sessions.expires_at > NOW()
       RETURNING sessions.session_id, sessions.csrf_token_hash, sessions.expires_at,
                 users.admin_user_id, users.username`,
      [tokenHash(sessionToken)],
    );
    if (result.rowCount === 0) return null;
    const row = result.rows[0];
    return {
      kind: "admin",
      clientType: "admin",
      transport: "cookie",
      subjectId: row.admin_user_id,
      username: row.username,
      sessionId: row.session_id,
      csrfTokenHash: row.csrf_token_hash,
      expiresAt: row.expires_at,
      revoked: false,
    };
  }

  async authenticateNodeToken(token, clientType = null) {
    const parsed = parseNodeToken(token);
    if (!parsed) return null;
    const result = await this.pool.query(
      `SELECT credentials.credential_id, credentials.secret_hash,
              credentials.revoked_at AS credential_revoked_at,
              nodes.node_id, nodes.node_key, nodes.approval_status
       FROM mira_node_credentials AS credentials
       JOIN codex_nodes AS nodes ON nodes.node_id = credentials.node_id
       WHERE credentials.credential_id = $1`,
      [parsed.credentialId],
    );
    if (result.rowCount === 0) {
      const enrollment = await this.pool.query(
        `SELECT enrollment_id, credential_secret_hash, status
         FROM mira_node_enrollment_requests WHERE credential_id = $1`,
        [parsed.credentialId],
      );
      const pending = enrollment.rows[0];
      if (!pending || !constantTimeHashEqual(pending.credential_secret_hash, parsed.secretHash)) return null;
      return {
        kind: "node", clientType: clientTypes.has(clientType) ? clientType : "cli",
        transport: "bearer", subjectId: parsed.credentialId,
        credentialId: parsed.credentialId, nodeId: null, nodeKey: null,
        enrollmentId: pending.enrollment_id, enrollmentStatus: pending.status, revoked: true,
      };
    }
    const row = result.rows[0];
    if (!constantTimeHashEqual(row.secret_hash, parsed.secretHash)) return null;
    const revoked = row.credential_revoked_at !== null || row.approval_status !== "approved";
    if (!revoked) {
      await this.pool.query(
        `WITH used AS (
           UPDATE mira_node_credentials SET last_used_at = NOW() WHERE credential_id = $1
         )
         UPDATE codex_nodes SET last_authenticated_at = NOW() WHERE node_id = $2`,
        [parsed.credentialId, row.node_id],
      );
    }
    return {
      kind: "node",
      clientType: clientTypes.has(clientType) ? clientType : "cli",
      transport: "bearer",
      subjectId: row.credential_id,
      credentialId: row.credential_id,
      nodeId: row.node_id,
      nodeKey: row.node_key,
      revoked,
    };
  }

  permits(principal, actorType) {
    if (!principal || principal.revoked) return false;
    if (actorType === "admin") return principal.kind === "admin";
    if (actorType === "node") return principal.kind === "node";
    if (actorType === "trusted") return principal.kind === "admin" || principal.kind === "node";
    return false;
  }

  validCsrf(request, principal) {
    if (principal?.kind !== "admin" || principal.transport !== "cookie") return true;
    const value = String(request.headers["x-mira-csrf"] ?? "");
    if (!value) return false;
    const actual = Buffer.from(tokenHash(value), "hex");
    const expected = Buffer.from(principal.csrfTokenHash, "hex");
    return actual.length === expected.length && crypto.timingSafeEqual(actual, expected);
  }

  async refreshCsrf(principal) {
    if (principal?.kind !== "admin" || !principal.sessionId) return null;
    const csrfToken = randomToken("mcsrf");
    await this.pool.query(
      `UPDATE mira_admin_sessions SET csrf_token_hash = $2, last_seen_at = NOW()
       WHERE session_id = $1 AND revoked_at IS NULL AND expires_at > NOW()`,
      [principal.sessionId, tokenHash(csrfToken)],
    );
    principal.csrfTokenHash = tokenHash(csrfToken);
    return csrfToken;
  }

  loginFailureKeys(request, username) {
    const address = requestAddress(request) ?? "unknown";
    return [`address:${address}`, `login:${address}\n${String(username).toLowerCase()}`];
  }

  loginDelayMs(request, username) {
    let delay = 0;
    for (const key of this.loginFailureKeys(request, username)) {
      const entry = this.loginFailures.get(key);
      if (!entry || entry.resetAt <= Date.now()) {
        this.loginFailures.delete(key);
        continue;
      }
      delay = Math.max(delay, Math.min(30_000, 250 * 2 ** Math.min(entry.count, 7)));
    }
    return delay;
  }

  recordLoginFailure(request, username) {
    if (this.loginFailures.size > 10_000) this.loginFailures.clear();
    for (const key of this.loginFailureKeys(request, username)) {
      const previous = this.loginFailures.get(key);
      this.loginFailures.set(key, {
        count: previous?.resetAt > Date.now() ? previous.count + 1 : 1,
        resetAt: Date.now() + 15 * 60 * 1000,
      });
    }
  }

  clearLoginFailures(request, username) {
    for (const key of this.loginFailureKeys(request, username)) this.loginFailures.delete(key);
  }

  async login(request, username, password) {
    if (!validUsername(username) || typeof password !== "string") return null;
    const delay = this.loginDelayMs(request, username);
    if (delay > 0) await new Promise((resolve) => setTimeout(resolve, delay));
    const result = await this.pool.query(
      `SELECT admin_user_id, username, password_hash FROM mira_admin_users
       WHERE LOWER(username) = LOWER($1)`,
      [username],
    );
    const row = result.rows[0];
    if (!row || !(await verifyPassword(password, row.password_hash))) {
      this.recordLoginFailure(request, username);
      return null;
    }
    this.clearLoginFailures(request, username);
    const sessionToken = randomToken("mas");
    const csrfToken = randomToken("mcsrf");
    const expiresAt = new Date(Date.now() + sessionLifetimeMs);
    const session = await this.pool.query(
      `INSERT INTO mira_admin_sessions (
         admin_user_id, token_hash, csrf_token_hash, expires_at
       ) VALUES ($1, $2, $3, $4)
       RETURNING session_id`,
      [row.admin_user_id, tokenHash(sessionToken), tokenHash(csrfToken), expiresAt],
    );
    const secure = this.secureCookies ? "; Secure" : "";
    return {
      principal: {
        kind: "admin",
        clientType: "admin",
        transport: "cookie",
        subjectId: row.admin_user_id,
        username: row.username,
        sessionId: session.rows[0].session_id,
        csrfTokenHash: tokenHash(csrfToken),
        expiresAt,
        revoked: false,
      },
      csrfToken,
      cookie: `${this.cookieName}=${sessionToken}; Path=/; HttpOnly; SameSite=Strict${secure}; Max-Age=${Math.floor(sessionLifetimeMs / 1000)}`,
    };
  }

  async logout(principal) {
    if (principal?.kind === "admin" && principal.sessionId) {
      await this.pool.query(
        "UPDATE mira_admin_sessions SET revoked_at = NOW() WHERE session_id = $1",
        [principal.sessionId],
      );
    }
    const secure = this.secureCookies ? "; Secure" : "";
    return `${this.cookieName}=; Path=/; HttpOnly; SameSite=Strict${secure}; Max-Age=0`;
  }
}
