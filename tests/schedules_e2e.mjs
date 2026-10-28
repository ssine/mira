// Real Node process execution via both reverse-channel transports, disposable DB.
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {once} from 'node:events';
import {spawn} from 'node:child_process';
import {randomUUID} from 'node:crypto';
const base=process.env.MIRA_SERVER_URL||'http://127.0.0.1:8787', binary=process.env.MIRA_TEST_BINARY;
assert.ok(binary,'MIRA_TEST_BINARY required');
const root=await fs.mkdtemp(path.join(os.tmpdir(),'mira-schedules-'));
const login=await fetch(base+'/v1/admin/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({username:'admin',password:process.env.MIRA_TEST_ADMIN_PASSWORD||'mira-local-admin-password'})});assert.equal(login.status,200);
const auth=await login.json(),cookie=login.headers.get('set-cookie').split(';')[0];
async function api(route,method='GET',body){const response=await fetch(base+route,{method,headers:{cookie,'x-mira-csrf':auth.csrfToken,'Content-Type':'application/json'},...(body?{body:JSON.stringify(body)}:{})});return {status:response.status,body:await response.json()};}
const key=`schedules-${process.pid}`,environment={...process.env,MIRA_SERVER_URL:base,MIRA_NODE_KEY:key,MIRA_IDENTITY_FILE:path.join(root,'identity.json'),MIRA_NODE_ALLOWED_ROOTS:JSON.stringify([root]),MIRA_NODE_HEARTBEAT_SECONDS:'1',APP_SERVER_AUTO_START:'false',CODEX_HOME:path.join(root,'codex-home'),MIRA_NODE_TOKEN:'',CONTROL_SERVER_TOKEN:''};
let child,logs='';
async function wait(fn){const end=Date.now()+40000;while(Date.now()<end){const v=await fn();if(v)return v;await new Promise(r=>setTimeout(r,100));}throw Error('timeout '+logs.slice(-1200));}
async function stop(){if(child?.exitCode===null){const done=once(child,'exit');child.kill('SIGTERM');await done;}}
try{
 child=spawn(binary,['node-worker'],{env:environment,stdio:['ignore','pipe','pipe']});for(const stream of [child.stdout,child.stderr])stream.on('data',v=>logs+=v);
 const pending=await wait(async()=> (await api('/v1/admin/enrollments?status=pending')).body.data?.find(n=>n.nodeKey===key));
 const approved=await api(`/v1/admin/enrollments/${pending.enrollmentId}/approve`,'POST',{});assert.equal(approved.status,200);const nodeID=approved.body.nodeId;
 await wait(async()=> (await api('/v1/nodes/'+nodeID)).body.channelStatus?.connected);
 const taskID=randomUUID(),requestID=randomUUID();
 const task={taskId:taskID,name:'Scheduled script test',nodeId:nodeID,cron:'30 6 * * *',timezone:'Asia/Shanghai',command:process.execPath,args:['-e',`require('fs').appendFileSync(${JSON.stringify(path.join(root,'executions'))},'one'+String.fromCharCode(10));console.log('脚本完成');setTimeout(()=>{},2000)`],cwd:root,enabled:false};
 assert.equal((await fetch(base+'/v1/schedules')).status,401);
 assert.equal((await fetch(base+'/v1/schedules',{method:'POST',headers:{cookie,'Content-Type':'application/json'},body:JSON.stringify(task)})).status,403);
 assert.equal((await api('/v1/schedules','POST',task)).status,200);
 assert.equal((await api('/v1/schedules','POST',task)).status,409);
 assert.equal((await api(`/v1/schedules/${taskID}/run`,'POST',{requestId:requestID})).status,202);
 assert.equal((await api(`/v1/schedules/${taskID}/run`,'POST',{requestId:requestID})).body.runId,requestID);
 assert.equal((await api(`/v1/schedules/${taskID}/run`,'POST',{requestId:randomUUID()})).status,409);
 assert.equal((await api(`/v1/schedules/${taskID}`,'DELETE',{expectedRevision:1})).status,409);
 const done=await wait(async()=>{const r=(await api(`/v1/schedules/${taskID}/runs`)).body.data?.[0];return r?.status==='succeeded'&&r;});
 assert.equal(done.exitCode,0);assert.match(done.output,/脚本完成/);assert.equal(await fs.readFile(path.join(root,'executions'),'utf8'),'one\n');
 assert.equal((await api(`/v1/schedules/${taskID}/run`,'POST',{requestId:requestID})).body.runId,requestID);
 assert.equal((await api(`/v1/schedules/${taskID}`,'PUT',{...task,name:'Edited',expectedRevision:1})).status,200);
 assert.equal((await api(`/v1/schedules/${taskID}`,'PATCH',{enabled:true,expectedRevision:1})).status,409);
 const cli=spawn(binary,['schedule','get',taskID,'--json'],{env:environment,stdio:['ignore','pipe','pipe']});let output='';cli.stdout.on('data',v=>output+=v);cli.stderr.on('data',v=>logs+=v);const [code]=await once(cli,'exit');assert.equal(code,0,logs);assert.ok(output.includes('Edited'));
 assert.equal((await api(`/v1/schedules/${taskID}`,'DELETE',{expectedRevision:2})).status,200);
 assert.equal((await api(`/v1/schedules/${taskID}/runs`)).body.data.length,1,'deleted task lost execution history');
 console.log(JSON.stringify({ok:true,transport:process.env.MIRA_NODE_TRANSPORT||'websocket',auth:true,csrf:true,noOverlap:true,idempotency:true,processExit:true,output:true,revision:true,cli:true}));
}finally{await stop();await fs.rm(root,{recursive:true,force:true});}
