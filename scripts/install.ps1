param(
    [string]$Server,
    [string]$Version = "latest",
    [switch]$Update,
    [switch]$NoService,
    [switch]$NoPath,
    [string]$ReleaseDirectory,
    [string]$InstallDirectory = (Join-Path $env:LOCALAPPDATA "Mira")
)

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$installRoot = $InstallDirectory
$binDirectory = Join-Path $installRoot "bin"
$taskName = "MiraNode-$env:USERNAME"
$identityPath = if ($env:MIRA_IDENTITY_FILE) { $env:MIRA_IDENTITY_FILE } else { Join-Path $env:LOCALAPPDATA "Mira\identity.json" }
$configPath = Join-Path (Split-Path $identityPath) "node.json"
$utf8 = New-Object Text.UTF8Encoding($false)
$stage = Join-Path ([IO.Path]::GetTempPath()) ("mira-install-" + [Guid]::NewGuid().ToString("N"))
$previousVersion = $null
$versionFile = Join-Path $installRoot "current-version"
$optionsFile = Join-Path $installRoot "install-options.json"
if ($Update -and (Test-Path $optionsFile)) {
    $installedOptions = Get-Content $optionsFile -Raw | ConvertFrom-Json
    $NoService = [bool]$installedOptions.noService
    $NoPath = [bool]$installedOptions.noPath
}
if (Test-Path $versionFile) { $previousVersion = [IO.File]::ReadAllText($versionFile).Trim() }
if ($Version -eq "latest") {
    $release = Invoke-RestMethod "https://api.github.com/repos/ssine/mira/releases/latest"
    $Version = $release.tag_name
}
$Version = $Version.TrimStart("v")
if ($Version -notmatch '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$') { throw "Invalid semantic version" }
if (-not $Server -and -not $Update) { $Server = Read-Host "Mira Server URL (for example https://mira.ssine.cc)" }
if ($Update -and -not $previousVersion) { throw "Mira is not installed. Run the installer with -Server first." }
if ([Environment]::Is64BitOperatingSystem -ne $true) { throw "Mira requires 64-bit Windows" }
$asset = "mira_${Version}_windows_amd64.zip"
$baseUrl = "https://github.com/ssine/mira/releases/download/v$Version"
$previousTask = $null
if (-not $NoService) {
    $previousTask = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    if ($previousTask -and $previousTask.Description -ne "Mira Node (per-user, managed by Mira installer)") {
        throw "Refusing to replace an independently managed scheduled task: $taskName"
    }
}

function Download-Asset([string]$Name) {
    $destination = Join-Path $stage $Name
    if ($ReleaseDirectory) { Copy-Item -LiteralPath (Join-Path $ReleaseDirectory $Name) -Destination $destination }
    else { Invoke-WebRequest "$baseUrl/$Name" -UseBasicParsing -OutFile $destination }
}

function Verify-Asset([string]$Name) {
    $pattern = '^([a-fA-F0-9]{64})\s+' + [regex]::Escape($Name) + '$'
    $line = Get-Content (Join-Path $stage "SHA256SUMS") | Where-Object { $_ -match $pattern } | Select-Object -First 1
    if (-not $line -or $line -notmatch $pattern) { throw "Checksum missing for $Name" }
    $expected = $Matches[1]
    if ((Get-FileHash (Join-Path $stage $Name) -Algorithm SHA256).Hash -ne $expected) { throw "Checksum verification failed for $Name" }
}

function Run-Mira([string]$Program, [string[]]$Arguments) {
    $outFile = Join-Path $stage "command.stdout"
    $errFile = Join-Path $stage "command.stderr"
    $process = Start-Process -FilePath $Program -ArgumentList $Arguments -Wait -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    $output = [IO.File]::ReadAllText($outFile)
    if ($output) { Write-Host $output.TrimEnd() }
    if ($process.ExitCode -ne 0) { throw [IO.File]::ReadAllText($errFile) }
}

