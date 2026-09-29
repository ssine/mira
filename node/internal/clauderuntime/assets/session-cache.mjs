// Disposable native transcript projection. Server remains authoritative on every load.
import { createHash, randomUUID } from "node:crypto";
import { createReadStream } from "node:fs";
import * as fs from "node:fs/promises";
import path from "node:path";
import { createInterface } from "node:readline";

export const DEFAULT_CACHE_BYTES = 4 * 1024 ** 3;
const reserve = 4096; // Metadata, including its atomic replacement, per transcript.
const hash = (s) => createHash("sha256").update(s).digest("hex");
const validLimit = (n) => Number.isSafeInteger(n) && n >= 0;
export async function cacheLimit(config) {
  try {
    const value = JSON.parse(await fs.readFile(config, "utf8"));
    if (!validLimit(value.maxBytes)) throw new Error("Invalid Claude cache limit");
    return value.maxBytes;
  } catch (error) {
    if (error.code === "ENOENT") return DEFAULT_CACHE_BYTES;
    throw error;
  }
}
async function inventory(root) {
  const rows = [];
  for (const name of await fs.readdir(root)) {
    if (!/^[a-f0-9]{64}\.(jsonl|json|tmp)$/.test(name)) continue;
    const info = await fs.stat(path.join(root, name)).catch(() => null);
    if (info?.isFile()) rows.push({ name, size: info.size, time: info.mtimeMs });
  }
  return rows;
}
async function remove(root, key) {
  for (const suffix of ["json", "jsonl", "tmp"])
    await fs.rm(path.join(root, `${key}.${suffix}`), { force: true });
}
async function prune(root, budget, keep = "") {
  const rows = await inventory(root);
  let used = rows.reduce((n, r) => n + r.size, 0);
  const groups = new Map();
  for (const row of rows) {
    const key = row.name.split(".")[0];
    const group = groups.get(key) ?? { key, size: 0, time: 0 };
    group.size += row.size;
    group.time = Math.max(group.time, row.time);
    groups.set(key, group);
  }
  for (const group of [...groups.values()].sort((a, b) => a.time - b.time)) {
    if (used <= budget) break;
    if (group.key === keep) continue;
    await remove(root, group.key);
    used -= group.size;
  }
  return used;
}
async function lock(root, repair = false) {
  await fs.mkdir(root, { recursive: true, mode: 0o700 });
  const filename = path.join(root, "writer.lock");
  // Only the serialized Node Manager maintenance path reaps abandoned locks.
  // SDK workers never unlink another writer's lock, avoiding competing reapers.
  if (repair) {
    try {
      const pid = Number(await fs.readFile(filename, "utf8"));
      const info = await fs.stat(filename);
      let dead = false;
      if (Number.isInteger(pid) && pid > 0) {
        try { process.kill(pid, 0); } catch (e) { dead = e.code === "ESRCH"; }
      } else dead = Date.now() - info.mtimeMs > 30_000;
      if (dead) await fs.unlink(filename);
    } catch (error) { if (error.code !== "ENOENT") throw error; }
  }
  let handle;
  try { handle = await fs.open(filename, "wx", 0o600); }
  catch (error) { if (error.code === "EEXIST") return null; throw error; }
  try { await handle.writeFile(String(process.pid)); }
  catch (error) { await handle.close(); await fs.rm(filename, { force: true }); throw error; }
  await handle.close();
  return () => fs.rm(filename, { force: true });
}
export async function cacheMaintenance({ root, config, action, maxBytes }) {
  if (action === "cache-configure") {
    if (!validLimit(maxBytes)) throw new Error("maxBytes must be a nonnegative safe integer");
    await fs.mkdir(path.dirname(config), { recursive: true, mode: 0o700 });
    const temporary = `${config}.${randomUUID()}.tmp`;
    try {
      await fs.writeFile(temporary, JSON.stringify({ maxBytes }) + "\n", { mode: 0o600, flag: "wx" });
      await fs.rename(temporary, config);
    } finally { await fs.rm(temporary, { force: true }); }
  }
  const limit = await cacheLimit(config);
  const release = await lock(root, true);
  try {
    if (release) await prune(root, limit);
    const rows = await inventory(root);
    const usedBytes = rows.reduce((n, r) => n + r.size, 0);
    return { maxBytes: limit, usedBytes, entries: rows.filter(r => r.name.endsWith(".json")).length, cleanupPending: usedBytes > limit };
  } finally { await release?.(); }
}
async function readCached(root, key, limit) {
  try {
    const meta = JSON.parse(await fs.readFile(path.join(root, `${key}.json`), "utf8"));
    if (meta.version !== 1 || !Number.isSafeInteger(meta.cursor) || meta.cursor < 1 ||
        !Number.isSafeInteger(meta.bytes) || meta.bytes <= 0 || meta.bytes + reserve > limit ||
        !/^[a-f0-9]{64}$/.test(meta.prefix) || !/^[a-f0-9]{64}$/.test(meta.digest)) return null;
    const stream = createReadStream(path.join(root, `${key}.jsonl`), { end: meta.bytes - 1 });
    const digest = createHash("sha256");
    let bytes = 0;
    stream.on("data", chunk => { digest.update(chunk); bytes += chunk.length; });
    const lines = createInterface({ input: stream, crlfDelay: Infinity });
    const entries = [];
    try {
      for await (const line of lines) entries.push(JSON.parse(line));
    } finally { lines.close(); stream.destroy(); }
    if (bytes !== meta.bytes || entries.length !== meta.cursor || digest.digest("hex") !== meta.digest) return null;
    return { meta, entries };
  } catch { return null; }
}
async function saveCached(options, key, entries, cached, prefix) {
  const { root, config } = options;
  const release = await lock(root);
  if (!release) return;
  try {
    const limit = await cacheLimit(config); // Re-read after acquiring the writer lock.
    let bytes = 0;
    const digest = createHash("sha256");
    for (const entry of entries) {
      const line = JSON.stringify(entry) + "\n";
      bytes += Buffer.byteLength(line);
      digest.update(line);
    }
    if (!entries.length || bytes + reserve > limit) {
      await remove(root, key);
      await prune(root, limit);
      return;
    }
    let append = false;
    if (cached) {
      const current = await fs.readFile(path.join(root, `${key}.json`), "utf8").catch(() => "");
      append = current === JSON.stringify(cached.meta);
    }
    // Reclaim the entire target reservation before writing. Partial/crashed
    // files are disposable and remain counted against the next writer's budget.
    if (!append) await remove(root, key);
    const currentSize = (await inventory(root)).filter(r => r.name.startsWith(key + ".")).reduce((n, r) => n + r.size, 0);
    await prune(root, limit - bytes - reserve + currentSize, key);
    const handle = await fs.open(path.join(root, `${key}.jsonl`), append ? "r+" : "w", 0o600);
    try {
      if (append) await handle.truncate(cached.meta.bytes);
      let position = append ? cached.meta.bytes : 0;
      for (let i = append ? cached.meta.cursor : 0; i < entries.length; i++) {
        const buffer = Buffer.from(JSON.stringify(entries[i]) + "\n");
        let offset = 0;
        while (offset < buffer.length) {
          const result = await handle.write(buffer, offset, buffer.length - offset, position);
          if (!result.bytesWritten) throw new Error("Cache write made no progress");
          offset += result.bytesWritten;
          position += result.bytesWritten;
        }
      }
      await handle.sync();
    } finally { await handle.close(); }
    const meta = { version: 1, bytes, cursor: entries.length, prefix, digest: digest.digest("hex") };
    await fs.writeFile(path.join(root, `${key}.tmp`), JSON.stringify(meta), { mode: 0o600 });
    await fs.rename(path.join(root, `${key}.tmp`), path.join(root, `${key}.json`));
  } finally { await release(); }
}
export async function loadTranscript({ cache, endpoint, sessionId, subpath = "", request }) {
  const key = hash(JSON.stringify([new URL(endpoint).origin, sessionId, subpath]));
  let cached = null;
  if (cache) {
    try { cached = await readCached(cache.root, key, await cacheLimit(cache.config)); } catch { /* disposable */ }
  }
  const query = new URLSearchParams({ subpath });
  if (cache) {
    query.set("cache", "1");
    query.set("after", String(cached?.meta.cursor ?? 0));
    query.set("prefix", cached?.meta.prefix ?? "");
  }
  // Storage/auth/ownership errors propagate; never fall back to offline data.
  const response = await request(`/entries?${query}`);
  const version = response.headers.get("X-Mira-Claude-Cache-Version");
  const start = Number(response.headers.get("X-Mira-Claude-Cache-Start"));
  const end = Number(response.headers.get("X-Mira-Claude-Cache-End"));
  const prefix = response.headers.get("X-Mira-Claude-Cache-Prefix") ?? "";
  if (version && (version !== "1" || !Number.isSafeInteger(start) || start < 0 ||
      !Number.isSafeInteger(end) || end < start || (start > 0 && start !== cached?.meta.cursor) ||
      (end > 0 && !/^[a-f0-9]{64}$/.test(prefix)))) throw new Error("Invalid Claude cache response");
  if (!version || start === 0) cached = null;
  const entries = cached?.entries ?? [];
  let remainder = "";
  for await (const chunk of response.body.pipeThrough(new TextDecoderStream())) {
    remainder += chunk;
    let index;
    while ((index = remainder.indexOf("\n")) >= 0) {
      const line = remainder.slice(0, index);
      remainder = remainder.slice(index + 1);
      if (line) entries.push(JSON.parse(line));
    }
  }
  if (remainder.trim()) entries.push(JSON.parse(remainder));
  if (version && entries.length !== end) throw new Error("Incomplete Claude transcript response");
  if (cache && version) {
    try { await saveCached(cache, key, entries, cached, prefix); } catch { /* disposable */ }
  }
  return entries;
}
