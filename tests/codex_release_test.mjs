import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const lock = JSON.parse(await fs.readFile(path.join(root, "node/internal/codex-runtime.json")));
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");

test("independent Codex archives preserve canonical companions and file hashes", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-codex-pack-test-"));
  try {
    const sources = [];
    for (const platform of ["linux-amd64", "windows-amd64"]) {
      const windows = platform.startsWith("windows");
      const source = path.join(temporary, platform);
      const suffix = windows ? ".exe" : "";
      const files = [`bin/codex${suffix}`, `bin/codex-code-mode-host${suffix}`, `codex-path/rg${suffix}`,
        ...(windows ? ["codex-resources/codex-command-runner.exe", "codex-resources/codex-windows-sandbox-setup.exe"] : ["codex-resources/bwrap", "codex-resources/zsh/bin/zsh"])];
      for (const file of files) {
        await fs.mkdir(path.dirname(path.join(source, file)), { recursive: true });
        await fs.writeFile(path.join(source, file), `fixture ${file}`, { mode: 0o644 });
      }
      await fs.writeFile(path.join(source, "codex-package.json"), JSON.stringify({ layoutVersion: 1, version: lock.upstreamVersion,
        target: windows ? "x86_64-pc-windows-msvc" : "x86_64-unknown-linux-musl", variant: "codex", entrypoint: `bin/codex${suffix}` }));
      sources.push(`${platform}=${source}`);
    }
    const output = path.join(temporary, "release");
    execFileSync(process.execPath, ["scripts/build-codex-release.mjs", output, ...sources], { cwd: root });
    const manifest = JSON.parse(await fs.readFile(path.join(output, "codex-runtime.json")));
    assert.equal(manifest.version, lock.version);
    assert.equal(manifest.patchSHA256, lock.patchSHA256);
    assert.equal(Object.keys(manifest.targets).length, 2);
    for (const [platform, target] of Object.entries(manifest.targets)) {
      const archive = await fs.readFile(path.join(output, target.archive));
      assert.equal(target.sha256, hash(archive));
      assert.equal(target.size, archive.length);
      for (const file of target.files) {
        const source = await fs.readFile(path.join(temporary, platform, file.path));
        assert.equal(file.sha256, hash(source));
        assert.equal(file.size, source.length);
        if (file.path.startsWith("bin/")) assert.equal(file.mode, 0o755);
      }
    }
    assert.throws(() => execFileSync(process.execPath, ["scripts/build-codex-release.mjs", output, ...sources], { cwd: root, stdio: "pipe" }), "must not overwrite existing artifacts");
    await fs.unlink(path.join(temporary, "linux-amd64/bin/codex-code-mode-host"));
    assert.throws(() => execFileSync(process.execPath, ["scripts/build-codex-release.mjs", path.join(temporary, "broken"), sources[0]], { cwd: root, stdio: "pipe" }), "must reject incomplete canonical packages");
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("Mira and Codex have separate jobs, tags and latest policy", async () => {
  const mira = await fs.readFile(path.join(root, ".github/workflows/release.yml"), "utf8");
  const codex = await fs.readFile(path.join(root, ".github/workflows/codex-release.yml"), "utf8");
  const bundle = await fs.readFile(path.join(root, "scripts/build-release.sh"), "utf8");
  assert.doesNotMatch(mira, /cargo build|codex-dist|MIRA_REQUIRE_CODEX_BUNDLE/);
  assert.doesNotMatch(bundle, /cp -R.*codex|mira-codex-package/);
  assert.match(codex, /"codex-v\*"/);
  assert.match(codex, /--latest=false/);
  assert.match(codex, /Runtime already exists/);
  assert.match(codex, /CODEX_VERSION patches\/codex\//);
});

test("Codex fault tests wait for PostgreSQL's final TCP SQL listener", async () => {
  const workflow = await fs.readFile(path.join(root, ".github/workflows/codex-release.yml"), "utf8");
  assert.match(workflow, /psql -h 127\.0\.0\.1 -U mira -d mira -XAtqc 'SELECT 1'/);
  assert.doesNotMatch(workflow, /pg_isready -U mira/,
    "socket readiness can match the temporary initdb server that is about to stop");
  assert.match(workflow, /docker logs mira-codex-test-postgres/);
});
