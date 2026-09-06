import { sidebarAction } from "./sidebar_browser_helpers.mjs";
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
const { chromium } = await import(process.argv[2] ?? 'playwright');
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8787';
const nodeId = crypto.randomUUID(), threadId = crypto.randomUUID();
const node = {nodeId,hostname:'Model test',platform:'linux',status:'online',capabilities:{appServer:true},reportedAppServer:{status:'running'},desiredAppServer:{defaultCwd:'/work'}};
const rows = [], modelCalls = [], mainCalls = [];
let loseCreationReply = true;
let configured = 'gpt-6-astra', holdConfig = null, failConfig = false, deferConfig = false;

const browser = await chromium.launch({headless:true});
try {
  const context = await browser.newContext({viewport:{width:1200,height:900}});
  await context.route('**/v1/nodes', route => route.fulfill({json:{data:[node]}}));
  await context.route(`**/v1/nodes/${nodeId}`, route => route.fulfill({json:node}));
  await context.route('**/v1/codex/threads?*', route => route.fulfill({json:{data:rows}}));
  await context.route(/\/v1\/codex\/threads\/[^?]+\?/, route => {
    const path = new URL(route.request().url()).pathname;
    return route.fulfill({json:path.endsWith('/transcript')?{generation:1,itemCount:0,trace:[],nextCursor:null}:rows[0]});
  });
  await context.routeWebSocket(/\/app-server\?storeId=personal$/, socket => {
    let client;
    socket.onMessage(data => {
      const request = JSON.parse(data); if (request.id === undefined) return;
      if (request.method === 'initialize') client = request.params.clientInfo.name;
      const reply = result => socket.send(JSON.stringify({id:request.id,result}));
      if (client === 'mira_web_account') { reply(request.method === 'account/read'?{account:null}:{});return; }
      if (client === 'mira_web_title') { socket.send(JSON.stringify({id:request.id,error:{message:'Title execution disabled in this fixture'}}));return; }
      if (client === 'mira_web_models') {
        modelCalls.push(request);
        if (request.method === 'initialize') reply({});
        else if (request.method === 'config/read') {
          assert.equal(request.params.includeLayers,false);
          if (failConfig) socket.send(JSON.stringify({id:request.id,error:{message:'Configuration unavailable'}}));
          else if (deferConfig) { const value=configured; holdConfig=()=>reply({config:{model:value}}); }
          else reply({config:{model:configured}});
        } else if (request.method === 'model/list') reply(request.params.cursor
          ? {data:[{model:'gpt-5.6-luna'}],nextCursor:null}
          : {data:[{model:'gpt-6-astra',isDefault:true},{model:'gpt-5.6-sol'}],nextCursor:'page2'});
        else throw Error('Model picker attempted execution: '+request.method);
        return;
      }
      mainCalls.push(request);
      if (request.method === 'initialize') reply({});
      else if (request.method === 'thread/start') {
        if (!rows.length) rows.push({threadId,title:'Model choices',cwd:request.params.cwd,model:request.params.model,runtimeNodeId:nodeId,generation:1,itemCount:0,activity:{state:'idle'},tokenUsage:null});
        if (loseCreationReply) { loseCreationReply=false; socket.send(JSON.stringify({id:request.id,error:{message:'Creation reply lost'}}));return; }
        reply({thread:{id:threadId},cwd:request.params.cwd,model:request.params.model});
      } else if (request.method === 'turn/start') {
        rows[0].model=request.params.model;
        reply({turn:{id:crypto.randomUUID(),status:'completed'}});
      } else if (request.method === 'thread/resume') reply({thread:{id:threadId,status:{type:'idle'}},cwd:'/work',model:'gpt-6-astra'});
      else if (request.method === 'thread/loaded/list') reply({data:[threadId]});
      else throw Error('Unexpected main method '+request.method);
    });
  });
  const page=await context.newPage(), errors=[];
  page.on('pageerror',error=>errors.push(error.message));
  page.setDefaultTimeout(10000);
  await page.goto(origin);await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD??'mira-local-admin-password');
  await page.locator('#loginForm button[type=submit]').click();await page.locator('#dashboardView:not(.hidden)').waitFor();
  await page.goto(`${origin}/?view=agent`);
  const picker=page.locator('#conversationModelSelect');
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(await picker.inputValue(),'gpt-6-astra');
  assert.match(await picker.locator('option:checked').textContent(), /默认/);
  assert.equal(await picker.locator('option').count(),3,'all catalog pages loaded');
  assert.equal(mainCalls.length,0,'previewing defaults does not resume/create a thread');
  assert.equal(modelCalls.find(call=>call.method==='config/read').params.cwd,'/work');
  await picker.selectOption('gpt-5.6-sol');
  await page.locator('#conversationInput').fill('Use selected model');await page.locator('#conversationSend').click();
  await page.waitForFunction(()=>document.querySelector('#conversationNotice').textContent.includes('Creation reply lost'));
  await picker.selectOption('gpt-5.6-luna');await page.locator('#conversationSend').click();
  await page.waitForURL(`**/?thread=${threadId}`);
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(mainCalls.find(call=>call.method==='thread/start').params.model,'gpt-5.6-sol');
  assert.equal(mainCalls.find(call=>call.method==='turn/start').params.model,'gpt-5.6-luna');
  const starts=mainCalls.filter(call=>call.method==='thread/start');
  assert.deepEqual(starts[0].params,starts[1].params,'lost creation retries keep the same UUID and body after a model change');
  await picker.selectOption('gpt-5.6-sol');
  await page.locator('#conversationInput').fill('Switch for the next turn');await page.locator('#conversationSend').click();
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(mainCalls.filter(call=>call.method==='turn/start').at(-1).params.model,'gpt-5.6-sol');
  assert.equal(mainCalls.filter(call=>call.method==='thread/start').length,2,'switching models preserves the thread');
  await page.locator('#conversationDetailsToggle').click();
  await page.waitForFunction(()=>document.querySelector('#conversationDetailsStatus').textContent==='');
  assert.match(await page.locator('#conversationDetailsFacts').textContent(),/最近使用的模型gpt-5.6-sol/);
  await page.locator('#conversationDetailsClose').click();
  // A new draft uses the Node default, not the previous thread's override.
  const reads=modelCalls.length;
  await page.locator('#agentNewThread').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='gpt-6-astra');
  assert.equal(modelCalls.length,reads,'same-node/project defaults are cached');
  configured='custom-provider-model';
  await page.locator('#conversationModelRefresh').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='custom-provider-model');
  assert.equal(await picker.locator('option').count(),4,'configured models outside the catalog remain selectable');
  // Late responses for another directory cannot replace this project's model.
  configured='old-project-model';deferConfig=true;
  await page.locator('#conversationModelRefresh').click();
  while(!holdConfig) await new Promise(resolve=>setTimeout(resolve,10));
  configured='new-project-model';deferConfig=false;
  await sidebarAction(page, 'agentNewProject');
  await page.locator('#projectPath').fill('/another-project');
  await page.locator('#projectForm button[type=submit]').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='new-project-model');
  holdConfig();
  await page.waitForTimeout(100);
  assert.equal(await picker.inputValue(),'new-project-model');
  // Failed reads on another scope must not display the previous default.
  failConfig=true;
  await sidebarAction(page, 'agentNewProject');await page.locator('#projectPath').fill('/unreadable');
  await page.locator('#projectForm button[type=submit]').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelStatus').textContent==='Configuration unavailable');
  assert.equal(await picker.inputValue(),'');assert.equal(await picker.isDisabled(),true);
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  assert.deepEqual(errors,[]);
  console.log('PASS: concrete defaults, read-only paginated catalog, caching, new-thread/turn model parameters, switching, latest-model details, custom defaults, project response races, failure clearing and mobile width');
} finally { await browser.close(); }
