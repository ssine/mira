import { sidebarAction, accountDetails } from "./sidebar_browser_helpers.mjs";
// Real Server/login + browser; read-only account RPCs are simulated independently of chat.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
const { chromium } = await import(process.argv[2] ?? 'playwright');
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8789';
const nodeIds = ['10000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000003'];
const threadIds = ['20000000-0000-4000-8000-000000000001','20000000-0000-4000-8000-000000000002'];
const nodes = nodeIds.map((nodeId, i) => ({ nodeId, hostname: `Account node ${i + 1}`, platform: i === 1 ? 'windows' : 'linux', nodeMode: i === 0 ? 'wsl' : i === 1 ? 'windows' : 'linux', status: 'online', capabilities: { appServer: true }, reportedAppServer: { status: 'running' } }));
const accounts = [{ type:'chatgpt', email:'first@example.test', planType:'pro' }, { type:'chatgpt', email:'second@example.test', planType:'plus' }, null];
const reset = Math.floor(Date.now() / 1000) + 86400;
const limits = [0,1,2].map(i => ({ rateLimitsByLimitId: { codex: { limitId:'codex', primary: { usedPercent:1, windowDurationMins:300 }, secondary:{usedPercent:52 + i, windowDurationMins:10080,resetsAt:reset} }, codex_bengalfox: { limitId:'codex_bengalfox', primary:{usedPercent:0,windowDurationMins:10080,resetsAt:reset} } }, rateLimitResetCredits:{availableCount:1,credits:[]} }));
const sockets = new Map(), calls = [], errors = [];
let holdFirst = false, heldReply, failLimits = false;
const threads = threadIds.map((threadId,i) => ({ threadId, title:`Conversation ${i + 1}`, runtimeNodeId:nodeIds[i], cwd:i === 1 ? 'C:\\work' : '/work', generation:1 }));
const browser = await chromium.launch({headless:true,...(process.env.MIRA_BROWSER_EXECUTABLE ? {executablePath:process.env.MIRA_BROWSER_EXECUTABLE}:{})});
try {
 const context = await browser.newContext({viewport:{width:1440,height:1000},timezoneId:'Asia/Shanghai'});
 await context.route('**/v1/nodes',r=>r.fulfill({json:{data:nodes}}));
 for (const [i,node] of nodes.entries()) {
  await context.route(`**/v1/nodes/${node.nodeId}`,r=>r.fulfill({json:node}));
  await context.routeWebSocket(new RegExp(`/v1/nodes/${node.nodeId}/app-server`),socket=>{
   sockets.set(i,socket);
   socket.onMessage(data=>{
    const request=JSON.parse(data); if(request.id===undefined)return;
    calls.push({node:i,...request});
    const reply=result=>socket.send(JSON.stringify({id:request.id,result}));
    if(request.method==='initialize') {assert.equal(request.params.clientInfo.name,'mira_web_account');reply({});}
    else if(request.method==='account/read') {
     assert.equal(request.params.refreshToken,false);
     if(i===0&&holdFirst) heldReply=()=>reply({account:accounts[i]});
     else reply({account:accounts[i]});
    } else if(request.method==='account/rateLimits/read') {
     if(failLimits)socket.send(JSON.stringify({id:request.id,error:{message:'temporary usage failure'}}));
     else reply(limits[i]);
    } else throw Error(`Account inspection triggered unexpected RPC ${request.method}`);
   });
  });
 }
 await context.route('**/v1/codex/threads?*',r=>r.fulfill({json:{data:threads}}));
 for(const thread of threads) {
  await context.route(`**/v1/codex/threads/${thread.threadId}?*`,r=>r.fulfill({json:thread}));
  await context.route(`**/v1/codex/threads/${thread.threadId}/transcript?*`,r=>r.fulfill({json:{generation:1,trace:[],nextCursor:null}}));
 }
 const page=await context.newPage();page.setDefaultTimeout(10000);page.on('pageerror',e=>errors.push(e.message));
 await page.clock.install();
 await page.goto(origin);await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD??'mira-local-admin-password');
 await page.locator('#loginForm button[type=submit]').click();await page.locator('#dashboardView:not(.hidden)').waitFor();
 await page.goto(`${origin}/?thread=${threadIds[0]}`);
 const panel=page.locator('#agentAccount');
 const expectText=async(selector,text)=>{try{await page.waitForFunction(([selector,text])=>document.querySelector(selector)?.textContent===text,[`#agentAccount ${selector}`,text]);}catch(error){console.log({expected:[selector,text],panel:await panel.textContent(),calls,errors});throw error;}};
 const idle=()=>page.waitForFunction(()=>document.querySelector('#agentAccount').getAttribute('aria-busy')==='false');
 await accountDetails(page);
 await expectText('[data-account-remaining]','48%');
 await expectText('[data-account-email]','first@example.test');
 await expectText('[data-account-credits]','1 次');
 await expectText('[data-account-summary-credits]','重置 1 次');
 assert.equal(await panel.locator('[data-account-plan]').textContent(),'PRO');
 assert.match(await panel.locator('[data-account-node]').textContent(),/WSL/);
 assert.match(await panel.locator('[data-account-reset]').textContent(),/^\d\d\/\d\d \d\d:\d\d$/);
 assert.equal(await panel.locator('meter').getAttribute('aria-label'),'周额度剩余百分比');
 assert.equal(await panel.locator('meter').evaluate(e=>e.value),48);
 assert.ok(calls.every(r=>['initialize','account/read','account/rateLimits/read'].includes(r.method)));
 // Frequent quota notifications and drawer toggles share a five-minute cache.
 await idle(); const initialCalls=calls.length;
 limits[0].rateLimitsByLimitId.codex.secondary.usedPercent=100;limits[0].rateLimitResetCredits.availableCount=0;
 sockets.get(0).send(JSON.stringify({method:'account/rateLimits/updated',params:{rateLimits:{limitId:'codex'}}}));
 await page.waitForTimeout(400);
 assert.equal(calls.length,initialCalls,'quota notifications do not bypass the cache');
 await page.getByRole('button',{name:'关闭账户详情',exact:true}).click();
 for(let i=0;i<3;i++) {
  await page.locator('#agentThreadDrawerToggle').click();await page.locator('#agentThreadDrawerToggle').click();
  assert.match(await panel.locator('[data-account-summary-remaining]').textContent(),/^\d\d\/\d\d \d\d:\d\d 前剩余 48%$/);
 }
 await page.clock.fastForward(4*60_000);
 assert.equal(calls.length,initialCalls,'reopening the sidebar does not reconnect or request account data');
 await page.clock.fastForward(60_100);
 await expectText('[data-account-remaining]','0%');await expectText('[data-account-credits]','0 次');
 await expectText('[data-account-summary-credits]','重置 0 次');
 assert.equal(calls.filter(call=>call.method==='account/rateLimits/read').length,2,'one automatic refresh after five minutes');
 await accountDetails(page);
 // Missing fields stay unknown, rather than borrowing Spark's separate weekly quota.
 delete limits[0].rateLimitsByLimitId.codex.secondary;limits[0].rateLimitResetCredits=null;
 await idle();await panel.locator('[data-account-refresh]').click();await expectText('[data-account-remaining]','未提供');await expectText('[data-account-credits]','未提供');
 await expectText('[data-account-summary-credits]','重置未提供');
 assert.equal(await panel.locator('meter').isVisible(),false);
 limits[0].rateLimitsByLimitId.codex.secondary={usedPercent:52,windowDurationMins:10080,resetsAt:reset};limits[0].rateLimitResetCredits={availableCount:1};
 await idle();await panel.locator('[data-account-refresh]').click();await expectText('[data-account-remaining]','48%');
 failLimits=true;await idle();await panel.locator('[data-account-refresh]').click();
 await expectText('[data-account-status]','额度更新失败，显示上次结果');await expectText('[data-account-remaining]','48%');failLimits=false;
 await idle();await panel.locator('[data-account-refresh]').click();await idle();
 // A delayed reply from another Node cannot overwrite the current account or draft.
 holdFirst=true;await panel.locator('[data-account-refresh]').click();
 for(let i=0;!heldReply&&i<100;i++)await page.waitForTimeout(10);
 assert.ok(heldReply);
 await page.locator(`button[data-thread-id="${threadIds[1]}"]`).click();
 await expectText('[data-account-email]','second@example.test');await expectText('[data-account-remaining]','47%');
 try{heldReply();}catch{}holdFirst=false;
 await page.waitForTimeout(100);await expectText('[data-account-email]','second@example.test');
 assert.match(await panel.locator('[data-account-node]').textContent(),/Windows/);
 await accountDetails(page);
 // Account replacement clears previous values immediately, then loads the new identity.
 accounts[1]={type:'apiKey'};
 sockets.get(1).send(JSON.stringify({method:'account/updated',params:{authMode:'apikey'}}));
 await expectText('[data-account-email]','API Key 登录');
 assert.equal(await panel.locator('dl').isVisible(),false);
 assert.equal(await panel.locator('meter').isVisible(),false);
 const chooseNode=async index=>{await sidebarAction(page, "agentHome");await page.locator('#globalRuntime').click();await page.locator('#agentRuntimeNode').selectOption(nodeIds[index]);await page.locator('#runtimeOpenChat').click();};
 // Pick an unlogged Node on a new conversation, so its stored runtime does not override the selection.
 await page.locator('#agentNewThread').click();await chooseNode(2);
 await expectText('[data-account-status]','此节点的 Codex 尚未登录');
 assert.equal(await panel.locator('dl').isVisible(),false);
 nodes[2].status='offline';await sidebarAction(page, "agentHome");await page.locator('#globalRuntime').click();await page.locator('#agentRefresh').click();await page.locator('#runtimeOpenChat').click();
 await expectText('[data-account-status]','运行节点离线');
 assert.equal(await panel.locator('[data-account-refresh]').isDisabled(),true);
 const firstNodeReads=calls.filter(call=>call.node===0&&call.method==='account/read').length;
 nodes[2].status='online';await chooseNode(0);await expectText('[data-account-email]','first@example.test');await expectText('[data-account-remaining]','48%');
 assert.equal(calls.filter(call=>call.node===0&&call.method==='account/read').length,firstNodeReads,'switching back reuses the correct Node cache');
 // The compact block and all existing footer navigation remain usable on mobile.
 await page.setViewportSize({width:390,height:844});await page.locator('#agentThreadDrawerToggle').click();
 await expectText('[data-account-remaining]','48%');
 assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
 for (const selector of ['[data-account-summary-remaining]', '[data-account-summary-credits]']) {
  assert.equal(await panel.locator(selector).evaluate(element => element.scrollWidth > element.clientWidth), false, 'compact quota summary fits without truncation');
 }
 await accountDetails(page);
 await panel.locator('[data-account-refresh]').scrollIntoViewIfNeeded();
 assert.equal(await panel.locator('[data-account-refresh]').isVisible(),true);
 await sidebarAction(page, "agentThemeToggle");
 assert.equal(await page.locator('html').getAttribute('data-theme'),'dark');
 if(process.env.MIRA_WEB_SCREENSHOT_DIR) {
  await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR,{recursive:true});
  await panel.scrollIntoViewIfNeeded();await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/account-sidebar-mobile.png`});
  await accountDetails(page);await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/account-details-mobile.png`});
  await page.setViewportSize({width:1440,height:1000});await sidebarAction(page, "agentThemeToggle");
  await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/account-sidebar-desktop.png`});
  await accountDetails(page);await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/account-details-desktop.png`});
 }
 await sidebarAction(page, "agentLogout");await page.locator('#loginView:not(.hidden)').waitFor();
 assert.equal((await panel.textContent()).includes('first@example.test'),false,'logout clears account data');
 assert.equal(await panel.locator('[data-account-refresh]').isDisabled(),true);
 const beforeLogin=calls.filter(call=>call.node===0&&call.method==='account/read').length;
 await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD??'mira-local-admin-password');
 await page.locator('#loginForm button[type=submit]').click();await page.locator('#agentView:not(.hidden)').waitFor();
 await expectText('[data-account-email]','API Key 登录');
 await page.locator('#agentNewThread').click();await chooseNode(0);
 await expectText('[data-account-email]','first@example.test');
 assert.ok(calls.filter(call=>call.node===0&&call.method==='account/read').length>beforeLogin,'logout clears the cache before another login');
 assert.deepEqual(errors,[]);
 console.log('PASS: account email/plan, weekly window selection, reset time/credits, notifications, unknown/zero, stale replies, API key, logout/offline, mobile/dark layout and read-only RPC isolation');
}finally{await browser.close();}
