// MSVC compiles C; LLD preserves Go's COFF SEH records rejected by link.exe.
import fs from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
const workspace=path.resolve(process.argv[2]??'');
if(!workspace.includes('mira-openssh-windows.')) throw Error('Use an isolated native build workspace');
const combined=path.join(workspace,'combined'),go=path.join(workspace,'go-objects');
// MSVC directives use names such as LIBCMT.lib while the SDK ships libcmt.lib.
// NTFS resolves these identically; a Linux CI linker needs explicit aliases.
const sdk=path.join(workspace,'sdk-libs');
for(const name of fs.readdirSync(sdk).filter(n=>/\.lib$/i.test(n))){
  for(const alias of [name.toLowerCase(),name.slice(0,-4).toUpperCase()+'.lib']){
    if(!fs.existsSync(path.join(sdk,alias)))fs.linkSync(path.join(sdk,name),path.join(sdk,alias));
  }
}
const lld=process.env.MIRA_LLD??'/usr/lib/llvm-18/bin/ld.lld';
const inputs=JSON.parse(fs.readFileSync(path.join(combined,'objects.json'),'utf8'));
const system='legacy_stdio_wide_specifiers bcrypt userenv crypt32 ws2_32 secur32 shlwapi kernel32 user32 gdi32 winspool comdlg32 advapi32 shell32 ole32 oleaut32 uuid odbc32 odbccp32 netapi32 rpcrt4 ntdll setupapi hid'.split(' ').map(n=>n+'.lib');
const deps=['crypto','zs','fido2_static','cbor'].map(n=>path.join(workspace,'deps/install/lib',n+'.lib'));
for(const file of deps) if(!fs.existsSync(file)) throw Error('Missing required static dependency: '+file);
const result=spawnSync(lld,['-flavor','link','/out:'+path.join(workspace,'mira-node.exe'),'/libpath:'+path.join(workspace,'sdk-libs'),
  path.join(workspace,'dispatcher.obj'),...inputs,...deps,path.join(go,'go-lazy.obj'),
  ...fs.readdirSync(go).filter(n=>/^000\d+\.o$/.test(n)).map(n=>path.join(go,n)),
  '/map:'+path.join(workspace,'mira.map'),'/incremental:no','/opt:ref','/opt:icf','/dynamicbase','/nxcompat','/highentropyva',...system],{stdio:'inherit'});
if(result.error)throw result.error;
process.exitCode=result.status??1;
