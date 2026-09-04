// Namespace each program's COFF definitions, preserving archive lazy extraction
// and MSVC COMDAT associations. No embedded executable or runtime extraction.
import fs from 'node:fs';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
const source=path.resolve(process.argv[2]??''),out=path.resolve(process.argv[3]??'');
if(!source.includes('mira-openssh-windows.')||!out.includes('mira-openssh-windows.')||fs.existsSync(out))throw Error('Use a fresh isolated output directory');
fs.mkdirSync(out);
const win=path.join(source,'contrib/win32/openssh'),llvm=(process.env.MIRA_LLVM_BIN??'/usr/lib/llvm-18/bin')+'/';
const libraries=['posix_compat','libssh','openbsd_compat'].map(n=>path.join(win,'lib/x64/Release',n+'.lib'));
const inputs=[],forcedSymbols=[];
const roles=['ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen','ssh-shellhost'];
roles.push('ssh-agent','ssh-add','ssh-keyscan','ssh-sk-helper','ssh-pkcs11-helper');
for(const role of roles){
  const buildName=role==='ssh-keygen'?'ssh-keygen':role;
  const directory=path.join(win,'x64/Release',buildName);
  const objects=fs.readdirSync(directory).filter(n=>n.endsWith('.obj')).map(n=>path.join(directory,n));
  if(!objects.length)throw Error('No relocatable objects: '+role);
  const all=[...objects,...libraries];
  const nm=execFileSync(llvm+'llvm-nm',['--defined-only','--extern-only','--format=posix',...all],{encoding:'utf8',maxBuffer:16*1024*1024});
  const symbols=new Set();
  for(const line of nm.split('\n')){const parts=line.trim().split(/\s+/);if(parts.length>=3&&/^[A-Za-z?]$/.test(parts[1]))symbols.add(parts[0]);}
  // These are UCRT process-wide settings, initialized by its startup objects.
  // Namespacing their inline definitions splits them from the initializer and
  // breaks legacy wide %s formatting (including Windows security descriptors).
  for(const symbol of symbols)if(symbol.includes('__local_stdio_'))symbols.delete(symbol);
  const prefix='openssh_'+role.replaceAll('-','_')+'_';
  const map=path.join(out,role+'.symbols');
  fs.writeFileSync(map,[...symbols].sort().map(s=>`${s} ${prefix}${s}`).join('\n')+'\n');
  const roleOut=path.join(out,role);fs.mkdirSync(roleOut);
  for(const file of all){
    const result=path.join(roleOut,path.basename(file));
    execFileSync(llvm+'llvm-objcopy',['--redefine-syms='+map,file,result]);
    if(path.basename(file)==='posix_compat.lib'){
      // COFF linker directives contain literal names, not symbol relocations.
      const member=execFileSync(llvm+'llvm-ar',['t',result],{encoding:'utf8'}).split('\n').find(n=>n.endsWith('sshTelemetry.obj'));
      if(!member)throw Error('Windows TraceLogging member changed');
      const object=path.join(roleOut,member),directives=path.join(roleOut,'telemetry.directives');
      fs.writeFileSync(object,execFileSync(llvm+'llvm-ar',['p',result,member]));
      execFileSync(llvm+'llvm-objcopy',['--dump-section','.drectve='+directives,object]);
      const data=fs.readFileSync(directives,'utf8').replace(/(\/include:)([^\s"]+)/gi,(all,option,name)=>{
        if(!symbols.has(name))return all;
        forcedSymbols.push('/INCLUDE:'+prefix+name);
        return ' '.repeat(all.length); // COFF section remains the original size.
      });
      fs.writeFileSync(directives,data);
      execFileSync(llvm+'llvm-objcopy',['--update-section','.drectve='+directives,object]);
      execFileSync(llvm+'llvm-ar',['rcs',result,object]);
    }
    inputs.push(result);
  }
  console.log(`Namespaced ${role}: ${objects.length} objects, ${symbols.size} definitions`);
}
fs.writeFileSync(path.join(out,'objects.json'),JSON.stringify([...inputs,...forcedSymbols]));
fs.writeFileSync(path.join(out,'manifest.json'),JSON.stringify({source,roles:roles.length,inputs:inputs.length},null,2));
