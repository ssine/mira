// Real Server/PostgreSQL + browser; only App Server inference is simulated.
// Requires a disposable MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL and MIRA_SERVER_URL.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import pg from '../server/node_modules/pg/lib/index.js';
import { putSnapshot, getSnapshot, getStoreHead } from '../server/thread-store.mjs';
import { renameThread } from '../server/thread-management.mjs';
const { chromium } = await import(process.argv[2] ?? 'playwright');
if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error('a disposable database is required');
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8789';
const id = crypto.randomUUID(), newId = crypto.randomUUID();
const node = { nodeId: crypto.randomUUID(), hostname: 'Title test node', platform: 'linux', status: 'online', capabilities: { appServer: true }, reportedAppServer: { status: 'running' }, desiredAppServer: { defaultCwd: '/work/title-test' } };
const history = [{ type: 'event_msg', payload: { type: 'user_message', message: '请给 Mira 网页添加对话标题', turn_id: 'original-turn' } },
  { type: 'response_item', payload: { type: 'message', role: 'assistant', phase: 'final_answer', content: [{ type: 'output_text', text: '已增加标题菜单。' }] } },
  ...Array.from({ length: 350 }, (_, i) => ({ type: 'response_item', payload: { type: 'message', role: 'assistant', phase: 'commentary', content: [{ type: 'output_text', text: `Progress ${i}` }] } }))];
