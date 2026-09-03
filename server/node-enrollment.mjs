import crypto from "node:crypto";

import { appendAudit, parseNodeToken, requestAddress } from "./auth.mjs";

const enrollmentLifetimeMinutes = 15;

function requiredString(name, value, maximum = 256) {
  if (typeof value !== "string" || value.length === 0 || value.length > maximum) {
    throw new Error(`${name} must be a non-empty string of at most ${maximum} characters`);
  }
  return value;
}

function normalizeDescriptor(body) {
  const nodeVersion = body.nodeVersion ?? body.agentVersion;
  const descriptor = {
    nodeKey: requiredString("nodeKey", body.nodeKey),
    hostname: requiredString("hostname", body.hostname),
    platform: requiredString("platform", body.platform, 64),
    architecture: requiredString("architecture", body.architecture, 64),
    nodeMode: requiredString("nodeMode", body.nodeMode, 64),
    nodeVersion: requiredString("nodeVersion", nodeVersion, 64),
    nodeBuild: body.nodeBuild ?? {},
    capabilities: body.capabilities ?? {},
    codexInstallations: Array.isArray(body.codexInstallations) ? body.codexInstallations : [],
    defaultDesiredAppServer: body.defaultDesiredAppServer ?? { running: false },
    machineStatus: body.machineStatus ?? {},
  };
  for (const [name, value] of [
    ["capabilities", descriptor.capabilities],
    ["defaultDesiredAppServer", descriptor.defaultDesiredAppServer],
    ["machineStatus", descriptor.machineStatus],
    ["nodeBuild", descriptor.nodeBuild],
  ]) {
    if (value === null || typeof value !== "object" || Array.isArray(value)) {
      throw new Error(`${name} must be an object`);
    }
  }
  if (descriptor.nodeBuild.version !== undefined && descriptor.nodeBuild.version !== descriptor.nodeVersion) {
    throw new Error("nodeBuild.version must match nodeVersion");
  }
  return descriptor;
}

function validCredentialId(value) {
  return typeof value === "string" &&
    /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(value);
}

function validSecretHash(value) {
  return typeof value === "string" && /^[0-9a-f]{64}$/i.test(value);
}

function fingerprint(secretHash) {
  return secretHash
    .slice(0, 16)
    .match(/.{1,4}/g)
    .join("-");
}

function constantTimeHashEqual(left, right) {
  const leftBytes = Buffer.from(left, "hex");
  const rightBytes = Buffer.from(right, "hex");
  return (
    leftBytes.length === 32 &&
    rightBytes.length === 32 &&
    crypto.timingSafeEqual(leftBytes, rightBytes)
  );
}

function enrollmentView(row, includeAdminFields = false) {
  return {
    enrollmentId: row.enrollment_id,
    nodeId: row.node_id ?? null,
    credentialId: row.credential_id,
    credentialFingerprint: row.credential_fingerprint,
    verificationCode: row.verification_code,
    nodeKey: row.node_key,
    hostname: row.hostname,
    platform: row.platform,
    architecture: row.architecture,
    nodeMode: row.node_mode,
    nodeVersion: row.node_version,
    nodeBuild: row.node_build,
    capabilities: row.capabilities,
    codexInstallations: row.codex_installations,
    machineStatus: row.machine_status,
    status: row.status,
    decisionNote: row.decision_note,
    requestedAt: row.requested_at.toISOString(),
    expiresAt: row.expires_at.toISOString(),
    approvedAt: row.approved_at?.toISOString() ?? null,
    rejectedAt: row.rejected_at?.toISOString() ?? null,
    ...(includeAdminFields ? { requestedFrom: row.requested_from } : {}),
  };
}

function requestNodeToken(request) {
  const raw = String(request.headers.authorization ?? "").match(/^Bearer ([^\s]+)$/)?.[1] ?? null;
  return parseNodeToken(raw);
}

async function expireRequests(client, enrollmentId = null) {
  await client.query(
    `UPDATE mira_node_enrollment_requests SET status = 'expired'
     WHERE status = 'pending' AND expires_at <= NOW()
       AND ($1::uuid IS NULL OR enrollment_id = $1)`,
    [enrollmentId],
  );
}

async function authenticatedEnrollment(pool, request, enrollmentId) {
  const token = requestNodeToken(request);
  if (!token) return null;
  await expireRequests(pool, enrollmentId);
  const result = await pool.query(
    `SELECT * FROM mira_node_enrollment_requests
     WHERE enrollment_id = $1 AND credential_id = $2`,
    [enrollmentId, token.credentialId],
  );
  const row = result.rows[0];
  return row && constantTimeHashEqual(row.credential_secret_hash, token.secretHash) ? row : null;
}

