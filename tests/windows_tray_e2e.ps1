param(
    [Parameter(Mandatory=$true)][string]$Binary,
    [string]$Screenshot,
    [string]$Installer,
    [string]$LegacyBinary
)
$ErrorActionPreference = 'Stop'
$utf8 = New-Object Text.UTF8Encoding($false)
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('mira-tray-e2e-' + [Guid]::NewGuid().ToString('N'))
$process = $null
$taskName = $null
$exitTitle = [string][char]0x9000 + [char]0x51fa + ' Mira Node' + [char]0xff1f
Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Runtime.InteropServices;
public static class MiraTrayTest {
    public delegate bool EnumProc(IntPtr w, IntPtr p);
    [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc callback, IntPtr p);
    [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr w, out uint pid);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassName(IntPtr w, StringBuilder text, int n);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr w, StringBuilder text, int n);
    public static string Describe(uint pid) {
        var result = new StringBuilder();
        EnumWindows((w,p) => { uint id; GetWindowThreadProcessId(w,out id); if (id==pid) { var c=new StringBuilder(512); var t=new StringBuilder(512); GetClassName(w,c,512); GetWindowText(w,t,512); result.AppendLine(c+": "+t); } return true; },IntPtr.Zero);
        return result.ToString();
    }
    public static IntPtr NodeWindow(uint pid) {
        IntPtr found=IntPtr.Zero;
        EnumWindows((w,p) => { uint id; GetWindowThreadProcessId(w,out id); if(id==pid) { var c=new StringBuilder(512); GetClassName(w,c,512); if(c.ToString().StartsWith("MiraNodeTray-")) { found=w; return false; } } return true; },IntPtr.Zero);
        return found;
    }
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern IntPtr FindWindow(string cls, string title);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr w);
    [DllImport("user32.dll")] public static extern bool PostMessage(IntPtr w, uint msg, IntPtr wp, IntPtr lp);
    [DllImport("user32.dll")] public static extern IntPtr SendMessage(IntPtr w, uint msg, IntPtr wp, IntPtr lp);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern uint RegisterWindowMessage(string text);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr w, out Rect rect);
    [DllImport("user32.dll")] public static extern bool PrintWindow(IntPtr w, IntPtr dc, uint flags);
    [DllImport("user32.dll")] public static extern IntPtr SetThreadDpiAwarenessContext(IntPtr value);
    public struct Rect { public int left, top, right, bottom; }
}
'@
function Assert-True($Value, [string]$Message) { if (-not $Value) { throw $Message } }
function Wait-Until([scriptblock]$Operation, [string]$Description) {
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    while ([DateTime]::UtcNow -lt $deadline) {
        $value = & $Operation
        if ($value) { return $value }
        if ($process -and $process.HasExited) { throw "Node exited ($($process.ExitCode)) while waiting for $Description" }
        Start-Sleep -Milliseconds 100
    }
    throw "Timed out waiting for $Description. Windows: $([MiraTrayTest]::Describe($process.Id))"
}
function Start-Tray {
    $start = New-Object Diagnostics.ProcessStartInfo
    $start.FileName = $nodeBinary
    $start.Arguments = '--tray --config "' + $configPath + '"'
    $start.WorkingDirectory = $testRoot
    $start.UseShellExecute = $false
    $start.CreateNoWindow = $true
    $start.EnvironmentVariables['MIRA_IDENTITY_FILE'] = $identityPath
    $start.EnvironmentVariables['CODEX_BINARY'] = (Join-Path $testRoot 'no-codex.exe')
    return [Diagnostics.Process]::Start($start)
}
try {
    New-Item -ItemType Directory -Path $testRoot | Out-Null
    $nodeBinary = Join-Path $testRoot 'mira-node.exe'
    Copy-Item -LiteralPath $Binary -Destination $nodeBinary
    $identityPath = Join-Path $testRoot 'identity.json'
    $configPath = Join-Path $testRoot "node's config.json"
    [IO.File]::WriteAllText($configPath, (@{serverUrl='http://127.0.0.1:9';identityFile=$identityPath;appServerAutoStart=$false;codexBinary=(Join-Path $testRoot 'no-codex.exe')} | ConvertTo-Json), $utf8)
    if ($Installer) {
        # Exercise the actual installer's launcher with an isolated task. Never
        # stop or replace the installed MiraNode task or the user's shortcut.
        $tokens=$null; $parseErrors=$null
        $ast=[Management.Automation.Language.Parser]::ParseFile($Installer,[ref]$tokens,[ref]$parseErrors)
        Assert-True (-not $parseErrors) 'Installer PowerShell syntax failed'
        $function=$ast.Find({param($item) $item -is [Management.Automation.Language.FunctionDefinitionAst] -and $item.Name -eq 'New-MiraTrayAction'},$true)
        Assert-True ($null -ne $function) 'Installer tray launcher is missing'
        Invoke-Expression $function.Extent.Text
        $probe=$ast.Find({param($item) $item -is [Management.Automation.Language.FunctionDefinitionAst] -and $item.Name -eq 'Test-MiraTraySupport'},$true)
        Assert-True ($null -ne $probe) 'Installer tray support probe is missing'
        Invoke-Expression $probe.Extent.Text
        $stage=$testRoot
        Assert-True (Test-MiraTraySupport $nodeBinary) 'New Node was not detected as tray capable'
        if ($LegacyBinary) { Assert-True (-not (Test-MiraTraySupport $LegacyBinary)) 'Legacy Node was incorrectly launched with --tray' }
        $taskName='MiraTrayTest-'+[Guid]::NewGuid().ToString('N')
        $action=New-MiraTrayAction $nodeBinary
        $principal=New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
        $settings=New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
        Register-ScheduledTask -TaskName $taskName -Action $action -Principal $principal -Settings $settings | Out-Null
        Start-ScheduledTask -TaskName $taskName
        $process=Wait-Until { Get-Process -Name mira-node -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $nodeBinary } | Select-Object -First 1 } 'scheduled Node process'
    } else { $process = Start-Tray }
    $window = Wait-Until { $w = [MiraTrayTest]::NodeWindow($process.Id); if ($w -ne [IntPtr]::Zero) { $w } } 'tray window'
    Assert-True (-not [MiraTrayTest]::IsWindowVisible($window)) 'Background startup showed the status window'
    $logPath = Join-Path $testRoot 'logs\node.log'
    Wait-Until { (Test-Path $logPath) -and (Get-Item $logPath).Length -gt 0 } 'file logging' | Out-Null
    [MiraTrayTest]::SendMessage($window, 0x111, [IntPtr]1001, [IntPtr]::Zero) | Out-Null
    Assert-True ([MiraTrayTest]::IsWindowVisible($window)) 'Status window did not open'
    if ($Screenshot) {
        Add-Type -AssemblyName System.Drawing
        [MiraTrayTest]::SetThreadDpiAwarenessContext([IntPtr](-4)) | Out-Null
        Start-Sleep -Milliseconds 300
        $rect = New-Object MiraTrayTest+Rect
        [MiraTrayTest]::GetWindowRect($window, [ref]$rect) | Out-Null
        $bitmap = New-Object Drawing.Bitmap(($rect.right-$rect.left), ($rect.bottom-$rect.top))
        $graphics = [Drawing.Graphics]::FromImage($bitmap)
        $dc = $graphics.GetHdc()
        try { Assert-True ([MiraTrayTest]::PrintWindow($window,$dc,2)) 'Could not render status screenshot' }
        finally { $graphics.ReleaseHdc($dc); $graphics.Dispose() }
        try { $bitmap.Save($Screenshot, [Drawing.Imaging.ImageFormat]::Png) }
        finally { $bitmap.Dispose() }
    }
    [MiraTrayTest]::SendMessage($window, 0x10, [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null
    Assert-True (-not [MiraTrayTest]::IsWindowVisible($window)) 'Close did not hide status'
    Assert-True (-not $process.HasExited) 'Closing status stopped Node'
    $duplicate = Start-Tray
    try {
        Assert-True ($duplicate.WaitForExit(10000)) 'Duplicate tray did not exit'
        Assert-True ($duplicate.ExitCode -eq 0) 'Duplicate tray failed'
    } finally { if (-not $duplicate.HasExited) { $duplicate.Kill() }; $duplicate.Dispose() }
    Wait-Until { [MiraTrayTest]::IsWindowVisible($window) } 'existing status window after duplicate start' | Out-Null
    [MiraTrayTest]::SendMessage($window, [MiraTrayTest]::RegisterWindowMessage('TaskbarCreated'), [IntPtr]::Zero, [IntPtr]::Zero) | Out-Null
    [MiraTrayTest]::PostMessage($window,0x111,[IntPtr]1004,[IntPtr]::Zero) | Out-Null
    $dialog = Wait-Until { $w=[MiraTrayTest]::FindWindow('#32770',$exitTitle); if ($w -ne [IntPtr]::Zero) { $w } } 'exit confirmation'
    [MiraTrayTest]::PostMessage($dialog,0x111,[IntPtr]7,[IntPtr]::Zero) | Out-Null
    Start-Sleep -Milliseconds 200
    Assert-True (-not $process.HasExited) 'Cancel exit stopped Node'
    [MiraTrayTest]::PostMessage($window,0x111,[IntPtr]1004,[IntPtr]::Zero) | Out-Null
    $dialog = Wait-Until { $w=[MiraTrayTest]::FindWindow('#32770',$exitTitle); if ($w -ne [IntPtr]::Zero) { $w } } 'second exit confirmation'
    [MiraTrayTest]::PostMessage($dialog,0x111,[IntPtr]6,[IntPtr]::Zero) | Out-Null
    Assert-True ($process.WaitForExit(15000)) 'Confirmed exit did not stop Node'
    if (-not $taskName) { Assert-True ($process.ExitCode -eq 0) 'User exit reported failure' }
    Assert-True ([IO.File]::ReadAllText($logPath).Contains('Mira Node stopped')) 'Shutdown was not flushed to disk'
    if ($taskName) {
        for ($attempt=0; $attempt -lt 100 -and (Get-ScheduledTask -TaskName $taskName).State -eq 'Running'; $attempt++) { Start-Sleep -Milliseconds 100 }
        Assert-True ((Get-ScheduledTask -TaskName $taskName).State -ne 'Running') 'Launcher outlived Node'
        Assert-True ((Get-ScheduledTaskInfo -TaskName $taskName).LastTaskResult -eq 0) 'Tray exit would trigger scheduled failure recovery'
        Write-Output 'WINDOWS_TRAY_SCHEDULED_TASK_OK'
    }
    Write-Output 'WINDOWS_TRAY_LIFECYCLE_OK'
} finally {
    if ($taskName) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false -ErrorAction SilentlyContinue
    }
    if ($process) { if (-not $process.HasExited) { $process.Kill(); $process.WaitForExit() }; $process.Dispose() }
    if (Test-Path $testRoot) { Remove-Item -LiteralPath $testRoot -Recurse -Force }
}
