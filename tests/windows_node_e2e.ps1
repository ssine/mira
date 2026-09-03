param(
    [Parameter(Mandatory = $true)][string]$BinaryDirectory,
    [string]$ServerUrl = "http://127.0.0.1:18787",
    [string]$ExpectedVersion = "0.9.1",
  [switch]$TestAppServer
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$utf8 = New-Object System.Text.UTF8Encoding($false)
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ("mira-windows-e2e-" + [Guid]::NewGuid().ToString("N"))
$workspacePath = Join-Path $testRoot "workspace"
$nodeKey = "windows-e2e-" + [Guid]::NewGuid().ToString("N")
$nodeProcess = $null
$nodeId = $null
$ptyId = $null
$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
$csrf = $null
$success = $false
$lastTerminalView = $null
$previousIdentity = $env:MIRA_IDENTITY_FILE
$previousPathExt = $env:PATHEXT
# WSL callers can supply a process-only PATHEXT different from a Windows login.
# Restore the registered Windows value in this test process only.
$env:PATHEXT = [Environment]::GetEnvironmentVariable("PATHEXT", "Machine")

function Assert-True($Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Invoke-Mira([string]$Path, [string]$Method = "GET", $Body = $null) {
    $arguments = @{ Uri = "$ServerUrl$Path"; Method = $Method; WebSession = $session; TimeoutSec = 60 }
    if ($csrf) { $arguments.Headers = @{ "x-mira-csrf" = $csrf } }
    if ($null -ne $Body) {
        $arguments.ContentType = "application/json; charset=utf-8"
        $arguments.Body = [Text.Encoding]::UTF8.GetBytes(($Body | ConvertTo-Json -Depth 30 -Compress))
    }
    return Invoke-RestMethod @arguments
}

function Invoke-Capability([string]$Capability, $Params) {
  $result = (Invoke-Mira "/v1/nodes/$nodeId/invoke" "POST" @{ capability = $Capability; params = $Params; timeoutMs = 30000 }).result
  if ($Capability -eq "pty" -and $Params.action -eq "poll") { $script:lastTerminalView = $result }
  return $result
}

function Wait-Until([scriptblock]$Operation, [string]$Description, [int]$Seconds = 30) {
    $deadline = [DateTime]::UtcNow.AddSeconds($Seconds)
    while ([DateTime]::UtcNow -lt $deadline) {
        $value = & $Operation
        if ($value) { return $value }
        Start-Sleep -Milliseconds 200
    }
    throw "Timed out waiting for $Description"
}

function Output-Text($View) {
    return (($View.output.chunks | ForEach-Object { $_.text }) -join "")
}

function Invoke-Native([string]$Program, [string[]]$Arguments) {
    $outputFile = Join-Path $testRoot ([Guid]::NewGuid().ToString("N") + ".stdout")
    $errorFile = Join-Path $testRoot ([Guid]::NewGuid().ToString("N") + ".stderr")
    $process = Start-Process -FilePath $Program -ArgumentList $Arguments -Wait -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $outputFile -RedirectStandardError $errorFile
    if ($process.ExitCode -ne 0) { throw "Native command failed: $([IO.File]::ReadAllText($errorFile))" }
    return [IO.File]::ReadAllText($outputFile)
}

try {
    New-Item -ItemType Directory -Path $testRoot | Out-Null
    Copy-Item (Join-Path $BinaryDirectory "mira-node.exe") $testRoot
    Copy-Item (Join-Path $BinaryDirectory "mira.exe") $testRoot
    $nodeBinary = Join-Path $testRoot "mira-node.exe"
    $cliBinary = Join-Path $testRoot "mira.exe"
    $identityFile = Join-Path $testRoot "identity.json"
    $configuration = @{
        serverUrl = $ServerUrl
        nodeKey = $nodeKey
        identityFile = $identityFile
        appServerAutoStart = $false
        appServerListenUrl = "ws://127.0.0.1:24510"
    appServerCodexHome = (Join-Path $testRoot "codex-home")
    }
    $configPath = Join-Path $testRoot "node.json"
    [IO.File]::WriteAllText($configPath, ($configuration | ConvertTo-Json), $utf8)
    $versionLine = Invoke-Native $nodeBinary @("--version")
    Assert-True ($versionLine -match [regex]::Escape($ExpectedVersion)) "mira-node does not report the expected version"
    $cliVersion = (Invoke-Native $cliBinary @("--json", "version") | ConvertFrom-Json)
    Assert-True ($cliVersion.data.build.version -eq $ExpectedVersion) "CLI version differs from Node version"

    $password = if ($env:MIRA_TEST_ADMIN_PASSWORD) { $env:MIRA_TEST_ADMIN_PASSWORD } else { "mira-local-admin-password" }
    $login = Invoke-Mira "/v1/admin/login" "POST" @{ username = "admin"; password = $password }
    $csrf = $login.csrfToken
    $nodeProcess = Start-Process -FilePath $nodeBinary -ArgumentList @("--config", "`"$configPath`"") `
        -WorkingDirectory $testRoot -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $testRoot "node.stdout.log") `
        -RedirectStandardError (Join-Path $testRoot "node.stderr.log")

    $pending = Wait-Until {
        (Invoke-Mira "/v1/admin/enrollments?status=pending").data | Where-Object { $_.nodeKey -eq $nodeKey }
    } "Windows enrollment"
    Invoke-Mira "/v1/admin/enrollments/$($pending.enrollmentId)/approve" "POST" @{} | Out-Null
    $node = Wait-Until {
        (Invoke-Mira "/v1/nodes").data | Where-Object { $_.nodeKey -eq $nodeKey -and $_.status -eq "online" }
    } "Windows reverse channel"
    $nodeId = $node.nodeId
    Assert-True ($node.platform -eq "windows" -and $node.nodeVersion -eq $ExpectedVersion) "Node did not register native Windows metadata"
    Assert-True ($node.nodeBuild.version -eq $ExpectedVersion -and $node.nodeBuild.protocolVersion -eq 1) "Node build metadata was not persisted"
  $identityACL = Get-Acl -LiteralPath $identityFile
  Assert-True $identityACL.AreAccessRulesProtected "Windows identity file still inherits broad ACL entries"

    $status = Invoke-Capability "status" @{}
    Assert-True ($status.cpuCount -gt 0 -and $status.memory.totalBytes -gt 0 -and $status.processCount -gt 0) "Windows resources are missing"
    Assert-True ($status.ptyBackend -eq "windows-conpty") "Windows still advertises a pipe fallback"
    Assert-True (@($status.disk | Where-Object { $_.totalBytes -gt 0 }).Count -gt 0) "Windows disk resources are missing"
    Assert-True ($null -ne $status.networks) "Windows network configuration is missing"

    $roots = Invoke-Capability "file" @{ action = "roots" }
    Assert-True (@($roots.roots | Where-Object { $_.configured -match "^[A-Z]:\\$" }).Count -gt 0) "Windows drive roots are missing"
    Invoke-Capability "file" @{ action = "mkdir"; path = $workspacePath } | Out-Null
    $filePath = Join-Path $workspacePath "windows-fixture.txt"
    $movedPath = Join-Path $workspacePath "windows-fixture-moved.txt"
    $content = "Mira Windows file round trip`n"
    Invoke-Capability "file" @{ action = "write"; path = $filePath; content = $content; overwrite = $false } | Out-Null
    $read = Invoke-Capability "file" @{ action = "read"; path = $filePath }
    Assert-True ($read.content -eq $content) "Windows file read/write round trip failed"
    Invoke-Capability "file" @{ action = "move"; path = $filePath; destination = $movedPath; overwrite = $false } | Out-Null
    $stat = Invoke-Capability "file" @{ action = "stat"; path = $movedPath }
    Assert-True ($stat.type -eq "file") "Windows file stat failed"
    $listing = Invoke-Capability "file" @{ action = "list"; path = $workspacePath }
    Assert-True ($null -ne $listing) "Windows directory listing failed"

    $env:MIRA_IDENTITY_FILE = $identityFile
    $cliNodes = (Invoke-Native $cliBinary @("--json", "nodes", "list") | ConvertFrom-Json)
    Assert-True (@($cliNodes.data | Where-Object { $_.nodeId -eq $nodeId }).Count -eq 1) "Windows CLI cannot share the Node identity"
    $cliFile = (Invoke-Native $cliBinary @("--json", "file", "read", "--node", $nodeId, "--path", $movedPath) | ConvertFrom-Json)
    Assert-True ($cliFile.data.content -eq $content) "Windows CLI file capability failed"

    $count = Invoke-Capability "process" @{ action = "count" }
    $systemProcesses = Invoke-Capability "process" @{ action = "list"; system = $true }
    Assert-True ($count.processCount -gt 0 -and $systemProcesses.format -eq "windows-processes") "Windows process enumeration failed"
    $started = Invoke-Capability "process" @{ action = "start"; command = "cmd.exe"; args = @("/d", "/c", "echo MIRA_PROCESS_OK"); cwd = $workspacePath }
    $finished = Wait-Until {
        $view = Invoke-Capability "process" @{ action = "poll"; processId = $started.processId; cursor = 0 }
        if (-not $view.running) { $view }
    } "Windows managed process"
    Assert-True ($finished.exitCode -eq 0 -and (Output-Text $finished).Contains("MIRA_PROCESS_OK")) "Windows process output failed"
    $sleeper = Invoke-Capability "process" @{ action = "start"; command = "powershell.exe"; args = @("-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 60"); cwd = $workspacePath }
    Invoke-Capability "process" @{ action = "signal"; processId = $sleeper.processId; signal = "SIGTERM" } | Out-Null
    Wait-Until {
        $view = Invoke-Capability "process" @{ action = "poll"; processId = $sleeper.processId }
        if (-not $view.running) { $view }
    } "Windows process termination" | Out-Null

    $opened = Invoke-Capability "pty" @{ action = "open"; command = "powershell.exe"; args = @("-NoLogo", "-NoProfile"); cwd = $workspacePath; rows = 24; cols = 100 }
    $ptyId = $opened.sessionId
    Assert-True ($opened.backend -eq "windows-conpty" -and $opened.resizeSupported) "Windows PTY is not ConPTY"
    Invoke-Capability "pty" @{ action = "write"; sessionId = $ptyId; input = "[Console]::WriteLine('MIRA_' + 'CONPTY_OK')`r" } | Out-Null
    Wait-Until {
        $view = Invoke-Capability "pty" @{ action = "poll"; sessionId = $ptyId; cursor = 0 }
        if ((Output-Text $view).Contains("MIRA_CONPTY_OK")) { $view }
    } "interactive PowerShell output" | Out-Null
    $resized = Invoke-Capability "pty" @{ action = "resize"; sessionId = $ptyId; rows = 37; cols = 132 }
    Assert-True ($resized.rows -eq 37 -and $resized.cols -eq 132) "ConPTY resize metadata is incorrect"
    Invoke-Capability "pty" @{ action = "write"; sessionId = $ptyId; input = "[Console]::WriteLine('SIZE_' + [Console]::WindowWidth + 'x' + [Console]::WindowHeight)`r" } | Out-Null
    Wait-Until {
        $view = Invoke-Capability "pty" @{ action = "poll"; sessionId = $ptyId; cursor = 0 }
        if ((Output-Text $view).Contains("SIZE_132x37")) { $view }
    } "native ConPTY window size" | Out-Null
    Invoke-Capability "pty" @{ action = "write"; sessionId = $ptyId; input = "Start-Sleep -Seconds 30`r" } | Out-Null
    Start-Sleep -Milliseconds 500
    Invoke-Capability "pty" @{ action = "write"; sessionId = $ptyId; input = ([string][char]3) } | Out-Null
  # Console control events are asynchronous; wait for PowerShell to leave the
  # interrupted pipeline before submitting the next command.
  Start-Sleep -Milliseconds 500
    Invoke-Capability "pty" @{ action = "write"; sessionId = $ptyId; input = "[Console]::WriteLine('MIRA_' + 'CTRL_C_OK')`r" } | Out-Null
    $terminalView = Wait-Until {
        $view = Invoke-Capability "pty" @{ action = "poll"; sessionId = $ptyId; cursor = 0 }
        if ((Output-Text $view).Contains("MIRA_CTRL_C_OK")) { $view }
    } "PowerShell Ctrl-C" 10
    Assert-True ((Output-Text $terminalView).Contains([string][char]27)) "ConPTY did not emit VT terminal output"
    Invoke-Capability "pty" @{ action = "close"; sessionId = $ptyId } | Out-Null
    Wait-Until {
        $view = Invoke-Capability "pty" @{ action = "poll"; sessionId = $ptyId }
        if (-not $view.running) { $view }
    } "ConPTY session close" | Out-Null
    $ptyId = $null
    Invoke-Capability "file" @{ action = "remove"; path = $movedPath } | Out-Null
    Invoke-Capability "file" @{ action = "remove"; path = $workspacePath } | Out-Null

  $appServerVerified = $false
  if ($TestAppServer) {
    Assert-True (@($node.codexInstallations | Where-Object { $_.appServerSupported }).Count -gt 0) "Windows Codex discovery found no validated App Server"
    Invoke-Mira "/v1/nodes/$nodeId/desired-app-server" "PUT" @{ running = $true } | Out-Null
    Wait-Until {
      $current = Invoke-Mira "/v1/nodes/$nodeId"
      if ($current.reportedAppServer.status -eq "running") { $current }
    } "Windows Codex App Server startup" | Out-Null
    $health = Invoke-RestMethod "http://127.0.0.1:24510/healthz"
    Assert-True ($null -ne $health) "Windows App Server health endpoint failed"
    Invoke-Mira "/v1/nodes/$nodeId/desired-app-server" "PUT" @{ running = $false } | Out-Null
    Wait-Until {
      $current = Invoke-Mira "/v1/nodes/$nodeId"
      if ($current.reportedAppServer.status -eq "stopped") { $current }
    } "Windows Codex App Server stop" | Out-Null
    $appServerVerified = $true
  }

    $success = $true
    @{
        ok = $true; platform = "windows"; nativePowerShell = $true; version = $ExpectedVersion
        enrollment = $true; reverseChannel = $true; sharedCLIIdentity = $true; fileCRUD = $true
        processListCount = $count.processCount; processStartPollTerminate = $true
        cpuMemoryDiskNetwork = $true; conpty = $true; conptyResize = $true; ctrlC = $true
        vtOutput = $true; protectedCredentialACL = $true
    codexDiscoveryAndAppServerLifecycle = $appServerVerified
    } | ConvertTo-Json -Compress
}
finally {
    if ($ptyId -and $nodeId) { try { Invoke-Capability "pty" @{ action = "close"; sessionId = $ptyId } | Out-Null } catch {} }
    if ($nodeId) { try { Invoke-Mira "/v1/admin/nodes/$nodeId/revoke" "POST" @{ reason = "Windows E2E completed" } | Out-Null } catch {} }
    if ($nodeProcess -and -not $nodeProcess.HasExited) { Stop-Process -Id $nodeProcess.Id -ErrorAction SilentlyContinue }
    $env:MIRA_IDENTITY_FILE = $previousIdentity
  $env:PATHEXT = $previousPathExt
    if ($success) {
        Remove-Item -LiteralPath $testRoot -Recurse -Force
    } else {
    if ($lastTerminalView) { [IO.File]::WriteAllText((Join-Path $testRoot "terminal.json"), ($lastTerminalView | ConvertTo-Json -Depth 20), $utf8) }
        Write-Warning "Windows E2E logs retained at $testRoot"
    }
}
