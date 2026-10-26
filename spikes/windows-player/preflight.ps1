#requires -Version 5.1

[CmdletBinding()]
param(
    [switch]$NoVersionProbe
)

Set-StrictMode -Version 2.0
$ErrorActionPreference = 'Continue'

function ConvertTo-PreflightText {
    param(
        [AllowNull()][object]$Value,
        [int]$MaxLength = 180
    )

    if ($null -eq $Value) {
        return ''
    }

    $text = [string]$Value
    $text = $text -replace '[\r\n\t]+', ' '
    $text = $text -replace '[\x00-\x1F\x7F]', ''
    $text = $text.Trim()
    $text = $text -replace '(?i)\b[a-z][a-z0-9+.-]*://[^\s]+', '<redacted-url>'
    $text = $text -replace '(?i)\bwww\.[^\s]+', '<redacted-host>'
    if ($text.Length -gt $MaxLength) {
        return $text.Substring(0, $MaxLength) + '...'
    }
    return $text
}

function Write-PreflightLine {
    param([string[]]$Fields)

    $safeFields = @()
    foreach ($field in $Fields) {
        $safeFields += ConvertTo-PreflightText -Value $field
    }
    [Console]::Out.WriteLine('PREFLIGHT|' + [string]::Join('|', $safeFields))
}

function Find-PreflightCommand {
    param([string[]]$Names)

    foreach ($name in $Names) {
        $command = Get-Command -Name $name -CommandType Application,ExternalScript -ErrorAction SilentlyContinue |
            Select-Object -First 1
        if ($null -ne $command) {
            return $command
        }
    }
    return $null
}

function Invoke-SafeVersionProbe {
    param(
        [string]$Path,
        [string[]]$ProbeArguments,
        [string]$VersionPattern = ''
    )

    if ($NoVersionProbe) {
        return [pscustomobject]@{
            Text = 'probe_skipped'
            ExitCode = $null
            Success = $true
        }
    }

    try {
        $output = & $Path @ProbeArguments 2>&1
        $exitCode = $LASTEXITCODE
        $firstLine = ''
        foreach ($item in @($output)) {
            if ($null -eq $item) {
                continue
            }
            $candidate = ConvertTo-PreflightText -Value $item -MaxLength 140
            if ([string]::IsNullOrWhiteSpace($firstLine) -and -not [string]::IsNullOrWhiteSpace($candidate)) {
                $firstLine = $candidate
            }
            if (-not [string]::IsNullOrWhiteSpace($VersionPattern) -and $candidate -match $VersionPattern) {
                $firstLine = $matches[1].Trim()
                break
            }
        }

        if ([string]::IsNullOrWhiteSpace($firstLine)) {
            return [pscustomobject]@{
                Text = 'probe_empty_exit=' + $exitCode
                ExitCode = $exitCode
                Success = ($exitCode -eq 0)
            }
        }
        $versionText = $firstLine + ' exit=' + $exitCode
        if ($exitCode -ne 0) {
            $versionText = 'probe_failed_exit=' + $exitCode
        }
        return [pscustomobject]@{
            Text = $versionText
            ExitCode = $exitCode
            Success = ($exitCode -eq 0)
        }
    }
    catch {
        return [pscustomobject]@{
            Text = 'probe_failed'
            ExitCode = $null
            Success = $false
        }
    }
}

