// Disposable Server/Node acceptance in WebSocket and HTTPS-only transport modes.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import http from 'node:http';
import { once } from 'node:events';
import { spawn } from 'node:child_process';
import { WebSocket, WebSocketServer } from 'ws';
const base=process.env.MIRA_SERVER_URL||'http://127.0.0.1:8787';
const binary=process.env.MIRA_TEST_BINARY;
assert.ok(binary,'MIRA_TEST_BINARY is required');
const root=await fs.mkdtemp(path.join(os.tmpdir(),'mira-port-site-'));
let stopped=false, disconnectedRequests=0, upstreamConnections=0;
const backend=http.createServer(async(req,res)=>{
 if(req.url==='/disconnect'){disconnectedRequests++;req.socket.destroy();return;}
 if(req.url==='/stream') {res.writeHead(200,{'Content-Type':'text/event-stream'});res.write('data: first\n\n');const timer=setInterval(()=>res.write('data: next\n\n'),1000);res.on('close',()=>{clearInterval(timer);stopped=true});return;}
 if(req.url==='/delayed'){setTimeout(()=>res.end('after idle'),Number(process.env.MIRA_SITE_IDLE_TEST_MS||200));return;}
 const chunks=[];for await(const chunk of req) chunks.push(chunk);
 res.writeHead(207,{'Content-Type':'application/json','Set-Cookie':'upstream=session; Path=/','X-Upstream':'yes'});
 res.end(JSON.stringify({method:req.method,url:req.url,headers:req.headers,bytes:Buffer.concat(chunks).length,connection:req.socket.siteConnectionID}));
});
backend.on('connection',socket=>{socket.siteConnectionID=++upstreamConnections});
const wss=new WebSocketServer({server:backend});wss.on('connection',ws=>ws.on('message',(data,binary)=>ws.send(data,{binary})));
backend.listen(0,'127.0.0.1');await once(backend,'listening');const port=backend.address().port;
const login=await fetch(base+'/v1/admin/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:'admin',password:process.env.MIRA_TEST_ADMIN_PASSWORD||'mira-local-admin-password'})});assert.equal(login.status,200);const auth=await login.json();const cookie=login.headers.get('set-cookie').split(';')[0];
async function api(route,method='GET',body){const r=await fetch(base+route,{method,headers:{cookie,'x-mira-csrf':auth.csrfToken,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});return {r,b:await r.json()};}
const key=`port-sites-${process.pid}`;let child,logs='';
function start(){child=spawn(binary,['node-worker'],{env:{...process.env,MIRA_SERVER_URL:base,MIRA_NODE_KEY:key,MIRA_IDENTITY_FILE:path.join(root,'identity.json'),MIRA_NODE_ALLOWED_ROOTS:JSON.stringify([root]),MIRA_NODE_HEARTBEAT_SECONDS:'1',APP_SERVER_AUTO_START:'false',CODEX_HOME:path.join(root,'codex-home'),MIRA_NODE_TOKEN:'',CONTROL_SERVER_TOKEN:''},stdio:['ignore','pipe','pipe']});for(const s of [child.stdout,child.stderr])s.on('data',v=>logs+=v);}
async function stop(){if(child?.exitCode===null){const exit=once(child,'exit');child.kill('SIGTERM');await exit;}}
async function wait(fn){const end=Date.now()+30000;while(Date.now()<end){const v=await fn();if(v)return v;await new Promise(r=>setTimeout(r,100))}throw Error('fixture timeout '+logs.slice(-1200));}
let nodeID,site,host;
function request(route,options={}){return http.request(base+route,{...options,headers:{Host:host,...options.headers}});}
async function siteFetch(route,options={}){return await new Promise((resolve,reject)=>{const req=request(route,options);req.on('error',reject);req.on('response',res=>{const chunks=[];res.on('error',reject);res.on('data',c=>chunks.push(c));res.on('end',()=>resolve({status:res.statusCode,headers:res.headers,text:Buffer.concat(chunks).toString()}));});req.end(options.body);});}
try {
 start();const pending=await wait(async()=>{const {b}=await api('/v1/admin/enrollments?status=pending');return b.data?.find(n=>n.nodeKey===key)});const approved=await api(`/v1/admin/enrollments/${pending.enrollmentId}/approve`,'POST',{});assert.equal(approved.r.status,200);nodeID=approved.b.nodeId;
 await wait(async()=>{const {b}=await api('/v1/nodes/'+nodeID);return b.channelStatus?.connected});
 console.error('connected');
 const body={name:`test-${process.pid}`,nodeId:nodeID,port};
 let result=await api('/v1/sites','POST',body);assert.equal(result.r.status,200,JSON.stringify(result.b));site=result.b;host=new URL(site.url).host;
 const tool=await api("/v1/dynamic-tools/call","POST",{tool:"site",arguments:{action:"create",...body,nodeId:key}});assert.equal(tool.r.status,200,JSON.stringify(tool.b));assert.equal(tool.b.result.siteId,site.siteId);
 const repeated=await api('/v1/sites','POST',body);assert.equal(repeated.b.siteId,site.siteId);
 assert.equal((await api('/v1/sites','POST',{...body,port:port+1})).r.status,409);
 assert.equal((await fetch(base+'/v1/sites')).status,401);
 assert.equal((await fetch(base+'/v1/sites',{method:'POST',headers:{cookie,'Content-Type':'application/json'},body:JSON.stringify(body)})).status,403);
 let r=await siteFetch('/path%20encoded?q=a%2Fb',{method:'POST',headers:{Authorization:'Bearer application-only',cookie:'backend=only','Content-Type':'application/octet-stream'},body:Buffer.alloc(5*1024*1024,42)});assert.equal(r.status,207);let echo=JSON.parse(r.text);assert.equal(echo.bytes,5*1024*1024);assert.equal(echo.headers.authorization,'Bearer application-only');assert.equal(echo.headers.cookie,'backend=only');assert.equal(echo.headers.host,host);assert.equal(echo.url,'/path%20encoded?q=a%2Fb');assert.equal(r.headers['set-cookie'][0],'upstream=session; Path=/');
 const firstConnection=echo.connection;
 for(let i=0;i<3;i++){r=await siteFetch('/reuse');assert.equal(r.status,207);assert.equal(JSON.parse(r.text).connection,firstConnection,'upstream connection was not reused');}
 r=await siteFetch('/disconnect');assert.equal(r.status,502);assert.equal(disconnectedRequests,1,'a lost response replayed the request');
 r=await siteFetch('/v1/admin/session');assert.equal(r.status,207);assert.equal(JSON.parse(r.text).url,'/v1/admin/session');
 console.error('HTTP passthrough complete');
 r=await siteFetch('/delayed');assert.equal(r.text,'after idle');
 console.error('idle complete');
 const ws=new WebSocket(base.replace('http','ws')+'/socket',{headers:{Host:host}});await once(ws,'open');ws.send(Buffer.alloc(100000,7));const [data]=await once(ws,'message');assert.equal(data.length,100000);ws.close();await once(ws,'close');
 console.error('WebSocket complete');
 const stream=request('/stream');stream.end();const [response]=await once(stream,'response');const first=await once(response,'data');assert.match(first[0].toString(),/first/);stream.destroy();await wait(()=>stopped);
 assert.equal((await api('/v1/sites/'+site.siteId,'PATCH',{expectedRevision:site.revision+1,enabled:false})).r.status,409);
 // A stop closes an active public stream, including through HTTPS fallback.
 console.error('SSE cancellation complete');
 const active=request('/stream');active.end();const [activeResponse]=await once(active,'response');await once(activeResponse,'data');const closed=new Promise(resolve=>activeResponse.once('close',resolve));
 result=await api('/v1/sites/'+site.siteId,'PATCH',{expectedRevision:site.revision,enabled:false});assert.equal(result.r.status,200);site=result.b;await Promise.race([closed,new Promise((_,reject)=>setTimeout(()=>reject(Error('stop did not close stream')),5000))]);assert.equal((await siteFetch('/')).status,404);
 result=await api('/v1/sites/'+site.name,'PATCH',{expectedRevision:site.revision,enabled:true});site=result.b;assert.equal((await siteFetch('/')).status,207);
 console.error('stop/re-enable complete');
 await stop();assert.equal((await api('/v1/sites/'+site.name)).b.siteId,site.siteId);start();await wait(async()=>{const {b}=await api('/v1/nodes/'+nodeID);return b.channelStatus?.connected});assert.equal((await siteFetch('/')).status,207);
 // CLI uses the existing identity without printing its credential.
 const cli=spawn(binary,['site','get',site.name,'--json'],{env:{...process.env,MIRA_IDENTITY_FILE:path.join(root,'identity.json')},stdio:['ignore','pipe','pipe']});let output='';cli.stdout.on('data',c=>output+=c);const [code]=await once(cli,'exit');assert.equal(code,0);assert.ok(output.includes(site.siteId));
 console.error('reconnect/CLI complete');
 result=await api('/v1/sites/'+site.name,'DELETE',{expectedRevision:site.revision});assert.equal(result.r.status,200);assert.equal((await siteFetch('/')).status,404);assert.equal((await api('/v1/sites','POST',body)).r.status,409);
 console.log(JSON.stringify({ok:true,transport:process.env.MIRA_NODE_TRANSPORT||'websocket',connectionReuse:true,noReplay:true,largeBody:true,SSE:true,websocket:true,cancellation:true,revision:true,stop:true,reconnect:true,cli:true,retiredName:true}));
} finally {await stop();wss.close();backend.closeAllConnections();backend.close();await fs.rm(root,{recursive:true,force:true});}
