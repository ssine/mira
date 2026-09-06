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

export function normalizeNodeAliasKey(value) {
  return String(value).normalize("NFKC").toLocaleLowerCase("en-US");
}

function normalizedNodeMetadata(body) {
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    throw new Error("metadata must be an object");
  }
  if (!Number.isSafeInteger(body.expectedRevision) || body.expectedRevision < 0) {
    throw new Error("expectedRevision must be a non-negative integer");
  }
  let displayName = body.displayName ?? null;
  if (displayName !== null) {
    if (typeof displayName !== "string") throw new Error("displayName must be a string or null");
    displayName = displayName.trim().normalize("NFKC");
    if (displayName.length === 0) displayName = null;
    if (displayName !== null && ([...displayName].length > 80 || /[\u0000-\u001f\u007f-\u009f]/u.test(displayName))) {
      throw new Error("displayName must contain at most 80 characters and no control characters");
    }
  }
  if (!Array.isArray(body.aliases) || body.aliases.length > 8) {
    throw new Error("aliases must be an array containing at most 8 values");
  }
  const aliases = [];
  const aliasKeys = new Set();
  for (const raw of body.aliases) {
    if (typeof raw !== "string") throw new Error("each alias must be a string");
    const alias = raw.trim().normalize("NFKC");
    if ([...alias].length > 48 || !/^[\p{L}\p{N}](?:[\p{L}\p{N}._-]*[\p{L}\p{N}])?$/u.test(alias)) {
      throw new Error("aliases must be 1-48 letters, numbers, dots, underscores or hyphens and must start and end with a letter or number");
    }
    const aliasKey = normalizeNodeAliasKey(alias);
    if (aliasKeys.has(aliasKey)) throw new Error(`duplicate alias: ${alias}`);
    aliasKeys.add(aliasKey);
    aliases.push({ alias, aliasKey });
  }
  if (body.labels === null || typeof body.labels !== "object" || Array.isArray(body.labels) || Object.keys(body.labels).length > 32) {
    throw new Error("labels must be an object containing at most 32 values");
  }
  const labels = {};
  for (const [rawKey, rawValue] of Object.entries(body.labels)) {
    const key = rawKey.trim().toLocaleLowerCase("en-US");
    if (!/^[a-z][a-z0-9._-]{0,31}$/.test(key) || typeof rawValue !== "string") {
      throw new Error("label keys must use 1-32 lowercase letters, numbers, dots, underscores or hyphens and values must be strings");
    }
    const value = rawValue.trim().normalize("NFKC");
    if ([...value].length === 0 || [...value].length > 64 || /[\u0000-\u001f\u007f-\u009f]/u.test(value)) {
      throw new Error(`label ${key} must contain 1-64 characters and no control characters`);
    }
    if (Object.hasOwn(labels, key)) throw new Error(`duplicate label key: ${key}`);
    labels[key] = value;
  }
  aliases.sort((left, right) => left.aliasKey.localeCompare(right.aliasKey, "en-US"));
  return { displayName, aliases, labels, expectedRevision: body.expectedRevision };
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
    displayName: row.display_name,
    aliases: row.aliases ?? [],
    labels: row.labels ?? {},
    metadataRevision: Number(row.metadata_revision ?? 0),
    approvalStatus: row.approval_status,
    approvedAt: row.approved_at?.toISOString() ?? null,
    revokedAt: row.revoked_at?.toISOString() ?? null,
    registeredAt: row.registered_at.toISOString(),
    lastSeenAt: row.last_seen_at.toISOString(),
    status: row.approval_status === "revoked" ? "revoked" : row.status,
  };
}

