// Real Mira Server + isolated PostgreSQL database + temporary approved Nodes.
// Embedded OpenSSH regression; never uses production credentials.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {spawn,execFileSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';
import pg from '../../../server/node_modules/pg/lib/index.js';
import net from 'node:net';
import {initializeDatabase} from '../../../server/db.mjs';
import {hashPassword} from '../../../server/auth.mjs';
import {loginAdmin,approvePendingNode,adminRequest} from '../../../tests/auth_helpers.mjs';

const repo=path.resolve(import.meta.dirname,'../../..');
const fixture=await fs.mkdtemp(path.join(os.homedir(),'.mira-openssh-e2e-'));
const database=`mira_openssh_${process.pid}_${crypto.randomBytes(3).toString('hex')}`;
const baseURL=process.env.MIRA_TEST_DATABASE_URL??'postgresql://mira:mira-local@127.0.0.1:55433/mira';
const connection=new URL(baseURL);connection.pathname='/'+database;
const rootPool=new pg.Pool({connectionString:baseURL});let pool,created=false;
const port=Number(process.env.MIRA_OPENSSH_TEST_PORT??18879);
const url=`http://127.0.0.1:${port}`;
const publicURL=process.env.MIRA_OPENSSH_TEST_PUBLIC_URL??url;
const binaries=path.join(fixture,'bin');await fs.mkdir(binaries);
const openSSHDir=binaries;
const nodeBinary=path.join(binaries,'mira-node'),cliBinary=path.join(binaries,'mira');
const processes=[],logs=[];
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
async function wait(fn,label){for(let i=0;i<150;i++){const value=await fn();if(value)return value;await sleep(200)}throw Error(`timeout: ${label}`)}
function launch(executable,args,env={}){const p=spawn(executable,args,{cwd:repo,env:{...process.env,...env},stdio:['ignore','pipe','pipe']});for(const s of [p.stdout,p.stderr])s.on('data',b=>{logs.push(b.toString());if(logs.length>150)logs.shift()});processes.push(p);return p;}
function cli(identity,args,{input='',timeout=20000}={}){return new Promise((resolve,reject)=>{
  const p=spawn(cliBinary,args,{env:{...process.env,MIRA_IDENTITY_FILE:identity,MIRA_NODE_OPENSSH_DIR:''}});
  const out=[],err=[];const timer=setTimeout(()=>{p.kill('SIGKILL');reject(Error(`CLI timeout: ${args[0]}`))},timeout);
  p.stdout.on('data',b=>out.push(b));p.stderr.on('data',b=>err.push(b));p.on('error',reject);
  p.on('close',code=>{clearTimeout(timer);resolve({code,stdout:Buffer.concat(out),stderr:Buffer.concat(err).toString()})});p.stdin.on('error',()=>{});p.stdin.end(input);
});}
async function good(identity,args,opts){const r=await cli(identity,args,opts);assert.equal(r.code,0,r.stderr);return r;}
async function verifyForward(identity,nodeKey,directory){
  const echo=net.createServer(s=>s.pipe(s));
  await new Promise(r=>echo.listen(0,'127.0.0.1',r));
  const reservation=net.createServer();await new Promise(r=>reservation.listen(0,'127.0.0.1',r));
  const port=reservation.address().port;await new Promise(r=>reservation.close(r));
  const control=path.join(directory,'forward-control');
  try{
    await good(identity,['ssh','-fNM','-S',control,'-oExitOnForwardFailure=yes','-oControlPersist=10','-L',`${port}:127.0.0.1:${echo.address().port}`,nodeKey]);
    const response=await new Promise((resolve,reject)=>{const s=net.connect(port,'127.0.0.1');s.setTimeout(5000,()=>s.destroy(Error('forward timeout')));s.on('error',reject);s.on('connect',()=>s.write('MIRA_FORWARD_OK'));s.once('data',b=>{s.destroy();resolve(b.toString())})});
    assert.equal(response,'MIRA_FORWARD_OK');
  }finally{await cli(identity,['ssh','-S',control,'-O','exit',nodeKey]);await new Promise(r=>echo.close(r))}
}
let admin;
async function enroll(name,env={}){
  const dir=path.join(fixture,name);await fs.mkdir(dir);
  const identity=path.join(dir,'identity.json'),key=`openssh-${process.pid}-${name}`;
  launch(nodeBinary,[],{MIRA_SERVER_URL:url,MIRA_NODE_KEY:key,MIRA_IDENTITY_FILE:identity,MIRA_NODE_TOKEN:'',CONTROL_SERVER_TOKEN:'',MIRA_NODE_OPENSSH_DIR:'',MIRA_NODE_ALLOWED_ROOTS:'["/"]',APP_SERVER_AUTO_START:'false',CODEX_BINARY:path.join(fixture,'no-codex'),MIRA_NODE_HEARTBEAT_SECONDS:'1',...env});
  await approvePendingNode(url,admin,key);
  const state=await wait(async()=>{try{const s=JSON.parse(await fs.readFile(identity));return s.nodeId?s:null}catch{return null}},'identity approval');
  await wait(async()=>{const s=await adminRequest(url,admin,`/v1/nodes/${state.nodeId}`);return s.channelStatus?.connected},'reverse channel');
  return {dir,identity,key,nodeId:state.nodeId};
}
try{
  await rootPool.query(`CREATE DATABASE ${database}`);created=true;
  pool=new pg.Pool({connectionString:connection.toString()});await initializeDatabase(pool);
  const password=crypto.randomBytes(24).toString('base64url');
  await pool.query("INSERT INTO mira_admin_users(username,password_hash) VALUES ('admin',$1)",[await hashPassword(password)]);
  assert(process.env.MIRA_TEST_LINUX_SINGLEFILE,'set MIRA_TEST_LINUX_SINGLEFILE to the linked Linux image');
  const image=path.resolve(process.env.MIRA_TEST_LINUX_SINGLEFILE);
  await fs.copyFile(image,nodeBinary);await fs.chmod(nodeBinary,0o700);
  for(const role of ['mira','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen'])await fs.link(nodeBinary,path.join(binaries,role));
  assert.equal(execFileSync(nodeBinary,['--mira-openssh-build'],{encoding:'utf8'}).trim(),'MIRA_LINKED_OPENSSH_LINUX_STATIC_V1');
  launch(process.execPath,['server/server.mjs'],{DATABASE_URL:connection.toString(),LISTEN_HOST:process.env.MIRA_OPENSSH_TEST_LISTEN??'127.0.0.1',LISTEN_PORT:String(port),MIRA_SECURE_COOKIES:'false'});
  await wait(async()=>{try{return(await fetch(url+'/healthz')).ok}catch{return false}},'test Server');
  admin=await loginAdmin(url,'admin',password);
  const a=await enroll('source'),b=await enroll('target');
  let r=await good(a.identity,['ssh',b.key,'--','printf RELAY_OK; id -u']);assert.equal(r.stdout.toString(),`RELAY_OK${process.getuid()}\n`);
  console.log('PASS native OpenSSH, Mira-derived keys, real approved reverse relay');
  r=await cli(a.identity,['ssh',b.key,'--','printf OUT; printf ERR >&2; exit 23']);assert.equal(r.code,23);assert.equal(r.stdout.toString(),'OUT');assert(r.stderr.includes('ERR'));
  const data=crypto.randomBytes(2*1024*1024);r=await good(a.identity,['ssh',b.key,'--','cat'],{input:data});assert.deepEqual(r.stdout,data);
  r=await good(a.identity,['ssh','-tt',b.key,'--','test -t 0 && printf PTY_OK']);assert(r.stdout.includes('PTY_OK'));
  await Promise.all(Array.from({length:4},()=>good(a.identity,['ssh',b.key,'--','true'])));
  const source=path.join(a.dir,'folder');await fs.mkdir(source);await fs.writeFile(path.join(source,'中文.bin'),data);
  await good(a.identity,['scp','-rp',source,`${b.key}:${b.dir}/`]);assert.deepEqual(await fs.readFile(path.join(b.dir,'folder/中文.bin')),data);
  await good(a.identity,['sftp','-b','-',b.key],{input:`rename ${b.dir}/folder ${b.dir}/renamed\nchmod 700 ${b.dir}/renamed/中文.bin\nget ${b.dir}/renamed/中文.bin ${a.dir}/download.bin\n`});assert.deepEqual(await fs.readFile(path.join(a.dir,'download.bin')),data);
  console.log('PASS separate stderr/exit, binary streams, PTY, concurrency, recursive SCP, SFTP batch');
  const control=path.join(a.dir,'control');
  await good(a.identity,['ssh','-M','-S',control,'-o','ControlPersist=10',b.key,'--','true']);
  await good(a.identity,['ssh','-S',control,b.key,'--','printf MUX_OK']);
  await good(a.identity,['ssh','-S',control,'-O','exit',b.key]);
  console.log('PASS native ControlPersist');
  await verifyForward(a.identity,b.key,a.dir);
  console.log('PASS native SSH local TCP forwarding over Mira relay');
  if(process.env.MIRA_OPENSSH_DEVICE_TEST){const hook=await import(pathToFileURL(path.resolve(process.env.MIRA_OPENSSH_DEVICE_TEST)));await hook.default({repo,fixture,url,publicURL,admin,nodes:[a,b],cli,good,wait,launch,nodeBinary,cliBinary,pool,openSSHDir,verifyForward});}
  const pidFile=path.join(b.dir,'remote.pid');
  const alive=cli(a.identity,['ssh',b.key,'--',`echo $$ > '${pidFile}'; exec sleep 60`],{timeout:15000});
  const pid=await wait(async()=>{try{return Number(await fs.readFile(pidFile,'utf8'))}catch{return null}},'remote command PID');
  await adminRequest(url,admin,`/v1/admin/nodes/${a.nodeId}/revoke`,{method:'POST',body:'{}'});
  assert.notEqual((await alive).code,0);
  await wait(async()=>{try{process.kill(pid,0);return false}catch(e){return e.code==='ESRCH'}},'reap OpenSSH descendant after revoke');
  assert.notEqual((await cli(a.identity,['ssh',b.key,'--','true'])).code,0);
  console.log('PASS revocation closes encrypted relay, reaps remote command and denies new connections');
}catch(e){console.error(logs.join('').slice(-10000));throw e}
finally{
  for(const p of processes.reverse()){if(p.exitCode!==null)continue;p.kill('SIGTERM');await Promise.race([new Promise(r=>p.once('close',r)),sleep(2500)]);if(p.exitCode===null)p.kill('SIGKILL')}
  await pool?.end();if(created)await rootPool.query(`DROP DATABASE ${database} WITH (FORCE)`);await rootPool.end();await fs.rm(fixture,{recursive:true,force:true});
}
