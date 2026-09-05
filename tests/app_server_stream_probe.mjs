// Opt-in real-model probe: opens a diagnostic conversation on an approved runtime.
// Required: MIRA_CLI_PATH, MIRA_STREAM_NODE_ID, MIRA_STREAM_DIRECT_URL (loopback).
// Optional: MIRA_STREAM_OUTPUT, MIRA_BROWSER_EXECUTABLE, argv[2] Playwright module.
// Captures contain diagnostic conversation payloads; keep them outside version control.
// The installed CLI handles authentication. This script never reads a Node credential.
import fs from 'node:fs/promises';
import http from 'node:http';
import {spawn} from 'node:child_process';
import {createInterface} from 'node:readline';
import {WebSocket, WebSocketServer} from '../server/node_modules/ws/wrapper.mjs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import assert from 'node:assert/strict';
const {chromium} = await import(process.argv[2] ?? 'playwright');
const root=fileURLToPath(new URL('../server/', import.meta.url));
const cli=process.env.MIRA_CLI_PATH;
assert(cli && path.isAbsolute(cli), 'MIRA_CLI_PATH must be the absolute installed Mira CLI path');
const nodeId=process.env.MIRA_STREAM_NODE_ID;
assert(nodeId && process.env.MIRA_STREAM_DIRECT_URL, 'MIRA_STREAM_NODE_ID and MIRA_STREAM_DIRECT_URL must identify the same approved local runtime');
const directUrl = new URL(process.env.MIRA_STREAM_DIRECT_URL);
assert(directUrl.protocol === 'ws:' && ['127.0.0.1', 'localhost', '[::1]'].includes(directUrl.hostname), 'the direct observer must use the local loopback App Server');
const label=process.env.MIRA_STREAM_LABEL ?? 'probe';
assert(/^[a-z0-9_-]+$/i.test(label), 'MIRA_STREAM_LABEL must be a filename label');
const output=process.env.MIRA_STREAM_OUTPUT ?? path.join(os.tmpdir(), `mira-stream-${label}.json`);
const stamp=()=>performance.timeOrigin+performance.now();
const capture=[]; const children=[]; const progress=setInterval(()=>{console.log(JSON.stringify({stage:'progress',direct:capture.filter(e=>e.stage==='direct'&&e.message.method==='item/agentMessage/delta').length,proxy:capture.filter(e=>e.stage==='proxy'&&e.message.method==='item/agentMessage/delta').length,last:capture.slice(-2).map(e=>({stage:e.stage,method:e.message.method}))}));},10000);
const vendors={'/vendor/xterm-addon-fit.js':'@xterm/addon-fit/lib/addon-fit.mjs','/vendor/xterm.js':'@xterm/xterm/lib/xterm.mjs','/vendor/xterm.css':'@xterm/xterm/css/xterm.css','/vendor/dompurify.js':'dompurify/dist/purify.es.mjs','/vendor/marked.js':'marked/lib/marked.esm.js'};
const publicAssets = new Set(['/', '/app.js', '/styles.css', '/trace-activity.js', '/conversation-progress.js', '/theme.js']);
const server=http.createServer(async(req,res)=>{
 try{
  if(req.url.startsWith('/v1/')){res.setHeader('content-type','application/json');res.end(JSON.stringify(req.url.includes('transcript')?{trace:[],generation:1,nextCursor:null}:{data:[]}));return;}
  if (!vendors[req.url] && !publicAssets.has(req.url)) {res.writeHead(404);res.end();return;}
  const path=vendors[req.url]?'node_modules/'+vendors[req.url]:'public/'+(req.url==='/'?'index.html':req.url.slice(1));
  let body=await fs.readFile(root+path,'utf8');
  if(req.url==='/app.js')body=body.replace('void bootstrap();',`show('agentView'); window.streamProbe={agent,call:rpc,upsertTrace,renderTranscript,ready:false};
  const probeSocket=new WebSocket('ws://'+location.host+'/stream'); agent.socket=probeSocket; agent.socketNodeId='probe';
  probeSocket.addEventListener('message',onAgentSocketMessage);
  probeSocket.addEventListener('open',async()=>{ await rpc('initialize',{clientInfo:{name:'mira_stream_probe',version:'1'},capabilities:{experimentalApi:true}});probeSocket.send(JSON.stringify({method:'initialized'}));agent.socketInitialized=true;window.streamProbe.ready=true;});`);
  res.setHeader('content-type',path.endsWith('.html')?'text/html':path.endsWith('.css')?'text/css':'text/javascript');res.end(body);
 }catch(e){res.writeHead(404);res.end();}
});
const wss=new WebSocketServer({server,path:'/stream'});
wss.on('connection',ws=>{
 const child=spawn(cli,['--timeout','5m','app-server','connect','--node',nodeId],{stdio:['pipe','pipe','pipe']});children.push(child);
 child.on('error', () => ws.close(1011, 'CLI connection failed'));
 child.stdin.on('error', () => ws.close(1011, 'CLI connection closed'));
 const lines=createInterface({input:child.stdout});
 lines.on('line',line=>{let message;try{message=JSON.parse(line);}catch{return;}capture.push({stage:'proxy',at:stamp(),message});if(ws.readyState===1)ws.send(line);});
 child.stderr.on('data',data=>process.stderr.write(data));
 ws.on('message',data=>{const message=JSON.parse(data.toString());capture.push({stage:'request',at:stamp(),message});child.stdin.write(data+'\n');});
 ws.on('close',()=>child.stdin.end());
});
await new Promise(r=>server.listen(0,'127.0.0.1',r));
const direct=new WebSocket(process.env.MIRA_STREAM_DIRECT_URL);
const pending=new Map();let rid=0;
function directCall(method,params={}){return new Promise((resolve,reject)=>{const id=++rid;const timer=setTimeout(()=>reject(new Error(method+' timeout')),30000);pending.set(id,{resolve,reject,timer});direct.send(JSON.stringify({id,method,params}));});}
direct.on('message',data=>{const message=JSON.parse(data.toString());capture.push({stage:'direct',at:stamp(),message});const p=pending.get(message.id);if(p){pending.delete(message.id);clearTimeout(p.timer);message.error?p.reject(new Error(JSON.stringify(message.error))):p.resolve(message.result);}});
let browser, diagnosticThread;
try{
 await new Promise((resolve,reject)=>{direct.once('open',resolve);direct.once('error',reject);});
 await directCall('initialize',{clientInfo:{name:'mira_direct_stream_probe',version:'1'},capabilities:{experimentalApi:true}});direct.send(JSON.stringify({method:'initialized'}));
 browser=await chromium.launch({headless:true,...(process.env.MIRA_BROWSER_EXECUTABLE ? { executablePath: process.env.MIRA_BROWSER_EXECUTABLE } : {})});
 const page=await browser.newPage({viewport:{width:390,height:844}});
 await page.addInitScript(()=>{
  window.streamMeasurements={events:[],frames:[],longTasks:[]};
  const now=()=>performance.timeOrigin+performance.now();
  const Native=window.WebSocket;
  window.WebSocket=class extends Native{constructor(...args){super(...args);this.addEventListener('message',event=>{const m=JSON.parse(event.data);if(!m.method)return;window.streamMeasurements.events.push({at:now(),method:m.method,itemId:m.params?.itemId,turnId:m.params?.turnId,delta:m.params?.delta,bytes:event.data.length});});}};
  new PerformanceObserver(list=>{for(const e of list.getEntries())window.streamMeasurements.longTasks.push({at:performance.timeOrigin+e.startTime,duration:e.duration});}).observe({type:'longtask',buffered:true});
  function frame(){let cards=[...document.querySelectorAll('.trace-card.assistant')];let card=cards.at(-1);let body=card?.querySelector('.trace-body');const n=body?.textContent?.length??0;const prev=window.streamMeasurements.frames.at(-1);if(!prev||prev.characters!==n||prev.key!==card?.dataset.traceKey){window.streamMeasurements.frames.push({at:now(),characters:n,key:card?.dataset.traceKey,text:body?.textContent});}requestAnimationFrame(frame);}
  requestAnimationFrame(frame);
 });
 page.on('pageerror',error=>console.error('PAGE',error.message));
 await page.goto('http://127.0.0.1:'+server.address().port+'/');await page.waitForFunction(()=>window.streamProbe?.ready,null,{timeout:30000});
 await page.evaluate(cwd => {window.streamProbe.cwd=cwd;}, os.tmpdir());
 const started=await page.evaluate(async()=>{const h=window.streamProbe;const s=await h.call('thread/start',{cwd:window.streamProbe.cwd,approvalPolicy:'never',sandbox:'danger-full-access'});h.agent.threadId=s.thread.id;return{threadId:s.thread.id,model:s.model,effort:s.reasoningEffort};});
 diagnosticThread=started.threadId;
 await directCall('thread/resume',{threadId:started.threadId,excludeTurns:true,approvalPolicy:'never',sandbox:'danger-full-access'});
 console.log(JSON.stringify({stage:'started',label,...started}));
 const sentAt=stamp();
 await page.evaluate(async()=>{const h=window.streamProbe;await h.call('turn/start',{threadId:h.agent.threadId,input:[{type:'text',text:'这是一次网页流式传输速度测试。不要调用任何工具，不需要思考或解释测试目的。直接输出30行编号短句，每行写一句约40字的中文，描述散步时看到的自然景物。一次连续写完，不要省略，不要总结。'}],approvalPolicy:'never'});});
 await page.waitForFunction(()=>window.streamMeasurements.events.some(e=>e.method==='turn/completed'),null,{timeout:180000});
 await page.waitForTimeout(200);
 const measurements=await page.evaluate(()=>window.streamMeasurements);
 const out={label,started,sentAt,capture,measurements};
 await fs.writeFile(output,JSON.stringify(out),{mode:0o600});
 const deltas=capture.filter(e=>e.stage==='proxy'&&e.message.method==='item/agentMessage/delta');
 const gaps=deltas.slice(1).map((e,i)=>e.at-deltas[i].at).sort((a,b)=>a-b);
 const sum=deltas.reduce((s,e)=>s+(e.message.params.delta?.length??0),0);
 const observed = capture.filter(e => e.stage === 'direct' && e.message.method === 'item/agentMessage/delta');
 const received = measurements.events.filter(e => e.method === 'item/agentMessage/delta');
 assert.equal(observed.length, deltas.length, 'both connections must observe every increment');
 assert.equal(received.length, deltas.length, 'the browser must receive every increment');
 const percentile = values => {const sorted = values.filter(Number.isFinite).sort((a, b) => a - b);return {p50: sorted[Math.floor(sorted.length * .5)], p95: sorted[Math.floor(sorted.length * .95)], max: sorted.at(-1)};};
 let characters = 0;
 const displayLags = received.map(event => {
  characters += event.delta?.length ?? 0;
  return measurements.frames.find(frame => frame.at >= event.at && frame.characters >= characters)?.at - event.at;
 });
 console.log(JSON.stringify({proxyMs: percentile(deltas.map((event, i) => event.at - observed[i].at)),
  browserReceiveMs: percentile(received.map((event, i) => event.at - deltas[i].at)),
  receiveToFrameMs: percentile(displayLags), observedInFrame: displayLags.filter(Number.isFinite).length}));
 console.log(JSON.stringify({label,events:capture.length,deltas:deltas.length,characters:sum,firstDeltaMs:deltas[0]?.at-sentAt,streamMs:deltas.at(-1)?.at-deltas[0]?.at,gapP50:gaps[Math.floor(gaps.length*.5)],gapP95:gaps[Math.floor(gaps.length*.95)],gapMax:gaps.at(-1),longTasks:measurements.longTasks.length,frames:measurements.frames.length,output:output}));
 await page.screenshot({path:output+'.png'});
 await directCall('thread/archive',{threadId:started.threadId});diagnosticThread=null;
}finally{clearInterval(progress);if(diagnosticThread&&direct.readyState===1)await directCall('thread/archive',{threadId:diagnosticThread}).catch(()=>{});await fs.writeFile(output+'.raw.json',JSON.stringify(capture),{mode:0o600});direct.close();for(const p of pending.values())clearTimeout(p.timer);await browser?.close();for(const child of children)child.kill();wss.close();server.close();}