function Set-VersionLaunchers([string]$SelectedVersion) {
    $directory = Join-Path $installRoot "versions\$SelectedVersion"
    foreach ($name in @("mira", "mira-node")) {
        $wrapper = "@echo off`r`n`"%~dp0..\versions\$SelectedVersion\$name.exe`" %*`r`n"
        [IO.File]::WriteAllText((Join-Path $binDirectory "$name.cmd"), $wrapper, [Text.Encoding]::ASCII)
    }
    [IO.File]::WriteAllText($versionFile, $SelectedVersion, $utf8)
}

try {
    New-Item -ItemType Directory -Path $stage | Out-Null
    Write-Host "Downloading Mira $Version for Windows amd64..."
    Download-Asset "SHA256SUMS"
    Download-Asset $asset
    Download-Asset "install.ps1"
    Verify-Asset $asset
    Verify-Asset "install.ps1"
    Expand-Archive (Join-Path $stage $asset) -DestinationPath $stage
    $packageDirectory = Join-Path $stage "mira_${Version}_windows_amd64"
    foreach ($name in @("mira.exe", "mira-node.exe")) {
        if (-not (Test-Path (Join-Path $packageDirectory $name))) { throw "Release archive is incomplete" }
    }
    New-Item -ItemType Directory -Path (Join-Path $installRoot "versions"), $binDirectory -Force | Out-Null
    if ($Server) { Run-Mira (Join-Path $packageDirectory "mira.exe") @("setup", "--server", $Server) }
    $versionDirectory = Join-Path $installRoot "versions\$Version"
    if (Test-Path $versionDirectory) {
        foreach ($name in @("mira.exe", "mira-node.exe")) {
            if ((Get-FileHash (Join-Path $versionDirectory $name)).Hash -ne (Get-FileHash (Join-Path $packageDirectory $name)).Hash) {
                throw "This version is installed with different contents; refusing to overwrite it."
            }
        }
    } else { Move-Item -LiteralPath $packageDirectory -Destination $versionDirectory }
    Copy-Item -LiteralPath (Join-Path $stage "install.ps1") -Destination (Join-Path $installRoot "install.ps1") -Force
    Set-VersionLaunchers $Version

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not $NoPath -and ($userPath -split ";") -notcontains $binDirectory) {
        [Environment]::SetEnvironmentVariable("Path", (([string]$userPath).TrimEnd(";") + ";" + $binDirectory), "User")
    }
    if (-not $NoService) {
        $previousTaskXml = if ($previousTask) { Export-ScheduledTask -TaskName $taskName } else { $null }
        if ($previousTask) { Stop-ScheduledTask -TaskName $taskName }
        try {
            $action = New-ScheduledTaskAction -Execute (Join-Path $versionDirectory "mira-node.exe") -Argument ('--config "' + $configPath + '"')
            $trigger = New-ScheduledTaskTrigger -AtLogOn -User ([Security.Principal.WindowsIdentity]::GetCurrent().Name)
            $principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
            $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
            Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Description "Mira Node (per-user, managed by Mira installer)" -Force | Out-Null
            Start-ScheduledTask -TaskName $taskName
            Start-Sleep -Seconds 2
            if ((Get-ScheduledTask -TaskName $taskName).State -ne "Running") { throw "Mira Node scheduled task did not stay running" }
        } catch {
            if ($previousVersion) { Set-VersionLaunchers $previousVersion }
            if ($previousTaskXml) {
                Register-ScheduledTask -TaskName $taskName -Xml $previousTaskXml -Force | Out-Null
                Start-ScheduledTask -TaskName $taskName
            }
            throw "Service update failed; previous version restored where available. $($_.Exception.Message)"
        }
        for ($attempt = 0; $attempt -lt 15 -and -not (Test-Path $identityPath); $attempt++) { Start-Sleep -Seconds 1 }
        Run-Mira (Join-Path $versionDirectory "mira.exe") @("status")
    } else { Write-Host "Start the Node with: $versionDirectory\mira-node.exe" }
    [IO.File]::WriteAllText($optionsFile, (@{noService=[bool]$NoService; noPath=[bool]$NoPath} | ConvertTo-Json -Compress), $utf8)
    Write-Host "Mira $Version installed. Open the Server website to approve a new Node."
    Write-Host "Open a new terminal, then use: mira status / mira update"
    Write-Host "Identity and configuration are preserved separately from versioned binaries. Previous versions remain available for rollback."
}
finally {
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}
