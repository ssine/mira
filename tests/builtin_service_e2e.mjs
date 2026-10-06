// Real background Supervisor lifecycle; all state and candidate images are isolated.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
const repo=path.resolve(import.meta.dirname,'..');
const fixture=await fs.mkdtemp(path.join(os.tmpdir(),'mira-builtin-e2e-'));
const binary=process.env.MIRA_TEST_BINARY;
assert(binary,'set MIRA_TEST_BINARY to a freshly built Mira image');
const state=path.join(fixture,'state'),bin=path.join(fixture,'bin');await fs.mkdir(bin);
await fs.writeFile(path.join(bin,'systemctl'),'#!/bin/sh\nexit 1\n',{mode:0o755});
const env={...Object.fromEntries(Object.entries(process.env).filter(([key])=>! /^(MIRA_|NODE_AGENT_|APP_SERVER_|CONTROL_SERVER_)/.test(key))),HOME:fixture,PATH:`${bin}:${process.env.PATH}`,CODEX_BINARY:path.join(fixture,'no-codex')};
const run=(args,executable=binary)=>JSON.parse(execFileSync(executable,['--json',...args],{env,encoding:'utf8',timeout:60000})).data;
const installed=()=>path.join(state,'current','mira');
const service=()=>run(['status','--state-dir',state],installed());
const wait=async(fn,label)=>{for(let i=0;i<200;i++){try{if(await fn())return}catch{}await new Promise(resolve=>setTimeout(resolve,50))}throw Error('timeout: '+label)};
async function children(pid){const tids=await fs.readdir(`/proc/${pid}/task`);const values=await Promise.all(tids.map(async tid=>{try{return await fs.readFile(`/proc/${pid}/task/${tid}/children`,'utf8')}catch{return ''}}));return [...new Set(values.join(' ').trim().split(/\s+/).filter(Boolean).map(Number))]}
try {
 const report=run(['install','--state-dir',state,'--server-url','http://127.0.0.1:9','--service-owner','mira']);
 assert.equal(report.serviceManager,'builtin');let current=service();const originalPID=current.pid;
 assert.equal(current.status,'running');assert.notEqual(originalPID,process.pid);
 assert.equal(run(['doctor','--state-dir',state],installed()).healthy,true);
 await Promise.all(Array.from({length:6},()=>new Promise((resolve,reject)=>{
  const p=spawn(installed(),['--json','start','--state-dir',state],{env,stdio:['ignore','pipe','pipe']});let output='';p.stdout.on('data',b=>output+=b);p.on('error',reject);p.on('exit',code=>code===0?resolve(JSON.parse(output)):reject(Error('concurrent start failed')))
 })));
 assert.equal(service().pid,originalPID);assert.equal((await children(originalPID)).length,1);
 const worker=(await children(originalPID))[0];process.kill(worker,'SIGKILL');await wait(async()=>{const p=await children(originalPID);return p.length===1&&p[0]!==worker},'worker recovery');
 // Compile a separate test candidate; this is not a release package.
 const goodDir=path.join(state,'versions','9.0.2');await fs.mkdir(goodDir,{recursive:true});
 execFileSync('go',['-C',path.join(repo,'node'),'build','-ldflags=-X github.com/ssine/mira/node/internal.Version=9.0.2','-o',path.join(goodDir,'mira'),'./cmd/mira'],{env:process.env,timeout:120000});
 const updated=run(['update','--state-dir',state,'--version','9.0.2'],installed());assert.equal(updated.status,'succeeded');
 await wait(()=>service().version==='9.0.2','exec successor');assert.equal(service().pid,originalPID);assert.equal((await children(originalPID)).length,1);
 const badDir=path.join(state,'versions','9.0.3');await fs.mkdir(badDir);
 await fs.writeFile(path.join(badDir,'mira'),'#!/bin/sh\ncase "$1" in --version) echo "mira 9.0.3";; supervisor-check) exit 0;; *) exit 23;; esac\n',{mode:0o755});
 assert.throws(()=>run(['update','--state-dir',state,'--version','9.0.3'],installed()));
 assert.equal(service().version,'9.0.2');assert.equal(service().pid,originalPID);assert.equal((await children(originalPID)).length,1);
 assert.equal(JSON.parse(await fs.readFile(path.join(state,'supervisor-update-status.json'))).phase,'rolled_back');
 run(['restart','--state-dir',state],installed());current=service();assert.notEqual(current.pid,originalPID);
 run(['stop','--state-dir',state],installed());assert.equal(service().status,'stopped');
 assert.equal(run(['doctor','--state-dir',state],installed()).healthy,false);
 run(['start','--state-dir',state],installed());assert.equal(service().status,'running');
 console.log('PASS auto builtin install, background detach, concurrent start, worker recovery, exec update with stable PID, rollback, restart, stop, doctor');
} finally {
 try {run(['stop','--state-dir',state],installed())}catch{}
 await fs.rm(fixture,{recursive:true,force:true});
}
