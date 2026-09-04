import { appendAudit } from "./auth.mjs";

function object(value, fallback = {}) {
  return value !== null && typeof value === "object" && !Array.isArray(value) ? value : fallback;
}

function requiredString(name, value, maximum = 256) {
  if (typeof value !== "string" || value.length === 0 || value.length > maximum) {
    throw new Error(`${name} must be a non-empty string of at most ${maximum} characters`);
  }
  return value;
}

function optionalAbsolutePath(name, value) {
  if (value === undefined) return undefined;
  if (value === null || value === "") return null;
  if (typeof value !== "string" || value.length > 4_096 || /[\u0000-\u001f\u007f]/.test(value)) {
    throw new Error(`${name} must be null or an absolute path of at most 4096 characters`);
  }
  const normalized = value.trim();
  if (!/^(?:\/|[a-zA-Z]:[\\/]|\\\\)/.test(normalized)) {
    throw new Error(`${name} must be an absolute Unix or Windows path`);
  }
  return normalized;
}

function nativeAbsolutePath(value, platform) {
  return platform === "windows"
    ? /^(?:[a-zA-Z]:[\\/]|\\\\)/.test(value)
    : value.startsWith("/");
}

function nodeView(row) {
  return {
    nodeId: row.node_id,
    nodeKey: row.node_key,
    hostname: row.hostname,
    platform: row.platform,
    architecture: row.architecture,
    nodeMode: row.node_mode,
    nodeVersion: row.node_version,
    nodeBuild: row.node_build,
    capabilities: row.capabilities,
    codexInstallations: row.codex_installations,
    desiredAppServer: row.desired_app_server,
    reportedAppServer: row.reported_app_server,
    machineStatus: row.machine_status,
    channelStatus: row.channel_status,
    approvalStatus: row.approval_status,
    approvedAt: row.approved_at?.toISOString() ?? null,
    revokedAt: row.revoked_at?.toISOString() ?? null,
    registeredAt: row.registered_at.toISOString(),
    lastSeenAt: row.last_seen_at.toISOString(),
    status: row.approval_status === "revoked" ? "revoked" : row.status,
  };
}

const selectNodeColumns = `
  node_id, node_key, hostname, platform, architecture, node_mode,
  node_version, node_build, capabilities, codex_installations, desired_app_server,
  reported_app_server, machine_status, channel_status, approval_status,
  approved_at, revoked_at, registered_at, last_seen_at,
  CASE WHEN (channel_status->>'connected')::boolean IS TRUE
             AND last_seen_at > NOW() - INTERVAL '15 seconds'
       THEN 'online' ELSE 'offline' END AS status`;

export async function registerNode(pool, nodeId, body) {
  let nodeVersion;
  try {
    requiredString("nodeKey", body.nodeKey);
    requiredString("hostname", body.hostname);
    requiredString("platform", body.platform, 64);
    requiredString("architecture", body.architecture, 64);
    requiredString("nodeMode", body.nodeMode, 64);
    nodeVersion = requiredString("nodeVersion", body.nodeVersion ?? body.agentVersion, 64);
  } catch (error) {
    return { status: 400, body: { error: error.message, code: "invalid_request" } };
  }
  const capabilities = object(body.capabilities);
  const nodeBuild = object(body.nodeBuild);
  if (nodeBuild.version !== undefined && nodeBuild.version !== nodeVersion) {
    return { status: 400, body: { error: "nodeBuild.version must match nodeVersion", code: "invalid_request" } };
  }
  const codexInstallations = Array.isArray(body.codexInstallations) ? body.codexInstallations : [];
  const result = await pool.query(
    `UPDATE codex_nodes SET
       hostname = $3, platform = $4, architecture = $5, node_mode = $6,
       node_version = $7, node_build = $8::jsonb, capabilities = $9::jsonb,
       codex_installations = $10::jsonb, last_seen_at = NOW(), updated_at = NOW()
     WHERE node_id = $1 AND node_key = $2 AND approval_status = 'approved'
     RETURNING node_id, desired_app_server, registered_at, last_seen_at`,
    [nodeId, body.nodeKey, body.hostname, body.platform, body.architecture, body.nodeMode,
      nodeVersion, JSON.stringify(nodeBuild), JSON.stringify(capabilities), JSON.stringify(codexInstallations)],
  );
  if (result.rowCount === 0) {
    return { status: 403, body: { error: "node identity is revoked or does not match", code: "node_forbidden" } };
  }
  const row = result.rows[0];
  return { status: 200, body: {
    nodeId: row.node_id, desiredAppServer: row.desired_app_server,
    heartbeatIntervalSeconds: 3, registeredAt: row.registered_at.toISOString(),
    lastSeenAt: row.last_seen_at.toISOString(),
  } };
}

