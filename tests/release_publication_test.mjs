import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const stateScript = path.join(root, "scripts/release-state.sh");
const containerScript = path.join(root, "scripts/release-container.sh");
const version = (await fs.readFile(path.join(root, "VERSION"), "utf8")).trim();
const wantedDigest = `sha256:${"a".repeat(64)}`;
const otherDigest = `sha256:${"b".repeat(64)}`;

function command(program, args, options = {}) {
  return execFileSync(program, args, { encoding: "utf8", ...options }).trim();
}

async function executable(file, source) {
  await fs.writeFile(file, source, { mode: 0o755 });
}

async function mockCommands(temporary) {
  const directory = path.join(temporary, "bin");
  await fs.mkdir(directory);
  await executable(path.join(directory, "curl"), `#!/usr/bin/env node
import fs from "node:fs";
const args = process.argv.slice(2);
const output = args[args.indexOf("--output") + 1];
const url = args.at(-1);
const latest = url.endsWith("/releases/latest");
const created = process.env.MOCK_CREATED_RELEASE_FILE && fs.existsSync(process.env.MOCK_CREATED_RELEASE_FILE);
const status = latest ? process.env.MOCK_LATEST_STATUS :
  (created ? process.env.MOCK_RELEASE_STATUS_AFTER_CREATE : process.env.MOCK_RELEASE_STATUS);
const body = latest ? process.env.MOCK_LATEST_JSON :
  (created ? process.env.MOCK_RELEASE_JSON_AFTER_CREATE : process.env.MOCK_RELEASE_JSON);
fs.writeFileSync(output, (body || "{}").replaceAll("__COMMIT__", process.env.MOCK_COMMIT || ""));
process.stdout.write(status || "404");
`);
  await executable(path.join(directory, "gh"), `#!/usr/bin/env node
import fs from "node:fs";
const args = process.argv.slice(2);
if (process.env.MOCK_GH_LOG) fs.appendFileSync(process.env.MOCK_GH_LOG, JSON.stringify(args) + "\\n");
if (args[0] === "api") {
  process.stdout.write((process.env.MOCK_RECORDED_DIGEST || "") + "\\n");
} else if (args[0] === "release" && args[1] === "create") {
  fs.writeFileSync(process.env.MOCK_CREATED_RELEASE_FILE, "created\\n");
} else {
  process.stderr.write("unexpected mock gh call: " + args.join(" ") + "\\n");
  process.exitCode = 99;
}
`);
  await executable(path.join(directory, "docker"), `#!/usr/bin/env node
import fs from "node:fs";
const args = process.argv.slice(2);
const stateFile = process.env.MOCK_DOCKER_STATE;
const state = JSON.parse(fs.readFileSync(stateFile, "utf8"));
if (args[0] !== "buildx" || args[1] !== "imagetools") process.exit(98);
if (args[2] === "inspect") {
  const image = args[3];
  const digest = state[image];
  if (!digest) {
    process.stderr.write("manifest unknown: " + image + "\\n");
    process.exit(1);
  }
  process.stdout.write("Name: " + image + "\\nDigest: " + digest + "\\n");
} else if (args[2] === "create") {
  const target = args[args.indexOf("--tag") + 1];
  const source = args.at(-1);
  state[target] = source.slice(source.lastIndexOf("@") + 1);
  fs.writeFileSync(stateFile, JSON.stringify(state));
} else {
  process.exit(97);
}
`);
  return directory;
}

async function repository(temporary, tagPosition = "none") {
  const remote = path.join(temporary, "remote.git");
  const checkout = path.join(temporary, "checkout");
  command("git", ["init", "--bare", "-q", remote]);
  command("git", ["init", "-q", checkout]);
  await fs.copyFile(path.join(root, "VERSION"), path.join(checkout, "VERSION"));
  command("git", ["config", "user.email", "release-test@example.invalid"], { cwd: checkout });
  command("git", ["config", "user.name", "Mira release test"], { cwd: checkout });
  command("git", ["add", "VERSION"], { cwd: checkout });
  command("git", ["commit", "-qm", "release"], { cwd: checkout });
  const releaseCommit = command("git", ["rev-parse", "HEAD"], { cwd: checkout });
  if (tagPosition === "conflict") command("git", ["tag", `v${version}`], { cwd: checkout });
  if (tagPosition === "conflict") {
    await fs.writeFile(path.join(checkout, "later"), "later\n");
    command("git", ["add", "later"], { cwd: checkout });
    command("git", ["commit", "-qm", "later"], { cwd: checkout });
  } else if (tagPosition === "head") {
    command("git", ["tag", `v${version}`], { cwd: checkout });
  }
  const head = command("git", ["rev-parse", "HEAD"], { cwd: checkout });
  command("git", ["remote", "add", "origin", remote], { cwd: checkout });
  command("git", ["push", "-q", "origin", "HEAD:refs/heads/main"], { cwd: checkout });
  if (tagPosition !== "none") command("git", ["push", "-q", "origin", `refs/tags/v${version}`], { cwd: checkout });
  return { checkout, head, releaseCommit, remote };
}

