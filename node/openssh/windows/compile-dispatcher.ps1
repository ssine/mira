param([Parameter(Mandatory=$true)][string]$Workspace)
$ErrorActionPreference='Stop'
. "$PSScriptRoot\toolchain.ps1"
if (-not $Workspace.Contains('mira-openssh-windows.')) { throw 'Use an isolated build workspace' }
Copy-Item "$PSScriptRoot\dispatcher.c" (Join-Path $Workspace 'dispatcher.c')
$arguments=@('/nologo','/O2','/MT','/W3','/utf-8','/c',('"'+(Join-Path $Workspace 'dispatcher.c')+'"'),('/Fo"'+(Join-Path $Workspace 'dispatcher.obj')+'"'))
$arguments+=@($MiraToolchain.Includes | ForEach-Object { '/I"'+$_+'"' })
Invoke-MiraBuildProgram $MiraToolchain.Compiler $arguments
# Link-time SDK/CRT libraries only; none are shipped with Mira.
$libs=Join-Path $Workspace 'sdk-libs'
New-Item -ItemType Directory -Path $libs -Force | Out-Null
foreach ($dir in $MiraToolchain.Libraries) {
    foreach ($file in Get-ChildItem -LiteralPath $dir -Filter '*.lib') {
        Copy-Item -LiteralPath $file.FullName -Destination (Join-Path $libs $file.Name.ToLowerInvariant()) -Force
    }
}
