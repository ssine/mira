import fs from 'node:fs/promises';
import path from 'node:path';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
const cache=process.argv[2];assert(cache&&path.isAbsolute(cache),'Provide an absolute source-cache directory');
const platform=process.argv[3];assert(['linux','windows','android'].includes(platform),'Select linux, windows or android');
const manifest=JSON.parse(await fs.readFile(new URL('./sources.json',import.meta.url)));
await fs.mkdir(cache,{recursive:true});
for(const source of manifest){
  if(!source.platforms.includes(platform))continue;
  const target=path.join(cache,source.file);
  let data;
  try{data=await fs.readFile(target)}catch(e){
    if(e.code!=='ENOENT')throw e;
    const response=await fetch(source.url,{signal:AbortSignal.timeout(120000)});
    assert(response.ok,`Download ${source.file}: HTTP ${response.status}`);
    data=Buffer.from(await response.arrayBuffer());
  }
  assert.equal(crypto.createHash(source.algorithm).update(data).digest('hex'),source.digest,`Checksum mismatch: ${source.file}`);
  try{await fs.writeFile(target,data,{flag:'wx'})}catch(e){
    if(e.code!=='EEXIST')throw e;
    assert.equal(crypto.createHash(source.algorithm).update(await fs.readFile(target)).digest('hex'),source.digest,`Concurrent cache change: ${source.file}`);
  }
  console.log(`Verified ${source.file}`);
}