function Get-DotnetSdkInfo {
    param(
        [AllowNull()][object]$DotnetCommand,
        [string]$DotnetRoot = '',
        [string]$DotnetRootX86 = ''
    )

    $roots = @()
    if ($null -ne $DotnetCommand -and -not [string]::IsNullOrWhiteSpace([string]$DotnetCommand.Source)) {
        $commandRoot = Split-Path -Path ([string]$DotnetCommand.Source) -Parent
        if (-not [string]::IsNullOrWhiteSpace($commandRoot)) {
            $roots += $commandRoot
        }
    }
    foreach ($root in @($DotnetRoot, $DotnetRootX86)) {
        if (-not [string]::IsNullOrWhiteSpace($root)) {
            $roots += $root.TrimEnd('\')
        }
    }

    $versions = @()
    foreach ($root in @($roots | Sort-Object -Unique)) {
        $sdkRoot = Join-Path -Path $root -ChildPath 'sdk'
        if (-not (Test-Path -LiteralPath $sdkRoot -PathType Container)) {
            continue
        }
        foreach ($directory in @(Get-ChildItem -LiteralPath $sdkRoot -Directory -ErrorAction SilentlyContinue)) {
            if ($directory.Name -match '^\d+\.\d+\.\d+') {
                $versions += [pscustomobject]@{
                    Version = $directory.Name
                    Source = $directory.FullName
                }
            }
        }
    }

    return @($versions | Sort-Object -Property Version -Descending -Unique)
}

function Get-WebView2Info {
    $registryRoots = @(
        'HKLM:\SOFTWARE\Microsoft\EdgeUpdate\Clients',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients',
        'HKCU:\SOFTWARE\Microsoft\EdgeUpdate\Clients',
        'HKCU:\SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients'
    )

    foreach ($root in $registryRoots) {
        if (-not (Test-Path -LiteralPath $root)) {
            continue
        }

        foreach ($client in @(Get-ChildItem -LiteralPath $root -ErrorAction SilentlyContinue)) {
            $properties = Get-ItemProperty -LiteralPath $client.PSPath -ErrorAction SilentlyContinue
            if ($null -eq $properties) {
                continue
            }

            $name = $null
            if ($null -ne $properties.PSObject.Properties['name']) {
                $name = $properties.name
            }
            elseif ($null -ne $properties.PSObject.Properties['DisplayName']) {
                $name = $properties.DisplayName
            }

            if ($null -ne $name -and [string]$name -match 'WebView2') {
                $version = ''
                if ($null -ne $properties.PSObject.Properties['pv']) {
                    $version = [string]$properties.pv
                }
                return [pscustomobject]@{
                    Found = $true
                    Version = $version
                    Source = $client.PSPath
                }
            }
        }
    }

    $applicationRoots = @(
        (Join-Path -Path $env:ProgramFiles -ChildPath 'Microsoft\EdgeWebView\Application'),
        (Join-Path -Path ${env:ProgramFiles(x86)} -ChildPath 'Microsoft\EdgeWebView\Application')
    )
    foreach ($applicationRoot in $applicationRoots) {
        if ([string]::IsNullOrWhiteSpace($applicationRoot) -or -not (Test-Path -LiteralPath $applicationRoot)) {
            continue
        }
        foreach ($versionDirectory in @(Get-ChildItem -LiteralPath $applicationRoot -Directory -ErrorAction SilentlyContinue)) {
            $executable = Join-Path -Path $versionDirectory.FullName -ChildPath 'msedgewebview2.exe'
            if (Test-Path -LiteralPath $executable) {
                return [pscustomobject]@{
                    Found = $true
                    Version = $versionDirectory.Name
                    Source = $executable
                }
            }
        }
    }

    return [pscustomobject]@{
        Found = $false
        Version = ''
        Source = ''
    }
}

function Get-VisualStudioInfo {
    $vswhereCandidates = @(
        (Join-Path -Path ${env:ProgramFiles(x86)} -ChildPath 'Microsoft Visual Studio\Installer\vswhere.exe'),
        (Join-Path -Path $env:ProgramFiles -ChildPath 'Microsoft Visual Studio\Installer\vswhere.exe')
    )

    foreach ($vswhere in $vswhereCandidates) {
        if ([string]::IsNullOrWhiteSpace($vswhere) -or -not (Test-Path -LiteralPath $vswhere)) {
            continue
        }

        $installationPath = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath 2>$null
        $installationPath = [string](@($installationPath) | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } | Select-Object -First 1)
        if ([string]::IsNullOrWhiteSpace($installationPath)) {
            continue
        }
        $installationPath = $installationPath.Trim()

        $version = 'probe_skipped'
        if (-not $NoVersionProbe) {
            $version = & $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property catalog_productDisplayVersion 2>$null
            $version = ConvertTo-PreflightText -Value $version
        }
        return [pscustomobject]@{
            Found = $true
            Version = $version
            Source = $installationPath
        }
    }

    return [pscustomobject]@{
        Found = $false
        Version = ''
        Source = ''
    }
}

