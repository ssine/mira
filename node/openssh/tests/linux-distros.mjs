// One immutable Linux image, different container userlands, actual Mira relay.
import fs from 'node:fs/promises';
import path from 'node:path';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import {execFileSync} from 'node:child_process';
import {approvePendingNode,adminRequest} from '../../../tests/auth_helpers.mjs';

export default async function(ctx){
  const {fixture,url,admin,nodes,good,wait,launch,openSSHDir}=ctx;
  assert(process.env.MIRA_TEST_LINUX_SINGLEFILE,'distro test requires the true linked image');
  const uid=process.getuid(),gid=process.getgid();assert(uid>0,'run as an ordinary Linux user');
  for(const image of ['alpine:3.22.2','ubuntu:24.04','rockylinux:9']){
    const key='openssh-distro-'+crypto.randomBytes(6).toString('hex');
    const directory=path.join(fixture,key);await fs.mkdir(directory);
    const passwd=path.join(directory,'passwd'),group=path.join(directory,'group');
    await fs.writeFile(passwd,`root:x:0:0:root:/root:/bin/sh\nmira-test:x:${uid}:${gid}:Mira test:${directory}:/bin/sh\n`);
    await fs.writeFile(group,`root:x:0:\nmira-test:x:${gid}:\n`);
    let device;
    try{
      launch('docker',['run','--rm','--name',key,'--network=host','--user',`${uid}:${gid}`,
        '-v',`${openSSHDir}:/mira:ro`,'-v',`${fixture}:${fixture}`,'-v',`${passwd}:/etc/passwd:ro`,'-v',`${group}:/etc/group:ro`,
        '-e',`MIRA_SERVER_URL=${url}`,'-e',`MIRA_NODE_KEY=${key}`,'-e',`MIRA_IDENTITY_FILE=${directory}/identity.json`,
        '-e','MIRA_NODE_OPENSSH_DIR=/mira','-e','MIRA_NODE_ALLOWED_ROOTS=["/"]','-e','MIRA_NODE_HEARTBEAT_SECONDS=1',
        '-e','APP_SERVER_AUTO_START=false','-e','CODEX_BINARY=/nonexistent/codex',image,'/mira/mira-node']);
      await approvePendingNode(url,admin,key);
      device=await wait(async()=>{const list=await adminRequest(url,admin,'/v1/nodes');return list.data.find(n=>n.nodeKey===key&&n.channelStatus?.connected)},'container '+image);
      assert.equal(device.machineStatus.ssh.username,'mira-test');
      const r=await good(nodes[0].identity,['ssh','-C','-c','aes256-gcm@openssh.com',key,'--','id -u']);assert.equal(Number(r.stdout.toString()),uid);
      const pty=await good(nodes[0].identity,['ssh','-tt',key,'--','test -t 0 && echo DISTRO_PTY_OK']);assert(pty.stdout.includes('DISTRO_PTY_OK'));
      const data=crypto.randomBytes(1024*1024+17),local=path.join(nodes[0].dir,'distro.bin'),back=path.join(nodes[0].dir,'distro-back.bin');await fs.writeFile(local,data);
      await good(nodes[0].identity,['scp',local,`${key}:${directory}/remote.bin`]);
      await good(nodes[0].identity,['sftp','-b','-',key],{input:`rename ${directory}/remote.bin ${directory}/renamed.bin\nget ${directory}/renamed.bin ${back}\n`});assert.deepEqual(await fs.readFile(back),data);
      const reverse=await good(nodes[0].identity,['ssh',key,'--',`env MIRA_IDENTITY_FILE=${directory}/identity.json MIRA_NODE_OPENSSH_DIR=/mira /mira/mira ssh ${nodes[1].key} -- printf DISTRO_REVERSE_OK`]);assert(reverse.stdout.includes('DISTRO_REVERSE_OK'));
      console.log(`PASS ${image}: same static image, Mira enrollment/relay, AES-GCM + compression, PTY, binary SCP/SFTP, reverse native CLI`);
    }finally{
      if(device)await adminRequest(url,admin,`/v1/admin/nodes/${device.nodeId}/revoke`,{method:'POST',body:'{}'}).catch(()=>{});
      try{execFileSync('docker',['stop','-t','3',key],{stdio:'pipe',timeout:15000})}catch{}
    }
  }
}
