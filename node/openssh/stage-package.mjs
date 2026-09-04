import fs from 'node:fs';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
const [source,destination,platform,arch]=process.argv.slice(2);
execFileSync(process.execPath,[path.join(import.meta.dirname,'verify-package.mjs'),source,platform,arch],{stdio:'inherit'});
const manifest=JSON.parse(fs.readFileSync(path.join(source,'openssh.json'),'utf8'));
const expected=['mira','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen'];
if(platform==='windows')expected.push('ssh-shellhost','ssh-agent','ssh-add','ssh-keyscan','ssh-sk-helper','ssh-pkcs11-helper');
if(JSON.stringify(manifest.roles)!==JSON.stringify(expected))throw Error('Unexpected native role set');
fs.mkdirSync(destination,{recursive:true});
for(const name of [manifest.image,'openssh.json','licenses'])fs.cpSync(path.join(source,name),path.join(destination,name),{recursive:true});
fs.chmodSync(path.join(destination,manifest.image),0o755);
if(platform==='linux'){
  for(const role of expected)fs.symlinkSync(manifest.image,path.join(destination,role));
  fs.copyFileSync(path.join(source,'builder-packages.txt'),path.join(destination,'builder-packages.txt'));
}
// ZIP cannot retain NTFS hard links. The Windows installer creates them from the
// single image after verifying the archive, before switching its version pointer.
