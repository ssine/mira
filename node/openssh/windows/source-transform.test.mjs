import test from 'node:test';
import assert from 'node:assert/strict';
import {allowLocalSystemPasswordLookup,reuseLocalSystemProcessToken} from './source-transform.mjs';

test('Win32 OpenSSH password lookup admits only the LocalSystem exception',()=>{
  const source='before\r\n\tif (account_type != SidTypeUser) {\r\n\t\terrno = ENOENT;\r\n\t}\r\nafter\r\n';
  const transformed=allowLocalSystemPasswordLookup(source);
  assert.match(transformed,/account_type != SidTypeUser &&\r\n\t    !IsWellKnownSid\(\(PSID\) binary_sid, WinLocalSystemSid\)/);
  assert.match(transformed,/errno = ENOENT/);
});

test('Win32 OpenSSH password lookup transform fails closed on source drift',()=>{
  assert.throws(()=>allowLocalSystemPasswordLookup('no matching guard'),/found 0/);
  const guard='if (account_type != SidTypeUser) {';
  assert.throws(()=>allowLocalSystemPasswordLookup(`${guard}\n${guard}\n`),/found 2/);
});

test('Win32 OpenSSH reuses a process token only for a LocalSystem target',()=>{
  const marker='\tif (!am_system()) {\r\n\t\tprocess_sid = get_sid(NULL);\r\n\t\tuser_sid = get_sid(user);';
  const transformed=reuseLocalSystemProcessToken(`before\r\n${marker}\r\nafter\r\n`);
  assert.match(transformed,/if \(am_system\(\)\) \{\r\n\t\tuser_sid = get_sid\(user\);/);
  assert.match(transformed,/user_sid != NULL && IsWellKnownSid\(user_sid, WinLocalSystemSid\)/);
  assert.match(transformed,/OpenProcessToken\(GetCurrentProcess\(\), TOKEN_ALL_ACCESS_P, &token\)/);
  assert.ok(transformed.indexOf('IsWellKnownSid')<transformed.indexOf('OpenProcessToken'));
  assert.ok(transformed.indexOf('OpenProcessToken')<transformed.indexOf(marker));
});

test('Win32 OpenSSH LocalSystem token transform fails closed on source drift',()=>{
  assert.throws(()=>reuseLocalSystemProcessToken('no matching branch'),/found 0/);
  const marker='\tif (!am_system()) {\n\t\tprocess_sid = get_sid(NULL);\n\t\tuser_sid = get_sid(user);';
  assert.throws(()=>reuseLocalSystemProcessToken(`${marker}\n${marker}\n`),/found 2/);
  assert.throws(
    ()=>reuseLocalSystemProcessToken(`/* A LocalSystem service already owns the exact target identity. */\n${marker}`),
    /already applied/,
  );
});
