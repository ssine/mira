// A resumed Claude CLI reads a private copy of the login in
// <tmpdir>/claude-resume-*/ (made by the Agent SDK without the refresh token).
// The mirror copies each renewal of the account's login into stale copies of
// that account only.
import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { mirrorResumeCredentials, syncResumeCredentials } from "../node/internal/clauderuntime/assets/credential-mirror.mjs";

const write = (file, oauth, extra = {}) => fs.writeFileSync(file, JSON.stringify({ claudeAiOauth: oauth, ...extra }), { mode: 0o600 });
const read = (file) => JSON.parse(fs.readFileSync(file, "utf8"));

test("renewals reach this account's resume copies, without the refresh token", async () => {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "mirror-"));
  const account = path.join(base, "account"), root = path.join(base, "tmp");
  for (const dir of [account, root, path.join(root, "claude-resume-mine"), path.join(root, "claude-resume-other"), path.join(root, "unrelated")]) fs.mkdirSync(dir, { recursive: true });
  write(path.join(account, ".credentials.json"), { accessToken: "a1", refreshToken: "r1", expiresAt: 1, subscriptionType: "max" });
  write(path.join(root, "claude-resume-mine", ".credentials.json"), { accessToken: "a1", expiresAt: 1, subscriptionType: "max" }, { mcpOAuth: { x: 1 } });
  write(path.join(root, "claude-resume-other", ".credentials.json"), { accessToken: "someone-else", expiresAt: 1 });
  const known = new Set(["a1"]);
  assert.equal(syncResumeCredentials(path.join(account, ".credentials.json"), known, root), 0, "nothing to do before a renewal");

  write(path.join(account, ".credentials.json"), { accessToken: "a2", refreshToken: "r2", expiresAt: 2, subscriptionType: "max" });
  assert.equal(syncResumeCredentials(path.join(account, ".credentials.json"), known, root), 1);
  const mine = read(path.join(root, "claude-resume-mine", ".credentials.json"));
  assert.equal(mine.claudeAiOauth.accessToken, "a2");
  assert.equal(mine.claudeAiOauth.expiresAt, 2);
  assert.equal(mine.claudeAiOauth.refreshToken, undefined, "the copy never receives the refresh token");
  assert.deepEqual(mine.mcpOAuth, { x: 1 }, "other stored fields stay");
  assert.equal(read(path.join(root, "claude-resume-other", ".credentials.json")).claudeAiOauth.accessToken, "someone-else", "another account's copy is untouched");

  // The watcher picks up a later renewal on its own.
  const stop = mirrorResumeCredentials(account, { intervalMs: 20, root });
  try {
    write(path.join(account, ".credentials.json"), { accessToken: "a3", refreshToken: "r3", expiresAt: 3 });
    const deadline = Date.now() + 3000;
    while (read(path.join(root, "claude-resume-mine", ".credentials.json")).claudeAiOauth.accessToken !== "a3") {
      assert.ok(Date.now() < deadline, "the watcher did not mirror the renewal");
      await new Promise((resolve) => setTimeout(resolve, 20));
    }
  } finally {
    stop();
    fs.rmSync(base, { recursive: true, force: true });
  }
});
