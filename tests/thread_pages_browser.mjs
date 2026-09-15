import assert from 'node:assert/strict';
const { chromium } = await import(process.argv[2] ?? 'playwright');
const origin = process.env.MIRA_SERVER_URL ?? 'http://127.0.0.1:8787';
const id = n => `10000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const projectKey = JSON.stringify(['', '/pages']), oldKey = JSON.stringify(['', '/old-project']);
const row = (n, fields = {}) => ({threadId:id(n),title:`Thread ${n}`,cwd:'/pages',generation:1,itemCount:0,
  updatedAt:new Date(Date.now()-n*1000).toISOString(),archived:false,listRoot:true,childCount:0,subagentCount:0,...fields});
const roots = Array.from({length:130}, (_, i) => row(i+1));
roots[0].childCount=121; roots[0].subagentCount=122; roots[0].hasSubagents=true;
const children = Array.from({length:121}, (_, i) => row(1000+i,{listRoot:false,parentThreadId:id(1)}));
children[0].childCount=1;children[0].subagentCount=1;
const nested=row(2000,{listRoot:false,parentThreadId:id(1000)});
const old=row(3000,{cwd:'/old-project',updatedAt:'2020-01-01T00:00:00Z'});
const all=[...roots,...children,nested,old], projects=[{key:projectKey,cwd:'/pages',nodeId:'',count:130},{key:oldKey,cwd:'/old-project',nodeId:'',count:1}];
const requests=[], errors=[];
const browser=await chromium.launch({headless:true});
try {
 const context=await browser.newContext({viewport:{width:1440,height:1000}});
 await context.route('**/v1/codex/threads?*',route=>{
  const q=new URL(route.request().url()).searchParams;requests.push(Object.fromEntries(q));
  const view=q.get('view'), key=q.get('projectKey'); let data=[], total=0, nextCursor=null;
  if(q.get('archived')!=='1') {
   if(view==='roots') data=key===oldKey?[old]:key===projectKey?roots:[...roots,old];
   if(view==='children') data=q.get('parentThreadId')===id(1)?children:[nested];
   if(view==='refresh') data=all.filter(r=>q.getAll('id').includes(r.threadId));
   if(view==='path') {let current=all.find(r=>r.threadId===q.get('threadId'));while(current){data.push(current);current=all.find(r=>r.threadId===current.parentThreadId);}}
   total=data.length;
   if(['roots','children'].includes(view)) {
    const offset=Number(q.get('cursor')??0),end=offset+Number(q.get('limit'));
    if(!q.has('head')&&end<total)nextCursor=String(end);
    data=data.slice(offset,end);
   }
  }
  return route.fulfill({json:{paged:true,data,total,nextCursor,...(view==='roots'?{projects:q.get('archived')==='1'?[]:projects}:{})}});
 });
 await context.route(/\/v1\/codex\/threads\/[^?]+\?/,route=>{
  const url=new URL(route.request().url()), thread=all.find(r=>url.pathname.includes(r.threadId));
  return route.fulfill({json:url.pathname.endsWith('/transcript')?{generation:1,itemCount:0,trace:[],nextCursor:null}:thread??{}});
 });
 const page=await context.newPage();page.setDefaultTimeout(15000);page.on('pageerror',error=>errors.push(error.message));
 await page.goto(origin);
 await page.locator('#password').fill(process.env.MIRA_TEST_ADMIN_PASSWORD??'mira-local-admin-password');
 await page.locator('#loginForm button[type=submit]').click();
 await page.locator('#dashboardView:not(.hidden)').waitFor();
 await page.goto(`${origin}/?thread=${id(1)}`);
 const family=page.locator(`[data-subagent-parent="${id(1)}"]`);
 await family.waitFor();
 assert.equal(await page.locator('.thread-project').count(),2,'old project is in the complete directory');
 assert.equal(await page.locator('.thread-subagent-threads [data-thread-row]').count(),0,'collapsed children have no DOM');
 assert.equal(requests.some(q=>q.view==='children'),false,'no eager child requests');
 await family.locator(':scope > summary').click();
 const direct=family.locator(':scope > .thread-subagent-threads > [data-thread-row]');
 await page.waitForFunction(()=>document.querySelectorAll('.thread-subagent-threads > [data-thread-row]').length===50);
 assert.equal(await page.locator(`[data-thread-row="${nested.threadId}"]`).count(),0);
 await family.locator(':scope > .thread-subagent-threads > .thread-page-more').click();
 await page.waitForFunction(()=>document.querySelectorAll('.thread-subagent-threads > [data-thread-row]').length===100);
 await family.locator(':scope > .thread-subagent-threads > .thread-page-more').click();
 await page.waitForFunction(()=>document.querySelectorAll('.thread-subagent-threads > [data-thread-row]').length===121);
 assert.equal(await direct.count(),121);
 await family.locator(':scope > summary').click();
 await page.waitForFunction(()=>document.querySelectorAll('.thread-subagent-threads > [data-thread-row]').length===0);
 assert.equal(await direct.count(),0,'collapse releases descendant DOM');
 const project=page.locator('.thread-project').filter({has:page.locator('.thread-project-identity strong',{hasText:/^pages$/})});
 await project.locator('.thread-project-history-summary').click();
 await project.locator('.thread-project-history-threads > .thread-page-more').click();
 await page.waitForFunction(()=>document.querySelectorAll('[data-thread-row]').length===100);
 await project.locator('.thread-project-history-threads > .thread-page-more').click();
 await page.waitForFunction(()=>document.querySelectorAll('[data-thread-row]').length===130);
 // Activity polling and head discovery must retain the older loaded pages.
 await page.waitForTimeout(11000);
 assert.equal(await project.locator('[data-thread-row]').count(),130);
 const oldProject=page.locator('.thread-project').filter({hasText:'old-project'});
 await oldProject.locator('.thread-project-history-summary').click();
 await page.locator(`[data-thread-row="${old.threadId}"]`).waitFor();
 await page.goto(`${origin}/?thread=${nested.threadId}`);
 await page.locator(`[data-thread-row="${nested.threadId}"]`).waitFor();
 assert.equal(await page.locator(`[data-subagent-parent="${id(1)}"]`).getAttribute('open'),'');
 assert.equal(await page.locator(`[data-subagent-parent="${id(1000)}"]`).getAttribute('open'),'');
 assert.equal(requests.some(q=>q.limit==='300'),false,'paged clients never poll the legacy 300-row list');
 assert.deepEqual(errors,[]);
 console.log('thread pagination and lazy subagent browser checks passed');
} finally {await browser.close();}
