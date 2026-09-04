import fs from 'node:fs';
import assert from 'node:assert/strict';
const image=fs.readFileSync(process.argv[2]);
assert(image.includes(Buffer.from('MIRA_LINKED_OPENSSH_ANDROID_ROOT_V1\0')),
  'APK OpenSSH mode requires the root-capable Mira single-image dispatcher');
console.log('Verified bundled Android OpenSSH/root image marker');