function Get-WindowsSdkInfo {
    $roots = @(
        'HKLM:\SOFTWARE\Microsoft\Microsoft SDKs\Windows\v10.0',
        'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Microsoft SDKs\Windows\v10.0'
    )

    foreach ($root in $roots) {
        if (-not (Test-Path -LiteralPath $root)) {
            continue
        }
        $properties = Get-ItemProperty -LiteralPath $root -ErrorAction SilentlyContinue
        if ($null -eq $properties) {
            continue
        }

        $version = ''
        $source = ''
        if ($null -ne $properties.PSObject.Properties['ProductVersion']) {
            $version = [string]$properties.ProductVersion
        }
        if ($null -ne $properties.PSObject.Properties['InstallationFolder']) {
            $source = [string]$properties.InstallationFolder
        }
        if (-not [string]::IsNullOrWhiteSpace($version) -or -not [string]::IsNullOrWhiteSpace($source)) {
            return [pscustomobject]@{
                Found = $true
                Version = $version
                Source = $source
            }
        }
    }

    return [pscustomobject]@{
        Found = $false
        Version = ''
        Source = ''
    }
}

function Get-PeImageMachine {
    param([string]$Path)

    $result = [pscustomobject]@{
        Machine = $null
        Reason = ''
    }
    $stream = $null
    $reader = $null

    try {
        $stream = [System.IO.File]::Open(
            $Path,
            [System.IO.FileMode]::Open,
            [System.IO.FileAccess]::Read,
            [System.IO.FileShare]::ReadWrite
        )
        if ($stream.Length -lt 64) {
            $result.Reason = 'file_too_small'
            return $result
        }

        $reader = New-Object System.IO.BinaryReader -ArgumentList $stream
        if ($reader.ReadUInt16() -ne 0x5A4D) {
            $result.Reason = 'missing_mz_header'
            return $result
        }

        $stream.Position = 0x3C
        $peOffset = [long]$reader.ReadUInt32()
        if ($peOffset -lt 64 -or ($peOffset + 6) -gt $stream.Length) {
            $result.Reason = 'invalid_pe_offset'
            return $result
        }

        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) {
            $result.Reason = 'missing_pe_signature'
            return $result
        }

        $result.Machine = [int]$reader.ReadUInt16()
        return $result
    }
    catch {
        $result.Reason = 'read_error'
        return $result
    }
    finally {
        if ($null -ne $reader) {
            $reader.Dispose()
        }
        if ($null -ne $stream) {
            $stream.Dispose()
        }
    }
}

function Get-PreflightProcessArchitecture {
    $architecture = ([string]$env:PROCESSOR_ARCHITECTURE).ToUpperInvariant()
    if ($architecture -eq 'AMD64' -and [Environment]::Is64BitProcess) {
        return [pscustomobject]@{
            Name = 'x64'
            Machine = 0x8664
            Reason = ''
        }
    }
    if ($architecture -eq 'ARM64' -and [Environment]::Is64BitProcess) {
        return [pscustomobject]@{
            Name = 'arm64'
            Machine = 0xAA64
            Reason = ''
        }
    }

    $reasonArchitecture = 'unknown'
    if (-not [string]::IsNullOrWhiteSpace($architecture)) {
        $reasonArchitecture = $architecture.ToLowerInvariant()
    }
    return [pscustomobject]@{
        Name = $reasonArchitecture
        Machine = 0
        Reason = 'unsupported_process_architecture_' + $reasonArchitecture
    }
}

function Get-PeMachineName {
    param([int]$Machine)

    switch ($Machine) {
        0x8664 { return 'x64' }
        0xAA64 { return 'arm64' }
        0x014C { return 'x86' }
        0x01C4 { return 'arm' }
        default { return ('unknown_0x{0:X4}' -f $Machine) }
    }
}

