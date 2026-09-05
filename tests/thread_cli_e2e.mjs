// Real CLI creation/resume with fresh CODEX_HOME directories and a loopback model.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import pg from "../server/node_modules/pg/lib/index.js";
import { initializeDatabase } from "../server/db.mjs";
import {
  commitDelta,
  getStoreHead,
  getThreadHistory,
} from "../server/thread-store.mjs";
import { closeRuntimeFixtureDatabase } from "./runtime_fixture_cleanup.mjs";

const binary = process.env.CODEX_TEST_BINARY;
assert(
  binary && path.isAbsolute(binary),
  "CODEX_TEST_BINARY must name the candidate runtime",
);
const base = new URL(
  process.env.MIRA_TEST_DATABASE_URL ??
    "postgresql://mira:mira-local@127.0.0.1:55432/mira",
);
assert(
  ["localhost", "127.0.0.1", "[::1]"].includes(base.hostname),
  "CLI fixture requires a local test database",
);
const database = `mira_retry_${process.pid}_${crypto.randomBytes(4).toString("hex")}`;
const owner = new pg.Pool({ connectionString: base.toString() });
base.pathname = `/${database}`;
const pool = new pg.Pool({ connectionString: base.toString() });
const directory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-cli-storage-"));
let created = false,
  modelRequests = 0,
  fixtureError;
const inputs = [],
  commits = [];
const send = (res, status, body) => {
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
};
const fixture = http.createServer(async (req, res) => {
  try {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const encoded = Buffer.concat(chunks).toString("utf8");
    const url = new URL(req.url, "http://localhost");
    if (url.pathname.endsWith("/responses")) {
      inputs.push(JSON.parse(encoded).input);
      const reply = ++modelRequests === 1 ? "FIRST_REPLY" : "SECOND_REPLY";
      const events = [
        { type: "response.created", response: { id: `resp-${modelRequests}` } },
        {
          type: "response.output_item.done",
          item: {
            type: "message",
            id: `msg-${modelRequests}`,
            role: "assistant",
            content: [{ type: "output_text", text: reply }],
          },
        },
        {
          type: "response.completed",
          response: {
            id: `resp-${modelRequests}`,
            usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 },
          },
        },
      ];
      res.writeHead(200, { "content-type": "text/event-stream" });
      res.end(
        events
          .map(
            (event) =>
              `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`,
          )
          .join(""),
      );
      return;
    }
    const parts = url.pathname.split("/").filter(Boolean);
    assert.equal(parts[1], "stores");
    if (req.method === "GET") {
      if (parts[3] === "threads") {
        const result = await getThreadHistory(
          pool,
          "cli",
          parts[4],
          Number(url.searchParams.get("generation")) || null,
          Number(url.searchParams.get("throughVersion")) || null,
        );
        send(res, result.status, result.body);
      } else
        send(
          res,
          200,
          await getStoreHead(
            pool,
            "cli",
            url.searchParams.has("threadId")
              ? [url.searchParams.get("threadId")]
              : null,
          ),
        );
    } else {
      const body = JSON.parse(encoded);
      commits.push(body);
      const result = await commitDelta(pool, "cli", body, req.headers);
      send(res, result.status, result.body);
    }
  } catch (error) {
    fixtureError = error;
    send(res, 500, { error: error.message });
  }
});

async function run(args, home) {
  await fs.mkdir(home);
  const endpoint = `http://127.0.0.1:${fixture.address().port}`;
  const settings = [
    'model="gpt-5.4"',
    'model_provider="fixture"',
    "features.code_mode=false",
    `model_providers.fixture={name="CLI fixture",base_url="${endpoint}/v1",wire_api="responses",request_max_retries=0,stream_max_retries=0}`,
    'experimental_thread_store.type="remote_http"',
    `experimental_thread_store.endpoint="${endpoint}"`,
    'experimental_thread_store.store_id="cli"',
    'experimental_thread_store.bearer_token="local-test-only"',
  ];
  const child = spawn(
    binary,
    [
      ...settings.flatMap((value) => ["-c", value]),
      "exec",
      ...args,
      "--skip-git-repo-check",
      "--json",
      "--dangerously-bypass-approvals-and-sandbox",
    ],
    {
      cwd: directory,
      env: { ...process.env, CODEX_HOME: home },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  let stdout = "",
    stderr = "";
  child.stdout.on("data", (data) => {
    stdout += data;
  });
  child.stderr.on("data", (data) => {
    stderr = (stderr + data).slice(-12000);
  });
  const timer = setTimeout(() => child.kill(), 30000);
  try {
    await new Promise((resolve, reject) => {
      child.on("error", reject);
      child.on("close", (code) =>
        code === 0 ? resolve() : reject(Error(`CLI exited ${code}: ${stderr}`)),
      );
    });
  } finally {
    clearTimeout(timer);
  }
  if (fixtureError) throw fixtureError;
  return stdout
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
}
try {
  await owner.query(`CREATE DATABASE ${database}`);
  created = true;
  await initializeDatabase(pool);
  await new Promise((resolve) => fixture.listen(0, "127.0.0.1", resolve));
  const first = await run(["FIRST_PROMPT"], path.join(directory, "first-home"));
  const thread = first.find(
    (event) => event.type === "thread.started",
  )?.thread_id;
  assert(thread, "CLI must report its persisted thread ID");
  assert(JSON.stringify(first).includes("FIRST_REPLY"));
  const second = await run(
    ["resume", thread, "SECOND_PROMPT"],
    path.join(directory, "fresh-home"),
  );
  assert(JSON.stringify(second).includes("SECOND_REPLY"));
  assert.equal(modelRequests, 2);
  assert(
    JSON.stringify(inputs[1]).includes("FIRST_REPLY"),
    "fresh CLI must reconstruct prior model context from PostgreSQL",
  );
  const history = await getThreadHistory(pool, "cli", thread, null, null);
  assert.equal(history.status, 200);
  for (const value of [
    "FIRST_PROMPT",
    "FIRST_REPLY",
    "SECOND_PROMPT",
    "SECOND_REPLY",
  ])
    assert(JSON.stringify(history.body.items).includes(value));
  assert(
    commits.some(
      (body) =>
        body.stateChanges.length === 0 &&
        body.historyChanges.some(
          (change) => change.mode === "append" && change.expectedItemCount > 0,
        ),
    ),
  );
  console.log(
    "PASS: real CLI create, durable messages and resume with a fresh CODEX_HOME using PostgreSQL history",
  );
} finally {
  fixture.closeAllConnections();
  await new Promise((resolve) => fixture.close(resolve));
  if (created) await closeRuntimeFixtureDatabase(pool, owner, database);
  else await pool.end();
  await owner.end();
  await fs.rm(directory, { recursive: true, force: true });
}
