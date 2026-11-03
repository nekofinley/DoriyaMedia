[CmdletBinding()]
param(
    [int]$HostProcessId = 0,
    [string]$OutputPath,
    [switch]$RunResizeMatrix,
    [switch]$RunResponsiveness,
    [ValidateRange(1, 10000)]
    [int]$ProbeCount = 120,
    [ValidateRange(1, 5000)]
    [int]$ProbeTimeoutMs = 500
)

$ErrorActionPreference = 'Stop'

Add-Type @'
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class DoriyaW0WindowProbeNative
{
    public delegate bool EnumProc(IntPtr hwnd, IntPtr lParam);

    [StructLayout(LayoutKind.Sequential)]
    public struct RECT
    {
        public int Left;
        public int Top;
        public int Right;
        public int Bottom;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct POINT
    {
        public int X;
        public int Y;
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct MONITORINFO
    {
        public int cbSize;
        public RECT rcMonitor;
        public RECT rcWork;
        public uint dwFlags;
    }

    [DllImport("user32.dll")]
    public static extern bool EnumChildWindows(IntPtr parent, EnumProc callback, IntPtr lParam);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetClassName(IntPtr hwnd, StringBuilder value, int capacity);

    [DllImport("user32.dll", CharSet = CharSet.Unicode)]
    public static extern int GetWindowText(IntPtr hwnd, StringBuilder value, int capacity);

    [DllImport("user32.dll")]
    public static extern uint GetWindowThreadProcessId(IntPtr hwnd, out uint processId);

    [DllImport("user32.dll")]
    public static extern bool GetWindowRect(IntPtr hwnd, out RECT rect);

    [DllImport("user32.dll")]
    public static extern bool GetClientRect(IntPtr hwnd, out RECT rect);

    [DllImport("user32.dll")]
    public static extern bool ClientToScreen(IntPtr hwnd, ref POINT point);

    [DllImport("user32.dll")]
    public static extern IntPtr GetParent(IntPtr hwnd);

    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr hwnd);

    [DllImport("user32.dll", EntryPoint = "GetWindowLongPtrW")]
    public static extern IntPtr GetWindowLongPtr(IntPtr hwnd, int index);

    [DllImport("user32.dll")]
    public static extern bool SetWindowPos(
        IntPtr hwnd,
        IntPtr insertAfter,
        int x,
        int y,
        int width,
        int height,
        uint flags);

    [DllImport("user32.dll")]
    public static extern bool SetProcessDpiAwarenessContext(IntPtr value);

    [DllImport("user32.dll")]
    public static extern uint GetDpiForWindow(IntPtr hwnd);

    [DllImport("user32.dll")]
    public static extern IntPtr MonitorFromWindow(IntPtr hwnd, uint flags);

    [DllImport("user32.dll")]
    public static extern bool GetMonitorInfo(IntPtr monitor, ref MONITORINFO info);

    [DllImport("user32.dll", SetLastError = true)]
    public static extern IntPtr SendMessageTimeout(
        IntPtr hwnd,
        uint message,
        UIntPtr wParam,
        IntPtr lParam,
        uint flags,
        uint timeoutMs,
        out UIntPtr result);
}
'@

$WM_NULL = 0
$SMTO_ABORTIFHUNG = 0x0002
$SWP_NOZORDER = 0x0004
$SWP_NOACTIVATE = 0x0010
$GW_OWNER_STYLE = -16
$WS_CHILD = 0x40000000
$DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 = [IntPtr](-4)
$MONITOR_DEFAULTTONEAREST = 2

$dpiAwarenessSet = [DoriyaW0WindowProbeNative]::SetProcessDpiAwarenessContext(
    $DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2)

function Get-WindowSnapshot {
    param([IntPtr]$Handle)

    if ($Handle -eq [IntPtr]::Zero) {
        return $null
    }

    $outer = New-Object DoriyaW0WindowProbeNative+RECT
    $client = New-Object DoriyaW0WindowProbeNative+RECT
    $clientOrigin = New-Object DoriyaW0WindowProbeNative+POINT
    if (-not [DoriyaW0WindowProbeNative]::GetWindowRect($Handle, [ref]$outer)) {
        return $null
    }
    if (-not [DoriyaW0WindowProbeNative]::GetClientRect($Handle, [ref]$client)) {
        return $null
    }
    [void][DoriyaW0WindowProbeNative]::ClientToScreen($Handle, [ref]$clientOrigin)

    $className = New-Object System.Text.StringBuilder 256
    $title = New-Object System.Text.StringBuilder 256
    [void][DoriyaW0WindowProbeNative]::GetClassName($Handle, $className, $className.Capacity)
    [void][DoriyaW0WindowProbeNative]::GetWindowText($Handle, $title, $title.Capacity)

    [uint32]$ownerProcessId = 0
    [void][DoriyaW0WindowProbeNative]::GetWindowThreadProcessId(
        $Handle,
        [ref]$ownerProcessId)

    $style = [DoriyaW0WindowProbeNative]::GetWindowLongPtr(
        $Handle,
        $GW_OWNER_STYLE).ToInt64()

    [PSCustomObject]@{
        Handle = $Handle.ToInt64()
        Hwnd = '0x{0:X}' -f $Handle.ToInt64()
        ProcessId = $ownerProcessId
        ClassName = $className.ToString()
        Title = $title.ToString()
        Parent = [DoriyaW0WindowProbeNative]::GetParent($Handle).ToInt64()
        Visible = [DoriyaW0WindowProbeNative]::IsWindowVisible($Handle)
        IsChild = (($style -band $WS_CHILD) -ne 0)
        Style = '0x{0:X}' -f $style
        OuterWidth = $outer.Right - $outer.Left
        OuterHeight = $outer.Bottom - $outer.Top
        OuterX = $outer.Left
        OuterY = $outer.Top
        ClientWidth = $client.Right - $client.Left
        ClientHeight = $client.Bottom - $client.Top
        ClientX = $clientOrigin.X
        ClientY = $clientOrigin.Y
    }
}

function Get-ChildWindowHandles {
    param([IntPtr]$Parent)

    $handles = New-Object System.Collections.Generic.List[IntPtr]
    $callback = [DoriyaW0WindowProbeNative+EnumProc]{
        param([IntPtr]$hwnd, [IntPtr]$lParam)
        $handles.Add($hwnd)
        return $true
    }
    [void][DoriyaW0WindowProbeNative]::EnumChildWindows(
        $Parent,
        $callback,
        [IntPtr]::Zero)
    return $handles
}

function Get-EmbeddedTree {
    param(
        [IntPtr]$HostHandle,
        [int]$ExpectedMpvProcessId
    )

    $hostWindow = Get-WindowSnapshot -Handle $HostHandle
    if ($null -eq $hostWindow) {
        throw 'The Tauri main window is no longer available.'
    }

    $children = foreach ($handle in (Get-ChildWindowHandles -Parent $HostHandle)) {
        Get-WindowSnapshot -Handle $handle
    }

    $surface = $children |
        Where-Object { $_.ClassName -eq 'DoriyaW0MpvSurface' } |
        Select-Object -First 1
    if ($null -eq $surface) {
        throw 'DoriyaW0MpvSurface was not found under the Tauri main window.'
    }

    $mpv = $children |
        Where-Object {
            $_.ClassName -eq 'mpv' -and
            $_.Parent -eq $surface.Handle -and
            ($ExpectedMpvProcessId -eq 0 -or $_.ProcessId -eq $ExpectedMpvProcessId)
        } |
        Select-Object -First 1
    if ($null -eq $mpv) {
        throw 'The expected mpv child window was not found under DoriyaW0MpvSurface.'
    }

    [PSCustomObject]@{
        Host = $hostWindow
        Surface = $surface
        Mpv = $mpv
        AllWindows = @($hostWindow) + @($children)
    }
}

function Assert-EmbeddedTree {
    param(
        [object]$Tree,
        [string]$Label
    )

    $errors = New-Object System.Collections.Generic.List[string]
    if ($Tree.Host.Parent -ne 0) {
        $errors.Add("$Label host has a parent window")
    }
    if (-not $Tree.Host.Visible) {
        $errors.Add("$Label host is not visible")
    }
    if ($Tree.Surface.Parent -ne $Tree.Host.Handle) {
        $errors.Add("$Label surface parent does not match the Tauri host")
    }
    if (-not $Tree.Surface.Visible -or -not $Tree.Surface.IsChild) {
        $errors.Add("$Label surface is not a visible WS_CHILD")
    }
    if ($Tree.Mpv.Parent -ne $Tree.Surface.Handle) {
        $errors.Add("$Label mpv parent does not match the video surface")
    }
    if (-not $Tree.Mpv.Visible -or -not $Tree.Mpv.IsChild) {
        $errors.Add("$Label mpv window is not a visible WS_CHILD")
    }
    if ($Tree.Surface.OuterWidth -ne $Tree.Mpv.OuterWidth -or
        $Tree.Surface.OuterHeight -ne $Tree.Mpv.OuterHeight) {
        $errors.Add("$Label surface and mpv outer rectangles differ")
    }
    if ($Tree.Surface.ClientWidth -ne $Tree.Mpv.ClientWidth -or
        $Tree.Surface.ClientHeight -ne $Tree.Mpv.ClientHeight) {
        $errors.Add("$Label surface and mpv client rectangles differ")
    }
    if ($Tree.Surface.OuterWidth -le 0 -or $Tree.Surface.OuterHeight -le 0) {
        $errors.Add("$Label surface has an empty rectangle")
    }

    if ($errors.Count -ne 0) {
        throw ($errors -join '; ')
    }
}

function Invoke-ResizeStep {
    param(
        [IntPtr]$HostHandle,
        [int]$MpvProcessId,
        [string]$Label,
        [int]$Width,
        [int]$Height,
        [int]$X,
        [int]$Y
    )

    $flags = $SWP_NOZORDER -bor $SWP_NOACTIVATE
    if (-not [DoriyaW0WindowProbeNative]::SetWindowPos(
        $HostHandle,
        [IntPtr]::Zero,
        $X,
        $Y,
        $Width,
        $Height,
        $flags)) {
        throw "SetWindowPos failed for $Label."
    }
    Start-Sleep -Milliseconds 700
    $tree = Get-EmbeddedTree -HostHandle $HostHandle -ExpectedMpvProcessId $MpvProcessId
    Assert-EmbeddedTree -Tree $tree -Label $Label
    [PSCustomObject]@{
        Label = $Label
        Host = $tree.Host
        Surface = $tree.Surface
        Mpv = $tree.Mpv
    }
}

function Invoke-ResponsivenessProbe {
    param(
        [IntPtr]$HostHandle,
        [int]$Count,
        [int]$TimeoutMs
    )

    $latencies = New-Object System.Collections.Generic.List[double]
    $failures = 0
    $timer = [System.Diagnostics.Stopwatch]::StartNew()
    for ($index = 0; $index -lt $Count; $index++) {
        $sample = [System.Diagnostics.Stopwatch]::StartNew()
        [UIntPtr]$result = [UIntPtr]::Zero
        $sent = [DoriyaW0WindowProbeNative]::SendMessageTimeout(
            $HostHandle,
            $WM_NULL,
            [UIntPtr]::Zero,
            [IntPtr]::Zero,
            $SMTO_ABORTIFHUNG,
            $TimeoutMs,
            [ref]$result)
        $sample.Stop()
        if ($sent -eq [IntPtr]::Zero) {
            $failures++
        } else {
            $latencies.Add($sample.Elapsed.TotalMilliseconds)
        }
        Start-Sleep -Milliseconds 50
    }
    $timer.Stop()

    $maximum = 0.0
    $average = 0.0
    if ($latencies.Count -ne 0) {
        $maximum = ($latencies | Measure-Object -Maximum).Maximum
        $average = ($latencies | Measure-Object -Average).Average
    }
    [PSCustomObject]@{
        Requested = $Count
        Completed = $latencies.Count
        Failed = $failures
        MaximumMs = [Math]::Round($maximum, 2)
        AverageMs = [Math]::Round($average, 2)
        DurationMs = [Math]::Round($timer.Elapsed.TotalMilliseconds, 0)
        TimeoutMs = $TimeoutMs
    }
}

if ($HostProcessId -gt 0) {
    $hostProcess = Get-Process -Id $HostProcessId -ErrorAction Stop
} else {
    $candidates = @(Get-Process -Name 'doriya-w0-tauri-mpv' -ErrorAction SilentlyContinue)
    if ($candidates.Count -ne 1) {
        throw "Expected exactly one doriya-w0-tauri-mpv process; found $($candidates.Count)."
    }
    $hostProcess = $candidates[0]
}

if ($hostProcess.ProcessName -ne 'doriya-w0-tauri-mpv') {
    throw "Process $($hostProcess.Id) is not doriya-w0-tauri-mpv."
}

$windowDeadline = [DateTime]::UtcNow.AddSeconds(10)
while ($hostProcess.MainWindowHandle -eq 0 -and [DateTime]::UtcNow -lt $windowDeadline) {
    Start-Sleep -Milliseconds 100
    $hostProcess.Refresh()
}
if ($hostProcess.MainWindowHandle -eq 0) {
    throw 'The Tauri host did not expose a main window within 10 seconds.'
}

$hostHandle = [IntPtr]$hostProcess.MainWindowHandle
$hostDpi = [DoriyaW0WindowProbeNative]::GetDpiForWindow($hostHandle)
$monitorHandle = [DoriyaW0WindowProbeNative]::MonitorFromWindow(
    $hostHandle,
    $MONITOR_DEFAULTTONEAREST)
$monitorInfo = New-Object DoriyaW0WindowProbeNative+MONITORINFO
$monitorInfo.cbSize = [System.Runtime.InteropServices.Marshal]::SizeOf($monitorInfo)
$monitorAvailable = (
    $monitorHandle -ne [IntPtr]::Zero -and
    [DoriyaW0WindowProbeNative]::GetMonitorInfo($monitorHandle, [ref]$monitorInfo))

$mpvProcesses = @(Get-Process -Name 'mpv' -ErrorAction SilentlyContinue)
if ($mpvProcesses.Count -ne 1) {
    throw "Expected exactly one mpv process; found $($mpvProcesses.Count)."
}
$mpvProcess = $mpvProcesses[0]

$initialTree = Get-EmbeddedTree -HostHandle $hostHandle -ExpectedMpvProcessId $mpvProcess.Id
Assert-EmbeddedTree -Tree $initialTree -Label 'initial'
$originalHost = $initialTree.Host

$snapshots = New-Object System.Collections.Generic.List[object]
$snapshots.Add([PSCustomObject]@{
    Label = 'initial'
    Host = $initialTree.Host
    Surface = $initialTree.Surface
    Mpv = $initialTree.Mpv
})

if ($RunResizeMatrix) {
    $snapshots.Add((Invoke-ResizeStep `
        -HostHandle $hostHandle `
        -MpvProcessId $mpvProcess.Id `
        -Label 'minimum' `
        -Width 656 `
        -Height 519 `
        -X $originalHost.OuterX `
        -Y $originalHost.OuterY))
    $snapshots.Add((Invoke-ResizeStep `
        -HostHandle $hostHandle `
        -MpvProcessId $mpvProcess.Id `
        -Label 'larger' `
        -Width 1558 `
        -Height 970 `
        -X $originalHost.OuterX `
        -Y $originalHost.OuterY))
    $restored = Invoke-ResizeStep `
        -HostHandle $hostHandle `
        -MpvProcessId $mpvProcess.Id `
        -Label 'restored' `
        -Width $originalHost.OuterWidth `
        -Height $originalHost.OuterHeight `
        -X $originalHost.OuterX `
        -Y $originalHost.OuterY
    $snapshots.Add($restored)
}

$responsiveness = $null
if ($RunResponsiveness) {
    $responsiveness = Invoke-ResponsivenessProbe `
        -HostHandle $hostHandle `
        -Count $ProbeCount `
        -TimeoutMs $ProbeTimeoutMs
    if ($responsiveness.Failed -ne 0) {
        throw "Responsiveness probe failed $($responsiveness.Failed) time(s)."
    }
}

$result = [PSCustomObject]@{
    RecordedAt = [DateTimeOffset]::Now.ToString('o')
    HostProcessId = $hostProcess.Id
    HostPath = $hostProcess.Path
    HostSha256 = (Get-FileHash -LiteralPath $hostProcess.Path -Algorithm SHA256).Hash.ToLowerInvariant()
    MpvProcessId = $mpvProcess.Id
    MpvPath = $mpvProcess.Path
    MpvSha256 = (Get-FileHash -LiteralPath $mpvProcess.Path -Algorithm SHA256).Hash.ToLowerInvariant()
    DpiAwarenessSet = $dpiAwarenessSet
    HostDpi = $hostDpi
    HostScale = [Math]::Round($hostDpi / 96.0, 4)
    Monitor = if ($monitorAvailable) {
        [PSCustomObject]@{
            X = $monitorInfo.rcMonitor.Left
            Y = $monitorInfo.rcMonitor.Top
            Width = $monitorInfo.rcMonitor.Right - $monitorInfo.rcMonitor.Left
            Height = $monitorInfo.rcMonitor.Bottom - $monitorInfo.rcMonitor.Top
            WorkX = $monitorInfo.rcWork.Left
            WorkY = $monitorInfo.rcWork.Top
            WorkWidth = $monitorInfo.rcWork.Right - $monitorInfo.rcWork.Left
            WorkHeight = $monitorInfo.rcWork.Bottom - $monitorInfo.rcWork.Top
        }
    } else {
        $null
    }
    InitialHost = $originalHost
    Snapshots = $snapshots
    Responsiveness = $responsiveness
    Status = 'PASS'
}

$json = $result | ConvertTo-Json -Depth 10
if ($OutputPath) {
    $json | Set-Content -LiteralPath $OutputPath -Encoding utf8
}
$json
