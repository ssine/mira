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
const nodes = ["Machine A", "Machine B"].map((hostname, i) => ({ nodeId: crypto.randomUUID(), hostname, platform: "linux", nodeMode: i === 0 ? "wsl" : "linux", status: "online", capabilities: { appServer: true }, reportedAppServer: { status: "running" }, desiredAppServer: { defaultCwd: "/default" } }));
const paths = ["/work/project", "/work/project", "/work/project", "/home/developer/workspaces/clients/a-very-long-parent-directory/elsewhere"];
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
  let forkCreations = 0, loseTitleReply = true;
  const titleRequests = [];
  await context.route('**/fork-title?storeId=personal', async route => {
    titleRequests.push(route.request().postData());
    const response = await route.fetch();
    assert.equal(response.status(), 200);
    if (loseTitleReply) { loseTitleReply = false; return route.abort('failed'); }
    return route.fulfill({ response });
  });
  await context.routeWebSocket(/\/app-server\?storeId=personal$/, socket=>{ let accountConnection=false; socket.onMessage(async data=>{
    const request=JSON.parse(data); if (request.id === undefined) return;
    if(request.method==='initialize') accountConnection=request.params.clientInfo.name==='mira_web_account';
    if(accountConnection) { socket.send(JSON.stringify({id:request.id,result:request.method==='account/read'?{account:null}:{}})); return; }
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
      if (forkCreations) { send({thread:{id:forkId,name:'Runtime source name'},cwd:'/work/project'}); return; }
      forkCreations++;
      const original=await getSnapshot(pool,'personal');
      original.snapshot.histories[forkId]=[];
      original.snapshot.metadata_updates[forkId]={title:'Forked thread',cwd:'/work/project',updated_at:new Date().toISOString()};
      assert.equal((await putSnapshot(pool,'personal',{expectedVersion:original.version,snapshot:original.snapshot},{'x-codex-operation-id':crypto.randomUUID()})).status,200);
      await pool.query("INSERT INTO mira_codex_thread_runtimes (store_id,thread_id,node_id) VALUES ('personal',$1,$2)",[forkId,nodes[0].nodeId]);
      send({thread:{id:forkId,name:'Runtime source name'},cwd:'/work/project'});
    }
    else if(request.method==='thread/start')send({thread:{id:crypto.randomUUID()},cwd:request.params.cwd});
    else if(request.method==='turn/start')send({turn:{id:'active-turn',status:'inProgress'}});
    else if(request.method==='turn/interrupt') {
      if(rejectStop) { rejectStop=false; socket.send(JSON.stringify({id:request.id,error:{message:'Try stopping again'}})); }
      else { send({}); socket.send(JSON.stringify({method:'turn/completed',params:{threadId:request.params.threadId,turn:{id:request.params.turnId,status:'interrupted'}}})); }
    } else throw Error('unexpected RPC '+request.method);
  }); });
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
  const firstProject = page.locator('.thread-project').first();
  const projectSummary = firstProject.locator('.thread-project-summary');
  assert.equal(await firstProject.locator('.thread-project-count').textContent(), '2 对话');
  assert.equal(await firstProject.locator('.thread-project-platform').textContent(), 'WSL');
  assert.equal(await page.locator('.thread-project').nth(1).locator('.thread-project-platform').count(), 0, 'Linux machines are not mislabeled as WSL');
  const projectPath = firstProject.locator('.thread-project-path');
  assert.equal(await projectPath.evaluate(e => getComputedStyle(e).direction), 'rtl', 'path ellipsis starts on the left');
  assert.equal(await projectPath.locator('bdi').getAttribute('dir'), 'ltr', 'native path order is preserved');
  assert.match(await firstProject.locator('.thread-project-location').getAttribute('title'), /Machine A · WSL · \/work\/project/);
  const longPath = page.locator('.thread-project').nth(2).locator('.thread-project-path');
  const suffixVisible = await longPath.evaluate(path => {
    const text = path.querySelector('bdi').firstChild;
    const suffix = document.createRange();
    suffix.setStart(text, text.length - 'elsewhere'.length);
    suffix.setEnd(text, text.length);
    const visible = path.getBoundingClientRect();
    const tail = suffix.getBoundingClientRect();
    return path.scrollWidth > path.clientWidth && tail.left >= visible.left && tail.right <= visible.right + 1;
  });
  assert.equal(suffixVisible, true, 'overflow keeps the final project directory visible');
  assert.equal(await row.locator('[aria-current="page"]').count(), 1, 'selected conversation is announced');
  const hierarchy = await firstProject.evaluate(project => {
    const summary = project.querySelector('summary');
    const name = summary.querySelector('strong');
    const thread = project.querySelector('.agent-thread strong');
    return {
      indent: thread.getBoundingClientRect().left - name.getBoundingClientRect().left,
      projectSize: parseFloat(getComputedStyle(name).fontSize),
      threadSize: parseFloat(getComputedStyle(thread).fontSize),
      header: getComputedStyle(summary).backgroundColor,
      list: getComputedStyle(project.closest('.chat-drawer')).backgroundColor,
    };
  });
  assert.ok(Math.abs(hierarchy.indent) <= 1, 'project and conversation titles share the available width');
  assert.ok(hierarchy.projectSize > hierarchy.threadSize, 'project headings have a distinct type hierarchy');
  assert.notEqual(hierarchy.header, hierarchy.list, 'project headers have their own surface');
  assert.equal(await firstProject.locator('summary svg').count(), 0, 'project headers have no leading folder icon');
  assert.equal(await page.locator('[data-open-thread-window]').count(), 0, 'rows have no separate new-window action');
  await projectSummary.focus();
  await projectSummary.press('Enter');
  await page.waitForFunction(() => !document.querySelector('.thread-project').open);
  assert.equal(await row.isVisible(), false, 'keyboard collapse hides conversations');
  assert.equal(await firstProject.locator('.thread-project-count').isVisible(), true, 'collapsed projects retain their conversation count');
  await projectSummary.press('Enter');
  await row.waitFor({ state: 'visible' });
  for (const theme of ['light', 'dark']) {
    if (await page.locator('html').getAttribute('data-theme') !== theme) await page.locator('#agentThemeToggle').click();
    assert.equal(await row.locator('[aria-current="page"]').isVisible(), true);
    if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
      await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR, { recursive: true });
      await page.screenshot({ path: `${process.env.MIRA_WEB_SCREENSHOT_DIR}/chat-projects-desktop-${theme}.png`, animations: 'disabled' });
    }
  }
  await page.emulateMedia({ forcedColors: 'active', reducedMotion: 'reduce' });
  assert.equal(await row.locator('.agent-thread.active').evaluate(e => getComputedStyle(e).borderLeftStyle), 'solid', 'selection remains marked in forced colors');
  await page.emulateMedia({ forcedColors: 'none', reducedMotion: 'no-preference' });
  await page.locator('#agentThemeToggle').click();
  const rowMenu = row.locator('[data-thread-menu]');
  await page.mouse.move(1400, 850);
  assert.equal(await rowMenu.evaluate(e => getComputedStyle(e).opacity), '0', 'mouse users see no menu until hover or focus');
  await row.hover();
  assert.equal(await rowMenu.evaluate(e => getComputedStyle(e).opacity), '1', 'hover reveals the menu');
  await page.mouse.move(1400, 850);
  await row.locator('[data-thread-id]').focus();
  assert.equal(await rowMenu.evaluate(e => getComputedStyle(e).opacity), '1', 'keyboard focus reveals the menu');
  await row.locator('[data-thread-id]').press('Tab');
  assert.equal(await rowMenu.evaluate(e => e === document.activeElement), true, 'the menu remains keyboard accessible');
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
  await page.waitForFunction(() => document.querySelector('#conversationNotice').textContent.includes('Failed to fetch'));
  assert.equal((await getSnapshot(pool,'personal')).snapshot.names[forkId], '项目的新标题 (1)');
  await page.locator('#conversationMenuToggle').click();
  await page.locator('#threadFork').click();
  await page.waitForURL(`**/?thread=${forkId}`);
  await page.waitForFunction(() => document.title === '项目的新标题 (1) · Mira');
  assert.equal((await getSnapshot(pool,'personal')).snapshot.names[forkId], '项目的新标题 (1)');
  await page.waitForFunction(()=>!document.querySelector('#agentNewThread').disabled);
  assert.equal(await page.locator('#conversationInput').inputValue(),'Draft kept while creating a fork');
  assert.equal(messages.filter(m=>m.method==='thread/fork').length,2);
  assert.equal(forkCreations,1,'retry reuses the acknowledged fork');
  assert.equal(titleRequests.length,2);
  assert.equal(titleRequests[0],titleRequests[1],'lost title response reuses the exact operation and body');
  assert.equal((await context.request.post(`${origin}/v1/codex/threads/${forkId}/fork-title`,{data:JSON.parse(titleRequests[0])})).status(),403);
  assert.equal((await fetch(`${origin}/v1/codex/threads/${forkId}/fork-title`,{method:'POST',headers:{'content-type':'application/json'},body:titleRequests[0]})).status,401);
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
  assert.equal(await page.locator('.thread-project').first().evaluate(e => e.open), true, 'new conversation keeps its project expanded');
  assert.equal(await page.locator('#conversationCwd').inputValue(),'/work/project');
  assert.equal(await page.locator('#agentRuntimeNode').inputValue(),nodes[0].nodeId);
  await page.locator('#conversationInput').fill('Start in this project');
  await page.locator('#conversationInput').press('Enter');
  await page.locator('#agentInterrupt:not(.hidden)').waitFor();
  assert.equal(messages.find(m=>m.method==='thread/start').params.cwd,'/work/project');
  const createdThreadId = new URL(page.url()).searchParams.get('thread');
  const createdThreadRow = page.locator(`[data-thread-row="${createdThreadId}"]`);
  assert.equal(await createdThreadRow.count(), 1, 'creation immediately inserts a sidebar item without a reload');
  assert.equal(await createdThreadRow.locator('[aria-current="page"]').count(), 1);
  assert.equal(await createdThreadRow.locator('strong').textContent(), 'Start in this project');
  assert.equal(await createdThreadRow.evaluate(e => e.closest('.thread-project').dataset.projectKey), JSON.stringify([nodes[0].nodeId, '/work/project']));
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
  const draftProject = page.locator('.thread-project').filter({ hasText: 'new-project' });
  assert.equal(await draftProject.locator('.thread-project-count').textContent(), '0 对话');
  assert.equal(await draftProject.locator('.thread-project-empty').count(), 1, 'an empty project explains how to start its first conversation');
  await page.locator('#conversationInput').fill('First conversation in a new project');
  await page.locator('#conversationInput').press('Enter');
  await page.waitForFunction(() => document.querySelector('#conversationInput').value === '');
  const newProjectThreadId = new URL(page.url()).searchParams.get('thread');
  const newProjectRow = page.locator(`[data-thread-row="${newProjectThreadId}"]`);
  assert.equal(await newProjectRow.locator('strong').textContent(), 'First conversation in a new project');
  assert.equal(await newProjectRow.locator('[aria-current="page"]').count(), 1);
  assert.equal(await newProjectRow.evaluate(e => e.closest('.thread-project').dataset.projectKey), JSON.stringify([nodes[1].nodeId, '/work/new-project']));
  assert.equal(await draftProject.locator('.thread-project-count').textContent(), '1 对话');
  assert.equal(await draftProject.locator('.thread-project-empty').count(), 0, 'first submission replaces the empty project hint immediately');
  await page.locator('#agentThreadDrawerToggle').click();
  await page.locator(`[data-thread-row="${ids[1]}"]`).hover();
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
  assert.equal(await page.locator('.thread-project-count').textContent(), '1 对话', 'archive count reflects only visible conversations');
  await page.locator(`[data-thread-row="${ids[1]}"]`).hover();
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
  const touchContext = await browser.newContext({ storageState: await context.storageState(), viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true });
  const touchPage = await touchContext.newPage();
  await touchPage.goto(`${origin}/?thread=${ids[0]}`);
  await touchPage.locator('#agentView:not(.hidden)').waitFor();
  await touchPage.locator('#agentThreadDrawerToggle').tap();
  const touchMenu = touchPage.locator(`[data-thread-menu="${ids[0]}"]`);
  assert.equal(await touchPage.evaluate(() => matchMedia('(pointer: coarse)').matches), true);
  assert.equal(await touchMenu.evaluate(e => getComputedStyle(e).opacity), '1', 'touch menus remain visible without hover');
  await touchMenu.tap();
  assert.equal(await touchPage.locator('#threadRename').isVisible(), true, 'touch users can open conversation actions');
  await touchContext.close();
  assert.deepEqual(errors,[]);
  console.log('PASS: project identity/order/inheritance, hidden IDs, desktop/mobile menus, durable rename, fork navigation/source preservation, CSRF/auth, mobile Enter, stop/retry/completion and draft preservation');
  console.log('PASS: archive/reload/restore, mobile deletion confirmation/cancel, route cleanup, deletion replay and mutation authorization');
} finally { await browser?.close(); await pool.end(); }