function Get-LibVlcInfo {
    param([AllowNull()][object]$VlcCommand)

    $processArchitecture = Get-PreflightProcessArchitecture
    $candidates = @()
    if ($null -ne $VlcCommand -and -not [string]::IsNullOrWhiteSpace([string]$VlcCommand.Source)) {
        $vlcDirectory = Split-Path -Path ([string]$VlcCommand.Source) -Parent
        if (-not [string]::IsNullOrWhiteSpace($vlcDirectory)) {
            $candidates += (Join-Path -Path $vlcDirectory -ChildPath 'libvlc.dll')
        }
    }
    $candidates += (Join-Path -Path $env:ProgramFiles -ChildPath 'VideoLAN\VLC\libvlc.dll')
    $candidates += (Join-Path -Path ${env:ProgramFiles(x86)} -ChildPath 'VideoLAN\VLC\libvlc.dll')

    $firstBroken = $null
    foreach ($candidate in @($candidates | Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } | Sort-Object -Unique)) {
        if (-not (Test-Path -LiteralPath $candidate -PathType Leaf -ErrorAction SilentlyContinue)) {
            continue
        }

        $libVlcDirectory = Split-Path -Path $candidate -Parent
        $reason = ''
        $source = $candidate
        if ($processArchitecture.Machine -eq 0) {
            $reason = $processArchitecture.Reason
        }

        $corePath = Join-Path -Path $libVlcDirectory -ChildPath 'libvlccore.dll'
        $coreMachine = $null
        if ([string]::IsNullOrWhiteSpace($reason)) {
            if (-not (Test-Path -LiteralPath $corePath -PathType Leaf -ErrorAction SilentlyContinue)) {
                $reason = 'missing_libvlccore'
                $source = $corePath
            }
            else {
                $libVlcPe = Get-PeImageMachine -Path $candidate
                if ($null -eq $libVlcPe.Machine) {
                    $reason = 'invalid_libvlc_pe_' + $libVlcPe.Reason
                }
                elseif ($libVlcPe.Machine -ne $processArchitecture.Machine) {
                    $reason = 'libvlc_arch_mismatch'
                }
            }
        }
        if ([string]::IsNullOrWhiteSpace($reason)) {
            $corePe = Get-PeImageMachine -Path $corePath
            if ($null -eq $corePe.Machine) {
                $reason = 'invalid_libvlccore_pe_' + $corePe.Reason
                $source = $corePath
            }
            elseif ($corePe.Machine -ne $processArchitecture.Machine) {
                $reason = 'libvlccore_arch_mismatch'
                $source = $corePath
            }
            else {
                $coreMachine = $corePe.Machine
            }
        }

    $pluginFiles = @()
    $pluginDllCount = 0
        $pluginsDirectory = Join-Path -Path $libVlcDirectory -ChildPath 'plugins'
        if ([string]::IsNullOrWhiteSpace($reason)) {
            if (-not (Test-Path -LiteralPath $pluginsDirectory -PathType Container -ErrorAction SilentlyContinue)) {
                $reason = 'plugins_directory_missing'
                $source = $pluginsDirectory
            }
            else {
                $pluginFiles = @(Get-ChildItem -LiteralPath $pluginsDirectory -Recurse -File -Filter '*.dll' -ErrorAction SilentlyContinue |
                    Where-Object { $_.Length -gt 0 })
                $pluginDllCount = $pluginFiles.Count
                if ($pluginDllCount -eq 0) {
                    $reason = 'plugins_directory_empty'
                    $source = $pluginsDirectory
                }
            }
        }
        if ([string]::IsNullOrWhiteSpace($reason) -and $pluginDllCount -lt 2) {
            $reason = 'plugins_insufficient'
            $source = $pluginsDirectory
        }
        if ([string]::IsNullOrWhiteSpace($reason)) {
            foreach ($pluginFile in $pluginFiles) {
                $pluginPe = Get-PeImageMachine -Path $pluginFile.FullName
                if ($null -eq $pluginPe.Machine) {
                    $reason = 'invalid_plugin_pe_' + $pluginPe.Reason
                    $source = $pluginFile.FullName
                    break
                }
                if ($pluginPe.Machine -ne $processArchitecture.Machine) {
                    $reason = 'plugin_arch_mismatch'
                    $source = $pluginFile.FullName
                    break
                }
            }
        }

        if (-not [string]::IsNullOrWhiteSpace($reason)) {
            if ($null -eq $firstBroken) {
                $firstBroken = [pscustomobject]@{
                    Found = $false
                    Status = 'BROKEN'
                    Version = ''
                    Source = $source
                    Reason = $reason
                    Detail = ''
                }
            }
            continue
        }

        $version = ''
        if ($null -ne $VlcCommand) {
            $version = [string]$VlcCommand.Version
        }
        if ([string]::IsNullOrWhiteSpace($version)) {
            $libVlcFile = Get-Item -LiteralPath $candidate -ErrorAction SilentlyContinue
            if ($null -ne $libVlcFile) {
                $version = [string]$libVlcFile.VersionInfo.FileVersion
            }
        }
        return [pscustomobject]@{
            Found = $true
            Status = 'FOUND'
            Version = $version
            Source = $candidate
            Reason = ''
            Detail = ('arch=' + $processArchitecture.Name +
                ' core_arch=' + (Get-PeMachineName -Machine $coreMachine) +
                ' plugins=' + $pluginDllCount)
        }
    }

    if ($null -ne $firstBroken) {
        return $firstBroken
    }

    return [pscustomobject]@{
        Found = $false
        Status = 'MISSING'
        Version = ''
        Source = ''
        Reason = 'libvlc_not_found'
        Detail = ''
    }
}

