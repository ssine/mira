param(
    [string]$Server,
    [string]$Version = "latest",
    [switch]$Update,
    [switch]$NoService,
    [switch]$NoPath,
    [string]$ReleaseDirectory,
    [string]$InstallDirectory = (Join-Path $env:USERPROFILE ".mira")
)

$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$installRoot = $InstallDirectory
$binDirectory = Join-Path $installRoot "bin"
$taskName = "MiraNode-$env:USERNAME"
$identityPath = if ($env:MIRA_IDENTITY_FILE) { $env:MIRA_IDENTITY_FILE } else { Join-Path $env:USERPROFILE ".mira\identity.json" }
if (-not $env:MIRA_IDENTITY_FILE -and -not (Test-Path $identityPath) -and (Test-Path (Join-Path $env:LOCALAPPDATA "Mira\identity.json"))) {
    $identityPath = Join-Path $env:LOCALAPPDATA "Mira\identity.json"
}
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

function New-MiraTrayAction([string]$Program, [switch]$Show) {
    # Keep the task alive for exactly the Node lifetime, including its exit code
    # for Task Scheduler failure recovery. Never detach an untracked child.
    $code = @'
$ErrorActionPreference = 'Stop'
$start = New-Object Diagnostics.ProcessStartInfo
$start.FileName = '__PROGRAM__'
$start.Arguments = '__ARGUMENTS__'
$start.WorkingDirectory = '__CWD__'
$start.UseShellExecute = $false
$start.CreateNoWindow = $true
$process = [Diagnostics.Process]::Start($start)
try { $process.WaitForExit(); exit $process.ExitCode }
finally { $process.Dispose() }
'@
    $arguments = if ($Show) { '--tray --show --config "' + $configPath + '"' } else { '--tray --config "' + $configPath + '"' }
    $code = $code.Replace('__PROGRAM__', $Program.Replace("'", "''")).Replace('__ARGUMENTS__', $arguments.Replace("'", "''")).Replace('__CWD__', $env:USERPROFILE.Replace("'", "''"))
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($code))
    return New-ScheduledTaskAction -Execute (Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe') -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand $encoded" -WorkingDirectory $env:USERPROFILE
}

function Test-MiraTraySupport([string]$Program) {
    $outFile = Join-Path $stage 'tray-probe.stdout'
    $errFile = Join-Path $stage 'tray-probe.stderr'
    $process = Start-Process -FilePath $Program -ArgumentList '--mira-tray-build' -PassThru -WindowStyle Hidden -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    $handle = $process.Handle
    try {
        if (-not $process.WaitForExit(10000)) { $process.Kill(); throw 'Mira tray capability probe timed out' }
        return $process.ExitCode -eq 0 -and ([IO.File]::ReadAllText($outFile)).Trim() -eq 'MIRA_WINDOWS_TRAY_V1'
    } finally { $process.Dispose() }
}

function Set-TrayShortcut($Action) {
    $programs = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs'
    $shell = New-Object -ComObject WScript.Shell
    $shortcutPath = Join-Path $programs 'Mira Node.lnk'
    $shortcut = $shell.CreateShortcut($shortcutPath)
    if ((Test-Path $shortcutPath) -and $shortcut.Description -ne 'Mira Node status and tray') { throw 'Refusing to replace an independently managed Mira Node shortcut' }
    if ($null -eq $Action) {
        if (Test-Path $shortcutPath) { Remove-Item -LiteralPath $shortcutPath }
        return
    }
    $shortcut.TargetPath = $Action.Execute
    $shortcut.Arguments = $Action.Arguments
    $shortcut.WorkingDirectory = $env:USERPROFILE
    $shortcut.WindowStyle = 7
    $shortcut.Description = 'Mira Node status and tray'
    $shortcut.Save()
}

