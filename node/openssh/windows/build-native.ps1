# Produces native C objects + static dependencies for the Linux linker stage.
param([Parameter(Mandatory=$true)][string]$Workspace,[string]$Cache,[ValidateSet('extract','build')][string]$Phase='extract')
$ErrorActionPreference='Stop'
if (-not $Workspace.Contains('mira-openssh-windows.')) { throw 'Use the Mira build workspace prefix' }
if ($Phase -eq 'extract') {
if (Test-Path $Workspace) { throw 'Choose a fresh Windows workspace' }
New-Item -ItemType Directory -Path $Workspace,(Join-Path $Workspace 'source'),(Join-Path $Workspace 'deps') | Out-Null
function Extract([string]$Archive,[string]$Destination,[switch]$Strip) {
    $arguments=@('-xf',('"'+$Archive+'"'),'-C',('"'+$Destination+'"'))
    if ($Strip) { $arguments+='--strip-components=1' }
    $p=Start-Process -FilePath 'tar.exe' -ArgumentList $arguments -Wait -PassThru -NoNewWindow
    if ($p.ExitCode -ne 0) { throw 'Source extraction failed' }
}
# Cache checksums must already be verified by fetch-sources.mjs.
Extract (Join-Path $Cache 'openssh-win32-dbc67119.tar.gz') (Join-Path $Workspace 'source') -Strip
foreach ($name in @('libressl-4.2.0','zlib-1.3.2','libcbor-0.14.0','libfido2-1.16.0')) {
    $directory=Join-Path $Workspace ('deps\'+$name)
    New-Item -ItemType Directory -Path $directory | Out-Null
    Extract (Join-Path $Cache ($name+'.tar.gz')) $directory -Strip
}
return
}
& "$PSScriptRoot\build-deps.ps1" -Workspace $Workspace
& "$PSScriptRoot\build-upstream.ps1" -Source (Join-Path $Workspace 'source')
& "$PSScriptRoot\compile-dispatcher.ps1" -Workspace $Workspace
