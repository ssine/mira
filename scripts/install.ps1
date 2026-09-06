param(
    [string]$Server,
    [string]$Version = "latest",
    [ValidateSet("node", "server")][string]$Role = "node",
    [ValidateSet("mira")][string]$ServiceOwner = "mira",
    [string]$StateDirectory = (Join-Path $env:USERPROFILE ".mira"),
    [string]$ReleaseDirectory,
    [switch]$NoPath,
    [switch]$Update
)

# This script only bootstraps the first verified image. The old Supervisor owns
# every later update transaction, including health checks and rollback.
$ErrorActionPreference = "Stop"
if ($Update) { throw "Installer-driven updates were removed. Run: mira update" }
if (-not [Environment]::Is64BitOperatingSystem) { throw "Mira requires 64-bit Windows" }
if (-not [IO.Path]::IsPathRooted($StateDirectory)) { throw "StateDirectory must be absolute" }

$stage = Join-Path ([IO.Path]::GetTempPath()) ("mira-install-" + [Guid]::NewGuid().ToString("N"))
try {
    New-Item -ItemType Directory -Path $stage | Out-Null
    if ($Version -eq "latest") {
        $release = Invoke-RestMethod "https://api.github.com/repos/ssine/mira/releases/latest"
        $Version = $release.tag_name
    }
    $Version = $Version.TrimStart("v")
    if ($Version -notmatch '^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$') { throw "Invalid semantic version" }
    $asset = "mira_${Version}_windows_amd64.zip"
    $baseUrl = "https://github.com/ssine/mira/releases/download/v$Version"

    function Download-Asset([string]$Name) {
        $destination = Join-Path $stage $Name
        if ($ReleaseDirectory) { Copy-Item -LiteralPath (Join-Path $ReleaseDirectory $Name) -Destination $destination }
        else { Invoke-WebRequest "$baseUrl/$Name" -UseBasicParsing -OutFile $destination }
    }
    Download-Asset "SHA256SUMS"
    Download-Asset $asset
    $pattern = '^([a-fA-F0-9]{64})\s+\*?' + [regex]::Escape($asset) + '$'
    $line = Get-Content (Join-Path $stage "SHA256SUMS") | Where-Object { $_ -match $pattern } | Select-Object -First 1
    if (-not $line) { throw "Release checksum is missing" }
    $expected = [regex]::Match($line, $pattern).Groups[1].Value
    $actual = (Get-FileHash (Join-Path $stage $asset) -Algorithm SHA256).Hash
    if ($actual -ne $expected) { throw "Release checksum verification failed" }

    Expand-Archive (Join-Path $stage $asset) -DestinationPath $stage
    $package = Join-Path $stage "mira_${Version}_windows_amd64"
    $image = Join-Path $package "mira-node.exe"
    if (-not (Test-Path $image)) { throw "Release has no Mira executable" }
    $versionOutput = & $image cli --version
    if ($LASTEXITCODE -ne 0 -or "$versionOutput" -notmatch [regex]::Escape($Version)) { throw "Release version validation failed" }

    $arguments = @("cli", "install", "--state-dir", $StateDirectory, "--role", $Role, "--service-owner", $ServiceOwner)
    if ($Server) { $arguments += @("--server-url", $Server) }
    & $image @arguments
    if ($LASTEXITCODE -ne 0) { throw "Mira service installation failed" }

    if (-not $NoPath) {
        $bin = Join-Path $StateDirectory "bin"
        New-Item -ItemType Directory -Path $bin -Force | Out-Null
        $escaped = $StateDirectory.Replace('%', '%%')
        $launcher = "@echo off`r`nset /p MIRA_SELECTED_VERSION=<`"$escaped\current`"`r`n`"$escaped\versions\%MIRA_SELECTED_VERSION%\mira.exe`" %*`r`n"
        [IO.File]::WriteAllText((Join-Path $bin "mira.cmd"), $launcher, (New-Object Text.UTF8Encoding($false)))
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if (($userPath -split ';') -notcontains $bin) {
            [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(';') + ';' + $bin), "User")
        }
    }
    Write-Host "Mira $Version installed as $Role. Later updates: mira update --state-dir `"$StateDirectory`""
}
finally {
    if (Test-Path $stage) { Remove-Item -LiteralPath $stage -Recurse -Force }
}
