import fs from 'node:fs';
import assert from 'node:assert/strict';
const image=fs.readFileSync(process.argv[2]);
assert(image.includes(Buffer.from('MIRA_LINKED_OPENSSH_ANDROID_ROOT_V1\0')),
  'APK OpenSSH mode requires the root-capable Mira single-image dispatcher');
assert(!image.includes(Buffer.from('/var/empty'))&&!image.includes(Buffer.from('MIRA_NODE_OPENSSH_PRIVSEP_DIR'))&&!image.includes(Buffer.from('preparing seccomp filter sandbox')),
  'APK OpenSSH must not require a host chroot, privilege-separation account or seccomp');
assert(!image.includes(Buffer.from('Mira Android SSH home is not an owner-private directory')),
  'APK OpenSSH must accept Android home permissions, including no_backup mode 0771');
console.log('Verified bundled Android OpenSSH/root image marker');
