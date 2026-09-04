// Package provenance and redistribution notices; never include build-host paths.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import {fileURLToPath} from 'node:url';
import {execFileSync} from 'node:child_process';
const [directory,platform,arch,workspace]=process.argv.slice(2);
const component=path.dirname(fileURLToPath(import.meta.url));
const markers={linux:'MIRA_LINKED_OPENSSH_LINUX_STATIC_V1',windows:'MIRA_LINKED_OPENSSH_WINDOWS_FULL_V1',android:'MIRA_LINKED_OPENSSH_ANDROID_ROOT_V1'};
if(!markers[platform]||!['amd64','arm64'].includes(arch)||!workspace)throw Error('Usage: manifest.mjs bundle platform arch build-workspace');
const filename='mira-node'+(platform==='windows'?'.exe':'');
const bytes=fs.readFileSync(path.join(directory,filename));
if(!bytes.includes(Buffer.from(markers[platform]+'\0')))throw Error('Not a linked OpenSSH image');
const info=execFileSync('go',['version','-m',path.join(directory,filename)],{encoding:'utf8'});
const metadata=name=>info.match(new RegExp('github.com/ssine/mira/node/internal\\.'+name+'=([^\\s"\\\\]+)'))?.[1];
if(metadata('BundledOpenSSH')!=='true')throw Error('Go adapter was not built for embedded OpenSSH');
const build={version:metadata('Version'),commit:metadata('Commit'),buildTime:metadata('BuildTime')};
if(!build.version||!build.commit||!build.buildTime)throw Error('Missing linked build metadata');
const roles=['mira','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen'];
if(platform==='windows')roles.push('ssh-shellhost','ssh-agent','ssh-add','ssh-keyscan','ssh-sk-helper','ssh-pkcs11-helper');
const sources=JSON.parse(fs.readFileSync(path.join(component,'sources.json'),'utf8')).filter(s=>s.platforms.includes(platform));
const licenses=path.join(directory,'licenses');fs.mkdirSync(licenses,{recursive:true});
const trees=platform==='windows'?['source','deps/libressl-4.2.0','deps/zlib-1.3.2','deps/libcbor-0.14.0','deps/libfido2-1.16.0']:['openssh-10.5p1','openssl-3.5.5'];
for(const tree of trees){
  const root=path.join(workspace,tree);
  const files=fs.readdirSync(root).filter(n=>/^(LICEN[CS]E|COPYING|NOTICE|AUTHORS)(\.|$)/i.test(n));
  // zlib's license is its README header.
  if(tree.includes('zlib'))files.push('README');
  if(!files.length)throw Error('Missing redistribution notice: '+tree);
  const target=path.join(licenses,path.basename(tree));fs.mkdirSync(target,{recursive:true});
  for(const name of files)if(fs.statSync(path.join(root,name)).isFile())fs.copyFileSync(path.join(root,name),path.join(target,name));
}
// Alpine provides the statically linked zlib used by the Linux builder.
if(platform==='linux'){
  for(const name of ['zlib-LICENSE','musl-COPYRIGHT'])fs.copyFileSync(path.join(component,'linux',name),path.join(licenses,name));
  fs.copyFileSync(path.join(workspace,'builder-packages.txt'),path.join(directory,'builder-packages.txt'));
}
fs.writeFileSync(path.join(directory,'openssh.json'),JSON.stringify({schemaVersion:1,backend:'embedded-openssh',platform,arch,image:filename,build,
  sha256:crypto.createHash('sha256').update(bytes).digest('hex'),roles,sources},null,2)+'\n');
