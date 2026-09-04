# Build-host discovery only; never configure the system OpenSSH installation.
param([string]$BuildTools=$env:MIRA_WINDOWS_BUILD_TOOLS, [string]$SdkVersion=$env:MIRA_WINDOWS_SDK_VERSION)
$ErrorActionPreference='Stop'
function Invoke-MiraBuildProgram([string]$Program,[string[]]$Arguments) {
    $process=New-Object Diagnostics.Process
    $process.StartInfo.FileName=$Program
    $process.StartInfo.Arguments=$Arguments -join ' '
    $process.StartInfo.UseShellExecute=$false
    try {
        if (-not $process.Start()) { throw "Could not start $Program" }
        $process.WaitForExit()
        if ($process.ExitCode -ne 0) { throw "Build failed ($($process.ExitCode)): $Program" }
    } finally { $process.Dispose() }
}
if (-not $BuildTools) {
    $vswhere=Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    $probe=[IO.Path]::GetTempFileName()
    try {
        $p=Start-Process -FilePath $vswhere -ArgumentList @('-latest','-products','*','-requires','Microsoft.VisualStudio.Component.VC.Tools.x86.x64','-property','installationPath') -NoNewWindow -Wait -PassThru -RedirectStandardOutput $probe
        if ($p.ExitCode -ne 0) { throw 'Visual Studio discovery failed' }
        $BuildTools=([IO.File]::ReadAllText($probe)).Trim()
    } finally { Remove-Item -LiteralPath $probe }
}
if (-not $BuildTools -or -not (Test-Path (Join-Path $BuildTools 'VC\Auxiliary\Build\vcvars64.bat'))) { throw 'Visual Studio C++ Build Tools are required' }
$sdk=Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10\Lib'
if (-not $SdkVersion) { $SdkVersion=(Get-ChildItem $sdk -Directory | Where-Object { Test-Path (Join-Path $_.FullName 'um\x64\kernel32.lib') } | Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1).Name }
$msvc=(Get-Content (Join-Path $BuildTools 'VC\Auxiliary\Build\Microsoft.VCToolsVersion.default.txt') -Raw).Trim()
$MiraToolchain=@{
    BuildTools=$BuildTools
    SdkVersion=$SdkVersion
    CMake=(Join-Path $BuildTools 'Common7\IDE\CommonExtensions\Microsoft\CMake\CMake\bin\cmake.exe')
    Compiler=(Join-Path $BuildTools "VC\Tools\MSVC\$msvc\bin\Hostx64\x64\cl.exe")
    Includes=@((Join-Path $BuildTools "VC\Tools\MSVC\$msvc\include"),(Join-Path (Split-Path $sdk) "Include\$SdkVersion\ucrt"),(Join-Path (Split-Path $sdk) "Include\$SdkVersion\shared"),(Join-Path (Split-Path $sdk) "Include\$SdkVersion\um"))
    Libraries=@((Join-Path $BuildTools "VC\Tools\MSVC\$msvc\lib\x64"),(Join-Path $sdk "$SdkVersion\ucrt\x64"),(Join-Path $sdk "$SdkVersion\um\x64"))
}
