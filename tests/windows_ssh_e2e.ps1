param(
    [Parameter(Mandatory = $true)][string]$BinaryDirectory,
    [Parameter(Mandatory = $true)][string]$ServerUrl,
    [Parameter(Mandatory = $true)][string]$LinuxNode
)
$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
$encoding = [Text.UTF8Encoding]::new($false)
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ("mira-ssh-native-" + [Guid]::NewGuid().ToString("N"))
$nodeKey = "windows-ssh-" + [Guid]::NewGuid().ToString("N")
$nodeProcess = $null
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
$csrf = $null
$oldIdentity = $env:MIRA_IDENTITY_FILE
$oldPathExt = $env:PATHEXT
$env:PATHEXT = [Environment]::GetEnvironmentVariable("PATHEXT", "Machine")
function Request([string]$Route, [string]$Method = "GET", $Body = $null) {
    $params = @{ Uri = "$ServerUrl$Route"; Method = $Method; WebSession = $session; TimeoutSec = 20 }
    if ($csrf) { $params.Headers = @{ "x-mira-csrf" = $csrf } }
    if ($null -ne $Body) { $params.ContentType = "application/json"; $params.Body = ($Body | ConvertTo-Json -Depth 10 -Compress) }
    Invoke-RestMethod @params
}
function Wait-For([scriptblock]$Operation) {
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ([DateTime]::UtcNow -lt $deadline) { $result = & $Operation; if ($result) { return $result }; Start-Sleep -Milliseconds 200 }
    throw "Windows SSH acceptance timed out"
}
function Run-CLI([string[]]$Arguments, [int]$ExpectedExit = 0) {
    $id = [Guid]::NewGuid().ToString("N")
    $stdout = Join-Path $testRoot "$id.out"; $stderr = Join-Path $testRoot "$id.err"
    $run = Start-Process -FilePath (Join-Path $BinaryDirectory "mira.exe") -ArgumentList $Arguments -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
	$processHandle = $run.Handle
    if (-not $run.WaitForExit(20000)) { $run.Kill(); throw "Windows SSH CLI timed out" }
    # PowerShell Start-Process can cache a null ExitCode without the process handle.
    $run.WaitForExit()
    if ($run.ExitCode -ne $ExpectedExit) { throw "CLI exit $($run.ExitCode), expected ${ExpectedExit}: $([IO.File]::ReadAllText($stderr))" }
    return [IO.File]::ReadAllText($stdout)
}
try {
    New-Item -ItemType Directory $testRoot | Out-Null
    $identity = Join-Path $testRoot "identity.json"
    $configPath = Join-Path $testRoot "node.json"
    $config = @{ serverUrl = $ServerUrl; nodeKey = $nodeKey; identityFile = $identity; appServerAutoStart = $false; allowedRoots = @($testRoot) }
    [IO.File]::WriteAllText($configPath, ($config | ConvertTo-Json), $encoding)
    $login = Request "/v1/admin/login" "POST" @{ username = "admin"; password = "mira-local-admin-password" }; $csrf = $login.csrfToken
    $nodeProcess = Start-Process -FilePath (Join-Path $BinaryDirectory "mira-node.exe") -ArgumentList @("--config", "`"$configPath`"") -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $testRoot "node.out") -RedirectStandardError (Join-Path $testRoot "node.err")
    $pending = Wait-For { (Request "/v1/admin/enrollments?status=pending").data | Where-Object { $_.nodeKey -eq $nodeKey } }
    Request "/v1/admin/enrollments/$($pending.enrollmentId)/approve" "POST" @{} | Out-Null
    $node = Wait-For { (Request "/v1/nodes").data | Where-Object { $_.nodeKey -eq $nodeKey -and $_.channelStatus.connected } }
    $env:MIRA_IDENTITY_FILE = $identity
    $output = Run-CLI @("ssh", $nodeKey, "--", '"echo WINDOWS_SSH_OK"')
    if ($output -notmatch "WINDOWS_SSH_OK") { throw "Windows SSH exec marker missing" }
    $output = Run-CLI @("ssh", "-t", $nodeKey, "--", '"echo WINDOWS_CONPTY_OK"')
    if ($output -notmatch "WINDOWS_CONPTY_OK") { throw "Windows SSH ConPTY marker missing" }
    $output = Run-CLI @("ssh", $LinuxNode, "--", '"printf WINDOWS_TO_LINUX_OK"')
    if ($output -notmatch "WINDOWS_TO_LINUX_OK") { throw "Windows-to-Linux SSH failed" }
    Run-CLI @("ssh", $nodeKey, "--", '"exit /b 9"') 9 | Out-Null
    $payload = [byte[]]::new(5 * 1024 * 1024 + 37); [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($payload)
    $source = Join-Path $testRoot "source.bin"; $restored = Join-Path $testRoot "restored.bin"
    [IO.File]::WriteAllBytes($source, $payload)
    $remote = "/" + (($testRoot + "\remote.bin") -replace '\\', '/')
    Run-CLI @("scp", "`"$source`"", "${nodeKey}:$remote") | Out-Null
    Run-CLI @("scp", "${nodeKey}:$remote", "`"$restored`"") | Out-Null
    if ((Get-FileHash $source).Hash -ne (Get-FileHash $restored).Hash) { throw "Windows SFTP binary roundtrip failed" }
    $output = Run-CLI @("sftp", $nodeKey, "ls", ("/" + ($testRoot -replace '\\', '/')))
    if ($output -notmatch "remote.bin") { throw "Windows SFTP listing failed" }
    Write-Output "Windows SSH E2E passed: native CLI, reverse relay, isolated worker, ConPTY, exit code, 5 MiB SFTP, Windows-to-Linux exec."
} finally {
    if ($nodeProcess -and -not $nodeProcess.HasExited) { Stop-Process -Id $nodeProcess.Id -Force; $nodeProcess.WaitForExit(5000) | Out-Null }
    $env:MIRA_IDENTITY_FILE = $oldIdentity; $env:PATHEXT = $oldPathExt
    if ($csrf) { try { Request "/v1/admin/logout" "POST" @{} | Out-Null } catch {} }
    # This directory was generated by this test and contains no user identity/data.
    if (Test-Path $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
