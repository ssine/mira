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
