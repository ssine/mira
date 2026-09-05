// Real Server/PostgreSQL + browser; only Node execution is simulated.
// MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL and MIRA_SERVER_URL must point to a disposable test server.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs/promises";
import pg from "../server/node_modules/pg/lib/index.js";
import { putSnapshot, getSnapshot } from "../server/thread-store.mjs";
const { chromium } = await import(process.argv[2] ?? "playwright");
if (!process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL) throw new Error("a disposable database is required");
const origin = process.env.MIRA_SERVER_URL ?? "http://127.0.0.1:8789";
const pool = new pg.Pool({ connectionString: process.env.MIRA_THREAD_MANAGEMENT_TEST_DATABASE_URL });
const ids = Array.from({ length: 4 }, () => crypto.randomUUID());
const nodes = ["Machine A", "Machine B"].map((hostname) => ({ nodeId: crypto.randomUUID(), hostname, platform: "linux", status: "online", capabilities: { appServer: true }, reportedAppServer: { status: "running" }, desiredAppServer: { defaultCwd: "/default" } }));
const paths = ["/work/project", "/work/project", "/work/project", "/work/elsewhere"];
let browser;
try {
  for (const node of nodes) await pool.query(`INSERT INTO codex_nodes (node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status) VALUES ($1::uuid,$1::text,$2,'linux','amd64','linux','test','{"appServer":true}','[]','approved')`, [node.nodeId, node.hostname]);
  assert.equal((await putSnapshot(pool, "personal", { expectedVersion: 0, snapshot: {
    metadata_updates: Object.fromEntries(ids.map((id, i) => [id, { title: `Thread ${i}`, cwd: paths[i], updated_at: `2026-09-0${5-i}T10:00:00Z` }])),
    histories: Object.fromEntries(ids.map((id) => [id, []])),
  } }, { "x-codex-operation-id": crypto.randomUUID() })).status, 200);
  for (const [i,id] of ids.entries()) await pool.query("INSERT INTO mira_codex_thread_runtimes (store_id,thread_id,node_id) VALUES ('personal',$1,$2)", [id,nodes[i===2?1:0].nodeId]);
  browser = await chromium.launch({ headless: true, ...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {}) });
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, permissions: ["clipboard-read", "clipboard-write"] });
  const errors=[]; context.on("page", page=>page.on("pageerror",error=>errors.push(error.message)));
  await context.route("**/v1/nodes", route=>route.fulfill({json:{data:nodes}}));
  for (const node of nodes) await context.route(`**/v1/nodes/${node.nodeId}`, route=>route.fulfill({json:node}));
  const messages=[]; let rejectStop = true; const forkId=crypto.randomUUID();
  await context.routeWebSocket(/\/app-server\?storeId=personal$/, socket=>socket.onMessage(async data=>{
    const request=JSON.parse(data); if (request.id === undefined) return;
    messages.push(request);
    const send=result=>socket.send(JSON.stringify({id:request.id,result}));
    if(request.method==='initialize')send({});
    else if(request.method==='thread/resume')send({thread:{id:request.params.threadId,preview:'Runtime preview',status:{type:request.params.threadId===ids[0]?'active':'idle'}},cwd:'/work/project'});
    else if(request.method==='thread/turns/list') { assert.equal(request.params.limit,1); assert.equal(request.params.itemsView,'notLoaded'); send({data:[{id:'resumed-turn',status:'inProgress',items:[]}]}); }
    else if(request.method==='thread/loaded/list')send({data:ids});
    else if(request.method==='thread/fork') {
      assert.equal(request.params.excludeTurns,true);
      assert.equal(request.params.deferGoalContinuation,true);
      assert.match(request.params.miraRequestId,/^[0-9a-f-]{36}$/);
      const original=await getSnapshot(pool,'personal');
      original.snapshot.histories[forkId]=[];
      original.snapshot.metadata_updates[forkId]={title:'Forked thread',cwd:'/work/project',updated_at:new Date().toISOString()};
      assert.equal((await putSnapshot(pool,'personal',{expectedVersion:original.version,snapshot:original.snapshot},{'x-codex-operation-id':crypto.randomUUID()})).status,200);
      await pool.query("INSERT INTO mira_codex_thread_runtimes (store_id,thread_id,node_id) VALUES ('personal',$1,$2)",[forkId,nodes[0].nodeId]);
      send({thread:{id:forkId},cwd:'/work/project'});
    }
    else if(request.method==='thread/start')send({thread:{id:crypto.randomUUID()},cwd:request.params.cwd});
    else if(request.method==='turn/start')send({turn:{id:'active-turn',status:'inProgress'}});
    else if(request.method==='turn/interrupt') {
      if(rejectStop) { rejectStop=false; socket.send(JSON.stringify({id:request.id,error:{message:'Try stopping again'}})); }
      else { send({}); socket.send(JSON.stringify({method:'turn/completed',params:{threadId:request.params.threadId,turn:{id:request.params.turnId,status:'interrupted'}}})); }
    } else throw Error('unexpected RPC '+request.method);
  }));
  const page=await context.newPage();
  await page.goto(origin);
  await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD??'mira-local-admin-password');
  await page.locator('#loginForm button[type=submit]').click();
  await page.locator('#dashboardView:not(.hidden)').waitFor();
  await page.goto(`${origin}/?thread=${ids[0]}`);
  await page.locator('.thread-project').first().waitFor();
  await page.locator('#conversationInput').focus();
  assert.equal(messages.filter(message=>message.method==='thread/resume').length,0,'opening or focusing a conversation only reads history');
  await page.locator('#conversationInput').fill('Draft begins runtime preparation');
  await page.locator('#agentInterrupt:not(.hidden)').waitFor();
  assert.equal(await page.locator('.thread-project').count(),3,'same path on different machines is a different project');
  assert.deepEqual(await page.locator('.thread-project').first().locator('button[data-thread-id]').evaluateAll(es=>es.map(e=>e.dataset.threadId)),ids.slice(0,2),'newest conversations first within each project');
  for(const id of ids)assert.equal((await page.locator('#agentThreadList').textContent()).includes(id),false,'IDs are hidden in the reading list');
  const row=page.locator(`[data-thread-row="${ids[0]}"]`);
  assert.equal(await row.locator('strong').evaluate(e=>getComputedStyle(e).fontSize),'13px');
  await row.click({button:'right'});
  await page.locator('#threadCopyId').click();
  assert.equal(await page.evaluate(()=>navigator.clipboard.readText()),ids[0]);
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadRename').click();
  await page.locator('#threadRenameInput').fill('项目的新标题');
  await page.locator('#threadRenameSave').click();
  await page.waitForFunction(()=>document.title==='项目的新标题 · Mira');
  await page.reload();
  await page.waitForFunction(()=>document.title==='项目的新标题 · Mira');
  // Server authorization and CSRF are enforced on the new mutation route.
  const payload={name:'CSRF attack',expectedName:'项目的新标题',generation:1,operationId:crypto.randomUUID()};
  const forbidden=await context.request.patch(`${origin}/v1/codex/threads/${ids[0]}`,{data:payload});
  assert.equal(forbidden.status(),403);
  assert.equal((await fetch(`${origin}/v1/codex/threads/${ids[0]}`,{method:'PATCH',headers:{'content-type':'application/json'},body:JSON.stringify(payload)})).status,401);
  await page.locator('#conversationInput').fill('Draft kept while creating a fork');
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadFork').click();
  await page.waitForURL(`**/?thread=${forkId}`);
  await page.waitForFunction(()=>!document.querySelector('#agentNewThread').disabled);
  assert.equal(await page.locator('#conversationInput').inputValue(),'Draft kept while creating a fork');
  assert.equal(messages.filter(m=>m.method==='thread/fork').length,1);
  assert.equal(await page.locator(`[data-thread-row="${ids[0]}"]`).count(),1,'fork preserves the source conversation');
  assert.equal(await page.locator(`[data-thread-row="${forkId}"]`).evaluate(e=>e.closest('.thread-project').dataset.projectKey),await page.locator(`[data-thread-row="${ids[0]}"]`).evaluate(e=>e.closest('.thread-project').dataset.projectKey));
  await page.locator(`button[data-thread-id="${ids[0]}"]`).click();
  await page.waitForFunction(()=>document.title==='项目的新标题 · Mira');
  // Mobile top-right menu and dialog remain usable without reopening the sidebar.
  await page.setViewportSize({width:390,height:844});
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadRename').click();
  await page.locator('#threadRenameInput').fill('手机编辑标题');
  await page.locator('#threadRenameSave').click();
  await page.waitForFunction(()=>document.title==='手机编辑标题 · Mira');
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  await page.locator('#agentThreadDrawerToggle').click();
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
    await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/chat-projects-mobile.png`, animations: "disabled" });
  }
  await page.locator('.thread-project').first().locator('[data-project-new]').click();
  assert.equal(await page.locator('#conversationCwd').inputValue(),'/work/project');
  assert.equal(await page.locator('#agentRuntimeNode').inputValue(),nodes[0].nodeId);
  await page.locator('#conversationInput').fill('Start in this project');
  await page.locator('#conversationInput').press('Enter');
  await page.locator('#agentInterrupt:not(.hidden)').waitFor();
  assert.equal(messages.find(m=>m.method==='thread/start').params.cwd,'/work/project');
  assert.equal(await page.locator('#agentInterrupt').evaluate(e=>e.closest('#conversationForm')!==null),true);
  assert.equal(await page.locator('#conversationSend').isVisible(),false);
  await page.waitForFunction(()=>document.querySelector('#conversationInput').value==='');
  await page.locator('#conversationInput').fill('Mobile follow up');
  await page.locator('#conversationInput').evaluate(e=>e.dispatchEvent(new InputEvent('beforeinput',{inputType:'insertLineBreak',bubbles:true,cancelable:true})));
  await page.waitForFunction(()=>document.querySelector('#conversationInput').value==='');
  assert.equal(messages.filter(m=>m.method==='turn/start').length,2,'mobile Enter adds input instead of interrupting');
  assert.equal(messages.filter(m=>m.method==='turn/interrupt').length,0);
  await page.locator('#conversationInput').fill('Draft survives stop');
  await page.locator('#agentInterrupt').click();
  await page.waitForFunction(()=>document.querySelector('#toast').textContent==='Try stopping again');
  assert.equal(await page.locator('#conversationInput').inputValue(),'Draft survives stop');
  await page.locator('#agentInterrupt').click();
  await page.locator('#conversationSend:not(.hidden)').waitFor();
  assert.equal(messages.filter(m=>m.method==='turn/interrupt').length,2);
  assert.equal(await page.locator('#conversationInput').inputValue(),'Draft survives stop');
  // New project chooses its machine and directory, not that machine's default cwd.
  await page.locator('#agentThreadDrawerToggle').click();
  await page.locator('#agentNewProject').click();
  await page.locator('#projectNode').selectOption(nodes[1].nodeId);
  await page.locator('#projectPath').fill('/work/new-project');
  await page.locator('#projectForm button[type=submit]').click();
  assert.equal(await page.locator('#agentRuntimeNode').inputValue(),nodes[1].nodeId);
  assert.equal(await page.locator('#conversationCwd').inputValue(),'/work/new-project');
  await page.locator('#agentThreadDrawerToggle').click();
  await page.locator(`[data-thread-menu="${ids[1]}"]`).click();
  await page.locator('#threadArchive').click();
  await page.waitForFunction(id=>!document.querySelector(`[data-thread-row="${id}"]`),ids[1]);
  await page.reload();
  await page.locator('.thread-project').first().waitFor();
  assert.equal(await page.locator(`[data-thread-row="${ids[1]}"]`).count(),0,'archive survives reload');
  await page.locator('#agentThreadDrawerToggle').click();
  await page.locator('#agentArchiveToggle').click();
  await page.locator(`[data-thread-row="${ids[1]}"]`).waitFor();
  assert.equal(await page.locator('[data-thread-row]').count(),1,'archive view excludes normal conversations');
  await page.locator(`[data-thread-menu="${ids[1]}"]`).click();
  assert.equal(await page.locator('#threadArchive').textContent(),'恢复对话');
  await page.locator('#threadArchive').click();
  await page.waitForFunction(()=>document.querySelectorAll('[data-thread-row]').length===0);
  await page.locator('#agentArchiveToggle').click();
  await page.locator(`button[data-thread-id="${ids[1]}"]`).click();
  await page.waitForURL(`**/?thread=${ids[1]}`);
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadDelete').click();
  await page.locator('#threadDeleteCancel').click();
  assert.equal((await context.request.get(`${origin}/v1/codex/threads/${ids[1]}`)).status(),200,'cancel never deletes');
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadDelete').click();
  const deleting=page.waitForRequest(request=>request.method()==='DELETE');
  let cleanupRefreshes=0;
  page.on('request',request=>{if(request.method()==='GET' && new URL(request.url()).pathname==='/v1/codex/threads') cleanupRefreshes++;});
  await page.locator('#threadDeleteConfirm').click();
  const deletion=await deleting;
  await page.waitForURL(url=>!url.searchParams.has('thread'));
  await page.waitForFunction(()=>document.querySelector('#toast').textContent==='对话已删除');
  assert.equal(await page.locator('#threadDeleteDialog').evaluate(dialog=>dialog.open),false);
  assert.equal(await page.locator(`[data-thread-row="${ids[1]}"]`).count(),0);
  assert.equal((await context.request.get(`${origin}/v1/codex/threads/${ids[1]}`)).status(),404,'deleted conversation has no DB projection');
  const session=await (await context.request.get(`${origin}/v1/admin/session`)).json();
  const replay=await context.request.delete(deletion.url(),{data:deletion.postDataJSON(),headers:{'x-mira-csrf':session.csrfToken}});
  assert.equal(replay.status(),200); assert.equal((await replay.json()).duplicate,true);
  for(const [method,route] of [['delete',`/v1/codex/threads/${ids[2]}`],['post',`/v1/codex/threads/${ids[2]}/archive`]]) {
    assert.equal((await context.request[method](origin+route,{data:{}})).status(),403,'thread actions require CSRF');
    assert.equal((await fetch(origin+route,{method:method.toUpperCase(),headers:{'content-type':'application/json'},body:'{}'})).status,401,'thread actions require authentication');
  }
  await page.waitForTimeout(3200);
  assert.equal(cleanupRefreshes,0,'successful deletion does not wait for or poll background erasure');
  assert.deepEqual(errors,[]);
  console.log('PASS: project identity/order/inheritance, hidden IDs, desktop/mobile menus, durable rename, fork navigation/source preservation, CSRF/auth, mobile Enter, stop/retry/completion and draft preservation');
  console.log('PASS: archive/reload/restore, mobile deletion confirmation/cancel, route cleanup, deletion replay and mutation authorization');
} finally { await browser?.close(); await pool.end(); }