const headers = () => ({ 'x-codex-operation-id': crypto.randomUUID() });
const titleJobs = [], mainRequests = [], temporaryIds = [], errors = [];
let browser, failFirstSend = true;
async function persistNewThread() {
  const current = await getSnapshot(pool, 'personal');
  current.snapshot.histories[newId] = [];
  current.snapshot.metadata_updates[newId] = { title: '新会话', cwd: '/work/title-test', updated_at: new Date().toISOString() };
  assert.equal((await putSnapshot(pool, 'personal', { expectedVersion: current.version, snapshot: current.snapshot }, headers())).status, 200);
  await pool.query("INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id) VALUES('personal',$1,$2)", [newId,node.nodeId]);
}
try {
  await pool.query(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status) VALUES($1::uuid,$1::text,$2,'linux','amd64','linux','test','{"appServer":true}','[]','approved')`, [node.nodeId,node.hostname]);
  const before = await getSnapshot(pool, 'personal');
  const snapshot = before.snapshot ?? {};
  snapshot.histories = { ...snapshot.histories, [id]: history };
  snapshot.metadata_updates = { ...snapshot.metadata_updates, [id]: { title: '原始预览', cwd: '/work/title-test', updated_at: new Date().toISOString() } };
  assert.equal((await putSnapshot(pool, 'personal', { expectedVersion: before.version, snapshot }, headers())).status, 200);
  await pool.query("INSERT INTO mira_codex_thread_runtimes(store_id,thread_id,node_id) VALUES('personal',$1,$2)", [id,node.nodeId]);
  browser = await chromium.launch({ headless: true, ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 } });
  context.on('page', page => page.on('pageerror', error => errors.push(error.message)));
  await context.route('**/v1/nodes', route => route.fulfill({ json: { data: [node] } }));
  await context.route(`**/v1/nodes/${node.nodeId}`, route => route.fulfill({ json: node }));
  let metadataDelay = 2;
  await context.route(`**/v1/codex/threads/${newId}?storeId=personal`, route => {
    if (route.request().method() === 'GET' && metadataDelay-- > 0) return route.fulfill({ status: 404, json: { error: 'projection pending' } });
    return route.fallback();
  });
  await context.routeWebSocket(/\/app-server\?storeId=personal$/, socket => {
    let titleConnection = false, tempId;
    socket.onMessage(async data => {
      const request = JSON.parse(data);
      if (request.method === 'initialize') titleConnection = request.params.clientInfo.name === 'mira_web_title';
      if (request.id === undefined) return;
      const send = result => socket.send(JSON.stringify({ id: request.id, result }));
      if (!titleConnection) {
        mainRequests.push(request);
        if (request.method === 'initialize') send({});
        else if (request.method === 'thread/resume') send({ thread: { id: request.params.threadId, status: { type: 'idle' } }, cwd: '/work/title-test' });
        else if (request.method === 'thread/turns/list') send({ data: [] });
        else if (request.method === 'thread/start') { await persistNewThread(); send({ thread: { id: newId }, cwd: '/work/title-test' }); }
        else if (request.method === 'turn/start') {
          if (failFirstSend) { failFirstSend = false; socket.send(JSON.stringify({ id: request.id, error: { message: 'first submission failed' } })); }
          else send({ turn: { id: 'main-turn', status: 'inProgress' } });
        } else if (request.method === 'thread/loaded/list') send({ data: [id, newId] });
        else throw Error('unexpected main RPC ' + request.method);
        return;
      }
      if (request.method === 'initialize') send({});
      else if (request.method === 'config/read') send({ config: { model: 'current-model', mcp_servers: { example: { command: 'never execute' } } } });
      else if (request.method === 'account/read') send({ account: { type: 'chatgpt' } });
      else if (request.method === 'model/list') send({ data: [{ model: 'gpt-5.6-luna' }] });
      else if (request.method === 'thread/start') {
        assert.equal(request.params.ephemeral, true);
        assert.equal(request.params.sandbox, 'read-only');
        assert.deepEqual(request.params.dynamicTools, []);
        assert.deepEqual(request.params.config.mcp_servers, { example: { enabled: false } });
        tempId = crypto.randomUUID(); temporaryIds.push(tempId);
        send({ thread: { id: tempId, ephemeral: true }, sandbox: { type: 'readOnly' } });
      } else if (request.method === 'turn/start') {
        assert.equal(request.params.threadId, tempId);
        assert.deepEqual(request.params.outputSchema.required, ['title']);
        const job = { prompt: request.params.input[0].text, interrupted: false, unsubscribed: false, complete: title => {
          socket.send(JSON.stringify({ method: 'item/completed', params: { threadId: tempId, item: { type: 'agentMessage', text: JSON.stringify({ title }) } } }));
          socket.send(JSON.stringify({ method: 'turn/completed', params: { threadId: tempId, turn: { id: tempId, status: 'completed' } } }));
        } };
        titleJobs.push(job);
        socket.send(JSON.stringify({ method: 'turn/started', params: { threadId: tempId, turn: { id: tempId } } }));
        send({ turn: { id: tempId } });
      } else if (request.method === 'turn/interrupt') { titleJobs.at(-1).interrupted = true; send({}); }
      else if (request.method === 'thread/unsubscribe') { titleJobs.at(-1).unsubscribed = true; send({}); }
      else throw Error('unexpected title RPC ' + request.method);
    });
  });
  const page = await context.newPage();
  page.setDefaultTimeout(10_000);
  await page.goto(origin);
  await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD ?? 'mira-local-admin-password');
  await page.locator('#loginForm button[type=submit]').click();
  await page.locator('#dashboardView:not(.hidden)').waitFor();
  await page.goto(`${origin}/?thread=${id}`);
  await page.locator(`[data-thread-row="${id}"]`).waitFor();
  assert.equal(titleJobs.length, 0, 'viewing an existing untitled conversation does not auto-name it');
  const waitForJob = async count => {
    for (let i = 0; titleJobs.length < count && i < 150; i++) await page.waitForTimeout(50);
    assert.equal(titleJobs.length, count);
  };
  const regenerate = async () => { await page.locator('#conversationMenuToggle').click(); await page.locator('#threadRegenerateTitle').click(); };
  console.log('CHECK manual generation');
  await regenerate(); await waitForJob(1);
  assert.match(titleJobs[0].prompt, /请给 Mira 网页添加对话标题/);
  assert.match(titleJobs[0].prompt, /已增加标题菜单/);
  await page.locator('#conversationInput').fill('Keep my draft');
  await page.locator('#conversationMenuToggle').click();
  assert.equal(await page.locator('#threadRegenerateTitle').isDisabled(), true, 'duplicate generation is disabled');
  assert.equal(await page.locator('#threadCancelTitle').isVisible(), true);
  await page.keyboard.press('Escape');
  titleJobs[0].complete('Mira 标题菜单');
  await page.waitForFunction(() => document.title === 'Mira 标题菜单 · Mira');
  assert.equal(await page.locator('#conversationInput').inputValue(), 'Keep my draft');
  assert.equal(titleJobs[0].unsubscribed, true);
  assert.equal(mainRequests.some(request => request.method === 'turn/start'), false, 'regeneration does not add a main conversation turn');
  await page.reload();
  await page.waitForFunction(() => document.title === 'Mira 标题菜单 · Mira');
  console.log('CHECK cancellation');
  await regenerate(); await waitForJob(2);
  await page.locator('#conversationMenuToggle').click(); await page.locator('#threadCancelTitle').click();
  await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('已取消生成'));
  assert.equal(titleJobs[1].interrupted, true); assert.equal(titleJobs[1].unsubscribed, true);
  assert.equal((await getStoreHead(pool, 'personal')).state.names[id], 'Mira 标题菜单');
  console.log('CHECK concurrent rename');
  await regenerate(); await waitForJob(3);
  assert.equal((await renameThread(pool, 'personal', id, { name: '人手修改的标题', expectedName: 'Mira 标题菜单', generation: 1, operationId: crypto.randomUUID() })).status, 200);
  titleJobs[2].complete('过期的生成结果');
  await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('其他窗口更新'));
  assert.equal((await getStoreHead(pool, 'personal')).state.names[id], '人手修改的标题');
  await page.reload(); await page.waitForFunction(() => document.title === '人手修改的标题 · Mira');
  node.status = 'offline';
  await regenerate();
  await page.waitForFunction(() => document.querySelector('#toast').textContent.includes('连接并启动'));
  assert.equal(titleJobs.length, 3); node.status = 'online';
  console.log('CHECK automatic generation');
  // First submission failure retains automatic-title eligibility; delayed central projection is retried.
  await page.locator('.thread-project').filter({ has: page.locator(`[data-thread-row="${id}"]`) }).locator('[data-project-new]').click();
  await page.locator('#conversationInput').fill('修复新对话首次提交后的自动标题');
  await page.locator('#conversationInput').press('Enter');
  await page.waitForFunction(() => document.querySelector('#conversationNotice').textContent.includes('first submission failed'));
  assert.equal(titleJobs.length, 3);
  await page.locator('#conversationInput').press('Enter'); await waitForJob(4);
  assert.match(titleJobs[3].prompt, /修复新对话首次提交后的自动标题/);
  assert.equal(mainRequests.filter(r => r.method === 'thread/start').length, 1, 'retry keeps the created conversation');
  await page.locator('#conversationInput').fill('Draft while main reply is running');
  titleJobs[3].complete('修复新对话自动标题');
  await page.waitForFunction(() => document.title === '修复新对话自动标题 · Mira');
  assert.equal(await page.locator('#conversationInput').inputValue(), 'Draft while main reply is running');
  assert.equal(await page.locator('#agentInterrupt').isVisible(), true, 'main reply continues');
  await page.locator('#conversationInput').press('Enter');
  await page.waitForFunction(() => document.querySelector('#conversationInput').value === '');
  assert.equal(titleJobs.length, 4, 'subsequent messages do not rename the conversation');
  const after = await getSnapshot(pool, 'personal');
  assert.deepEqual(after.snapshot.histories[id], history, 'title generation does not alter original history');
  assert.equal(after.snapshot.names[newId], '修复新对话自动标题');
  for (const tempId of temporaryIds) {
    assert.equal(Object.hasOwn(after.snapshot.histories, tempId), false);
    assert.equal(await page.locator(`[data-thread-row="${tempId}"]`).count(), 0);
  }
  assert.equal((await pool.query("SELECT node_id FROM mira_codex_thread_runtimes WHERE store_id='personal' AND thread_id=$1", [id])).rows[0].node_id, node.nodeId);
  assert.deepEqual(errors, []);
  console.log('PASS: manual/automatic titles, retries, projection delay, cancellation, concurrent rename, offline handling, drafts/live turns, durable reload and history isolation');
} finally { await browser?.close(); await pool.end(); }