$os = Get-CimInstance -ClassName Win32_OperatingSystem -ErrorAction SilentlyContinue
if ($null -ne $os) {
    $buildNumber = 0
    [void][int]::TryParse([string]$os.BuildNumber, [ref]$buildNumber)
    $windowsName = [string]$os.Caption
    if ($buildNumber -ge 22000) {
        $windowsName = 'Windows 11'
    }
    elseif ($buildNumber -ge 10240 -and $windowsName -notmatch 'Windows 10|Windows 11') {
        $windowsName = 'Windows 10'
    }
    Write-PreflightLine -Fields @(
        'system',
        'windows',
        'FOUND',
        ('name=' + $windowsName + ' version=' + [string]$os.Version + ' build=' + [string]$os.BuildNumber + ' arch=' + [string]$env:PROCESSOR_ARCHITECTURE)
    )
}
else {
    Write-PreflightLine -Fields @('system', 'windows', 'MISSING', 'reason=wmi_unavailable')
}

Write-PreflightLine -Fields @(
    'system',
    'powershell',
    'FOUND',
    ('version=' + [string]$PSVersionTable.PSVersion + ' edition=' + [string]$PSVersionTable.PSEdition + ' process_arch=' + [string]$env:PROCESSOR_ARCHITECTURE)
)

$gpuRecords = @(Get-CimInstance -ClassName Win32_VideoController -ErrorAction SilentlyContinue)
if ($gpuRecords.Count -eq 0) {
    Write-PreflightLine -Fields @('system', 'gpu', 'MISSING', 'reason=wmi_unavailable')
}
else {
    foreach ($gpu in $gpuRecords) {
        Write-PreflightLine -Fields @(
            'system',
            'gpu',
            'FOUND',
            ('name=' + [string]$gpu.Name + ' driver=' + [string]$gpu.DriverVersion)
        )
    }
}

$webView2 = Get-WebView2Info
if ($webView2.Found) {
    Write-PreflightLine -Fields @('runtime', 'webview2', 'FOUND', ('version=' + $webView2.Version), $webView2.Source)
}
else {
    Write-PreflightLine -Fields @('runtime', 'webview2', 'MISSING', '', '')
}

$visualStudio = Get-VisualStudioInfo
if ($visualStudio.Found) {
    Write-PreflightLine -Fields @('buildchain', 'visual-studio-cpp', 'FOUND', ('version=' + $visualStudio.Version), $visualStudio.Source)
}
else {
    Write-PreflightLine -Fields @('buildchain', 'visual-studio-cpp', 'MISSING', '', '')
}

$windowsSdk = Get-WindowsSdkInfo
if ($windowsSdk.Found) {
    Write-PreflightLine -Fields @('buildchain', 'windows-sdk', 'FOUND', ('version=' + $windowsSdk.Version), $windowsSdk.Source)
}
else {
    Write-PreflightLine -Fields @('buildchain', 'windows-sdk', 'MISSING', '', '')
}