function releaseJSON({ commit = "main", draft = false, digest = "" } = {}) {
  return JSON.stringify({
    tag_name: `v${version}`,
    draft,
    prerelease: false,
    target_commitish: commit,
    assets: digest ? [{ name: `mira_${version}_container.digest`, id: 7 }] : [],
  });
}

function releaseEnvironment(fakeBin, temporary, overrides = {}) {
  return {
    ...process.env,
    PATH: `${fakeBin}:${process.env.PATH}`,
    GH_REPO: "example/mira",
    GH_TOKEN: "test-token",
    MOCK_CREATED_RELEASE_FILE: path.join(temporary, "created-release"),
    MOCK_GH_LOG: path.join(temporary, "gh.log"),
    MOCK_RELEASE_STATUS: "404",
    MOCK_RELEASE_JSON: "{}",
    MOCK_RELEASE_STATUS_AFTER_CREATE: "200",
    MOCK_LATEST_STATUS: "404",
    MOCK_LATEST_JSON: "{}",
    ...overrides,
  };
}

async function runState(temporary, tagPosition, mode, overrides = {}) {
  const repo = await repository(temporary, tagPosition);
  const fakeBin = await mockCommands(temporary);
  const output = path.join(temporary, "output");
  await fs.writeFile(output, "");
  const env = releaseEnvironment(fakeBin, temporary, { MOCK_COMMIT: repo.head, ...overrides });
  const result = spawnSync("bash", [stateScript, "prepare", mode, repo.head, output], {
    cwd: repo.checkout, env, encoding: "utf8",
  });
  return { ...repo, env, output, result };
}

function outputFields(value) {
  return Object.fromEntries(value.trim().split("\n").filter(Boolean).map((line) => {
    const separator = line.indexOf("=");
    return [line.slice(0, separator), line.slice(separator + 1)];
  }));
}

