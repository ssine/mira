import crypto from "node:crypto";
import { listNodes, getNode } from "./node-registry.mjs";
import { weeklyQuota } from "./public/account-quota.js";

export const sampleIntervalMs = 5 * 60_000;
export const historyRanges = { "24h": 86400_000, "7d": 7 * 86400_000, "30d": 30 * 86400_000 };
export const runtimeKey = node => crypto.createHash("sha256").update(JSON.stringify([
  node.reportedAppServer?.codexHome ?? null, node.reportedAppServer?.codexPath ?? null,
])).digest("hex");
const safeText = value => typeof value === "string" && value.length <= 512 && !/[\u0000-\u001f]/.test(value) ? value : null;
const eligible = (node, channel) => node.approvalStatus === "approved" && node.status === "online" &&
  node.capabilities?.appServer === true && node.reportedAppServer?.status === "running" && channel.isConnected(node.nodeId);

export async function sampleAccount(pool, channel, node, { signal, now = () => Date.now() } = {}) {
  if (signal?.aborted || !eligible(node, channel)) return false;
  const key = runtimeKey(node), client = await pool.connect();
  let locked = false;
  const lockKey = `mira-account:${node.nodeId}:${key}`;
  try {
    // A per-node lock coalesces timer/browser-independent workers across Servers.
    locked = (await client.query("SELECT pg_try_advisory_lock(hashtextextended($1, 0)) AS locked", [lockKey])).rows[0].locked;
    if (!locked) return false;
    const last = (await client.query(`SELECT sampled_at FROM mira_account_quota_samples
      WHERE node_id=$1 AND runtime_key=$2 ORDER BY sampled_at DESC LIMIT 1`, [node.nodeId, key])).rows[0];
    if (last && now() - last.sampled_at.getTime() < sampleIntervalMs) return false;
    let account = null, quota = weeklyQuota(null), status = "ok";
    try {
      const result = await channel.accountReader.read(node.nodeId, { signal });
      account = result.account;
      quota = account?.type === "chatgpt" ? weeklyQuota(result.limits) : quota;
    } catch {
      if (signal?.aborted) return false;
      status = "error";
    }
    // Discard a response from a runtime replaced/revoked while reading it.
    const current = await getNode(client, node.nodeId);
    if (signal?.aborted || !current || !eligible(current, channel) || runtimeKey(current) !== key) return false;
    const timestamp = now();
    await client.query(`INSERT INTO mira_account_quota_samples
      (node_id,runtime_key,sampled_at,sample_slot,status,account_type,email,plan_type,remaining,resets_at,reset_count)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT DO NOTHING`, [
      node.nodeId, key, new Date(timestamp), Math.floor(timestamp / sampleIntervalMs), status,
      safeText(account?.type), safeText(account?.email), safeText(account?.planType),
      quota.remaining, quota.resetsAt === null ? null : new Date(quota.resetsAt), quota.resetCount,
    ]);
    return true;
  } finally {
    try { if (locked) await client.query("SELECT pg_advisory_unlock(hashtextextended($1, 0))", [lockKey]); }
    finally { client.release(); }
  }
}

export function startAccountSampler(pool, channel, { tickMs = 30_000 } = {}) {
  const controller = new AbortController();
  let timer, pending = Promise.resolve();
  const tick = async () => {
    try {
      const nodes = (await listNodes(pool)).filter(node => eligible(node, channel));
      // Two bounded readers leave database connections available for chat writes.
      let index = 0;
      const results = await Promise.allSettled(Array.from({ length: Math.min(2, nodes.length) }, async () => {
        while (!controller.signal.aborted && index < nodes.length) {
          await sampleAccount(pool, channel, nodes[index++], { signal: controller.signal });
        }
      }));
      if (results.some(result => result.status === "rejected")) throw new Error("account sampling failed");
    } catch { if (!controller.signal.aborted) console.error("account history sampling failed"); }
    finally { if (!controller.signal.aborted) { timer = setTimeout(run, tickMs); timer.unref?.(); } }
  };
  const run = () => { pending = tick(); };
  timer = setTimeout(run, 1_000);
  timer.unref?.();
  return async () => { controller.abort(); clearTimeout(timer); await pending; };
}

export async function accountHistory(pool, node, range = "7d", now = Date.now()) {
  const duration = Object.hasOwn(historyRanges, range) ? historyRanges[range] : null;
  if (!duration) throw Object.assign(new Error("range must be 24h, 7d or 30d"), { statusCode: 400 });
  const key = runtimeKey(node), from = now - duration;
  // Include other identities as gaps, never as another account's quota values.
  // At most one row per five-minute slot: 30 days fits in 8,641 rows.
  const { rows } = await pool.query(`WITH identity AS (
      SELECT account_type,email,plan_type,sampled_at FROM mira_account_quota_samples
      WHERE node_id=$1 AND runtime_key=$2 AND status='ok' ORDER BY sampled_at DESC LIMIT 1
    ), points AS (
      SELECT sampled_at,status,account_type,email,remaining,resets_at,reset_count
      FROM mira_account_quota_samples WHERE node_id=$1 AND runtime_key=$2 AND sampled_at >= $3 AND sampled_at <= $4
      ORDER BY sampled_at LIMIT 8642
    ) SELECT (SELECT row_to_json(identity) FROM identity) AS identity,
      COALESCE((SELECT json_agg(points ORDER BY sampled_at) FROM points), '[]'::json) AS points`,
  [node.nodeId, key, new Date(from), new Date(now)]);
  const identity = rows[0].identity;
  const account = identity ? { type: identity.account_type, email: identity.email, planType: identity.plan_type } : null;
  const points = rows[0].points.map(point => {
    const matches = point.status === "ok" && account?.type === "chatgpt" && Boolean(account.email) &&
      point.account_type === account.type && point.email === account.email;
    return { at: Date.parse(point.sampled_at), remaining: matches ? point.remaining : null,
      resetsAt: matches && point.resets_at ? Date.parse(point.resets_at) : null,
      resetCount: matches && point.reset_count !== null ? Number(point.reset_count) : null };
  });
  return { account, range, from, to: now, intervalMs: sampleIntervalMs, points };
}
