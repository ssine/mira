// Mechanical transformation of a disposable upstream source copy only.
// The only supported variant uses full, statically linked crypto and compression.
import fs from 'node:fs';
import path from 'node:path';
const root=path.resolve(process.argv[2]??'');
if(!root.includes('mira-openssh-windows.') || !fs.existsSync(path.join(root,'sshd.c')))
  throw Error('Expected an isolated mira-openssh-windows.* source tree');
const win=path.join(root,'contrib/win32/openssh');
const config=path.join(win,'config.h.vs');
let text=fs.readFileSync(config,'utf8');
if(!text.includes('#define HAVE_EVP_DIGESTSIGN 1'))text+='\n/* LibreSSL 4.2 provides the one-shot EVP API. */\n#define HAVE_EVP_DIGESTSIGN 1\n#define HAVE_EVP_DIGESTVERIFY 1\n';
fs.writeFileSync(config,text);
const paths=path.join(win,'paths.targets');
text=fs.readFileSync(paths,'utf8');
text=text.replaceAll('libcrypto.lib','crypto.lib');
fs.writeFileSync(paths,text);
// The normal OpenSSL include graph supplies stdlib.h accidentally. Without it,
// this file implicitly declares malloc/getenv as int and truncates x64 pointers.
const doexec=path.join(root,'contrib/win32/win32compat/w32-doexec.c');
let doexecCode=fs.readFileSync(doexec,'utf8');
if(!doexecCode.includes('#include <stdlib.h>'))fs.writeFileSync(doexec,doexecCode.replace('#include "includes.h"','#include "includes.h"\n#include <stdlib.h>'));
// Without /GL, MSVC cannot infer GNU noreturn attributes that this port erases.
// Preserve their meaning explicitly instead of suppressing uninitialized warnings.
for(const name of ['log.h','packet.h','auth.h','sftp.h']){
  const file=path.join(root,name);
  const header=fs.readFileSync(file,'utf8');
  fs.writeFileSync(file,header.replace(/^void(\s+(?:cleanup_exit|sshlogdie|sshfatal|ssh_packet_disconnect|sshpkt_fatal|auth_maxtries_exceeded|sftp_server_cleanup_exit)\s*\()/gm,
    '__declspec(noreturn) void$1'));
}
for(const name of fs.readdirSync(win).filter(n=>n.endsWith('.vcxproj'))){
  const file=path.join(win,name);
  let xml=fs.readFileSync(file,'utf8').replaceAll('<WholeProgramOptimization>true</WholeProgramOptimization>','<WholeProgramOptimization>false</WholeProgramOptimization>');
  {
    if(!xml.includes('deps\\install\\include;'))xml=xml.replaceAll('<AdditionalIncludeDirectories>','<AdditionalIncludeDirectories>$(SolutionDir)..\\..\\..\\..\\deps\\install\\include;')
      .replaceAll('<AdditionalLibraryDirectories>','<AdditionalLibraryDirectories>$(SolutionDir)..\\..\\..\\..\\deps\\install\\lib;');
    xml=xml.replaceAll('fido2.lib;','fido2_static.lib;');
  }
  fs.writeFileSync(file,xml);
}
console.log('Prepared full Windows static-crypto build');
