param([Parameter(Mandatory=$true)][string]$Image,[Parameter(Mandatory=$true)][string]$Destination)
$ErrorActionPreference='Stop'
if(Test-Path -LiteralPath $Destination){throw 'Choose a fresh isolated destination'}
New-Item -ItemType Directory -Path $Destination | Out-Null
$target=Join-Path $Destination 'mira-node.exe'
Copy-Item -LiteralPath $Image -Destination $target
$roles=@('mira','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen','ssh-shellhost')
$roles+=@('ssh-agent','ssh-add','ssh-keyscan','ssh-sk-helper','ssh-pkcs11-helper')
foreach($role in $roles){
    New-Item -ItemType HardLink -Path (Join-Path $Destination ($role+'.exe')) -Target $target | Out-Null
}
Write-Output 'Staged one linked image with NTFS hard-link role names; no service was installed.'