export async function createEnrollment(pool, request, body) {
  let descriptor;
  try {
    descriptor = normalizeDescriptor(body);
    if (!validCredentialId(body.credentialId)) throw new Error("credentialId must be a UUID");
    if (!validSecretHash(body.credentialSecretHash)) {
      throw new Error("credentialSecretHash must be a SHA-256 hex digest");
    }
  } catch (error) {
    return { status: 400, body: { error: error.message } };
  }

  const credentialId = body.credentialId.toLowerCase();
  const secretHash = body.credentialSecretHash.toLowerCase();
  const prior = await pool.query(
    `SELECT * FROM mira_node_enrollment_requests
     WHERE credential_id = $1 AND credential_secret_hash = $2`,
    [credentialId, secretHash],
  );
  if (prior.rowCount > 0) {
    return { status: 202, body: enrollmentView(prior.rows[0]) };
  }

  const existingCredential = await pool.query(
    "SELECT credential_id FROM mira_node_credentials WHERE credential_id = $1",
    [credentialId],
  );
  if (existingCredential.rowCount > 0) {
    return { status: 409, body: { error: "credential is already enrolled", code: "enrollment_conflict" } };
  }
  const activeNode = await pool.query(
    `SELECT node_id FROM codex_nodes WHERE node_key = $1 AND approval_status = 'approved'`,
    [descriptor.nodeKey],
  );
  if (activeNode.rowCount > 0) {
    return { status: 409, body: { error: "an approved node already uses this node key", code: "enrollment_conflict" } };
  }

  const address = requestAddress(request);
  const recent = await pool.query(
    `SELECT COUNT(*)::integer AS count FROM mira_node_enrollment_requests
     WHERE requested_from IS NOT DISTINCT FROM $1
       AND requested_at > NOW() - INTERVAL '1 hour'
       AND status = 'pending'`,
    [address],
  );
  if (recent.rows[0].count >= 50) {
    return { status: 429, body: { error: "too many pending enrollment requests" } };
  }
  const verificationCode = crypto.randomInt(0, 1_000_000).toString().padStart(6, "0");
  let result;
  try {
    result = await pool.query(
      `INSERT INTO mira_node_enrollment_requests (
         credential_id, credential_secret_hash, credential_fingerprint,
         node_key, verification_code, hostname, platform, architecture,
         node_mode, node_version, node_build, capabilities, codex_installations,
         default_desired_app_server, machine_status, requested_from, expires_at
       ) VALUES (
         $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
         $11::jsonb, $12::jsonb, $13::jsonb, $14::jsonb, $15::jsonb, $16,
         NOW() + make_interval(mins => $17)
       ) RETURNING *`,
      [
        credentialId,
        secretHash,
        fingerprint(secretHash),
        descriptor.nodeKey,
        verificationCode,
        descriptor.hostname,
        descriptor.platform,
        descriptor.architecture,
        descriptor.nodeMode,
        descriptor.nodeVersion,
        JSON.stringify(descriptor.nodeBuild),
        JSON.stringify(descriptor.capabilities),
        JSON.stringify(descriptor.codexInstallations),
        JSON.stringify(descriptor.defaultDesiredAppServer),
        JSON.stringify(descriptor.machineStatus),
        address,
        enrollmentLifetimeMinutes,
      ],
    );
  } catch (error) {
    if (error.code === "23505") return { status: 409, body: { error: "credential already requested" } };
    throw error;
  }
  const row = result.rows[0];
  await appendAudit(pool, {
    action: "node.enrollment.requested",
    request,
    metadata: {
      enrollmentId: row.enrollment_id,
      nodeKey: descriptor.nodeKey,
      credentialFingerprint: row.credential_fingerprint,
    },
  });
  return { status: 202, body: enrollmentView(row) };
}

export async function getEnrollment(pool, request, enrollmentId) {
  const row = await authenticatedEnrollment(pool, request, enrollmentId);
  if (!row) return { status: 401, body: { error: "invalid enrollment credential" } };
  return { status: 200, body: enrollmentView(row) };
}

export async function listEnrollments(pool, status = null) {
  await expireRequests(pool);
  const result = await pool.query(
    `SELECT * FROM mira_node_enrollment_requests
     ${status ? "WHERE status = $1" : ""}
     ORDER BY requested_at DESC LIMIT 500`,
    status ? [status] : [],
  );
  return result.rows.map((row) => enrollmentView(row, true));
}