export async function heartbeatNode(pool, nodeId, body) {
  const reported = object(body.reportedAppServer, { status: "unknown" });
  const result = await pool.query(
    `UPDATE codex_nodes SET
       reported_app_server = $2::jsonb,
       codex_installations = COALESCE($3::jsonb, codex_installations),
       capabilities = COALESCE($4::jsonb, capabilities),
       machine_status = COALESCE($5::jsonb, machine_status),
       last_seen_at = NOW(), updated_at = NOW()
     WHERE node_id = $1 AND approval_status = 'approved'
     RETURNING desired_app_server, last_seen_at`,
    [nodeId, JSON.stringify(reported),
      body.codexInstallations === undefined ? null : JSON.stringify(body.codexInstallations),
      body.capabilities === undefined ? null : JSON.stringify(object(body.capabilities)),
      body.machineStatus === undefined ? null : JSON.stringify(object(body.machineStatus))],
  );
  if (result.rowCount === 0) {
    return { status: 403, body: { error: "node is revoked", code: "node_forbidden" } };
  }
  return { status: 200, body: {
    desiredAppServer: result.rows[0].desired_app_server,
    serverTime: result.rows[0].last_seen_at.toISOString(),
  } };
}

export async function listNodes(pool, { includeRevoked = false } = {}) {
  const result = await pool.query(
    `SELECT ${selectNodeColumns} FROM codex_nodes
     WHERE ($1::boolean OR approval_status = 'approved') ORDER BY hostname, node_key`,
    [includeRevoked],
  );
  return result.rows.map(nodeView);
}

export async function getNode(pool, nodeId, { includeRevoked = false } = {}) {
  const result = await pool.query(
    `SELECT ${selectNodeColumns} FROM codex_nodes
     WHERE node_id = $1 AND ($2::boolean OR approval_status = 'approved')`,
    [nodeId, includeRevoked],
  );
  return result.rowCount === 0 ? null : nodeView(result.rows[0]);
}

export async function setNodeChannelStatus(pool, nodeId, status) {
  await pool.query(
    `UPDATE codex_nodes SET channel_status = $2::jsonb, updated_at = NOW() WHERE node_id = $1`,
    [nodeId, JSON.stringify(status)],
  );
}

export async function setDesiredAppServer(pool, nodeId, body) {
  if (typeof body.running !== "boolean") {
    return { status: 400, body: { error: "running must be a boolean", code: "invalid_request" } };
  }
  const configOverrides = body.configOverrides ?? [];
  if (!Array.isArray(configOverrides) || configOverrides.length > 20 ||
      configOverrides.some((value) => typeof value !== "string" || value.length === 0 || value.length > 2_048) ||
      configOverrides.some((value) => /(?:bearer_token|access_token|password|secret|api_key)\s*=/i.test(value))) {
    return { status: 400, body: {
      error: "configOverrides must be non-secret and contain at most 20 strings", code: "invalid_request",
    } };
  }
  let defaultCwd;
  try {
    defaultCwd = optionalAbsolutePath("defaultCwd", body.defaultCwd);
  } catch (error) {
    return { status: 400, body: { error: error.message, code: "invalid_request" } };
  }
  if (defaultCwd !== undefined && defaultCwd !== null) {
    const target = await pool.query(
      `SELECT platform FROM codex_nodes WHERE node_id = $1 AND approval_status = 'approved'`,
      [nodeId],
    );
    if (target.rowCount === 0) {
      return { status: 404, body: { error: "approved node not found", code: "not_found" } };
    }
    if (!nativeAbsolutePath(defaultCwd, target.rows[0].platform)) {
      return { status: 400, body: {
        error: `defaultCwd must be an absolute ${target.rows[0].platform} path`, code: "invalid_request",
      } };
    }
  }
  const desired = { running: body.running, revision: Date.now() };
  for (const [field, value] of [
    ["listenUrl", body.listenUrl], ["codexPath", body.codexPath], ["codexHome", body.codexHome],
  ]) {
    if (value !== undefined) desired[field] = value ?? null;
  }
  if (body.configOverrides !== undefined) desired.configOverrides = configOverrides;
  if (defaultCwd !== undefined) desired.defaultCwd = defaultCwd;
  const result = await pool.query(
    `UPDATE codex_nodes SET desired_app_server = desired_app_server || $2::jsonb, updated_at = NOW()
     WHERE node_id = $1 AND approval_status = 'approved' RETURNING desired_app_server`,
    [nodeId, JSON.stringify(desired)],
  );
  if (result.rowCount === 0) {
    return { status: 404, body: { error: "approved node not found", code: "not_found" } };
  }
  return { status: 200, body: { nodeId, desiredAppServer: result.rows[0].desired_app_server } };
}

export async function revokeNode(pool, request, principal, nodeId, reason = null) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const result = await client.query(
      `UPDATE codex_nodes SET approval_status = 'revoked', revoked_at = NOW(),
         desired_app_server = jsonb_set(desired_app_server, '{running}', 'false'::jsonb),
         channel_status = '{"connected":false,"reason":"revoked"}'::jsonb, updated_at = NOW()
       WHERE node_id = $1 AND approval_status = 'approved' RETURNING node_key`,
      [nodeId],
    );
    if (result.rowCount === 0) {
      await client.query("ROLLBACK");
      return { status: 404, body: { error: "approved node not found", code: "not_found" } };
    }
    await client.query(
      `UPDATE mira_node_credentials SET revoked_at = NOW() WHERE node_id = $1 AND revoked_at IS NULL`,
      [nodeId],
    );
    await appendAudit(client, {
      action: "node.revoked", principal, targetNodeId: nodeId, request,
      metadata: { nodeKey: result.rows[0].node_key, hasReason: reason !== null },
    });
    await client.query("COMMIT");
    return { status: 200, body: { nodeId, status: "revoked" } };
  } catch (error) {
    await client.query("ROLLBACK");
    throw error;
  } finally {
    client.release();
  }
}
