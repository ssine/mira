// Tests the real APK Service/configuration code under a separate application ID.
// ADB only configures/inspects this explicit test package; transport is Mira SSH.
import fs from 'node:fs/promises';
import path from 'node:path';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {approvePendingNode,adminRequest} from '../../../tests/auth_helpers.mjs';

export default async function(ctx){
  const {url,publicURL,admin,nodes,good,cli,wait}=ctx;
  const serial=process.env.MIRA_TEST_ADB_SERIAL;
  assert(serial,'MIRA_TEST_ADB_SERIAL is required');
  const pkg='com.ssine.mira.opensshpoc',base=`/data/user/0/${pkg}/no_backup`;
  const nonce=crypto.randomBytes(8).toString('hex'),key='openssh-apk-'+nonce;
  const adb=(args,options={})=>execFileSync('adb',['-s',serial,...args],{encoding:'utf8',timeout:30000,...options});
  const app=(command,options={})=>adb(['shell',`run-as ${pkg} ${command}`],options);
  const escape=s=>s.replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;');
  const quote=s=>"'"+s.replaceAll("'","'\\''")+"'";
  let device;
  let upgraded=false;
  const source=nodes[0];
  function start(mode){
    adb(['shell','am','force-stop',pkg]);
    app('mkdir -p shared_prefs');
    app('tee shared_prefs/codex_node.xml >/dev/null',{input:`<?xml version="1.0" encoding="utf-8"?><map><string name="server_url">${escape(publicURL)}</string><string name="node_key">${key}</string><string name="privilege_mode">${mode}</string><boolean name="auto_start" value="true"/><boolean name="user_stopped" value="false"/></map>`});
    app('chmod 600 shared_prefs/codex_node.xml');
    adb(['shell','am','start','-n',pkg+'/com.ssine.codexnode.MainActivity']);
  }
  try {
    adb(['shell','am','force-stop',pkg]);
    // Only the isolated package's test identity, never the production identity.
    app('mkdir -p no_backup');app('chmod 700 no_backup');app('rm -f no_backup/identity.json');
    start('app');
    await approvePendingNode(url,admin,key);
    device=await wait(async()=>{const list=await adminRequest(url,admin,'/v1/nodes');return list.data.find(n=>n.nodeKey===key&&n.channelStatus?.connected)},'real APK app-mode Node');
    const originalIdentity=JSON.parse(app('cat no_backup/identity.json'));
    const uid=Number(app('id -u'));
    const invoke=async(capability,params)=>(await adminRequest(url,admin,`/v1/nodes/${device.nodeId}/invoke`,{method:'POST',body:JSON.stringify({capability,params,timeoutMs:30000})})).result;
    for(const mode of ['app','root','app']){
      if(mode==='root'||device.machineStatus.ssh.username==='root')start(mode);
      device=await wait(async()=>{const n=await adminRequest(url,admin,`/v1/nodes/${device.nodeId}`);return n.channelStatus?.connected&&n.machineStatus?.ssh?.backend==='openssh'&&(n.machineStatus.ssh.username==='root')===(mode==='root')?n:null},'mode '+mode);
      assert.equal(device.nodeId,originalIdentity.nodeId,'mode switch must preserve approved node identity');
      const currentIdentity=JSON.parse(app('cat no_backup/identity.json'));
      assert.equal(currentIdentity.nodeId,originalIdentity.nodeId);
      const identityStat=app('stat -c "%u %a" no_backup/identity.json').trim();
      assert.equal(identityStat,`${uid} 600`,'root mode must not take ownership of the app identity');
      if(mode==='app'&&!upgraded&&process.env.MIRA_TEST_ANDROID_REINSTALL_APK){
        const apk=path.resolve(process.env.MIRA_TEST_ANDROID_REINSTALL_APK);
        assert(apk.endsWith('/node/android/build/outputs/apk/debug/mira-node-debug.apk'),'use the real-source isolated debug APK');
        assert(adb(['install','-r',apk],{timeout:60000}).includes('Success'));
        adb(['shell','am','start','-n',pkg+'/com.ssine.codexnode.MainActivity']);
        await wait(async()=>{const n=await adminRequest(url,admin,`/v1/nodes/${device.nodeId}`);return n.channelStatus?.connected},'APK update reconnect');
        assert.equal(JSON.parse(app('cat no_backup/identity.json')).nodeId,originalIdentity.nodeId);
        const codePath=adb(['shell','dumpsys','package',pkg]).match(/codePath=(\S+)/)?.[1];
        assert(codePath);
        await wait(async()=>app('readlink no_backup/openssh-bin/sshd').trim()===codePath+'/lib/arm64/libmira_node.so','new APK native role target');
        upgraded=true;
        console.log('PASS APK reinstall: identity retained and role symlinks retargeted to new package native directory');
      }
      let r=await good(source.identity,['ssh',key,'--','id -u']);assert.equal(Number(r.stdout.toString()),mode==='root'?0:uid);
      r=await good(source.identity,['ssh','-tt',key,'--','test -t 0 && printf APK_PTY_OK']);assert(r.stdout.includes('APK_PTY_OK'));
      r=await cli(source.identity,['ssh',key,'--','exit 31']);assert.equal(r.code,31);
      const data=crypto.randomBytes(1024*1024+53),local=path.join(source.dir,'apk.bin'),back=path.join(source.dir,'apk-back.bin');await fs.writeFile(local,data);
      const remote=`${base}/transfer-${nonce}-${mode}`;
      await good(source.identity,['scp',local,`${key}:${remote}`]);
      await good(source.identity,['sftp','-b','-',key],{input:`rename ${remote} ${remote}.renamed\nget ${remote}.renamed ${back}\nrm ${remote}.renamed\n`});assert.deepEqual(await fs.readFile(back),data);
      await Promise.all(Array.from({length:4},()=>good(source.identity,['ssh',key,'--','true'])));
      const command=`env MIRA_IDENTITY_FILE=${base}/identity.json MIRA_NODE_OPENSSH_DIR=${base}/openssh-bin MIRA_NODE_OPENSSH_ANDROID_ROOT=1 ${base}/openssh-bin/mira`;
      r=await good(source.identity,['ssh',key,'--',`${command} ssh ${nodes[1].key} -- printf APK_TO_LINUX_OK`]);assert(r.stdout.includes('APK_TO_LINUX_OK'));
      const status=await invoke('status',{});assert.equal(status.rootEnabled,mode==='root');assert(status.memory.totalBytes>0);
      assert((await invoke('process',{action:'count'})).processCount>0);
      assert((await invoke('file',{action:'roots'})).roots.some(r=>r.configured==='/'));
      if(mode==='app')assert.notEqual((await cli(source.identity,['ssh',key,'--','ls /data/system'])).code,0);
      if(mode==='root'){
        const shot=await invoke('screen',{action:'screenshot'});assert.equal(Buffer.from(shot.content,'base64').subarray(0,8).toString('hex'),'89504e470d0a1a0a');
      }
      if(mode==='app'&&process.env.MIRA_TEST_WINDOWS_BIN){
        const {default:windows}=await import('./windows.mjs');
        await windows({...ctx,extraNode:{key,runCLI:args=>good(source.identity,['ssh',key,'--',command+' '+args.map(quote).join(' ')])}});
        delete process.env.MIRA_TEST_WINDOWS_BIN;
      }
      console.log(`PASS real APK ${mode}: same identity, SSH/PTY/exit, binary SCP/SFTP, concurrency, outbound CLI, status/process/files${mode==='root'?', root screenshot':''}`);
    }
    const pidFile=`${base}/remote-${nonce}.pid`;
    const alive=cli(source.identity,['ssh',key,'--',`echo $$ > ${pidFile}; exec sleep 60`],{timeout:20000});
    const pid=await wait(async()=>{try{return Number(app(`cat ${pidFile}`,{stdio:['ignore','pipe','pipe']}))}catch{return null}},'APK remote command');
    await adminRequest(url,admin,`/v1/admin/nodes/${device.nodeId}/revoke`,{method:'POST',body:'{}'});
    assert.notEqual((await alive).code,0);
    await wait(async()=>{try{app(`/system/bin/kill -0 ${pid}`,{stdio:['ignore','pipe','pipe']});return false}catch{return true}},'APK revoked descendant');
    app(`rm -f ${pidFile}`);
    console.log('PASS real APK app → root → app without re-enrollment; revocation reaps remote child');
  }catch(e){try{console.error(app('cat shared_prefs/codex_node.xml'))}catch{}throw e}
  finally{
    if(device)await adminRequest(url,admin,`/v1/admin/nodes/${device.nodeId}/revoke`,{method:'POST',body:'{}'}).catch(()=>{});
    adb(['shell','am','force-stop',pkg]);
    // Remove the temporary enrollment, retaining only the stopped test APK.
    app('rm -f no_backup/identity.json no_backup/node-config.json shared_prefs/codex_node.xml');
  }
}
