import fs from 'node:fs/promises';
import path from 'node:path';
import crypto from 'node:crypto';
import readline from 'node:readline';
import {spawn,execFileSync} from 'node:child_process';
import assert from 'node:assert/strict';
import {approvePendingNode,adminRequest} from '../../../tests/auth_helpers.mjs';

export default async function(ctx){
  const {repo,url,publicURL,admin,nodes,good,cli,wait}=ctx;
  const directory=process.env.MIRA_TEST_WINDOWS_BIN;
  assert(directory && path.isAbsolute(directory),'set MIRA_TEST_WINDOWS_BIN to the linked bundle');
  await fs.mkdir(directory,{recursive:true});
  {
    // Do not silently replace the linked Go/OpenSSH image with standalone Go.
    const hashes=await Promise.all(['mira','mira-node','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen','ssh-shellhost'].map(async command=>crypto.createHash('sha256').update(await fs.readFile(path.join(directory,command+'.exe'))).digest('hex')));
    assert.equal(new Set(hashes).size,1,'all roles must contain the same linked image');
    console.log('Testing one linked Windows PE image for Node, CLI and all OpenSSH roles');
  }
  const windowsPath=p=>execFileSync('wslpath',['-w',p],{encoding:'utf8'}).trim();
  const key='openssh-windows-'+crypto.randomUUID();
  const peer=spawn('/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe',['-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File',windowsPath(path.join(repo,'node/openssh/tests/windows-peer.ps1')),'-BinaryDirectory',windowsPath(directory),'-ServerUrl',publicURL,'-NodeKey',key,...(process.env.MIRA_TEST_OPENSSH_DEBUG==='1'?['-DebugOpenSSH']:[])]);
  let peerErrors='';peer.stderr.on('data',b=>peerErrors+=b);
  const pending=new Map();let readyResolve;
  const ready=new Promise(r=>readyResolve=r);
  readline.createInterface({input:peer.stdout}).on('line',line=>{try{const r=JSON.parse(line);if(r.type==='ready')readyResolve(r);else pending.get(r.id)?.(r)}catch{peerErrors+=line+'\n'}});
  const exited=new Promise(r=>peer.on('exit',r));
  function request(op,args=[],input=''){const id=crypto.randomUUID();return new Promise((resolve,reject)=>{const timer=setTimeout(()=>reject(Error('Windows peer timeout: '+peerErrors)),40000);pending.set(id,r=>{clearTimeout(timer);pending.delete(id);resolve(r)});peer.stdin.write(JSON.stringify({id,op,args,input:Buffer.from(input).toString('base64')})+'\n')});}
  async function windowsCLI(args,input=''){const r=await request('cli',args,input);return{code:r.code,stdout:Buffer.from(r.stdout,'base64'),stderr:Buffer.from(r.stderr,'base64').toString()}}
  let device;
  try{
    const started=await Promise.race([ready,exited.then(()=>{throw Error(peerErrors||'Windows peer exited')})]);
    await approvePendingNode(url,admin,key);
    device=await wait(async()=>{const list=await adminRequest(url,admin,'/v1/nodes');return list.data.find(n=>n.nodeKey===key && n.channelStatus?.connected)},'Windows native Node channel');
    assert.equal(device.machineStatus.ssh.backend,'openssh');
    const source=nodes[0];
    {
      for(const [query,required] of [['cipher','aes256-gcm@openssh.com'],['key','ecdsa-sha2-nistp256']]){
        const result=await request('native',['ssh','-Q',query]);assert.equal(result.code,0);assert(Buffer.from(result.stdout,'base64').includes(required));
      }
      for(const type of ['rsa','ecdsa']){
        const keygen=await request('native',['ssh-keygen','-q','-t',type,'-N','','-f',path.win32.join(started.fixture,type)]);
        assert.equal(keygen.code,0,Buffer.from(keygen.stderr,'base64').toString());
      }
      const compressed=await good(source.identity,['ssh','-C','-c','aes256-gcm@openssh.com',key,'--','echo FULL_CRYPTO_OK']);assert(compressed.stdout.includes('FULL_CRYPTO_OK'));
      const native=await windowsCLI(['ssh','-C','-c','aes256-gcm@openssh.com',nodes[1].key,'--','printf FULL_CRYPTO_REVERSE_OK']);assert.equal(native.code,0,native.stderr);assert(native.stdout.includes('FULL_CRYPTO_REVERSE_OK'));
      console.log('PASS full Windows crypto: RSA/ECDSA keygen, AES-GCM + compression in both directions');
    }
    let r=await good(source.identity,['ssh',key,'--','echo WINDOWS_OPENSSH_OK']);assert(r.stdout.includes('WINDOWS_OPENSSH_OK'));
    r=await good(source.identity,['ssh','-tt',key,'--','echo WINDOWS_PTY_OK']);assert(r.stdout.includes('WINDOWS_PTY_OK'));
    r=await cli(source.identity,['ssh',key,'--','exit /b 19']);assert.equal(r.code,19,r.stderr);
    const win=await windowsCLI(['ssh',nodes[1].key,'--','printf WINDOWS_TO_LINUX_OK']);assert.equal(win.code,0,win.stderr);assert.equal(win.stdout.toString(),'WINDOWS_TO_LINUX_OK');
    const self=await windowsCLI(['ssh',key,'--','echo WINDOWS_TO_WINDOWS_OK']);assert.equal(self.code,0,self.stderr);assert(self.stdout.includes('WINDOWS_TO_WINDOWS_OK'));
    await Promise.all(Array.from({length:4},()=>good(source.identity,['ssh',key,'--','echo CONCURRENT_OK'])));
    await ctx.verifyForward(source.identity,key,source.dir);
    const data=crypto.randomBytes(1024*1024+37),local=path.join(source.dir,'windows-source.bin'),restored=path.join(source.dir,'windows-restored.bin');await fs.writeFile(local,data);
    const remote='/'+started.fixture.replaceAll('\\','/')+'/remote.bin';
    await good(source.identity,['scp',local,`${key}:${remote}`]);await good(source.identity,['scp',`${key}:${remote}`,restored]);assert.deepEqual(await fs.readFile(restored),data);
    await good(source.identity,['sftp','-b','-',key],{input:`rename ${remote} ${remote}.renamed\nget ${remote}.renamed ${restored}\n`});assert.deepEqual(await fs.readFile(restored),data);
    if(ctx.extraNode){
      const r=await windowsCLI(['ssh',ctx.extraNode.key,'--','printf WINDOWS_TO_ANDROID_OK']);assert.equal(r.code,0,r.stderr);assert(r.stdout.includes('WINDOWS_TO_ANDROID_OK'));
      const reverse=await ctx.extraNode.runCLI(['ssh',key,'--','echo ANDROID_TO_WINDOWS_OK']);assert(reverse.stdout.includes('ANDROID_TO_WINDOWS_OK'));
      console.log('PASS Windows <-> Android native clients, each using its own Mira identity and reverse relay');
    }
    const pidPath=started.fixture+'\\remote.pid';
    const encoded=Buffer.from(`[IO.File]::WriteAllText('${pidPath}',[string]$PID);Start-Sleep -Seconds 60`,'utf16le').toString('base64');
    const alive=cli(source.identity,['ssh',key,'--','powershell.exe -NoProfile -NonInteractive -EncodedCommand '+encoded],{timeout:20000});
    await wait(async()=>{const r=await request('probe');return r.alive&&r.remotePid},'Windows remote child');
    await adminRequest(url,admin,`/v1/admin/nodes/${device.nodeId}/revoke`,{method:'POST',body:'{}'});
    assert.notEqual((await alive).code,0);
    await wait(async()=>!(await request('probe')).alive,'Windows Job Object descendant cleanup');
    console.log('PASS native Windows OpenSSH through Mira identity/relay: exec, PTY, exit, 4-way concurrency, binary SCP/SFTP/rename, Windows-to-Linux and Windows-to-Windows clients');
    console.log('PASS Windows target revocation reaps remote PowerShell process');
  }catch(e){try{const logs=await request('logs');console.error(String(logs.stdout).slice(-4000),String(logs.stderr).slice(-5000))}catch{}console.error(peerErrors);throw e}
  finally{
    if(device)await adminRequest(url,admin,`/v1/admin/nodes/${device.nodeId}/revoke`,{method:'POST',body:'{}'}).catch(()=>{});
    peer.stdin.end(JSON.stringify({op:'quit'})+'\n');await Promise.race([exited,new Promise(r=>setTimeout(()=>{peer.kill();r()},5000))]);
  }
}
