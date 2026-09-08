import { sidebarAction } from "./sidebar_browser_helpers.mjs";
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
const { chromium } = await import(process.argv[2] ?? 'playwright');
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8787';
const nodeId = crypto.randomUUID(), threadId = crypto.randomUUID();
const node = {nodeId,hostname:'Model test',platform:'linux',status:'online',approvalStatus:'approved',capabilities:{appServer:true},reportedAppServer:{status:'running'},desiredAppServer:{defaultCwd:'/work'}};
const rows = [], modelCalls = [], mainCalls = [];
let loseCreationReply = true;
let configured = 'gpt-6-astra', configuredEffort = 'medium', holdConfig = null, failConfig = false, deferConfig = false;
const advertisedModel = (model, displayName) => ({model,displayName,description:`${displayName} model`,
  isDefault:model==='gpt-6-astra',defaultReasoningEffort:'medium',supportedReasoningEfforts:[
    {reasoningEffort:'low',description:'更快响应'},{reasoningEffort:'medium',description:'均衡思考'},
    {reasoningEffort:'high',description:'更深入思考'}]});

const browser = await chromium.launch({headless:true});
try {
  const context = await browser.newContext({viewport:{width:1200,height:900}});
  await context.route('**/v1/nodes', route => route.fulfill({json:{data:[node]}}));
  await context.route('**/v1/nodes?*', route => route.fulfill({json:{data:[node]}}));
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
          else if (deferConfig) { const value=configured, effort=configuredEffort; holdConfig=()=>reply({config:{model:value,model_reasoning_effort:effort}}); }
          else reply({config:{model:configured,model_reasoning_effort:configuredEffort}});
        } else if (request.method === 'model/list') reply(request.params.cursor
          ? {data:[advertisedModel('gpt-5.6-luna','Luna')],nextCursor:null}
          : {data:[advertisedModel('gpt-6-astra','Astra'),advertisedModel('gpt-5.6-sol','Sol')],nextCursor:'page2'});
        else throw Error('Model picker attempted execution: '+request.method);
        return;
      }
      mainCalls.push(request);
      if (request.method === 'initialize') reply({});
      else if (request.method === 'thread/start') {
        if (!rows.length) rows.push({threadId,title:'Model choices',cwd:request.params.cwd,model:request.params.model,runtimeNodeId:nodeId,generation:1,itemCount:0,activity:{state:'idle'},tokenUsage:null});
        if (loseCreationReply) { loseCreationReply=false; socket.send(JSON.stringify({id:request.id,error:{message:'Creation reply lost'}}));return; }
        reply({thread:{id:threadId},cwd:request.params.cwd,model:request.params.model,
          reasoningEffort:request.params.config?.model_reasoning_effort});
      } else if (request.method === 'turn/start') {
        rows[0].model=request.params.model;
        reply({turn:{id:crypto.randomUUID(),status:'completed'}});
      } else if (request.method === 'thread/resume') reply({thread:{id:threadId,status:{type:'idle'}},cwd:'/work',model:'gpt-6-astra',reasoningEffort:'high'});
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
  const picker=page.locator('#conversationModelSelect'), effortPicker=page.locator('#conversationEffortSelect');
  const modelValue=()=>picker.evaluate(element=>element.value);
  const effortValue=()=>effortPicker.evaluate(element=>element.value);
  const chooseModel=async value=>{await picker.click();await page.locator(`[data-model-choice="${value}"]`).click();};
  const chooseEffort=async value=>{await effortPicker.click();await page.locator(`[data-effort-choice="${value}"]`).click();};
  const goToNodes=async()=>{await page.locator('#agentNavMenuToggle').click();await page.locator('#agentHome').click();await page.locator('#dashboardView:not(.hidden)').waitFor();};
  const goToAgent=async()=>{await page.locator('#agentConsoleButton').click();await page.locator('#agentView:not(.hidden)').waitFor();};
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(await modelValue(),'gpt-6-astra');
  assert.equal(await effortValue(),'medium');
  assert.equal(await picker.evaluate(element=>element.tagName),'BUTTON','model picker uses the styled menu control');
  assert.equal(await page.locator('#conversationModelRefresh').count(),0,'low-frequency refresh is absent from the composer');
  await picker.click();
  assert.equal(await page.locator('#conversationModelMenu .composer-choice-option').count(),3,'all catalog pages loaded');
  assert.match(await page.locator('[data-model-choice="gpt-6-astra"] small').textContent(),/默认/);
  assert.ok((await page.locator('#conversationModelMenu').boundingBox()).y < (await picker.boundingBox()).y,'model menu opens above the bottom toolbar');
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) {
    await fs.mkdir(process.env.MIRA_WEB_SCREENSHOT_DIR,{recursive:true});
    await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/composer-model-menu.png`});
  }
  await picker.click();
  await effortPicker.click();
  assert.equal(await page.locator('#conversationEffortMenu .composer-choice-option').count(),3,'effort menu follows the selected model capabilities');
  assert.equal(await page.locator('[data-effort-choice="medium"]').getAttribute('aria-selected'),'true');
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/composer-effort-menu.png`});
  await page.keyboard.press('ArrowDown');
  assert.equal(await page.locator('[data-effort-choice="medium"]').evaluate(element=>element===document.activeElement),true,'effort menu supports keyboard navigation');
  await page.keyboard.press('Escape');
  await page.waitForFunction(()=>document.querySelector('#conversationEffortSelect').getAttribute('aria-expanded')==='false');
  assert.equal(await effortPicker.getAttribute('aria-expanded'),'false');
  const inputBox=await page.locator('#conversationInput').boundingBox(), zoneBox=await page.locator('#conversationDropZone').boundingBox();
  const toolbarBox=await page.locator('.composer-toolbar').boundingBox();
  assert.ok(inputBox.width>zoneBox.width-24,'message input owns the full first row');
  assert.ok(inputBox.y+inputBox.height<=toolbarBox.y+1,'attachment, model, effort and send controls stay on the bottom row');
  assert.equal(await page.locator('.composer-toolbar #conversationAttach').count(),1);
  assert.equal(await page.locator('.composer-toolbar #conversationSend').count(),1);
  assert.equal(mainCalls.length,0,'previewing defaults does not resume/create a thread');
  assert.equal(modelCalls.find(call=>call.method==='config/read').params.cwd,'/work');
  await chooseModel('gpt-5.6-sol');await chooseEffort('high');
  await page.locator('#conversationInput').fill('Use selected model');await page.locator('#conversationSend').click();
  await page.waitForFunction(()=>document.querySelector('#conversationNotice').textContent.includes('Creation reply lost'));
  await chooseModel('gpt-5.6-luna');await chooseEffort('low');await page.locator('#conversationSend').click();
  await page.waitForURL(`**/?thread=${threadId}`);
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(mainCalls.find(call=>call.method==='thread/start').params.model,'gpt-5.6-sol');
  assert.equal(mainCalls.find(call=>call.method==='thread/start').params.config.model_reasoning_effort,'high');
  assert.equal(mainCalls.find(call=>call.method==='turn/start').params.model,'gpt-5.6-luna');
  assert.equal(mainCalls.find(call=>call.method==='turn/start').params.effort,'low');
  const starts=mainCalls.filter(call=>call.method==='thread/start');
  assert.deepEqual(starts[0].params,starts[1].params,'lost creation retries keep the same UUID and body after a model change');
  await chooseModel('gpt-5.6-sol');await chooseEffort('medium');
  await page.locator('#conversationInput').fill('Switch for the next turn');await page.locator('#conversationSend').click();
  await page.waitForFunction(()=>!document.querySelector('#conversationModelSelect').disabled);
  assert.equal(mainCalls.filter(call=>call.method==='turn/start').at(-1).params.model,'gpt-5.6-sol');
  assert.equal(mainCalls.filter(call=>call.method==='turn/start').at(-1).params.effort,'medium');
  assert.equal(mainCalls.filter(call=>call.method==='thread/start').length,2,'switching models preserves the thread');
  await page.locator('#conversationDetailsToggle').click();
  await page.waitForFunction(()=>document.querySelector('#conversationDetailsStatus').textContent==='');
  assert.match(await page.locator('#conversationDetailsFacts').textContent(),/最近使用的模型gpt-5.6-sol/);
  await page.locator('#conversationDetailsModel').selectOption('gpt-5.6-luna');
  await page.locator('#conversationDetailsEffort').selectOption('low');
  assert.equal(await picker.evaluate(element=>element.value),'gpt-5.6-luna','details shares the next-turn model');
  assert.equal(await effortValue(),'low');
  await page.locator('#conversationDetailsClose').click();
  // A new draft uses the Node default, not the previous thread's override.
  const reads=modelCalls.length;
  await page.locator('#agentNewThread').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='gpt-6-astra');
  assert.equal(await effortValue(),'medium');
  assert.equal(modelCalls.length,reads,'same-node/project defaults are cached');
  configured='custom-provider-model';
  await goToNodes();
  const nodeRefresh=page.locator(`[data-action="refresh-models"][data-id="${nodeId}"]`);
  await nodeRefresh.waitFor({state:'visible'});
  await nodeRefresh.click();
  await page.waitForFunction(()=>document.querySelector('#toast').textContent.includes('模型目录已刷新'));
  await goToAgent();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='custom-provider-model');
  await picker.click();assert.equal(await page.locator('#conversationModelMenu .composer-choice-option').count(),4,'configured models outside the catalog remain selectable');await picker.click();
  await page.setViewportSize({width:390,height:844});
  await page.waitForFunction(()=>document.querySelector('#agentThreadDrawer').getAttribute('aria-hidden')==='true');
  await page.locator('#agentThreadDrawer').evaluate(element=>Promise.all(element.getAnimations().map(animation=>animation.finished.catch(()=>{}))));
  assert.equal(await picker.isVisible(),false,'mobile hides the model picker');
  assert.equal(await page.locator('#conversationEffortSelect').isVisible(),false,'mobile hides the effort picker');
  const attachBox=await page.locator('#conversationAttach').boundingBox();
  const mobileInput=await page.locator('#conversationInput').boundingBox();
  const sendBox=await page.locator('#conversationSend').boundingBox();
  assert.ok(attachBox.x+attachBox.width<=mobileInput.x&&mobileInput.x+mobileInput.width<=sendBox.x,'mobile input sits between attachment and send');
  assert.ok(Math.abs(attachBox.y+attachBox.height/2-(mobileInput.y+mobileInput.height/2))<=2&&Math.abs(sendBox.y-attachBox.y)<=1,'mobile composer uses one row');
  assert.ok(sendBox.x+sendBox.width<=390,'mobile composer stays inside the viewport');
  await page.locator('#conversationDetailsToggle').click();
  assert.equal(await page.locator('#conversationDetailsName').textContent(),'新会话');
  assert.equal(await page.locator('#conversationDetailsModel').inputValue(),'custom-provider-model');
  await page.locator('#conversationDetailsModel').selectOption('gpt-5.6-sol');
  await page.locator('#conversationDetailsEffort').selectOption('high');
  assert.equal(await picker.evaluate(element=>element.value),'gpt-5.6-sol','mobile draft settings share the composer model');
  assert.equal(await effortValue(),'high');
  await page.locator('#conversationDetailsClose').click();
  await page.waitForFunction(()=>!document.querySelector('#conversationDetails').open);
  if (process.env.MIRA_WEB_SCREENSHOT_DIR) await page.screenshot({path:`${process.env.MIRA_WEB_SCREENSHOT_DIR}/composer-mobile.png`});
  await page.setViewportSize({width:1200,height:900});
  assert.equal(await picker.isVisible(),true,'desktop restores the model picker');
  // Late responses for another directory cannot replace this project's model.
  configured='old-project-model';deferConfig=true;
  await goToNodes();await nodeRefresh.click();
  while(!holdConfig) await new Promise(resolve=>setTimeout(resolve,10));
  await goToAgent();
  configured='new-project-model';deferConfig=false;
  await sidebarAction(page, 'agentNewProject');
  await page.locator('#projectPath').fill('/another-project');
  await page.locator('#projectForm button[type=submit]').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelSelect').value==='new-project-model');
  holdConfig();
  await page.waitForTimeout(100);
  assert.equal(await modelValue(),'new-project-model');
  // Failed reads on another scope must not display the previous default.
  failConfig=true;
  await sidebarAction(page, 'agentNewProject');await page.locator('#projectPath').fill('/unreadable');
  await page.locator('#projectForm button[type=submit]').click();
  await page.waitForFunction(()=>document.querySelector('#conversationModelStatus').textContent==='Configuration unavailable');
  assert.equal(await modelValue(),'');assert.equal(await picker.isDisabled(),true);
  await page.setViewportSize({width:390,height:844});
  assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
  assert.deepEqual(errors,[]);
  console.log('PASS: desktop two-row and mobile single-row composer, styled model/effort menus, Node-scoped refresh, concrete defaults, paginated caching, new-thread/turn settings, project races and failure clearing');
} finally { await browser.close(); }
