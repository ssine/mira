// Run with MIRA_SCCACHE_TEST_BINARY set to the pinned sccache executable.
// MIRA_SCCACHE_TEST_CC may select a native compiler (cl.exe on Windows).
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";

const binary = process.env.MIRA_SCCACHE_TEST_BINARY;
assert(binary, "MIRA_SCCACHE_TEST_BINARY is required");
const compiler = process.env.MIRA_SCCACHE_TEST_CC || (process.platform === "win32" ? "cl.exe" : "cc");
const root = await fs.mkdtemp(path.join(os.tmpdir(), "mira-compiler-cache-"));
const server = net.createServer();
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
const port = server.address().port;
await new Promise((resolve) => server.close(resolve));
const env = { ...process.env };
for (const key of Object.keys(env)) if (key.startsWith("SCCACHE_")) delete env[key];
Object.assign(env, {
  SCCACHE_GHA_ENABLED: "false", SCCACHE_DIRECT: "false", SCCACHE_CACHE_SIZE: "2G",
  SCCACHE_IDLE_TIMEOUT: "0", SCCACHE_SERVER_PORT: String(port),
  SCCACHE_CONF: path.join(root, "config.toml"), SCCACHE_CACHED_CONF: path.join(root, "cached-config"),
  SCCACHE_DIR: path.join(root, "cold"),
});
await fs.writeFile(env.SCCACHE_CONF, "");
const run = (command, args) => execFileSync(command, args, {
  cwd: root, env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 60000,
});
const cache = (...args) => run(binary, args);
const stats = () => JSON.parse(cache("--show-stats", "--stats-format=json"));
const count = (value) => Object.values(value.counts).reduce((sum, n) => sum + n, 0);
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
const object = path.join(root, process.platform === "win32" ? "fixture.obj" : "fixture.o");
const source = path.join(root, "fixture.c");
const compile = () => cache(compiler, ...(process.platform === "win32"
  ? ["/nologo", "/c", source, `/Fo${object}`]
  : ["-c", source, "-o", object]));
let running = false;
try {
  await fs.writeFile(source, '#include "value.h"\nint mira_cache_answer(void) { return MIRA_VALUE; }\n');
  await fs.writeFile(path.join(root, "value.h"), "#define MIRA_VALUE 17\n");
  cache("--start-server"); running = true;
  compile();
  const cold = stats();
  assert.match(cold.cache_location, /Local disk/i);
  assert.equal(count(cold.stats.cache_misses), 1);
  assert.equal(cold.stats.cache_write_errors, 0);
  const original = hash(await fs.readFile(object));
  cache("--stop-server"); running = false;
  const archive = path.join(root, "snapshot.tar.gz");
  run("tar", ["-czf", archive, "-C", env.SCCACHE_DIR, "."]);
  env.SCCACHE_DIR = path.join(root, "restored");
  await fs.mkdir(env.SCCACHE_DIR);
  run("tar", ["-xzf", archive, "-C", env.SCCACHE_DIR]);
  await fs.unlink(object);
  cache("--start-server"); running = true;
  compile();
  const hot = stats();
  assert.equal(count(hot.stats.cache_hits), 1, "restored snapshot must serve the compiled object");
  assert.equal(hash(await fs.readFile(object)), original);
  await fs.writeFile(path.join(root, "value.h"), "#define MIRA_VALUE 29\n");
  compile();
  const changed = stats();
  assert.equal(count(changed.stats.cache_misses), 1, "changed header must invalidate the cached object");
  assert.notEqual(hash(await fs.readFile(object)), original);
  assert.equal(changed.stats.cache_write_errors, 0);
  console.log(`PASS ${process.platform}: cold compile, archived restore hit, changed-header miss, zero cache write errors`);
} finally {
  if (running) { try { cache("--stop-server"); } catch {} }
  await fs.rm(root, { recursive: true, force: true });
}
