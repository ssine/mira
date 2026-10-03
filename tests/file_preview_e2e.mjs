// Dedicated binary file/ZIP/static-site data plane against a disposable Server.
// MIRA_SERVER_URL=... MIRA_TEST_BINARY=... node tests/file_preview_e2e.mjs
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import http from 'node:http';
const hostFetch=(url,options={})=>new Promise((resolve,reject)=>{const req=http.request(url,{method:options.method||'GET',headers:options.headers},res=>{const chunks=[];res.on('data',chunk=>chunks.push(chunk));res.on('end',()=>resolve(new Response(Buffer.concat(chunks),{status:res.statusCode,headers:res.headers})));res.on('error',reject)});req.on('error',reject);req.end(options.body);});
import path from 'node:path';
import { spawn, execFileSync } from 'node:child_process';
const base=(process.env.MIRA_SERVER_URL||'http://127.0.0.1:8787').replace(/\/$/,'');
const root=await fs.mkdtemp(path.join(os.tmpdir(),'mira-file-preview-'));
const binary=process.env.MIRA_TEST_BINARY||path.join(root,'mira');if(!process.env.MIRA_TEST_BINARY)execFileSync('go',['build','-o',binary,'./cmd/mira'],{cwd:'node'});
await fs.mkdir(path.join(root,'site/pages'),{recursive:true});await fs.mkdir(path.join(root,'site/assets'));
await fs.writeFile(path.join(root,'site/pages/index.html'),'<!doctype html><link rel="stylesheet" href="/assets/style.css"><h1>Preview website</h1><script type="module" src="/assets/main.js"></script>');
await fs.copyFile(path.join(root,'site/pages/index.html'),path.join(root,'site/index.html'));
await fs.writeFile(path.join(root,'site/assets/style.css'),'h1 { color: rgb(0, 120, 212) }');
await fs.writeFile(path.join(root,'site/assets/main.js'),"const data=await fetch('/assets/data.json').then(r=>r.json());document.body.dataset.result=data.value;try{await navigator.serviceWorker.register('/sw.js')}catch(e){document.body.dataset.worker='blocked'}");
await fs.writeFile(path.join(root,'site/assets/data.json'),'{"value":"relative fetch works"}');await fs.writeFile(path.join(root,'site/sw.js'),'self.addEventListener("fetch",()=>{})');
await fs.writeFile(path.join(root,'readme.md'),'# Preview document\n\n![relative asset](pixel.png)\n\n[Next document](other.md)\n\n```js\nconst value = 42;\n```\n');await fs.writeFile(path.join(root,'other.md'),'# Second document\n\n**Rendered content**\n');
await fs.writeFile(path.join(root,'pixel.png'),Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==','base64'));
await fs.writeFile(path.join(root,'big.txt'),Array.from({length:20000},(_,i)=>`source line ${i+1} ${'a'.repeat(32)}`).join('\n'));
if (process.env.MIRA_TEST_FFMPEG) {
 execFileSync(process.env.MIRA_TEST_FFMPEG,['-v','error','-f','lavfi','-i','testsrc2=size=160x90:rate=10','-f','lavfi','-i','sine=frequency=440:sample_rate=48000','-t','2','-c:v','libvpx','-c:a','libvorbis',path.join(root,'sample.webm')]);
 execFileSync(process.env.MIRA_TEST_FFMPEG,['-v','error','-f','lavfi','-i','sine=frequency=440:sample_rate=48000','-t','2',path.join(root,'sample.wav')]);
}
await fs.writeFile(path.join(root,'bytes.bin'),Buffer.from('0123456789'.repeat(500000)));
await fs.writeFile(path.join(root,'private.txt'),'secret outside site');await fs.symlink('../private.txt',path.join(root,'site/leak.txt'));
execFileSync('python3',['-c',`import zipfile,sys,os
r=sys.argv[1]
with zipfile.ZipFile(os.path.join(r,'bundle.zip'),'w',zipfile.ZIP_DEFLATED) as z:
 for name in ['readme.md','other.md','pixel.png','bytes.bin','site/pages/index.html','site/assets/main.js','site/assets/style.css','site/assets/data.json']:
  z.write(os.path.join(r,name),name)
 z.writestr('duplicate.txt','first');z.writestr('duplicate.txt','second');z.writestr('../escape.txt','unsafe')`,root],{stdio:['ignore','ignore','ignore']});
const login=await fetch(base+'/v1/admin/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:'admin',password:process.env.MIRA_TEST_ADMIN_PASSWORD||'mira-local-admin-password'})});assert.equal(login.status,200);const auth=await login.json();const cookie=login.headers.get('set-cookie').split(';')[0];
async function api(route,method='GET',body){const r=await fetch(base+route,{method,headers:{cookie,'x-mira-csrf':auth.csrfToken,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});let b;try{b=await r.json()}catch{b={}};return {r,b};}
const key=`file-preview-${process.pid}`;const child=spawn(binary,['node-worker'],{env:{...process.env,MIRA_SERVER_URL:base,MIRA_NODE_KEY:key,MIRA_IDENTITY_FILE:path.join(root,'identity.json'),MIRA_NODE_ALLOWED_ROOTS:JSON.stringify([root]),MIRA_NODE_HEARTBEAT_SECONDS:'1',APP_SERVER_AUTO_START:'false',CODEX_HOME:path.join(root,'codex-home'),MIRA_NODE_TOKEN:'',CONTROL_SERVER_TOKEN:''},stdio:['ignore','pipe','pipe']});let logs='';for(const stream of [child.stdout,child.stderr])stream.on('data',v=>logs+=v);
const wait=async(fn,timeout=30000)=>{const end=Date.now()+timeout;while(Date.now()<end){const value=await fn();if(value)return value;await new Promise(r=>setTimeout(r,100))}throw new Error('fixture timeout: '+logs.slice(-2000))};let nodeID;
try{
 const pending=await wait(async()=>{const {b}=await api('/v1/admin/enrollments?status=pending');return b.data?.find(x=>x.nodeKey===key)});const approved=await api(`/v1/admin/enrollments/${pending.enrollmentId}/approve`,'POST',{});assert.equal(approved.r.status,200);nodeID=approved.b.nodeId;
 await wait(async()=>{const {b}=await api(`/v1/nodes/${nodeID}`);return b.channelStatus?.connected});
 const resource=(relative,extra={})=>new URLSearchParams({nodeId:nodeID,path:path.join(root,relative),...extra});
 const meta=await api('/v1/files/meta?'+resource('readme.md'));assert.equal(meta.r.status,200);assert.equal(meta.b.type,'file');
 const page=await api('/v1/files/meta?'+resource('',{action:'list',cursor:'0'}));assert.equal(page.r.status,200);assert.ok(page.b.entries.some(x=>x.name==='readme.md'));
 const denied=await fetch(base+'/v1/files/content?'+resource('readme.md'));assert.equal(denied.status,401);
 const query=resource('bytes.bin');let r=await fetch(`${base}/v1/files/content?${query}`,{headers:{cookie,Range:'bytes=100-199'}});assert.equal(r.status,206);assert.equal((await r.arrayBuffer()).byteLength,100);assert.equal(r.headers.get('content-range'),'bytes 100-199/5000000');const etag=r.headers.get('etag');
 r=await fetch(`${base}/v1/files/content?${query}`,{headers:{cookie,Range:'bytes=-10'}});assert.equal(await r.text(),'0123456789');
 r=await fetch(`${base}/v1/files/content?${query}`,{headers:{cookie,'If-None-Match':etag}});assert.equal(r.status,304);
 const controller=new AbortController();r=await fetch(`${base}/v1/files/content?${query}`,{signal:controller.signal,headers:{cookie}});const reader=r.body.getReader();await reader.read();controller.abort();await reader.cancel().catch(()=>{});
 const archive=resource('bundle.zip',{archive:'true',archiveEntry:'',action:'list'});const listing=await api('/v1/files/meta?'+archive);assert.equal(listing.r.status,200);assert.equal(listing.b.entries.filter(x=>x.name==='duplicate.txt').length,2);assert.ok(!listing.b.entries.some(x=>String(x.archiveEntry).includes('..')));
 r=await fetch(`${base}/v1/files/content?${resource('bundle.zip',{archive:'true',archiveEntry:'bytes.bin'})}`,{headers:{cookie,Range:'bytes=123-132'}});assert.equal(r.status,206);assert.equal(await r.text(),'3456789012');
 r=await fetch(`${base}/v1/files/content?${resource('bundle.zip',{archive:'true',archiveEntry:'duplicate.txt'})}`,{headers:{cookie}});assert.equal(r.status,400);
 const missingCSRF=await fetch(base+'/v1/file-previews',{method:'POST',headers:{cookie,'Content-Type':'application/json'},body:'{}'});assert.equal(missingCSRF.status,403);
 const created=await api('/v1/file-previews','POST',{nodeId:nodeID,root:path.join(root,'site'),entry:'pages/index.html',resource:{path:path.join(root,'site/pages/index.html')}});assert.equal(created.r.status,201,JSON.stringify(created.b));const u=new URL(created.b.url);const host=u.host;const origin=u.origin;
 // Multiple public ingress hosts reach this same Server listener.
 if(process.env.MIRA_TEST_PREVIEW_INGRESSES){
  const ingresses=JSON.parse(process.env.MIRA_TEST_PREVIEW_INGRESSES);assert.ok(ingresses.length>=2);
  const ingressAPI=async(consoleHost,route,method='GET',body)=>{
   const response=await hostFetch(base+route,{method,headers:{Host:consoleHost,cookie,'x-mira-csrf':auth.csrfToken,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});
   return {r:response,b:await response.json()};
  };
  const siteBody={nodeId:nodeID,root:path.join(root,'site'),entry:'pages/index.html',resource:{path:path.join(root,'site/pages/index.html')}};
  for(const ingress of ingresses){
   const consoleHost=new URL(ingress.consoleOrigin).host;
   const result=await ingressAPI(consoleHost,'/v1/file-previews','POST',siteBody);assert.equal(result.r.status,201,JSON.stringify(result.b));
   const siteURL=new URL(result.b.url);const expected=new URL(ingress.previewOrigin);expected.hostname=result.b.id+'.'+expected.hostname;assert.equal(siteURL.origin,expected.origin);
   const other=ingresses.find(value=>value.previewOrigin!==ingress.previewOrigin);assert.ok(other);
   const otherURL=new URL(other.previewOrigin);otherURL.hostname=result.b.id+'.'+otherURL.hostname;
   let response=await hostFetch(base+'/__mira_preview/claim',{method:'POST',headers:{Host:otherURL.host,Origin:otherURL.origin,'Content-Type':'application/json'},body:JSON.stringify({grant:siteURL.hash.slice(1)})});assert.equal(response.status,410);
   response=await hostFetch(base+'/__mira_preview/claim',{method:'POST',headers:{Host:siteURL.host,Origin:siteURL.origin,'Content-Type':'application/json'},body:JSON.stringify({grant:siteURL.hash.slice(1)})});assert.equal(response.status,200);const boundCookie=response.headers.get('set-cookie').split(';')[0];
   response=await hostFetch(base+'/assets/data.json',{headers:{Host:siteURL.host,cookie:boundCookie}});assert.equal(response.status,200);assert.equal((await response.json()).value,'relative fetch works');
   response=await hostFetch(base+'/assets/data.json',{headers:{Host:otherURL.host,cookie:boundCookie}});assert.equal(response.status,410);
   const wrongPort=new URL(siteURL);wrongPort.port=siteURL.port==='24444'?'24445':'24444';
   response=await hostFetch(base+'/assets/data.json',{headers:{Host:wrongPort.host,cookie:boundCookie}});assert.equal(response.status,410);
   response=await hostFetch(base+'/v1/admin/session',{headers:{Host:siteURL.host,cookie}});assert.notEqual(response.status,200);
   await ingressAPI(consoleHost,`/v1/file-previews/${result.b.id}`,'DELETE');
  }
  const unmapped=await ingressAPI('unknown.console.example.test:24443','/v1/file-previews','POST',siteBody);assert.equal(unmapped.r.status,409);assert.equal(unmapped.b.code,'preview_ingress_unconfigured');
 }
 const preview=async(route,headers={},method='GET',body)=>hostFetch(base+route,{method,headers:{Host:host,...headers},...(body?{body}:{})});
 r=await preview('/pages/index.html');assert.equal(r.status,403);
 assert.equal(r.headers.get('cross-origin-opener-policy'),'same-origin');
 r=await preview('/__mira_preview/bootstrap');assert.equal(r.status,200);assert.equal(r.headers.get('cross-origin-opener-policy'),'same-origin');
 r=await preview('/__mira_preview/claim',{'Content-Type':'application/json',Origin:origin},'POST',JSON.stringify({grant:u.hash.slice(1)}));assert.equal(r.status,200,await r.clone().text());const siteCookie=r.headers.get('set-cookie').split(';')[0];assert.equal((await r.json()).entry,'/pages/index.html');
 r=await preview('/__mira_preview/claim',{'Content-Type':'application/json',Origin:origin},'POST',JSON.stringify({grant:u.hash.slice(1)}));assert.equal(r.status,403);
 for(const [route,text]of [['/pages/index.html','Preview website'],['/assets/main.js','fetch'],['/assets/data.json','relative fetch works']]){r=await preview(route,{cookie:siteCookie});assert.equal(r.status,200);assert.equal(r.headers.get('cross-origin-opener-policy'),'same-origin');assert.ok((await r.text()).includes(text));}
 r=await preview('/v1/admin/session',{cookie:siteCookie+'; '+cookie});assert.notEqual(r.status,200);assert.ok(!(await r.text()).includes('csrfToken'));
 r=await preview('/leak.txt',{cookie:siteCookie});assert.equal(r.status,400);assert.ok(!(await r.text()).includes('secret outside site'));
 r=await hostFetch(base+'/assets/data.json',{headers:{Host:'another.'+host.split('.').slice(1).join('.'),cookie:siteCookie}});assert.equal(r.status,410);
 await api(`/v1/file-previews/${created.b.id}`,'DELETE');r=await preview('/assets/data.json',{cookie:siteCookie});assert.equal(r.status,410);
 const zipSite=await api('/v1/file-previews','POST',{nodeId:nodeID,root:'site',entry:'pages/index.html',resource:{path:path.join(root,'bundle.zip'),archive:true}});assert.equal(zipSite.r.status,201,JSON.stringify(zipSite.b));
 const zipURL=new URL(zipSite.b.url);r=await hostFetch(base+'/__mira_preview/claim',{method:'POST',headers:{Host:zipURL.host,Origin:zipURL.origin,'Content-Type':'application/json'},body:JSON.stringify({grant:zipURL.hash.slice(1)})});assert.equal(r.status,200);const zipCookie=r.headers.get('set-cookie').split(';')[0];r=await hostFetch(base+'/assets/data.json',{headers:{Host:zipURL.host,cookie:zipCookie}});assert.equal(r.status,200);assert.equal((await r.json()).value,'relative fetch works');await api(`/v1/file-previews/${zipSite.b.id}`,'DELETE');
 const logoutSite=await api('/v1/file-previews','POST',{nodeId:nodeID,root:path.join(root,'site'),entry:'pages/index.html',resource:{path:path.join(root,'site/pages/index.html')}});assert.equal(logoutSite.r.status,201);const logoutURL=new URL(logoutSite.b.url);
 r=await hostFetch(base+'/__mira_preview/claim',{method:'POST',headers:{Host:logoutURL.host,Origin:logoutURL.origin,'Content-Type':'application/json'},body:JSON.stringify({grant:logoutURL.hash.slice(1)})});assert.equal(r.status,200);const logoutCookie=r.headers.get('set-cookie').split(';')[0];await api('/v1/admin/logout','POST',{});
 r=await hostFetch(base+'/assets/data.json',{headers:{Host:logoutURL.host,cookie:logoutCookie}});assert.equal(r.status,403);
 if(process.env.MIRA_KEEP_FILE_FIXTURE){await fs.writeFile(process.env.MIRA_KEEP_FILE_FIXTURE,JSON.stringify({root,nodeID,base,pid:child.pid,media:Boolean(process.env.MIRA_TEST_FFMPEG)}));console.log('fixture retained for browser acceptance');await new Promise(resolve=>{process.once('SIGTERM',resolve);process.once('SIGINT',resolve)});}
 console.log(JSON.stringify({ok:true,binaryStream:true,range:true,cancel:true,zip:true,isolatedSite:true,zipSite:true,multiIngress:Boolean(process.env.MIRA_TEST_PREVIEW_INGRESSES),oneTimeGrant:true,csrf:true,close:true,logoutRevocation:true}));
}catch(e){console.error(e);console.error(logs.slice(-1200));process.exitCode=1;}finally{if(child.exitCode===null){const exit=new Promise(resolve=>child.once('exit',resolve));child.kill('SIGTERM');await exit;}await fs.rm(root,{recursive:true,force:true});}
