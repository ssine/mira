export async function registerNode(pool, body) {
  const requiredStrings = [
    ["nodeKey", body.nodeKey],
    ["hostname", body.hostname],
    ["platform", body.platform],
    ["architecture", body.architecture],
    ["nodeMode", body.nodeMode],
    ["agentVersion", body.agentVersion],
  ];
  for (const [name, value] of requiredStrings) {
    if (typeof value !== "string" || value.length === 0 || value.length > 256) {
      return { status: 400, body: { error: `${name} must be a non-empty string` } };
    }
  }
  const capabilities = body.capabilities ?? {};
  const codexInstallations = Array.isArray(body.codexInstallations) ? body.codexInstallations : [];
  const defaultDesired = body.defaultDesiredAppServer ?? { running: true };
  const result = await pool.query(
    `INSERT INTO codex_nodes (
       node_key, hostname, platform, architecture, node_mode, agent_version,
       capabilities, codex_installations, desired_app_server
     ) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb)
     ON CONFLICT (node_key) DO UPDATE SET
       hostname = EXCLUDED.hostname,
       platform = EXCLUDED.platform,
       architecture = EXCLUDED.architecture,
       node_mode = EXCLUDED.node_mode,
       agent_version = EXCLUDED.agent_version,
       capabilities = EXCLUDED.capabilities,
       codex_installations = EXCLUDED.codex_installations,
       last_seen_at = NOW(),
       updated_at = NOW()
     RETURNING node_id, desired_app_server, registered_at, last_seen_at`,
    [
      body.nodeKey,
      body.hostname,
      body.platform,
      body.architecture,
      body.nodeMode,
      body.agentVersion,
      JSON.stringify(capabilities),
      JSON.stringify(codexInstallations),
      JSON.stringify(defaultDesired),
    ],
  );
  const row = result.rows[0];
  return {
    status: 200,
    body: {
      nodeId: row.node_id,
      desiredAppServer: row.desired_app_server,
      heartbeatIntervalSeconds: 3,
      registeredAt: row.registered_at.toISOString(),
      lastSeenAt: row.last_seen_at.toISOString(),
    },
  };
}

export async function heartbeatNode(pool, nodeId, body) {
  const reported = body.reportedAppServer ?? { status: "unknown" };
  const result = await pool.query(
    `UPDATE codex_nodes SET
       reported_app_server = $2::jsonb,
       codex_installations = COALESCE($3::jsonb, codex_installations),
       capabilities = COALESCE($4::jsonb, capabilities),
       machine_status = COALESCE($5::jsonb, machine_status),
       last_seen_at = NOW(),
       updated_at = NOW()
     WHERE node_id = $1
     RETURNING desired_app_server, last_seen_at`,
    [
      nodeId,
      JSON.stringify(reported),
      body.codexInstallations === undefined ? null : JSON.stringify(body.codexInstallations),
      body.capabilities === undefined ? null : JSON.stringify(body.capabilities),
      body.machineStatus === undefined ? null : JSON.stringify(body.machineStatus),
    ],
  );
  if (result.rowCount === 0) {
    return { status: 404, body: { error: "node not found" } };
  }
  return {
    status: 200,
    body: {
      desiredAppServer: result.rows[0].desired_app_server,
      serverTime: result.rows[0].last_seen_at.toISOString(),
    },
  };
}

export async function listNodes(pool) {
  const result = await pool.query(
    `SELECT node_id, node_key, hostname, platform, architecture, node_mode,
            agent_version, capabilities, codex_installations,
            desired_app_server, reported_app_server,
            machine_status, channel_status,
            registered_at, last_seen_at,
            CASE WHEN last_seen_at > NOW() - INTERVAL '10 seconds'
                 THEN 'online' ELSE 'offline' END AS status
     FROM codex_nodes ORDER BY hostname, node_key`,
  );
  return result.rows.map((row) => ({
    nodeId: row.node_id,
    nodeKey: row.node_key,
    hostname: row.hostname,
    platform: row.platform,
    architecture: row.architecture,
    nodeMode: row.node_mode,
    agentVersion: row.agent_version,
    capabilities: row.capabilities,
    codexInstallations: row.codex_installations,
    desiredAppServer: row.desired_app_server,
    reportedAppServer: row.reported_app_server,
    machineStatus: row.machine_status,
    channelStatus: row.channel_status,
    registeredAt: row.registered_at.toISOString(),
    lastSeenAt: row.last_seen_at.toISOString(),
    status: row.status,
  }));
}

export async function setNodeChannelStatus(pool, nodeId, status) {
  await pool.query(
    `UPDATE codex_nodes SET channel_status = $2::jsonb, updated_at = NOW()
     WHERE node_id = $1`,
    [nodeId, JSON.stringify(status)],
  );
}

export async function setDesiredAppServer(pool, nodeId, body) {
  if (typeof body.running !== "boolean") {
    return { status: 400, body: { error: "running must be a boolean" } };
  }
  const configOverrides = body.configOverrides ?? [];
  if (
    !Array.isArray(configOverrides) ||
    configOverrides.length > 20 ||
    configOverrides.some(
      (value) => typeof value !== "string" || value.length === 0 || value.length > 2_048,
    )
  ) {
    return { status: 400, body: { error: "configOverrides must contain at most 20 strings" } };
  }
  const desired = {
    running: body.running,
    listenUrl: body.listenUrl ?? null,
    codexPath: body.codexPath ?? null,
    codexHome: body.codexHome ?? null,
    configOverrides,
    revision: Date.now(),
  };
  const result = await pool.query(
    `UPDATE codex_nodes SET desired_app_server = $2::jsonb, updated_at = NOW()
     WHERE node_id = $1 RETURNING desired_app_server`,
    [nodeId, JSON.stringify(desired)],
  );
  if (result.rowCount === 0) {
    return { status: 404, body: { error: "node not found" } };
  }
  return { status: 200, body: { desiredAppServer: result.rows[0].desired_app_server } };
}