$toolDefinitions = @(
    [pscustomobject]@{ Id = 'cargo'; Names = @('cargo.exe', 'cargo'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'rustc'; Names = @('rustc.exe', 'rustc'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'dotnet'; Names = @('dotnet.exe', 'dotnet'); ProbeArguments = @('--info'); VersionPattern = '^Version:\s*(.+)$' },
    [pscustomobject]@{ Id = 'ffmpeg'; Names = @('ffmpeg.exe', 'ffmpeg'); ProbeArguments = @('-version'); VersionPattern = '^ffmpeg version\s+([0-9]+(?:\.[0-9]+){1,3})' },
    [pscustomobject]@{ Id = 'ffprobe'; Names = @('ffprobe.exe', 'ffprobe'); ProbeArguments = @('-version'); VersionPattern = '^ffprobe version\s+([0-9]+(?:\.[0-9]+){1,3})' },
    [pscustomobject]@{ Id = 'node'; Names = @('node.exe', 'node'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'npm'; Names = @('npm.cmd', 'npm.exe', 'npm'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'pnpm'; Names = @('pnpm.cmd', 'pnpm.exe', 'pnpm'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'mpv'; Names = @('mpv.exe', 'mpv'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'vlc'; Names = @('vlc.exe', 'vlc'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'java'; Names = @('java.exe', 'java'); ProbeArguments = @('-version') },
    [pscustomobject]@{ Id = 'gradle'; Names = @('gradle.bat', 'gradle.exe', 'gradle'); ProbeArguments = $null },
    [pscustomobject]@{ Id = 'adb'; Names = @('adb.exe', 'adb'); ProbeArguments = $null },
    [pscustomobject]@{ Id = 'cmake'; Names = @('cmake.exe', 'cmake'); ProbeArguments = @('--version') },
    [pscustomobject]@{ Id = 'msbuild'; Names = @('msbuild.exe', 'MSBuild.exe', 'msbuild'); ProbeArguments = @('-version') },
    [pscustomobject]@{ Id = 'git'; Names = @('git.exe', 'git'); ProbeArguments = @('--version') }
)

$toolResults = @{}
foreach ($definition in $toolDefinitions) {
    $command = Find-PreflightCommand -Names $definition.Names
    if ($null -eq $command) {
        if ($definition.Id -eq 'msbuild' -and $visualStudio.Found) {
            $visualStudioMsBuild = Join-Path -Path $visualStudio.Source -ChildPath 'MSBuild\Current\Bin\MSBuild.exe'
            if (Test-Path -LiteralPath $visualStudioMsBuild) {
                $toolResults[$definition.Id] = $true
                Write-PreflightLine -Fields @('tool', 'msbuild', 'FOUND', 'discovered_via_visual_studio', $visualStudioMsBuild)
                continue
            }
        }
        $toolResults[$definition.Id] = $false
        Write-PreflightLine -Fields @('tool', $definition.Id, 'MISSING', '', '')
        continue
    }

    $version = ''
    $probeSucceeded = $true
    if ($null -ne $definition.ProbeArguments) {
        $versionPattern = ''
        if ($null -ne $definition.PSObject.Properties['VersionPattern']) {
            $versionPattern = [string]$definition.VersionPattern
        }
        $probe = Invoke-SafeVersionProbe -Path ([string]$command.Source) -ProbeArguments $definition.ProbeArguments -VersionPattern $versionPattern
        $version = $probe.Text
        $probeSucceeded = [bool]$probe.Success
    }
    else {
        $version = 'presence_only'
    }
    $toolResults[$definition.Id] = $probeSucceeded
    if ($probeSucceeded) {
        Write-PreflightLine -Fields @('tool', $definition.Id, 'FOUND', $version, [string]$command.Source)
    }
    else {
        Write-PreflightLine -Fields @('tool', $definition.Id, 'BROKEN', $version, [string]$command.Source)
    }
}

$vlcCommand = $null
if ($toolResults['vlc']) {
    $vlcCommand = Find-PreflightCommand -Names @('vlc.exe', 'vlc')
}
$libVlc = Get-LibVlcInfo -VlcCommand $vlcCommand
if ($libVlc.Found) {
    Write-PreflightLine -Fields @('runtime', 'libvlc', 'FOUND', ('version=' + $libVlc.Version + ' ' + $libVlc.Detail), $libVlc.Source)
}
elseif ($libVlc.Status -eq 'BROKEN') {
    Write-PreflightLine -Fields @('runtime', 'libvlc', 'BROKEN', ('reason=' + $libVlc.Reason), $libVlc.Source)
}
else {
    Write-PreflightLine -Fields @('runtime', 'libvlc', 'MISSING', '', '')
}

$dotnetCommand = $null
if ($toolResults['dotnet']) {
    $dotnetCommand = Find-PreflightCommand -Names @('dotnet.exe', 'dotnet')
}
$dotnetSdkVersions = @(Get-DotnetSdkInfo -DotnetCommand $dotnetCommand -DotnetRoot ([string]$env:DOTNET_ROOT) -DotnetRootX86 ([string]${env:DOTNET_ROOT(x86)}))

if ($dotnetSdkVersions.Count -gt 0) {
    $sdkSummary = [string]::Join('; ', @($dotnetSdkVersions | ForEach-Object { $_.Version } | Select-Object -First 4))
    if ($NoVersionProbe) {
        $sdkSummary += ' version_probe_skipped'
    }
    Write-PreflightLine -Fields @('runtime', 'dotnet-sdk', 'FOUND', $sdkSummary, [string]$dotnetSdkVersions[0].Source)
}
else {
    $sdkRootDetail = 'reason=sdk_directory_not_found'
    if ($null -eq $dotnetCommand) {
        $sdkRootDetail = 'reason=dotnet_host_missing'
    }
    Write-PreflightLine -Fields @('runtime', 'dotnet-sdk', 'MISSING', $sdkRootDetail, '')
}

$available = @{
    'cargo' = [bool]$toolResults['cargo']
    'rustc' = [bool]$toolResults['rustc']
    'dotnet' = [bool]$toolResults['dotnet']
    'node' = [bool]$toolResults['node']
    'npm' = [bool]$toolResults['npm']
    'mpv' = [bool]$toolResults['mpv']
    'msbuild' = [bool]$toolResults['msbuild']
    'webview2' = [bool]$webView2.Found
    'visual-studio-cpp' = [bool]$visualStudio.Found
    'windows-sdk' = [bool]$windowsSdk.Found
    'libvlc' = [bool]$libVlc.Found
    'dotnet-sdk' = [bool]($dotnetSdkVersions.Count -gt 0)
}

$tauriRequired = @('cargo', 'rustc', 'node', 'npm', 'mpv', 'webview2', 'visual-studio-cpp', 'windows-sdk')
$winuiRequired = @('dotnet-sdk', 'windows-sdk', 'libvlc')

$allMissing = @()
foreach ($candidate in @(
    [pscustomobject]@{ Id = 'tauri-mpv'; Required = $tauriRequired },
    [pscustomobject]@{ Id = 'winui-libvlcsharp'; Required = $winuiRequired }
)) {
    $missing = @()
    foreach ($requirement in $candidate.Required) {
        if (-not $available[$requirement]) {
            $missing += $requirement
            $allMissing += $requirement
        }
    }

    if ($missing.Count -eq 0) {
        if ($NoVersionProbe) {
            Write-PreflightLine -Fields @('candidate', $candidate.Id, 'UNVERIFIED', 'reason=version_probe_skipped', '')
        }
        else {
            Write-PreflightLine -Fields @('candidate', $candidate.Id, 'READY_FOR_PROTOTYPE', 'missing=none', '')
        }
    }
    else {
        Write-PreflightLine -Fields @('candidate', $candidate.Id, 'BLOCKED', ('missing=' + [string]::Join(',', $missing)), '')
    }
}

$uniqueMissing = @($allMissing | Sort-Object -Unique)
if ($uniqueMissing.Count -eq 0) {
    if ($NoVersionProbe) {
        Write-PreflightLine -Fields @('summary', 'preflight', 'UNVERIFIED', 'reason=version_probe_skipped', 'W0_NOT_RUN')
        exit 2
    }
    Write-PreflightLine -Fields @('summary', 'preflight', 'READY_FOR_PROTOTYPE', 'missing=none', 'W0_NOT_RUN')
    exit 0
}

Write-PreflightLine -Fields @(
    'summary',
    'preflight',
    'BLOCKED',
    ('missing=' + [string]::Join(',', $uniqueMissing)),
    'W0_NOT_RUN'
)
exit 2