const selectNodeColumns = `
  nodes.node_id, nodes.node_key, nodes.hostname, nodes.platform, nodes.architecture, nodes.node_mode,
  nodes.node_version, nodes.node_build, nodes.capabilities, nodes.codex_installations, nodes.desired_app_server,
  nodes.reported_app_server, nodes.machine_status, nodes.channel_status, nodes.approval_status,
  nodes.display_name, nodes.labels, nodes.metadata_revision,
  COALESCE((SELECT jsonb_agg(alias.alias ORDER BY alias.alias_key)
            FROM mira_node_aliases alias WHERE alias.node_id = nodes.node_id), '[]'::jsonb) AS aliases,
  nodes.approved_at, nodes.revoked_at, nodes.registered_at, nodes.last_seen_at,
  CASE WHEN (nodes.channel_status->>'connected')::boolean IS TRUE
             AND nodes.last_seen_at > NOW() - INTERVAL '15 seconds'
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
    `SELECT ${selectNodeColumns} FROM codex_nodes nodes
     WHERE ($1::boolean OR nodes.approval_status = 'approved') ORDER BY nodes.hostname, nodes.node_key`,
    [includeRevoked],
  );
  return result.rows.map(nodeView);
}

export async function getNode(pool, nodeId, { includeRevoked = false } = {}) {
  const result = await pool.query(
    `SELECT ${selectNodeColumns} FROM codex_nodes nodes
     WHERE nodes.node_id = $1 AND ($2::boolean OR nodes.approval_status = 'approved')`,
    [nodeId, includeRevoked],
  );
  return result.rowCount === 0 ? null : nodeView(result.rows[0]);
}

export function nodeSummary(node) {
  const capabilities = Object.fromEntries(
    Object.entries(node.capabilities ?? {}).filter(([, enabled]) => enabled === true),
  );
  return {
    nodeId: node.nodeId, nodeKey: node.nodeKey, displayName: node.displayName,
    aliases: node.aliases ?? [], labels: node.labels ?? {}, hostname: node.hostname,
    platform: node.platform, architecture: node.architecture, nodeMode: node.nodeMode,
    status: node.status, capabilities,
    appServerStatus: node.capabilities?.appServer === true
      ? (node.reportedAppServer?.status ?? "unknown") : "unsupported",
    lastSeenAt: node.lastSeenAt,
  };
}

export async function resolveNode(pool, selector, { includeRevoked = false } = {}) {
  if (typeof selector !== "string" || selector.length === 0 || selector.length > 256 || /[\u0000-\u001f\u007f]/u.test(selector)) {
    return { status: 400, body: { error: "invalid Node selector", code: "invalid_selector" } };
  }
  const aliasKey = normalizeNodeAliasKey(selector);
  const result = await pool.query(
    `SELECT ${selectNodeColumns},
       CASE
         WHEN nodes.node_id::text = LOWER($1) THEN 1
         WHEN nodes.node_key = $1 THEN 2
         WHEN selected_alias.alias_key IS NOT NULL THEN 3
         ELSE 4
       END AS selector_rank
     FROM codex_nodes nodes
     LEFT JOIN mira_node_aliases selected_alias
       ON selected_alias.node_id = nodes.node_id AND selected_alias.alias_key = $2
     WHERE ($3::boolean OR nodes.approval_status = 'approved')
       AND (nodes.node_id::text = LOWER($1) OR nodes.node_key = $1
            OR selected_alias.alias_key IS NOT NULL OR nodes.hostname = $1)
     ORDER BY selector_rank, nodes.hostname, nodes.node_key`,
    [selector, aliasKey, includeRevoked],
  );
  if (result.rowCount === 0) {
    return { status: 404, body: { error: `no Node matches selector ${selector}`, code: "not_found" } };
  }
  const rank = Number(result.rows[0].selector_rank);
  const matches = result.rows.filter((row) => Number(row.selector_rank) === rank);
  if (matches.length !== 1) {
    return { status: 409, body: { error: `Node selector is ambiguous: ${selector}`, code: "ambiguous_selector" } };
  }
  const matchedBy = [null, "nodeId", "nodeKey", "alias", "hostname"][rank];
  return { status: 200, body: { node: nodeView(matches[0]), matchedBy } };
}

export async function setNodeMetadata(pool, request, principal, nodeId, body) {
  let metadata;
  try {
    metadata = normalizedNodeMetadata(body);
  } catch (error) {
    return { status: 400, body: { error: error.message, code: "invalid_request" } };
  }
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    await client.query("SELECT pg_advisory_xact_lock(hashtext('mira_node_selector_namespace'))");
    const target = await client.query(
      `SELECT node_key, metadata_revision FROM codex_nodes WHERE node_id = $1 FOR UPDATE`, [nodeId],
    );
    if (target.rowCount === 0) {
      await client.query("ROLLBACK");
      return { status: 404, body: { error: "Node not found", code: "not_found" } };
    }
    const currentRevision = Number(target.rows[0].metadata_revision);
    if (currentRevision !== metadata.expectedRevision) {
      await client.query("ROLLBACK");
      return { status: 409, body: {
        error: "Node metadata changed; reload it before saving", code: "metadata_conflict", currentRevision,
      } };
    }
    const nodeKeys = await client.query("SELECT node_key FROM codex_nodes");
    const reserved = new Set(nodeKeys.rows.map((row) => normalizeNodeAliasKey(row.node_key)));
    const conflict = metadata.aliases.find(({ aliasKey }) => reserved.has(aliasKey));
    if (conflict) {
      await client.query("ROLLBACK");
      return { status: 409, body: {
        error: `alias conflicts with a Node key: ${conflict.alias}`, code: "alias_conflict",
      } };
    }
    await client.query("DELETE FROM mira_node_aliases WHERE node_id = $1", [nodeId]);
    for (const { alias, aliasKey } of metadata.aliases) {
      await client.query(
        `INSERT INTO mira_node_aliases(alias_key, alias, node_id) VALUES ($1, $2, $3)`,
        [aliasKey, alias, nodeId],
      );
    }
    const updated = await client.query(
      `UPDATE codex_nodes SET display_name = $2, labels = $3::jsonb,
         metadata_revision = metadata_revision + 1, updated_at = NOW()
       WHERE node_id = $1 RETURNING metadata_revision`,
      [nodeId, metadata.displayName, JSON.stringify(metadata.labels)],
    );
    const revision = Number(updated.rows[0].metadata_revision);
    await appendAudit(client, {
      action: "node.metadata.updated", principal, targetNodeId: nodeId, request,
      metadata: {
        aliasCount: metadata.aliases.length, labelCount: Object.keys(metadata.labels).length,
        hasDisplayName: metadata.displayName !== null, revision,
      },
    });
    await client.query("COMMIT");
    return { status: 200, body: {
      nodeId, displayName: metadata.displayName, aliases: metadata.aliases.map(({ alias }) => alias),
      labels: metadata.labels, metadataRevision: revision,
    } };
  } catch (error) {
    await client.query("ROLLBACK");
    if (error.code === "23505") {
      return { status: 409, body: { error: "alias is already assigned to another Node", code: "alias_conflict" } };
    }
    throw error;
  } finally {
    client.release();
  }
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
  let developerInstructionsFile;
  try {
    defaultCwd = optionalAbsolutePath("defaultCwd", body.defaultCwd);
    developerInstructionsFile = optionalAbsolutePath("developerInstructionsFile", body.developerInstructionsFile);
  } catch (error) {
    return { status: 400, body: { error: error.message, code: "invalid_request" } };
  }
  if ([defaultCwd, developerInstructionsFile].some((value) => value !== undefined && value !== null)) {
    const target = await pool.query(
      `SELECT platform FROM codex_nodes WHERE node_id = $1 AND approval_status = 'approved'`,
      [nodeId],
    );
    if (target.rowCount === 0) {
      return { status: 404, body: { error: "approved node not found", code: "not_found" } };
    }
    for (const [name, value] of [["defaultCwd", defaultCwd], ["developerInstructionsFile", developerInstructionsFile]]) {
      if (value !== undefined && value !== null && !nativeAbsolutePath(value, target.rows[0].platform)) {
        return { status: 400, body: {
          error: `${name} must be an absolute ${target.rows[0].platform} path`, code: "invalid_request",
        } };
      }
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
  if (developerInstructionsFile !== undefined) desired.developerInstructionsFile = developerInstructionsFile;
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