test("initial publication creates a commit-bound draft and tag", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-release-initial-"));
  try {
    const fixture = await runState(temporary, "none", "publish", {
      MOCK_RELEASE_JSON_AFTER_CREATE: releaseJSON({ commit: "__COMMIT__", draft: true }),
    });
    assert.equal(fixture.result.status, 0, fixture.result.stderr);
    const fields = outputFields(await fs.readFile(fixture.output, "utf8"));
    assert.equal(fields.release_state, "draft");
    assert.equal(fields.recorded_digest, "");
    assert.equal(command("git", ["--git-dir", fixture.remote, "rev-parse", `refs/tags/v${version}`]), fixture.head);
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("a public release at the same commit is a safe idempotent retry", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-release-retry-"));
  try {
    const fixture = await runState(temporary, "head", "promote", {
      MOCK_RELEASE_STATUS: "200",
      MOCK_RELEASE_JSON: releaseJSON({ digest: wantedDigest }),
      MOCK_LATEST_STATUS: "200",
      MOCK_LATEST_JSON: releaseJSON(),
      MOCK_RECORDED_DIGEST: wantedDigest,
    });
    assert.equal(fixture.result.status, 0, fixture.result.stderr);
    const fields = outputFields(await fs.readFile(fixture.output, "utf8"));
    assert.equal(fields.release_state, "published");
    assert.equal(fields.recorded_digest, wantedDigest);
    const calls = (await fs.readFile(fixture.env.MOCK_GH_LOG, "utf8")).trim().split("\n").map(JSON.parse);
    assert.deepEqual(calls.map((args) => args[0]), ["api"], "retry must not recreate the release");
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("a conflicting remote version tag fails before creating a release", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-release-tag-conflict-"));
  try {
    const fixture = await runState(temporary, "conflict", "publish");
    assert.notEqual(fixture.result.status, 0);
    assert.match(fixture.result.stderr, /Remote tag .* points to .* expected/);
    assert.equal(await fs.readFile(fixture.env.MOCK_GH_LOG, "utf8").catch(() => ""), "");
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("an older release cannot move latest backward", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-release-monotonic-"));
  try {
    const [major] = version.split(".");
    const newer = `${major}.${"9".repeat(80)}.0`;
    const fixture = await runState(temporary, "none", "publish", {
      MOCK_LATEST_STATUS: "200",
      MOCK_LATEST_JSON: JSON.stringify({ tag_name: `v${newer}`, draft: false, prerelease: false }),
    });
    assert.notEqual(fixture.result.status, 0);
    assert.match(fixture.result.stderr, new RegExp(`Refusing to move latest backward from v${newer.replaceAll(".", "\\.")}`));
    assert.equal(await fs.readFile(fixture.env.MOCK_GH_LOG, "utf8").catch(() => ""), "");
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("offline publication state tests run in CI and before a release", async () => {
  const releaseWorkflow = await fs.readFile(path.join(root, ".github/workflows/release.yml"), "utf8");
  const ciWorkflow = await fs.readFile(path.join(root, ".github/workflows/node-ci.yml"), "utf8");
  assert.match(releaseWorkflow, /node --test tests\/release_publication_test\.mjs/);
  assert.match(ciWorkflow, /tests\/release_publication_test\.mjs/);
});

async function runContainer(temporary, args, images) {
  const fakeBin = await mockCommands(temporary);
  const state = path.join(temporary, "docker-state.json");
  const output = path.join(temporary, "container-output");
  await fs.writeFile(state, JSON.stringify(images));
  await fs.writeFile(output, "");
  const result = spawnSync("bash", [containerScript, ...args, output], {
    cwd: root,
    env: { ...process.env, PATH: `${fakeBin}:${process.env.PATH}`, MOCK_DOCKER_STATE: state },
    encoding: "utf8",
  });
  return { result, output, state };
}

test("container digest match skips the immutable version tag", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-container-match-"));
  try {
    const image = `ghcr.io/example/mira:${version}`;
    const fixture = await runContainer(temporary, ["check", image, wantedDigest], { [image]: wantedDigest });
    assert.equal(fixture.result.status, 0, fixture.result.stderr);
    assert.deepEqual(outputFields(await fs.readFile(fixture.output, "utf8")), {
      push: "false", existing_digest: wantedDigest,
      staging_image: `${image}-staging`, needs_record: "false",
    });
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("container digest conflict is rejected", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-container-conflict-"));
  try {
    const image = `ghcr.io/example/mira:${version}`;
    const fixture = await runContainer(temporary, ["check", image, wantedDigest], { [image]: otherDigest });
    assert.notEqual(fixture.result.status, 0);
    assert.match(fixture.result.stderr, /Immutable image .* but the release records/);
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("matching staging digest recovers a crash before digest recording", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-container-recovery-"));
  try {
    const image = `ghcr.io/example/mira:${version}`;
    const staging = `${image}-staging`;
    const fixture = await runContainer(temporary, ["check", image, ""], {
      [image]: wantedDigest, [staging]: wantedDigest,
    });
    assert.equal(fixture.result.status, 0, fixture.result.stderr);
    const fields = outputFields(await fs.readFile(fixture.output, "utf8"));
    assert.equal(fields.push, "false");
    assert.equal(fields.needs_record, "true");
    assert.equal(fields.existing_digest, wantedDigest);
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});

test("initial container publication promotes staging by digest", async () => {
  const temporary = await fs.mkdtemp(path.join(os.tmpdir(), "mira-container-initial-"));
  try {
    const image = `ghcr.io/example/mira:${version}`;
    const checked = await runContainer(temporary, ["check", image, ""], {});
    assert.equal(checked.result.status, 0, checked.result.stderr);
    const check = outputFields(await fs.readFile(checked.output, "utf8"));
    assert.equal(check.push, "true");
    const images = JSON.parse(await fs.readFile(checked.state, "utf8"));
    images[check.staging_image] = wantedDigest;
    await fs.writeFile(checked.state, JSON.stringify(images));
    await fs.writeFile(checked.output, "");
    const fakeBin = path.join(temporary, "bin");
    const promoted = spawnSync("bash", [containerScript, "promote", image, check.staging_image,
      wantedDigest, checked.output], {
      cwd: root,
      env: { ...process.env, PATH: `${fakeBin}:${process.env.PATH}`, MOCK_DOCKER_STATE: checked.state },
      encoding: "utf8",
    });
    assert.equal(promoted.status, 0, promoted.stderr);
    assert.equal(outputFields(await fs.readFile(checked.output, "utf8")).digest, wantedDigest);
    assert.equal(JSON.parse(await fs.readFile(checked.state, "utf8"))[image], wantedDigest);
  } finally { await fs.rm(temporary, { recursive: true, force: true }); }
});
