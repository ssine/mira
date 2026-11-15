// A question answered after another process rotated the account's OAuth tokens
// must not reach the waiting Claude process: the worker stops its response and
// resumes the session in a fresh process with the answer as the next message.
// Runs the real worker against a fake Agent SDK and Mira store.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const assets = path.join(path.dirname(fileURLToPath(import.meta.url)), "../node/internal/clauderuntime/assets");

const fakeSdk = `
import { appendFileSync } from "node:fs";
const log = (value) => appendFileSync(process.env.FAKE_LOG, JSON.stringify(value) + "\\n");
let queries = 0;
export function query({ prompt, options }) {
  const n = ++queries;
  log({ kind: "query", n, resume: options.resume ?? null, sessionId: options.sessionId ?? null });
  const input = prompt[Symbol.asyncIterator]();
  async function* run() {
    const first = await input.next();
    log({ kind: "prompt", n, text: first.value.message.content[0].text });
    yield { type: "system", subtype: "session_state_changed", state: "running" };
    if (n === 1) {
      const questions = [{ question: "部署到哪里？", header: "位置", options: [] }];
      yield { type: "assistant", message: { id: "m1", content: [{ type: "tool_use", id: "t1", name: "AskUserQuestion", input: { questions } }] } };
      const decision = await options.canUseTool("AskUserQuestion", { questions }, { signal: new AbortController().signal });
      log({ kind: "decision", behavior: decision.behavior, interrupt: decision.interrupt ?? false, answers: decision.updatedInput?.answers ?? null });
      yield { type: "result", is_error: decision.behavior === "deny" };
    } else {
      yield { type: "result", is_error: false };
    }
    yield { type: "system", subtype: "session_state_changed", state: "idle" };
    await input.next(); // The CLI exits once its input ends.
  }
  return Object.assign(run(), { interrupt: async () => {}, close() {}, [Symbol.asyncDispose]: async () => {} });
}
`;

const fakeStore = `
import { appendFileSync } from "node:fs";
const log = (value) => appendFileSync(process.env.FAKE_LOG, JSON.stringify(value) + "\\n");
export function serverClient() {
  return {
    store: {},
    event: async (payload) => log({ kind: "event", payload }),
    emit: async () => {},
    backlog: () => 0,
    failure: () => null,
    close() {},
  };
}
`;

async function scenario(rotate) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "mira-question-resume-"));
  try {
    for (const file of ["worker.mjs", "turn-lifecycle.mjs", "question-credentials.mjs", "credential-mirror.mjs"])
      await fs.copyFile(path.join(assets, file), path.join(dir, file));
    await fs.writeFile(path.join(dir, "store.mjs"), fakeStore);
    await fs.writeFile(path.join(dir, "mira-tools.mjs"), "export const createMiraTools = () => ({});\n");
    const sdk = path.join(dir, "node_modules/@anthropic-ai/claude-agent-sdk");
    await fs.mkdir(sdk, { recursive: true });
    await fs.writeFile(path.join(sdk, "package.json"), JSON.stringify({ name: "@anthropic-ai/claude-agent-sdk", type: "module", exports: "./index.mjs" }));
    await fs.writeFile(path.join(sdk, "index.mjs"), fakeSdk);
    const home = path.join(dir, "account");
    await fs.mkdir(home);
    const credentials = (token) => fs.writeFile(path.join(home, ".credentials.json"), JSON.stringify({ claudeAiOauth: { accessToken: `a-${token}`, refreshToken: `r-${token}` } }));
    await credentials(1);
    const logPath = path.join(dir, "log.jsonl");
    await fs.writeFile(logPath, "");
    const entries = async () => (await fs.readFile(logPath, "utf8")).split("\n").filter(Boolean).map((line) => JSON.parse(line));
    const child = spawn(process.execPath, [path.join(dir, "worker.mjs")], {
      cwd: dir,
      env: { ...process.env, FAKE_LOG: logPath, MIRA_NODE_CLAUDE_CONFIG_DIR: home },
      stdio: ["pipe", "ignore", "inherit"],
    });
    const exited = new Promise((resolve) => child.on("exit", resolve));
    const sessionId = "00000000-0000-4000-8000-0000000000aa";
    child.stdin.write(JSON.stringify({ action: "start", sessionId, cwd: dir, text: "先问我", credential: "secret" }) + "\n");
    let question;
    for (let i = 0; i < 200 && !question; i++) {
      question = (await entries()).find((entry) => entry.payload?.type === "mira_question");
      if (!question) await new Promise((resolve) => setTimeout(resolve, 25));
    }
    assert.ok(question, "the worker never asked the question");
    if (rotate) await credentials(2); // Another process refreshed the account's tokens.
    child.stdin.write(JSON.stringify({ action: "answer", questionId: question.payload.questionId, answers: { "部署到哪里？": "Home Server" } }) + "\n");
    const code = await Promise.race([exited, new Promise((_, reject) => setTimeout(() => reject(new Error("worker did not exit")), 10_000))]);
    assert.equal(code, 0);
    return await entries();
  } finally {
    await fs.rm(dir, { recursive: true, force: true });
  }
}

{
  const log = await scenario(true);
  const queries = log.filter((entry) => entry.kind === "query");
  assert.equal(queries.length, 2, "a stale answer must continue in a fresh process");
  assert.equal(queries[0].sessionId, "00000000-0000-4000-8000-0000000000aa");
  assert.equal(queries[1].resume, "00000000-0000-4000-8000-0000000000aa");
  const decision = log.find((entry) => entry.kind === "decision");
  assert.deepEqual([decision.behavior, decision.interrupt], ["deny", true], "the stale process must stop without another request");
  const prompt = log.find((entry) => entry.kind === "prompt" && entry.n === 2);
  assert.match(prompt.text, /"部署到哪里？"="Home Server"/);
  const types = log.filter((entry) => entry.kind === "event").map((entry) => entry.payload.type);
  assert.deepEqual(types, ["mira_user", "mira_question", "mira_answer", "mira_question_resume", "mira_completed"]);
  assert.equal(log.at(-1).payload.failed, false);
}
{
  const log = await scenario(false);
  assert.equal(log.filter((entry) => entry.kind === "query").length, 1, "a fresh answer stays in the same process");
  const decision = log.find((entry) => entry.kind === "decision");
  assert.equal(decision.behavior, "allow");
  assert.deepEqual(decision.answers, { "部署到哪里？": "Home Server" });
  assert.equal(log.at(-1).payload.failed, false);
}
console.log("PASS: stale question answers resume in a fresh Claude process; fresh answers continue in place");