function Add-MiraPath {
    # SetEnvironmentVariable(..., User) broadcasts synchronously to every desktop
    # window. A hung window can stall installation for minutes. Persist first,
    # then perform a best-effort notification in a strictly bounded child process.
    if (($env:Path -split ";") -notcontains $binDirectory) { $env:Path += ";" + $binDirectory }
    $pathUpdate = @'
$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey("Environment")
try {
    $rawPath = [string]$key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (($rawPath -split ";") -notcontains $binDirectory) {
        $key.SetValue("Path", ($rawPath.TrimEnd(";") + ";" + $binDirectory), [Microsoft.Win32.RegistryValueKind]::ExpandString)
    }
} finally { $key.Close() }
'@
    $notification = @'
Add-Type -Namespace Mira -Name EnvironmentNotification -MemberDefinition '[System.Runtime.InteropServices.DllImport("user32.dll", CharSet=System.Runtime.InteropServices.CharSet.Unicode)] public static extern System.IntPtr SendMessageTimeout(System.IntPtr window, uint message, System.UIntPtr wParam, string lParam, uint flags, uint timeout, out System.UIntPtr result);'
[UIntPtr]$result = [UIntPtr]::Zero
[void][Mira.EnvironmentNotification]::SendMessageTimeout([IntPtr]0xffff, 0x1a, [UIntPtr]::Zero, "Environment", 2, 100, [ref]$result)
'@
    if (-not $NoService) {
        # A Scheduled Task executes outside a calling MSIX app's HKCU virtualization.
        # Use the same ordinary user identity; never elevate or modify machine PATH.
        $helperName = "MiraInstallPath-" + [Guid]::NewGuid().ToString("N")
        $marker = Join-Path $installRoot ($helperName + ".done")
        $code = '$ErrorActionPreference="Stop"; $binDirectory=' + "'" + $binDirectory.Replace("'", "''") + "';`n" + $pathUpdate + "`n[IO.File]::WriteAllText('" + $marker.Replace("'", "''") + "', 'updated')`n" + $notification
        $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($code))
        try {
            $action = New-ScheduledTaskAction -Execute (Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe") -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -EncodedCommand $encoded" -WorkingDirectory $env:USERPROFILE
            $principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
            Register-ScheduledTask -TaskName $helperName -Action $action -Principal $principal | Out-Null
            Start-ScheduledTask -TaskName $helperName
            for ($attempt = 0; $attempt -lt 100 -and -not (Test-Path $marker); $attempt++) { Start-Sleep -Milliseconds 100 }
            if (-not (Test-Path $marker)) { throw "User PATH helper did not complete" }
            Start-Sleep -Seconds 2
        } catch { Write-Warning "Could not persist user PATH; use $binDirectory\mira.cmd or add that directory from an ordinary PowerShell terminal." }
        finally {
            Stop-ScheduledTask -TaskName $helperName -ErrorAction SilentlyContinue
            Unregister-ScheduledTask -TaskName $helperName -Confirm:$false -ErrorAction SilentlyContinue
            if (Test-Path $marker) { Remove-Item -LiteralPath $marker }
        }
        return
    }
    & ([scriptblock]::Create($pathUpdate))
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($notification))
    $notifier = $null
    try {
        $notifier = Start-Process -FilePath (Join-Path $env:SystemRoot "System32\WindowsPowerShell\v1.0\powershell.exe") -WindowStyle Hidden -PassThru -ArgumentList @("-NoProfile", "-NonInteractive", "-EncodedCommand", $encoded)
        if (-not $notifier.WaitForExit(2000)) { $notifier.Kill() }
    } catch { Write-Warning "PATH saved. Other terminals may need their launcher restarted or a new Windows login." }
    finally { if ($notifier) { $notifier.Dispose() } }
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
    foreach ($name in @("openssh.json", "mira-node.exe")) {
        if (-not (Test-Path (Join-Path $packageDirectory $name))) { throw "Release archive is incomplete" }
    }
    $nativeManifest = Get-Content (Join-Path $packageDirectory 'openssh.json') -Raw | ConvertFrom-Json
    $nativeImage = Join-Path $packageDirectory 'mira-node.exe'
    if ($nativeManifest.schemaVersion -ne 1 -or $nativeManifest.backend -ne 'embedded-openssh' -or $nativeManifest.platform -ne 'windows' -or $nativeManifest.arch -ne 'amd64' -or $nativeManifest.image -ne 'mira-node.exe' -or (Get-FileHash $nativeImage -Algorithm SHA256).Hash -ne $nativeManifest.sha256) { throw 'Invalid embedded OpenSSH manifest' }
    Run-Mira $nativeImage @('--mira-openssh-build')
    if (([IO.File]::ReadAllText((Join-Path $stage 'command.stdout'))).Trim() -ne 'MIRA_LINKED_OPENSSH_WINDOWS_FULL_V1') { throw 'Release has no embedded OpenSSH' }
    $supportsTray = Test-MiraTraySupport $nativeImage
    # Fixed role names, never arbitrary manifest-supplied paths; no admin needed.
    foreach ($role in @('mira','ssh','sshd','sshd-session','sshd-auth','scp','sftp','sftp-server','ssh-keygen','ssh-shellhost','ssh-agent','ssh-add','ssh-keyscan','ssh-sk-helper','ssh-pkcs11-helper')) {
        New-Item -ItemType HardLink -Path (Join-Path $packageDirectory ($role+'.exe')) -Target $nativeImage | Out-Null
    }
    New-Item -ItemType Directory -Path (Join-Path $installRoot "versions"), $binDirectory -Force | Out-Null
    if ($Server) { Run-Mira (Join-Path $packageDirectory "mira.exe") @("setup", "--server", $Server) }
    $versionDirectory = Join-Path $installRoot "versions\$Version"
    if (Test-Path $versionDirectory) {
        foreach ($file in Get-ChildItem -LiteralPath $packageDirectory -Recurse -File) {
            $name = $file.FullName.Substring($packageDirectory.Length).TrimStart('\')
            $installed = Join-Path $versionDirectory $name
            if (-not (Test-Path -LiteralPath $installed) -or
                (Get-FileHash -LiteralPath $installed).Hash -ne (Get-FileHash -LiteralPath $file.FullName).Hash) {
                throw "This version is installed with different contents; refusing to overwrite it."
            }
        }
    } else { Move-Item -LiteralPath $packageDirectory -Destination $versionDirectory }
    Copy-Item -LiteralPath (Join-Path $stage "install.ps1") -Destination (Join-Path $installRoot "install.ps1") -Force
    Set-VersionLaunchers $Version

    if (-not $NoPath) { Add-MiraPath }
    if (-not $NoService) {
        $previousTaskXml = if ($previousTask) { Export-ScheduledTask -TaskName $taskName } else { $null }
        if ($previousTask) { Stop-ScheduledTask -TaskName $taskName }
        try {
            $action = if ($supportsTray) { New-MiraTrayAction (Join-Path $versionDirectory "mira-node.exe") }
                else { New-ScheduledTaskAction -Execute (Join-Path $versionDirectory 'mira-node.exe') -Argument ('--config "' + $configPath + '"') -WorkingDirectory $env:USERPROFILE }
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
        try {
            if ($supportsTray) { Set-TrayShortcut (New-MiraTrayAction (Join-Path $versionDirectory "mira-node.exe") -Show) }
            else { Set-TrayShortcut $null }
        } catch { Write-Warning "Could not update the Mira Node Start menu shortcut: $($_.Exception.Message)" }
        for ($attempt = 0; $attempt -lt 15 -and -not (Test-Path $identityPath); $attempt++) { Start-Sleep -Seconds 1 }
        Run-Mira (Join-Path $versionDirectory "mira.exe") @("status")
    } else { Write-Host "Start the Node with: $versionDirectory\mira-node.exe" }
    [IO.File]::WriteAllText($optionsFile, (@{noService=[bool]$NoService; noPath=[bool]$NoPath} | ConvertTo-Json -Compress), $utf8)
    Write-Host "Mira $Version installed. Open the Server website to approve a new Node."
    Write-Host "Open a new terminal, then use: mira status / mira update"
    if (-not $NoService -and $supportsTray) { Write-Host "Mira Node runs in the notification area. Use its tray icon or the Start menu to view status. Closing the status window keeps it running." }
    Write-Host "Identity and configuration are preserved separately from versioned binaries. Previous versions remain available for rollback."
}
finally {
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}
