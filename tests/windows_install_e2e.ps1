param(
    [string]$Installer,
    [string]$ReleaseDirectory,
    [string]$CurrentVersion,
    [string]$PreviousVersion,
    [switch]$TestService,
    [switch]$TestPath
)
$ErrorActionPreference = "Stop"
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ("mira-install-e2e-" + [Guid]::NewGuid().ToString("N"))
$installDirectory = Join-Path $testRoot "Mira"
$previousIdentity = $env:MIRA_IDENTITY_FILE
$previousCodex = $env:CODEX_BINARY
$env:MIRA_IDENTITY_FILE = Join-Path $testRoot "state\identity.json"
$env:CODEX_BINARY = Join-Path $testRoot "no-codex.exe"
$nodeProcess = $null
$taskName = "MiraNode-$env:USERNAME"
$createdTask = $false
if ($TestService -and (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)) {
    throw "Service test refuses to replace an existing Mira task"
}
try {
    $createdTask = [bool]$TestService
    & $Installer -Version $PreviousVersion -Server "http://127.0.0.1:9" -ReleaseDirectory $ReleaseDirectory -InstallDirectory $installDirectory -NoService:(!$TestService) -NoPath:(!$TestPath)
    if ($TestPath -and ([Environment]::GetEnvironmentVariable("Path", "User") -split ";") -notcontains (Join-Path $installDirectory "bin")) { throw "Installer did not persist PATH" }
    if (-not $TestService) {
        $nodeProcess = Start-Process -FilePath (Join-Path $installDirectory "versions\$PreviousVersion\mira-node.exe") -WindowStyle Hidden -PassThru
    }
    for ($attempt = 0; $attempt -lt 100 -and -not (Test-Path $env:MIRA_IDENTITY_FILE); $attempt++) { Start-Sleep -Milliseconds 100 }
    $identityHash = (Get-FileHash $env:MIRA_IDENTITY_FILE).Hash
    $configFile = Join-Path $testRoot "state\node.json"
    $configHash = (Get-FileHash $configFile).Hash
    if ($nodeProcess) { Stop-Process -Id $nodeProcess.Id }
    $nodeProcess = $null
    & $Installer -Version $CurrentVersion -Update -ReleaseDirectory $ReleaseDirectory -InstallDirectory $installDirectory
    if ((Get-FileHash $env:MIRA_IDENTITY_FILE).Hash -ne $identityHash) { throw "Windows update changed the Node identity" }
    if ((Get-FileHash $configFile).Hash -ne $configHash) { throw "Windows update changed configuration" }
    if ((Get-Content (Join-Path $installDirectory "current-version") -Raw).Trim() -ne $CurrentVersion) { throw "Windows version pointer was not updated" }
    if (-not (Test-Path (Join-Path $installDirectory "versions\$PreviousVersion\mira-node.exe"))) { throw "Windows update discarded the previous version" }
    $codexPackage = Join-Path $installDirectory "versions\$CurrentVersion\mira-codex-package"
    foreach ($name in @("bin\codex.exe", "bin\codex-code-mode-host.exe", "codex-resources\codex-command-runner.exe", "codex-resources\codex-windows-sandbox-setup.exe", "codex-path\rg.exe", "codex-package.json")) {
        if (-not (Test-Path (Join-Path $codexPackage $name))) { throw "Windows canonical Codex package is missing $name" }
    }
    if ($TestService) {
        $task = Get-ScheduledTask -TaskName $taskName
        if ($task.State -ne "Running" -or $task.Actions.Execute -notlike "*\$CurrentVersion\mira-node.exe") { throw "Scheduled task did not update to the new binary" }
        Write-Output "WINDOWS_SERVICE_UPDATE_OK"
    }
    Write-Output "WINDOWS_INSTALL_UPDATE_OK"
} finally {
    if ($createdTask -and (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue)) {
        Stop-ScheduledTask -TaskName $taskName
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
        Start-Sleep -Milliseconds 500
    }
    if ($nodeProcess -and -not $nodeProcess.HasExited) { Stop-Process -Id $nodeProcess.Id -ErrorAction SilentlyContinue }
    if ($TestPath) {
        $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
        try {
            $rawPath = [string]$key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
            $cleanPath = (($rawPath -split ";") | Where-Object { $_ -ne (Join-Path $installDirectory "bin") }) -join ";"
            $key.SetValue("Path", $cleanPath, [Microsoft.Win32.RegistryValueKind]::ExpandString)
        } finally { $key.Close() }
    }
    $env:MIRA_IDENTITY_FILE = $previousIdentity
    $env:CODEX_BINARY = $previousCodex
    if (Test-Path $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