export async function approveEnrollment(pool, request, principal, enrollmentId, body) {
  const note = typeof body.note === "string" ? body.note.slice(0, 2_000) : null;
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await expireRequests(client, enrollmentId);
    const requestResult = await client.query(
      `SELECT * FROM mira_node_enrollment_requests WHERE enrollment_id = $1 FOR UPDATE`,
      [enrollmentId],
    );
    if (requestResult.rowCount === 0) {
      await client.query("ROLLBACK");
      return { status: 404, body: { error: "enrollment not found" } };
    }
    const row = requestResult.rows[0];
    if (row.status !== "pending") {
      await client.query("ROLLBACK");
      return { status: 409, body: { error: `enrollment is ${row.status}` } };
    }
    const existing = await client.query(
      "SELECT node_id, approval_status FROM codex_nodes WHERE node_key = $1 FOR UPDATE",
      [row.node_key],
    );
    let nodeId;
    if (existing.rowCount > 0) {
      if (existing.rows[0].approval_status !== "revoked") {
        await client.query("ROLLBACK");
        return { status: 409, body: { error: "an approved node already uses this node key" } };
      }
      nodeId = existing.rows[0].node_id;
      await client.query(
        `UPDATE codex_nodes SET
           enrollment_id = $2, hostname = $3, platform = $4, architecture = $5,
           node_mode = $6, node_version = $7, node_build = $8::jsonb, capabilities = $9::jsonb,
           codex_installations = $10::jsonb, machine_status = $11::jsonb,
           desired_app_server = $12::jsonb, approval_status = 'approved',
           approved_at = NOW(), revoked_at = NULL, updated_at = NOW()
         WHERE node_id = $1`,
        [
          nodeId,
          enrollmentId,
          row.hostname,
          row.platform,
          row.architecture,
          row.node_mode,
          row.node_version,
          JSON.stringify(row.node_build),
          JSON.stringify(row.capabilities),
          JSON.stringify(row.codex_installations),
          JSON.stringify(row.machine_status),
          JSON.stringify(row.default_desired_app_server),
        ],
      );
      await client.query(
        `UPDATE mira_node_credentials SET revoked_at = NOW()
         WHERE node_id = $1 AND revoked_at IS NULL`,
        [nodeId],
      );
    } else {
      const inserted = await client.query(
        `INSERT INTO codex_nodes (
           enrollment_id, node_key, hostname, platform, architecture, node_mode,
           node_version, node_build, capabilities, codex_installations, machine_status,
           desired_app_server, approval_status, approved_at
         ) VALUES (
           $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb,
           $11::jsonb, $12::jsonb, 'approved', NOW()
         ) RETURNING node_id`,
        [
          enrollmentId,
          row.node_key,
          row.hostname,
          row.platform,
          row.architecture,
          row.node_mode,
          row.node_version,
          JSON.stringify(row.node_build),
          JSON.stringify(row.capabilities),
          JSON.stringify(row.codex_installations),
          JSON.stringify(row.machine_status),
          JSON.stringify(row.default_desired_app_server),
        ],
      );
      nodeId = inserted.rows[0].node_id;
    }
    await client.query(
      `INSERT INTO mira_node_credentials (
         credential_id, node_id, enrollment_id, secret_hash
       ) VALUES ($1, $2, $3, $4)`,
      [row.credential_id, nodeId, enrollmentId, row.credential_secret_hash],
    );
    const approved = await client.query(
      `UPDATE mira_node_enrollment_requests SET
         status = 'approved', decision_note = $2, approved_by = $3,
         approved_at = NOW(), node_id = $4
       WHERE enrollment_id = $1 RETURNING *`,
      [enrollmentId, note, principal.subjectId, nodeId],
    );
    await appendAudit(client, {
      action: "node.enrollment.approved",
      principal,
      targetNodeId: nodeId,
      request,
      metadata: {
        enrollmentId,
        nodeKey: row.node_key,
        credentialFingerprint: row.credential_fingerprint,
      },
    });
    await client.query("COMMIT");
    return { status: 200, body: enrollmentView(approved.rows[0], true) };
  } catch (error) {
    await client.query("ROLLBACK");
    if (error.code === "23505") return { status: 409, body: { error: "credential already approved" } };
    throw error;
  } finally {
    client.release();
  }
}

export async function rejectEnrollment(pool, request, principal, enrollmentId, body) {
  const note = typeof body.note === "string" ? body.note.slice(0, 2_000) : null;
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const result = await client.query(
      `UPDATE mira_node_enrollment_requests SET
         status = 'rejected', decision_note = $2, rejected_at = NOW(), approved_by = $3
       WHERE enrollment_id = $1 AND status = 'pending' AND expires_at > NOW()
       RETURNING *`,
      [enrollmentId, note, principal.subjectId],
    );
    if (result.rowCount === 0) {
      await client.query("ROLLBACK");
      return { status: 409, body: { error: "enrollment is missing, expired, or already decided" } };
    }
    await appendAudit(client, {
      action: "node.enrollment.rejected",
      principal,
      request,
      metadata: { enrollmentId, nodeKey: result.rows[0].node_key, hasDecisionNote: note !== null },
    });
    await client.query("COMMIT");
    return { status: 200, body: enrollmentView(result.rows[0], true) };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}
