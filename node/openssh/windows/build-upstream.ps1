param([Parameter(Mandatory=$true)][string]$Source)
$ErrorActionPreference='Stop'
. "$PSScriptRoot\toolchain.ps1"
$env:MSBUILDDISABLENODEREUSE='1'
if (-not $Source.Contains('mira-openssh-windows.') -or -not (Test-Path (Join-Path $Source 'sshd.c'))) {throw 'Use an isolated source copy'}
$solution=Join-Path $Source 'contrib\win32\openssh\Win32-OpenSSH.sln'
# Build all fourteen supported roles, omitting unrelated upstream tests.
$filter=Join-Path (Split-Path $solution) 'mira-openssh.slnf'
$projects=@('ssh.vcxproj','sshd.vcxproj','sshd-session.vcxproj','sshd-auth.vcxproj','scp.vcxproj','sftp.vcxproj','sftp-server.vcxproj','keygen.vcxproj','ssh-shellhost.vcxproj')
$projects+=@('config.vcxproj','libssh.vcxproj','openbsd_compat.vcxproj','win32iocompat.vcxproj')
$projects+=@('ssh-agent.vcxproj','ssh-add.vcxproj','ssh-keyscan.vcxproj','ssh-sk-helper.vcxproj','ssh-pkcs11-helper.vcxproj')
[IO.File]::WriteAllText($filter,(@{solution=@{path='Win32-OpenSSH.sln';projects=$projects}}|ConvertTo-Json -Depth 4),[Text.UTF8Encoding]::new($false))
$msbuild=Join-Path $MiraToolchain.BuildTools 'MSBuild\Current\Bin\MSBuild.exe'
$arguments=@(('"'+$filter+'"'),'/m:6','/t:Build','/p:Configuration=Release','/p:Platform=x64','/p:VcpkgEnableManifest=false','/p:VcpkgEnabled=false','/p:PlatformToolset=v143',('/p:WindowsTargetPlatformVersion='+$MiraToolchain.SdkVersion),'/verbosity:minimal','/nologo')
Invoke-MiraBuildProgram $msbuild $arguments
