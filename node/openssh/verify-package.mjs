import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
const [directory,platform,arch]=process.argv.slice(2);
const m=JSON.parse(fs.readFileSync(path.join(directory,'openssh.json'),'utf8'));
if(m.schemaVersion!==1||m.backend!=='embedded-openssh'||m.platform!==platform||m.arch!==arch)throw Error('Invalid native package manifest');
const filename='mira-node'+(platform==='windows'?'.exe':'');
if(m.image!==filename)throw Error('Invalid image name');
const b=fs.readFileSync(path.join(directory,filename));
if(m.build?.version!==fs.readFileSync(path.join(import.meta.dirname,'../../VERSION'),'utf8').trim())throw Error('Native image version does not match this release');
if(platform==='windows'){
  const pe=b.length>=64?b.readUInt32LE(60):-1;
  if(arch!=='amd64'||pe<0||pe+6>b.length||b.toString('ascii',0,2)!=='MZ'||b.toString('ascii',pe,pe+4)!=='PE\0\0'||b.readUInt16LE(pe+4)!==0x8664)throw Error('Wrong Windows image architecture');
}else if(b.length<64||b.subarray(0,4).toString('hex')!=='7f454c46'||b[4]!==2||b[5]!==1||b.readUInt16LE(18)!==({amd64:62,arm64:183}[arch]))throw Error('Wrong ELF image architecture');
if(crypto.createHash('sha256').update(b).digest('hex')!==m.sha256)throw Error('Native package digest mismatch');
const marker={linux:'MIRA_LINKED_OPENSSH_LINUX_STATIC_V1',windows:'MIRA_LINKED_OPENSSH_WINDOWS_FULL_V1',android:'MIRA_LINKED_OPENSSH_ANDROID_ROOT_V1'}[platform];
if(!marker||!b.includes(Buffer.from(marker+'\0')))throw Error('Missing linked OpenSSH marker');
if(!fs.statSync(path.join(directory,'licenses')).isDirectory())throw Error('Missing dependency notices');
console.log(`Verified embedded OpenSSH ${platform}/${arch}`);
