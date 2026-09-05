// Real Codex + PostgreSQL; a loopback model fixture avoids production credentials/sampling.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import { spawn } from "node:child_process";
import pg from "../server/node_modules/pg/lib/index.js";
import { commitDelta, getStoreHead, getThreadHistory, getSnapshot } from "../server/thread-store.mjs";
import { manageThread, renameThread } from "../server/thread-management.mjs";

const binary = process.env.CODEX_TEST_BINARY;
assert(binary && path.isAbsolute(binary), "CODEX_TEST_BINARY must name the candidate runtime");
assert(process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL, "a disposable database is required");
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const directory = await fs.mkdtemp(path.join(os.tmpdir(), "mira-thread-fork-"));
const store = `fork-${crypto.randomUUID()}`;
const children = [];
let fixtureError;
const fixture = http.createServer(async (request, response) => {
  const json = (status, body) => { response.writeHead(status, { "content-type": "application/json" }); response.end(JSON.stringify(body)); };
  try {
    const chunks = []; for await (const chunk of request) chunks.push(chunk);
    const url = new URL(request.url, "http://localhost");
    if (url.pathname.endsWith("/responses")) {
      const events = [
        { type: "response.created", response: { id: "fork-response" } },
        { type: "response.output_item.done", item: { type: "message", role: "assistant", id: "fork-message", content: [{ type: "output_text", text: "FORK_HISTORY" }] } },
        { type: "response.completed", response: { id: "fork-response", usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } },
      ];
      response.writeHead(200, { "content-type": "text/event-stream" });
      response.end(events.map(event => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join("")); return;
    }
    const parts = url.pathname.split("/").filter(Boolean);
    assert.equal(parts[2], store);
    if (request.method === "GET") {
      if (parts[3] === "threads") { const result = await getThreadHistory(pool, store, parts[4], Number(url.searchParams.get("generation")) || null, Number(url.searchParams.get("throughVersion")) || null); json(result.status, result.body); }
      else json(200, await getStoreHead(pool, store, url.searchParams.has('threadId') ? [url.searchParams.get('threadId')] : null));
    } else {
      const result = await commitDelta(pool, store, JSON.parse(Buffer.concat(chunks)), request.headers);
      json(result.status, result.body);
    }
  } catch (error) { fixtureError = error; json(500, { error: error.message }); }
});
async function client(name) {
  const home = path.join(directory, name); await fs.mkdir(home);
  const endpoint = `http://127.0.0.1:${fixture.address().port}`;
  const config = ['model="gpt-5.4"', 'model_provider="fixture"', `model_providers.fixture={name="Fixture",base_url="${endpoint}/v1",wire_api="responses"}`,
    'experimental_thread_store.type="remote_http"', `experimental_thread_store.endpoint="${endpoint}"`, `experimental_thread_store.store_id="${store}"`, 'experimental_thread_store.bearer_token="test-only"', 'features.code_mode=false'];
  const proc = spawn(binary, ["app-server", ...config.flatMap(value => ["-c", value])], { cwd: directory, env: { ...process.env, CODEX_HOME: home }, stdio: ["pipe", "pipe", "pipe"] });
  children.push(proc); let stderr = "", next = 0; const pending = new Map(), events = [];
  proc.stderr.on("data", chunk => { stderr = (stderr + chunk).slice(-5000); });
  const send = message => proc.stdin.write(JSON.stringify(message) + "\n");
  readline.createInterface({ input: proc.stdout }).on("line", line => {
    let message; try { message = JSON.parse(line); } catch { return; }
    const request = pending.get(message.id);
    if (request) { pending.delete(message.id); clearTimeout(request.timer); message.error ? request.reject(Error(JSON.stringify(message.error))) : request.resolve(message.result); }
    else events.push(message);
  });
  const call = (method, params) => new Promise((resolve,reject) => {
    const id=++next; const timer=setTimeout(()=>{ pending.delete(id); reject(Error(`timeout ${method}: ${stderr}`)); },30000);
    pending.set(id,{resolve,reject,timer}); send({id,method,params});
  });
  const turn = async threadId => {
    const result = await call("turn/start", { threadId, input: [{ type: "text", text: "Continue with a short response" }], approvalPolicy: "never" });
    const deadline=Date.now()+30000;
    while(!events.some(event=>event.method==='turn/completed' && event.params.turn.id===result.turn.id)) {
      if(fixtureError)throw fixtureError;
      if(Date.now()>deadline)throw Error(`turn did not complete: ${stderr}`);
      await new Promise(resolve=>setTimeout(resolve,20));
    }
    assert.equal(events.find(event=>event.method==='turn/completed' && event.params.turn.id===result.turn.id).params.turn.status,'completed');
  };
  await call("initialize",{clientInfo:{name:"mira_fork_test",version:"1"},capabilities:{experimentalApi:true}});send({method:'initialized'});
  return {call,turn};
}
try {
  await new Promise(resolve=>fixture.listen(0,"127.0.0.1",resolve));
  const first=await client('first');
  const started=await first.call('thread/start',{cwd:directory,approvalPolicy:'never',sandbox:'read-only'});
  const parent=started.thread.id;
  await first.turn(parent);
  let head=await getStoreHead(pool,store);
  assert.equal((await renameThread(pool,store,parent,{name:'Source title',expectedName:head.state.names?.[parent]??null,generation:head.historyManifest[parent].generation,operationId:crypto.randomUUID()})).status,200);
  const second=await client('second');
  const resumed=await second.call('thread/resume',{threadId:parent,excludeTurns:true,approvalPolicy:'never',sandbox:'read-only'});
  assert.equal(resumed.thread.name,'Source title','App Server sees central name changes');
  const before=(await getSnapshot(pool,store)).snapshot.histories[parent];
  const fork=await second.call('thread/fork',{threadId:parent,excludeTurns:true,deferGoalContinuation:true,approvalPolicy:'never',sandbox:'read-only'});
  assert.notEqual(fork.thread.id,parent);
  assert.equal(fork.cwd,directory);
  const child=fork.thread.id;
  const after=(await getSnapshot(pool,store)).snapshot;
  assert.deepEqual(after.histories[parent],before,'fork cannot rewrite the source');
  assert.match(JSON.stringify(after.histories[child]),/FORK_HISTORY/,'fork retains source messages');
  head=await getStoreHead(pool,store);
  assert.equal((await renameThread(pool,store,child,{name:'Fork title',expectedName:head.state.names?.[child]??null,generation:head.historyManifest[child].generation,operationId:crypto.randomUUID()})).status,200);
  const third=await client('third');
  const recovered=await third.call('thread/resume',{threadId:child,excludeTurns:true,approvalPolicy:'never',sandbox:'read-only'});
  assert.equal(recovered.thread.name,'Fork title');
  await third.turn(child);
  const final=(await getSnapshot(pool,store)).snapshot;
  assert.deepEqual(final.histories[parent],before,'continuing the fork keeps source history independent');
  assert(final.histories[child].length>after.histories[child].length);
  head=await getStoreHead(pool,store);
  assert.equal((await manageThread(pool,store,parent,'delete',{generation:head.historyManifest[parent].generation,itemCount:head.historyManifest[parent].itemCount,operationId:crypto.randomUUID()})).status,200);
  const fourth=await client('fourth');
  await assert.rejects(fourth.call('thread/resume',{threadId:parent,excludeTurns:true,approvalPolicy:'never',sandbox:'read-only'}),/not found|no rollout|does not exist/i);
  const surviving=await fourth.call('thread/resume',{threadId:child,excludeTurns:true,approvalPolicy:'never',sandbox:'read-only'});
  assert.equal(surviving.thread.id,child,'deleting the source does not delete its fork');
  await fourth.turn(child);
  if(fixtureError)throw fixtureError;
  console.log('PASS: real Codex central rename, full-history fork, new-process resume, permanent source deletion and independent fork continuation');
} finally {
  for(const proc of children) if(proc.exitCode===null)proc.kill('SIGTERM');
  await Promise.all(children.filter(proc=>proc.exitCode===null).map(proc=>new Promise(resolve=>proc.once('exit',resolve))));
  await new Promise(resolve=>fixture.close(resolve));
  await pool.end(); await fs.rm(directory,{recursive:true,force:true});
}
