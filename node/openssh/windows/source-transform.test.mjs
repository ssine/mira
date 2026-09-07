import test from 'node:test';
import assert from 'node:assert/strict';
import {allowLocalSystemPasswordLookup,forceUTF8CommandShell,reuseLocalSystemProcessToken} from './source-transform.mjs';

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

test('Win32 OpenSSH direct cmd sessions switch to UTF-8 without touching subsystems',()=>{
  const source=[
    'int do_exec_windows(void) {',
    '\tchar *exec_command = NULL, *posix_cmd_input = NULL, *shell = NULL, *pty_cmd_cp = NULL;;',
    '\tsend_shell_telemetry(pty, shell_type);',
    '\t\tif (shell_type == SH_PS || shell_type == SH_BASH ||',
    '\t\t\tshell_type == SH_CYGWIN || (shell_type == SH_OTHER) && arg_escape) {',
    'cleanup:',
    '\tif (exec_command)',
    '\t\tfree(exec_command);',
    '}',
  ].join('\r\n');
  const transformed=forceUTF8CommandShell(source);
  assert.match(transformed,/strcmp\(s->env\[mira_env_index\]\.name, "MIRA_SSH_TEXT"\) == 0/);
  assert.match(transformed,/mira_text = strcmp\(s->env\[mira_env_index\]\.val, "1"\) == 0/);
  assert.match(transformed,/_stricmp\(strrchr\(s->pw->pw_shell/);
  assert.match(transformed,/text_command = sshbuf_dtob64_string\(command_buffer, 0\)/);
  assert.match(transformed,/spawn_argv\[1\] = "ssh-command-worker";/);
  assert.match(transformed,/_strnicmp\(candidate, "scp\.exe", 7\)/);
  assert.match(transformed,/cleanup:\r\n\tsshbuf_free\(command_buffer\);/);
  assert.ok(transformed.indexOf('free(text_command)')<transformed.indexOf('free(exec_command)'));
});

test('Win32 OpenSSH UTF-8 command transform fails closed on source drift',()=>{
  assert.throws(()=>forceUTF8CommandShell('no matching source'),/declaration anchor, found 0/);
  const source='\tchar *exec_command = NULL, *posix_cmd_input = NULL, *shell = NULL, *pty_cmd_cp = NULL;;\n' +
    '\tsend_shell_telemetry(pty, shell_type);\n' +
    '\t\tif (shell_type == SH_PS || shell_type == SH_BASH ||\n' +
    '\t\t\tshell_type == SH_CYGWIN || (shell_type == SH_OTHER) && arg_escape) {\n' +
    'cleanup:\n\tif (exec_command)';
  assert.throws(()=>forceUTF8CommandShell(source+'\n'+source),/declaration anchor, found 2/);
  assert.throws(()=>forceUTF8CommandShell(source+'\n/* Mira normalizes opted-in cmd.exe command output without changing binary subsystems. */'),/already applied/);
});
