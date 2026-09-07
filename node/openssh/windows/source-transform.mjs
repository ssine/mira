export function allowLocalSystemPasswordLookup(source) {
  const needle='if (account_type != SidTypeUser) {';
  const matches=source.split(needle).length-1;
  if(matches!==1)throw Error(`Expected exactly one Win32 OpenSSH account-type guard, found ${matches}`);
  const eol=source.includes('\r\n')?'\r\n':'\n';
  return source.replace(needle,`if (account_type != SidTypeUser &&${eol}\t    !IsWellKnownSid((PSID) binary_sid, WinLocalSystemSid)) {`);
}

export function reuseLocalSystemProcessToken(source) {
  const eol=source.includes('\r\n')?'\r\n':'\n';
  const marker='/* A LocalSystem service already owns the exact target identity. */';
  if(source.includes(marker))throw Error('Win32 OpenSSH LocalSystem token transform was already applied');
  const needle=`\tif (!am_system()) {${eol}\t\tprocess_sid = get_sid(NULL);${eol}\t\tuser_sid = get_sid(user);`;
  const matches=source.split(needle).length-1;
  if(matches!==1)throw Error(`Expected exactly one Win32 OpenSSH non-SYSTEM token branch, found ${matches}`);
  const localSystem=[
    `\t${marker}`,
    '\tif (am_system()) {',
    '\t\tuser_sid = get_sid(user);',
    '\t\tif (user_sid != NULL && IsWellKnownSid(user_sid, WinLocalSystemSid)) {',
    '\t\t\tif (!OpenProcessToken(GetCurrentProcess(), TOKEN_ALL_ACCESS_P, &token)) {',
    '\t\t\t\terror("%s - OpenProcessToken failed with %d", __func__, GetLastError());',
    '\t\t\t\ttoken = NULL;',
    '\t\t\t}',
    '\t\t\telse',
    '\t\t\t\tdebug3("%s - reusing LocalSystem process token", __func__);',
    '\t\t\tgoto done;',
    '\t\t}',
    '\t\tif (user_sid) {',
    '\t\t\tfree(user_sid);',
    '\t\t\tuser_sid = NULL;',
    '\t\t}',
    '\t}',
    '',
  ].join(eol);
  return source.replace(needle,localSystem+needle);
}

export function forceUTF8CommandShell(source) {
  const eol=source.includes('\r\n')?'\r\n':'\n';
  const marker='/* Mira normalizes opted-in cmd.exe command output without changing binary subsystems. */';
  if(source.includes(marker))throw Error('Win32 OpenSSH UTF-8 command transform was already applied');
  const declaration='\tchar *exec_command = NULL, *posix_cmd_input = NULL, *shell = NULL, *pty_cmd_cp = NULL;;';
  const setup='\tsend_shell_telemetry(pty, shell_type);';
  const spawn=`\t\tif (shell_type == SH_PS || shell_type == SH_BASH ||${eol}\t\t\tshell_type == SH_CYGWIN || (shell_type == SH_OTHER) && arg_escape) {`;
  const cleanup=`cleanup:${eol}\tif (exec_command)`;
  for(const [name,needle] of [['declaration',declaration],['setup',setup],['spawn',spawn],['cleanup',cleanup]]){
    const matches=source.split(needle).length-1;
    if(matches!==1)throw Error(`Expected exactly one Win32 OpenSSH UTF-8 ${name} anchor, found ${matches}`);
  }
  const block=[
    setup,
    '',
    `\t${marker}`,
    '\tfor (mira_env_index = 0; mira_env_index < s->num_env; mira_env_index++) {',
    '\t\tif (strcmp(s->env[mira_env_index].name, "MIRA_SSH_TEXT") == 0) {',
    '\t\t\tmira_text = strcmp(s->env[mira_env_index].val, "1") == 0;',
    '\t\t\tbreak;',
    '\t\t}',
    '\t}',
    '\tif (!pty && command && !s->is_subsystem &&',
    '\t    _stricmp(strrchr(s->pw->pw_shell, \'\\\\\') != NULL ?',
    '\t    strrchr(s->pw->pw_shell, \'\\\\\') + 1 : s->pw->pw_shell, "cmd.exe") == 0 &&',
    '\t    mira_text) {',
    '\t\tconst char *candidate = command;',
    '\t\tint is_scp;',
    '\t\twhile (*candidate == \' \' || *candidate == \'\\t\')',
    '\t\t\tcandidate++;',
    '\t\tis_scp = (_strnicmp(candidate, "scp", 3) == 0 &&',
    '\t\t    (candidate[3] == \' \' || candidate[3] == \'\\t\')) ||',
    '\t\t    (_strnicmp(candidate, "scp.exe", 7) == 0 &&',
    '\t\t    (candidate[7] == \' \' || candidate[7] == \'\\t\'));',
    '\t\tif (!is_scp) {',
    '\t\t\tsize_t length = strlen(__progdir) + strlen("\\\\mira.exe") + 1;',
    '\t\t\tcommand_buffer = sshbuf_from(command, strlen(command));',
    '\t\t\tif (command_buffer == NULL ||',
    '\t\t\t    (text_command = sshbuf_dtob64_string(command_buffer, 0)) == NULL ||',
    '\t\t\t    (mira_path = malloc(length)) == NULL) {',
    '\t\t\t\terrno = ENOMEM;',
    '\t\t\t\tgoto cleanup;',
    '\t\t\t}',
    '\t\t\tstrcpy_s(mira_path, length, __progdir);',
    '\t\t\tstrcat_s(mira_path, length, "\\\\mira.exe");',
    '\t\t}',
    '\t}',
  ].join(eol);
  const spawnBlock=[
    '\t\tif (text_command) {',
    '\t\t\tspawn_argv[0] = mira_path;',
    '\t\t\tspawn_argv[1] = "ssh-command-worker";',
    '\t\t\tspawn_argv[2] = text_command;',
    '\t\t}',
    `\t\telse if (shell_type == SH_PS || shell_type == SH_BASH ||`,
    '\t\t\tshell_type == SH_CYGWIN || (shell_type == SH_OTHER) && arg_escape) {',
  ].join(eol);
  source=source.replace(declaration,declaration+eol+'\tchar *mira_path = NULL, *text_command = NULL;'+eol+'\tstruct sshbuf *command_buffer = NULL;'+eol+'\tint mira_text = 0;'+eol+'\tu_int mira_env_index;');
  source=source.replace(setup,block);
  source=source.replace(spawn,spawnBlock);
  return source.replace(cleanup,`cleanup:${eol}\tsshbuf_free(command_buffer);${eol}\tif (text_command)${eol}\t\tfree(text_command);${eol}\tif (mira_path)${eol}\t\tfree(mira_path);${eol}\tif (exec_command)`);
}
