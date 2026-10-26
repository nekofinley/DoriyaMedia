[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.Net.Http

$root = Split-Path -Parent $PSScriptRoot
$tempBase = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$scratch = Join-Path $tempBase ("doriyamedia-playback-e2e-" + [guid]::NewGuid().ToString('N'))
$dataDir = Join-Path $scratch 'data'
$mediaDir = Join-Path $scratch 'media'
$toolsDir = Join-Path $scratch 'tools'
$configPath = Join-Path $scratch 'config.json'
$exePath = Join-Path $scratch 'doriyamedia-e2e.exe'
$fakeProbeSource = Join-Path $toolsDir 'fake-ffprobe.go'
$fakeProbeExe = Join-Path $toolsDir 'fake-ffprobe.exe'
$buildLog = Join-Path $scratch 'build.log'
$fakeProbeBuildLog = Join-Path $scratch 'fake-probe-build.log'
$mediaFixtureLog = Join-Path $scratch 'media-fixture.log'
$hlsDecodeLog = Join-Path $scratch 'hls-decode.log'
$hlsSegmentPath = Join-Path $scratch 'playback-segment.ts'
# Keep the video and sidecar below the library root so normal external-subtitle
# attachment is exercised rather than the root-directory edge case.
$proxyMediaDir = Join-Path $mediaDir 'movies'
$proxyMediaPath = Join-Path $proxyMediaDir 'proxy-source.mp4'
$subtitleSrtPath = Join-Path $proxyMediaDir 'proxy-source.srt'
$directStrmPath = Join-Path $mediaDir 'direct-source.strm'
$subtitleSrt = @'
1
00:00:07,000 --> 00:00:09,000
Playback subtitle first line

2
00:00:12,500 --> 00:00:14,000
Playback subtitle second line
'@

$script:passCount = 0
$script:failCount = 0
$script:skipCount = 0
$script:serverProcess = $null
$script:serverRun = 0
$script:serverLogs = [System.Collections.Generic.List[string]]::new()
$script:clients = [System.Collections.Generic.List[object]]::new()
$script:sensitiveValues = [System.Collections.Generic.List[string]]::new()
$script:mediaRequestMade = $false
$script:subtitleRequestMade = $false
$oldLocalEnv = $env:DORIYAMEDIA_LOCAL

function Check {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][bool]$Ok
    )

    if ($Ok) {
        $script:passCount++
        Write-Output "[PASS] $Name"
        return
    }
    $script:failCount++
    Write-Output "[FAIL] $Name"
}

function Skip {
    param([Parameter(Mandatory = $true)][string]$Name)

    $script:skipCount++
    Write-Output "[SKIP] $Name"
}

function Add-SensitiveValue {
    param([AllowNull()][AllowEmptyString()][string]$Value)

    if ([string]::IsNullOrWhiteSpace($Value) -or $Value.Length -lt 8) {
        return
    }
    if (-not $script:sensitiveValues.Contains($Value)) {
        $script:sensitiveValues.Add($Value)
    }
}

function Get-Field {
    param($Object, [string]$Name)

    if ($null -eq $Object) {
        return $null
    }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return $null
    }
    return $property.Value
}

function Get-ErrorCode {
    param($Payload)

    $errorObject = Get-Field $Payload 'error'
    return [string](Get-Field $errorObject 'code')
}

function Get-ErrorDetails {
    param($Payload)

    $errorObject = Get-Field $Payload 'error'
    return Get-Field $errorObject 'details'
}

function Get-ResponseHeader {
    param($Response, [string]$Name)

    if ($null -eq $Response) {
        return ''
    }
    if ($Response.Headers.ContainsKey($Name)) {
        return [string]$Response.Headers[$Name]
    }
    if ($Response.ContentHeaders.ContainsKey($Name)) {
        return [string]$Response.ContentHeaders[$Name]
    }
    return ''
}

function Get-ResponseSummary {
    param($Response)

    if ($null -eq $Response) {
        return 'no response'
    }
    $code = Get-ErrorCode $Response.Payload
    if ($code) {
        return "HTTP $($Response.Status), error=$code"
    }
    return "HTTP $($Response.Status)"
}

function ConvertTo-SafeMessage {
    param([AllowNull()][AllowEmptyString()][string]$Message)

    if ([string]::IsNullOrWhiteSpace($Message)) {
        return 'unknown error'
    }
    $safe = $Message
    foreach ($value in $script:sensitiveValues) {
        $safe = [regex]::Replace($safe, [regex]::Escape($value), '<redacted>')
    }
    $safe = [regex]::Replace($safe, 'https?://[^\s"''<>]+', '<redacted-url>')
    $safe = [regex]::Replace(
        $safe,
        '(?i)(authorization|access_token|refresh_token|device_code|user_code|csrf_token|signature)\s*[:=]\s*[^\s,;]+',
        '$1=<redacted>'
    )
    if ($safe.Length -gt 400) {
        $safe = $safe.Substring(0, 400)
    }
    return $safe
}

function New-WebClient {
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.UseCookies = $true
    $handler.CookieContainer = [System.Net.CookieContainer]::new()
    $handler.UseProxy = $false
    $handler.AllowAutoRedirect = $false
    $client = [System.Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(20)
    $entry = [pscustomobject]@{ Client = $client; Handler = $handler }
    $script:clients.Add($entry)
    return $entry
}

function Invoke-Json {
    param(
        [Parameter(Mandatory = $true)]$WebClient,
        [Parameter(Mandatory = $true)][System.Net.Http.HttpMethod]$Method,
        [Parameter(Mandatory = $true)][string]$Uri,
        $Body = $null,
        [string]$Bearer = '',
        [hashtable]$Headers = @{}
    )

    $request = [System.Net.Http.HttpRequestMessage]::new($Method, $Uri)
    $request.Headers.ExpectContinue = $false
    try {
        if ($null -ne $Body) {
            $json = $Body | ConvertTo-Json -Depth 20 -Compress
            $request.Content = [System.Net.Http.StringContent]::new(
                $json,
                [System.Text.Encoding]::UTF8,
                'application/json'
            )
        }
        if ($Bearer) {
            $request.Headers.TryAddWithoutValidation('Authorization', "Bearer $Bearer") | Out-Null
        }
        foreach ($key in $Headers.Keys) {
            $request.Headers.TryAddWithoutValidation([string]$key, [string]$Headers[$key]) | Out-Null
        }

        $response = $WebClient.Client.SendAsync($request).GetAwaiter().GetResult()
        try {
            $status = [int]$response.StatusCode
            $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
            $payload = $null
            if (-not [string]::IsNullOrWhiteSpace($text)) {
                try {
                    $payload = $text | ConvertFrom-Json -ErrorAction Stop
                } catch {
                    $payload = $null
                }
            }
            $responseHeaders = @{}
            foreach ($header in $response.Headers) {
                $responseHeaders[$header.Key] = (@($header.Value) -join ', ')
            }
            $contentHeaders = @{}
            foreach ($header in $response.Content.Headers) {
                $contentHeaders[$header.Key] = (@($header.Value) -join ', ')
            }
            return [pscustomobject]@{
                Status = $status
                Payload = $payload
                Body = $text
                Headers = $responseHeaders
                ContentHeaders = $contentHeaders
            }
        } finally {
            $response.Dispose()
        }
    } finally {
        $request.Dispose()
    }
}

function Invoke-Bytes {
    param(
        [Parameter(Mandatory = $true)]$WebClient,
        [Parameter(Mandatory = $true)][System.Net.Http.HttpMethod]$Method,
        [Parameter(Mandatory = $true)][string]$Uri
    )

    $request = [System.Net.Http.HttpRequestMessage]::new($Method, $Uri)
    $request.Headers.ExpectContinue = $false
    try {
        $response = $WebClient.Client.SendAsync($request).GetAwaiter().GetResult()
        try {
            $bytes = $response.Content.ReadAsByteArrayAsync().GetAwaiter().GetResult()
            $responseHeaders = @{}
            foreach ($header in $response.Headers) {
                $responseHeaders[$header.Key] = (@($header.Value) -join ', ')
            }
            $contentHeaders = @{}
            foreach ($header in $response.Content.Headers) {
                $contentHeaders[$header.Key] = (@($header.Value) -join ', ')
            }
            return [pscustomobject]@{
                Status = [int]$response.StatusCode
                Bytes = $bytes
                Headers = $responseHeaders
                ContentHeaders = $contentHeaders
            }
        } finally {
            $response.Dispose()
        }
    } finally {
        $request.Dispose()
    }
}

function New-RandomBase64Url {
    param([int]$ByteCount)

    $bytes = [byte[]]::new($ByteCount)
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
    } finally {
        $rng.Dispose()
    }
    return [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function New-PKCEPair {
    $verifier = New-RandomBase64Url -ByteCount 32
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $digest = $sha.ComputeHash([System.Text.Encoding]::ASCII.GetBytes($verifier))
    } finally {
        $sha.Dispose()
    }
    $challenge = [Convert]::ToBase64String($digest).TrimEnd('=').Replace('+', '-').Replace('/', '_')
    return [pscustomobject]@{ Verifier = $verifier; Challenge = $challenge }
}

function Get-FreeLoopbackPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    $listener.Start()
    try {
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    } finally {
        $listener.Stop()
    }
}

function Write-Utf8File {
    param([string]$Path, [string]$Text)

    [System.IO.File]::WriteAllText($Path, $Text, [System.Text.UTF8Encoding]::new($false))
}

function Initialize-FakeFFProbe {
    $source = @'
package main

import "fmt"

const result = `{
  "format": {"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"120.0"},
  "streams": [
    {"index":0,"codec_type":"video","codec_name":"h264","width":1920,"height":1080},
    {"index":1,"codec_type":"audio","codec_name":"aac","channels":2,"tags":{"language":"eng"}},
    {"index":2,"codec_type":"subtitle","codec_name":"hdmv_pgs_subtitle",
     "tags":{"language":"eng","title":"English PGS"}}
  ]
}`

func main() {
	fmt.Print(result)
}
'@
    Write-Utf8File -Path $fakeProbeSource -Text $source
}

function Write-Config {
    param(
        [bool]$TicketEnabled = $true,
        [bool]$DirectEnabled = $true,
        [bool]$ProxyEnabled = $true,
        [bool]$HLSEnabled = $true
    )

    $ffmpegCommand = Get-Command ffmpeg -ErrorAction SilentlyContinue
    $ffmpegPath = 'ffmpeg'
    if ($null -ne $ffmpegCommand) {
        $ffmpegPath = $ffmpegCommand.Source
    }

    $config = [ordered]@{
        port = $script:port
        data_dir = $dataDir
        secret = $script:serverSecret
        ffmpeg = $ffmpegPath
        ffprobe = $fakeProbeExe
        users = [ordered]@{
            registration = 'closed'
            min_password = 4
            require_email = $false
            base_url = 'https://media.example'
        }
        playback = [ordered]@{
            preprobe_after_scan = $false
            probe_size_mb = 1
            probe_analyze_sec = 1
        }
        scan_concurrency = 1
        client = [ordered]@{
            v1_enabled = $true
            device_auth_enabled = $true
            playback_ticket_enabled = $TicketEnabled
            direct_http_enabled = $DirectEnabled
            server_proxy_v1_enabled = $ProxyEnabled
            server_hls_v1_enabled = $HLSEnabled
            direct_dash_enabled = $false
            local_relay_enabled = $false
            native_player_enabled = $true
            libmpv_fallback_enabled = $false
        }
    }

    $json = $config | ConvertTo-Json -Depth 20
    Write-Utf8File -Path $configPath -Text $json
}

function Start-Server {
    param([Parameter(Mandatory = $true)][string]$Phase)

    $script:serverRun++
    $safePhase = [regex]::Replace($Phase, '[^A-Za-z0-9_-]+', '-')
    $stdout = Join-Path $scratch ("server-{0:D2}-{1}.out.log" -f $script:serverRun, $safePhase)
    $stderr = Join-Path $scratch ("server-{0:D2}-{1}.err.log" -f $script:serverRun, $safePhase)
    $script:serverLogs.Add($stdout)
    $script:serverLogs.Add($stderr)

    $script:serverProcess = Start-Process -FilePath $exePath `
        -ArgumentList @('-config', $configPath) `
        -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $stdout -RedirectStandardError $stderr

    $probe = New-WebClient
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(20)
        while ([DateTime]::UtcNow -lt $deadline) {
            if ($script:serverProcess.HasExited) {
                return $false
            }
            try {
                $response = $probe.Client.GetAsync("$base/api/bootstrap").GetAwaiter().GetResult()
                try {
                    if ([int]$response.StatusCode -eq 200) {
                        return $true
                    }
                } finally {
                    $response.Dispose()
                }
            } catch {
                # The listener is not ready yet.
            }
            Start-Sleep -Milliseconds 100
        }
        return $false
    } finally {
        $probe.Client.Dispose()
        $probe.Handler.Dispose()
    }
}

function Stop-Server {
    if ($null -ne $script:serverProcess -and -not $script:serverProcess.HasExited) {
        Stop-Process -Id $script:serverProcess.Id -Force -ErrorAction SilentlyContinue
        $null = $script:serverProcess.WaitForExit(5000)
    }
    $script:serverProcess = $null
    Start-Sleep -Milliseconds 150
}

function New-DeviceSession {
    param(
        [Parameter(Mandatory = $true)]$WebClient,
        [Parameter(Mandatory = $true)][string]$DeviceName,
        [Parameter(Mandatory = $true)][string]$Platform
    )

    $deviceClient = New-WebClient
    $pkce = New-PKCEPair
    $start = Invoke-Json -WebClient $deviceClient `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/auth/device/authorizations" `
        -Body @{
            device_name = $DeviceName
            platform = $Platform
            client_version = '0.1.0'
            code_challenge = $pkce.Challenge
            code_challenge_method = 'S256'
        }
    $authorizationID = [string](Get-Field $start.Payload 'authorization_id')
    $deviceCode = [string](Get-Field $start.Payload 'device_code')
    $userCode = [string](Get-Field $start.Payload 'user_code')
    Add-SensitiveValue $deviceCode
    Add-SensitiveValue $userCode
    Check "device authorization start: $DeviceName" (
        $start.Status -eq 201 -and
        -not [string]::IsNullOrEmpty($authorizationID) -and
        -not [string]::IsNullOrEmpty($deviceCode) -and
        -not [string]::IsNullOrEmpty($userCode)
    )
    if (-not $authorizationID -or -not $deviceCode -or -not $userCode) {
        throw "device authorization did not return a usable grant"
    }

    $lookup = Invoke-Json -WebClient $WebClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base/api/v1/auth/device/authorizations/lookup?user_code=$([uri]::EscapeDataString($userCode))"
    $csrf = [string](Get-Field $lookup.Payload 'csrf_token')
    Add-SensitiveValue $csrf
    if ($lookup.Status -ne 200 -or -not $csrf) {
        throw "device authorization lookup failed: $(Get-ResponseSummary $lookup)"
    }

    $approve = Invoke-Json -WebClient $WebClient `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationID))/decision" `
        -Body @{ decision = 'approve' } `
        -Headers @{
            Origin = $base
            'Sec-Fetch-Site' = 'same-origin'
            'X-CSRF-Token' = $csrf
        }
    Check "device authorization approve: $DeviceName" (
        $approve.Status -eq 200 -and (Get-Field $approve.Payload 'state') -eq 'authorized'
    )
    if ($approve.Status -ne 200) {
        throw "device authorization approval failed: $(Get-ResponseSummary $approve)"
    }

    $token = Invoke-Json -WebClient $deviceClient `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/auth/device/token" `
        -Body @{ device_code = $deviceCode; code_verifier = $pkce.Verifier }
    $access = [string](Get-Field $token.Payload 'access_token')
    $refresh = [string](Get-Field $token.Payload 'refresh_token')
    $deviceObject = Get-Field $token.Payload 'device'
    $deviceID = [string](Get-Field $deviceObject 'id')
    Add-SensitiveValue $access
    Add-SensitiveValue $refresh
    Check "device token exchange: $DeviceName" (
        $token.Status -eq 200 -and
        -not [string]::IsNullOrEmpty($access) -and
        -not [string]::IsNullOrEmpty($refresh) -and
        -not [string]::IsNullOrEmpty($deviceID)
    )
    if (-not $access -or -not $refresh -or -not $deviceID) {
        throw "device token exchange did not return a usable token pair"
    }

    return [pscustomobject]@{
        Client = $deviceClient
        Access = $access
        Refresh = $refresh
        DeviceID = $deviceID
    }
}

function Wait-ScanDone {
    param([long]$LibraryID)

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $lastPhase = ''
    while ([DateTime]::UtcNow -lt $deadline) {
        $status = Invoke-Json -WebClient $web `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/scan/status"
        if ($status.Status -ne 200) {
            throw "scan status failed: $(Get-ResponseSummary $status)"
        }
        $states = @($status.Payload)
        $state = $states |
            Where-Object { [long](Get-Field $_ 'library_id') -eq $LibraryID } |
            Select-Object -First 1
        if ($null -ne $state) {
            $lastPhase = [string](Get-Field $state 'phase')
            if ($lastPhase -eq 'done') {
                return $state
            }
            if ($lastPhase -eq 'error') {
                Check 'library scan completed without a server-side error' $false
                throw 'library scan reported an error'
            }
        }
        Start-Sleep -Milliseconds 150
    }
    throw "library scan did not finish (last phase: $lastPhase)"
}

function Find-LibraryFiles {
    param([long]$LibraryID)

    $list = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base/api/items?library_id=$LibraryID&limit=100"
    if ($list.Status -ne 200) {
        throw "item listing failed: $(Get-ResponseSummary $list)"
    }

    $out = [System.Collections.Generic.List[object]]::new()
    foreach ($item in @(Get-Field $list.Payload 'items')) {
        $itemID = [long](Get-Field $item 'id')
        if ($itemID -le 0) {
            continue
        }
        $detail = Invoke-Json -WebClient $web `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/items/$itemID"
        if ($detail.Status -ne 200) {
            continue
        }
        foreach ($file in @(Get-Field $detail.Payload 'files')) {
            $fileID = [long](Get-Field $file 'id')
            $path = [string](Get-Field $file 'path')
            if ($fileID -gt 0 -and $path) {
                $out.Add([pscustomobject]@{
                    ID = $fileID
                    Path = $path
                    ItemID = $itemID
                })
            }
        }
    }
    return @($out)
}

function New-TicketBody {
    param(
        [long]$FileID,
        [string]$DeviceID,
        [string[]]$PreferredTransports
    )

    return @{
        file_id = $FileID
        device_id = $DeviceID
        client = @{
            platform = 'windows'
            version = '0.1.0'
            engine = 'mpv'
            containers = @('mp4', 'mkv')
            video_codecs = @('h264', 'hevc')
            audio_codecs = @('aac', 'flac')
            subtitle_codecs = @('text', 'ass')
            supports_range = $true
            supports_headers = $true
        }
        preferred_transports = $PreferredTransports
    }
}

function Wait-TicketReady {
    param(
        [Parameter(Mandatory = $true)]$Device,
        [Parameter(Mandatory = $true)][string]$TicketPath
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(25)
    $lastState = ''
    while ([DateTime]::UtcNow -lt $deadline) {
        $status = Invoke-Json -WebClient $Device.Client `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base$TicketPath" `
            -Bearer $Device.Access
        if ($status.Status -ne 200) {
            throw "ticket status failed: $(Get-ResponseSummary $status)"
        }
        $lastState = [string](Get-Field $status.Payload 'state')
        if ($lastState -eq 'ready' -or $lastState -eq 'consuming') {
            return $status
        }
        if ($lastState -eq 'failed' -or $lastState -eq 'unsupported' -or
            $lastState -eq 'expired' -or $lastState -eq 'revoked') {
            return $status
        }
        Start-Sleep -Milliseconds 100
    }
    throw "ticket did not become ready (last state: $lastState)"
}

function Invoke-MediaRange {
    param(
        [Parameter(Mandatory = $true)]$WebClient,
        [Parameter(Mandatory = $true)][string]$Uri,
        [string]$Range = '',
        [string]$IfRange = '',
        [switch]$Head
    )

    $method = [System.Net.Http.HttpMethod]::Get
    if ($Head) {
        $method = [System.Net.Http.HttpMethod]::Head
    }
    $request = [System.Net.Http.HttpRequestMessage]::new($method, $Uri)
    $request.Headers.ExpectContinue = $false
    if ($Range) {
        $request.Headers.TryAddWithoutValidation('Range', $Range) | Out-Null
    }
    if ($IfRange) {
        $request.Headers.TryAddWithoutValidation('If-Range', $IfRange) | Out-Null
    }
    try {
        $response = $WebClient.Client.SendAsync(
            $request,
            [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead
        ).GetAwaiter().GetResult()
        try {
            $bytes = $response.Content.ReadAsByteArrayAsync().GetAwaiter().GetResult()
            $headers = @{}
            foreach ($header in $response.Headers) {
                $headers[$header.Key] = (@($header.Value) -join ', ')
            }
            $contentHeaders = @{}
            foreach ($header in $response.Content.Headers) {
                $contentHeaders[$header.Key] = (@($header.Value) -join ', ')
            }
            $text = [System.Text.Encoding]::UTF8.GetString($bytes)
            $parsed = $null
            if (-not [string]::IsNullOrWhiteSpace($text)) {
                try {
                    $parsed = $text | ConvertFrom-Json -ErrorAction Stop
                } catch {
                    $parsed = $null
                }
            }
            return [pscustomobject]@{
                Status = [int]$response.StatusCode
                Payload = $parsed
                Body = $text
                Bytes = $bytes
                Headers = $headers
                ContentHeaders = $contentHeaders
            }
        } finally {
            $response.Dispose()
        }
    } finally {
        $request.Dispose()
    }
}

function Find-SensitiveValue {
    param(
        [AllowNull()][AllowEmptyString()][string]$Text,
        [string[]]$ExtraValues = @()
    )

    if ($null -eq $Text) {
        return $null
    }
    foreach ($value in @($script:sensitiveValues) + @($ExtraValues)) {
        if ([string]::IsNullOrWhiteSpace($value) -or $value.Length -lt 8) {
            continue
        }
        if ($Text.IndexOf($value, [System.StringComparison]::Ordinal) -ge 0) {
            return $value
        }
    }
    return $null
}

function Test-NoSensitiveValues {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [AllowNull()][AllowEmptyString()][string]$Text,
        [string[]]$ExtraValues = @()
    )

    Check $Name ($null -eq (Find-SensitiveValue -Text $Text -ExtraValues $ExtraValues))
}

function Get-MediaCapability {
    param([string]$MediaURL)

    try {
        $uri = [Uri]$MediaURL
    } catch {
        return ''
    }
    if ($null -eq $uri -or -not $uri.IsAbsoluteUri) {
        return ''
    }
    $path = [string]$uri.AbsolutePath
    if (-not $path) {
        return ''
    }
    $segments = @($path.Split('/', [System.StringSplitOptions]::RemoveEmptyEntries))
    for ($i = 0; $i -lt $segments.Count - 1; $i++) {
        if ($segments[$i] -eq 'media-sessions' -and $segments[$i + 1].StartsWith('ms_')) {
            return $segments[$i + 1]
        }
    }
    return ''
}

function Get-SubtitleCapability {
    param([string]$SubtitleURL)

    try {
        $uri = [Uri]$SubtitleURL
    } catch {
        return ''
    }
    if ($null -eq $uri -or -not $uri.IsAbsoluteUri) {
        return ''
    }
    $path = [string]$uri.AbsolutePath
    if (-not $path) {
        return ''
    }
    $segments = @($path.Split('/', [System.StringSplitOptions]::RemoveEmptyEntries))
    for ($i = 0; $i -lt $segments.Count - 1; $i++) {
        if ($segments[$i] -eq 'subtitle-sessions' -and
            $segments[$i + 1].StartsWith('ss_')) {
            return $segments[$i + 1]
        }
    }
    return ''
}

try {
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    New-Item -ItemType Directory -Force -Path $mediaDir | Out-Null
    New-Item -ItemType Directory -Force -Path $proxyMediaDir | Out-Null
    New-Item -ItemType Directory -Force -Path $toolsDir | Out-Null
    $resolvedScratch = [System.IO.Path]::GetFullPath($scratch)
    if (
        -not $resolvedScratch.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -or
        $resolvedScratch -eq $tempBase
    ) {
        throw 'scratch path escaped the temporary directory'
    }

    $script:port = Get-FreeLoopbackPort
    $base = "http://127.0.0.1:$($script:port)"
    $script:serverSecret = New-RandomBase64Url -ByteCount 32
    $sourceSecret = 'src_' + (New-RandomBase64Url -ByteCount 32)
    $directTarget = "https://media.invalid/direct/e2e.mp4?signature=$sourceSecret"
    Add-SensitiveValue $sourceSecret

    $go = Get-Command go -ErrorAction SilentlyContinue
    Check 'go toolchain available' ($null -ne $go)
    if ($null -eq $go) {
        throw 'go is required'
    }

    $ffmpegCommand = Get-Command ffmpeg -ErrorAction SilentlyContinue
    $script:hasDecodableMediaFixture = $false
    if ($null -ne $ffmpegCommand) {
        & $ffmpegCommand.Source -hide_banner -loglevel error -y `
            -f lavfi -i 'testsrc2=size=640x360:rate=30:duration=8' `
            -f lavfi -i 'sine=frequency=440:sample_rate=48000:duration=8' `
            -c:v libx264 -preset ultrafast -b:v 2M -pix_fmt yuv420p `
            -c:a aac -movflags +faststart $proxyMediaPath *> $mediaFixtureLog
        $script:hasDecodableMediaFixture =
            $LASTEXITCODE -eq 0 -and
            (Test-Path -LiteralPath $proxyMediaPath -PathType Leaf) -and
            (Get-Item -LiteralPath $proxyMediaPath).Length -gt 0
    }
    if (-not $script:hasDecodableMediaFixture) {
        $mediaBytes = [byte[]]::new(2 * 1024 * 1024)
        for ($i = 0; $i -lt $mediaBytes.Length; $i++) {
            $mediaBytes[$i] = [byte]($i % 251)
        }
        [System.IO.File]::WriteAllBytes($proxyMediaPath, $mediaBytes)
    }
    $mediaBytes = [System.IO.File]::ReadAllBytes($proxyMediaPath)
    Write-Utf8File -Path $subtitleSrtPath -Text ($subtitleSrt.TrimEnd() + "`n")
    Write-Utf8File -Path $directStrmPath -Text ($directTarget + "`n")
    Check 'local media fixture created' (
        (Test-Path -LiteralPath $proxyMediaPath -PathType Leaf) -and
        (Test-Path -LiteralPath $directStrmPath -PathType Leaf)
    )
    $subtitleFixtureBytes = [System.IO.File]::ReadAllBytes($subtitleSrtPath)
    $subtitleFixtureText = [System.Text.Encoding]::UTF8.GetString($subtitleFixtureBytes)
    Check 'deterministic UTF-8 external subtitle fixture created' (
        (Test-Path -LiteralPath $subtitleSrtPath -PathType Leaf) -and
        $subtitleFixtureBytes.Length -gt 0 -and
        -not (
            $subtitleFixtureBytes.Length -ge 3 -and
            $subtitleFixtureBytes[0] -eq 0xEF -and
            $subtitleFixtureBytes[1] -eq 0xBB -and
            $subtitleFixtureBytes[2] -eq 0xBF
        ) -and
        $subtitleFixtureText.Contains("00:00:07,000 --> 00:00:09,000") -and
        $subtitleFixtureText.Contains('Playback subtitle second line')
    )

    Initialize-FakeFFProbe
    Push-Location -LiteralPath $root
    try {
        & go build -o $exePath ./cmd/doriyamedia *> $buildLog
        $buildExit = $LASTEXITCODE
        & go build -o $fakeProbeExe $fakeProbeSource *> $fakeProbeBuildLog
        $fakeProbeBuildExit = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    Check 'server build' ($buildExit -eq 0 -and (Test-Path -LiteralPath $exePath -PathType Leaf))
    Check 'deterministic ffprobe helper build' (
        $fakeProbeBuildExit -eq 0 -and (Test-Path -LiteralPath $fakeProbeExe -PathType Leaf)
    )
    if ($buildExit -ne 0 -or $fakeProbeBuildExit -ne 0) {
        throw 'required test binary build failed'
    }

    Write-Config -TicketEnabled $true -DirectEnabled $true -ProxyEnabled $true -HLSEnabled $true
    $env:DORIYAMEDIA_LOCAL = '1'
    Check 'server start: all playback transports enabled' (Start-Server -Phase 'all-enabled')
    if ($null -eq $script:serverProcess -or $script:serverProcess.HasExited) {
        throw 'server did not start'
    }

    $web = New-WebClient
    $publicClient = New-WebClient
    $meta = Invoke-Json -WebClient $publicClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base/api/v1/meta"
    $features = Get-Field $meta.Payload 'features'
    Check 'v1 meta reports playback flags enabled' (
        $meta.Status -eq 200 -and
        (Get-Field $features 'playback_ticket_enabled') -eq $true -and
        (Get-Field $features 'direct_http_enabled') -eq $true -and
        (Get-Field $features 'server_proxy_v1_enabled') -eq $true -and
        (Get-Field $features 'server_hls_v1_enabled') -eq $true
    )

    $password = New-RandomBase64Url -ByteCount 24
    Add-SensitiveValue $password
    $setup = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/setup" `
        -Body @{ name = 'playback-e2e'; password = $password }
    $setupToken = [string](Get-Field $setup.Payload 'token')
    Add-SensitiveValue $setupToken
    $cookies = $web.Handler.CookieContainer.GetCookies([Uri]$base)
    Check 'admin setup and Web session cookie' (
        $setup.Status -eq 200 -and
        -not [string]::IsNullOrEmpty($setupToken) -and
        $cookies.Count -gt 0
    )
    if ($setup.Status -ne 200) {
        throw "admin setup failed: $(Get-ResponseSummary $setup)"
    }

    $login = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/auth/login" `
        -Body @{ name = 'playback-e2e'; password = $password }
    $loginToken = [string](Get-Field $login.Payload 'token')
    Add-SensitiveValue $loginToken
    Check 'admin Web login' (
        $login.Status -eq 200 -and -not [string]::IsNullOrEmpty($loginToken)
    )
    if ($login.Status -ne 200) {
        throw "admin login failed: $(Get-ResponseSummary $login)"
    }

    $deviceOne = New-DeviceSession -WebClient $web -DeviceName 'Playback E2E Windows' -Platform 'windows'
    $deviceTwo = New-DeviceSession -WebClient $web -DeviceName 'Playback E2E Android' -Platform 'android'
    Check 'two device sessions have distinct identities' (
        $deviceOne.DeviceID -ne $deviceTwo.DeviceID -and
        $deviceOne.Access -ne $deviceTwo.Access
    )

    $createSource = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/sources" `
        -Body @{
            name = 'playback-e2e-local'
            type = 'local'
            config = @{ root = $mediaDir }
        }
    $sourceID = [long](Get-Field $createSource.Payload 'id')
    Check 'local source created' ($createSource.Status -eq 200 -and $sourceID -gt 0)
    if ($sourceID -le 0) {
        throw "source creation failed: $(Get-ResponseSummary $createSource)"
    }

    $createLibrary = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries" `
        -Body @{
            name = 'playback-e2e-library'
            type = 'movie'
            source_id = $sourceID
            path = '/'
            providers = '[]'
        }
    $libraryID = [long](Get-Field $createLibrary.Payload 'id')
    Check 'movie library created' ($createLibrary.Status -eq 200 -and $libraryID -gt 0)
    if ($libraryID -le 0) {
        throw "library creation failed: $(Get-ResponseSummary $createLibrary)"
    }

    $scan = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries/$libraryID/scan"
    Check 'library scan started' (
        $scan.Status -eq 200 -and (Get-Field $scan.Payload 'ok') -eq $true
    )
    if ($scan.Status -ne 200) {
        throw "library scan start failed: $(Get-ResponseSummary $scan)"
    }
    $scanState = Wait-ScanDone -LibraryID $libraryID
    Check 'library scan reached the done phase' (
        (Get-Field $scanState 'phase') -eq 'done' -and
        (Get-Field $scanState 'running') -eq $false
    )

    $libraryFiles = @(Find-LibraryFiles -LibraryID $libraryID)
    $proxyFile = $libraryFiles |
        Where-Object { -not $_.Path.StartsWith('http://') -and -not $_.Path.StartsWith('https://') } |
        Select-Object -First 1
    $directFile = $libraryFiles |
        Where-Object { $_.Path.StartsWith('https://') } |
        Select-Object -First 1
    Check 'library exposes a local proxy file' (
        $null -ne $proxyFile -and [long]$proxyFile.ID -gt 0
    )
    Check 'library exposes an HTTPS .strm direct file' (
        $null -ne $directFile -and
        [long]$directFile.ID -gt 0 -and
        $directFile.Path -eq $directTarget
    )
    if ($null -eq $proxyFile -or $null -eq $directFile) {
        throw 'library fixtures were not indexed as expected'
    }

    if ($script:hasDecodableMediaFixture) {
        Write-Output '[INFO] Using deterministic probe metadata and a synthetic decodable MP4 for local HLS byte/decode checks.'
    } else {
        Write-Output '[INFO] Using deterministic probe metadata; ffmpeg is unavailable, so real HLS bytes/decode will be skipped.'
    }

    $ticketCreate = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceOne.Access `
        -Body (New-TicketBody -FileID $proxyFile.ID -DeviceID $deviceOne.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    $proxyTicketID = [string](Get-Field $ticketCreate.Payload 'ticket_id')
    $proxyTicketPath = "/api/v1/playback/tickets/$([uri]::EscapeDataString($proxyTicketID))"
    $location = Get-ResponseHeader $ticketCreate 'Location'
    Check 'ticket create returns 202 probing' (
        $ticketCreate.Status -eq 202 -and
        (Get-Field $ticketCreate.Payload 'state') -eq 'probing' -and
        -not [string]::IsNullOrEmpty($proxyTicketID) -and
        $location -eq $proxyTicketPath -and
        (Get-ResponseHeader $ticketCreate 'Cache-Control') -eq 'no-store'
    )
    if ($ticketCreate.Status -ne 202 -or -not $proxyTicketID) {
        throw "ticket creation failed: $(Get-ResponseSummary $ticketCreate)"
    }

    $ready = Wait-TicketReady -Device $deviceOne -TicketPath $proxyTicketPath
    Check 'probing ticket transitions to ready' (
        $ready.Status -eq 200 -and (Get-Field $ready.Payload 'state') -eq 'ready'
    )
    Check 'ready status omits one-time transport' ($null -eq (Get-Field $ready.Payload 'transport'))
    Test-NoSensitiveValues -Name 'ready status contains no credentials or source signature' `
        -Text $ready.Body -ExtraValues @($sourceSecret)

    $proxyRefresh = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$proxyTicketPath/refresh" `
        -Bearer $deviceOne.Access
    $proxyTransport = Get-Field $proxyRefresh.Payload 'transport'
    $proxyStreams = @(Get-Field $proxyTransport 'streams')
    $proxyStream = $proxyStreams | Select-Object -First 1
    $proxyMediaURL = [string](Get-Field $proxyStream 'url')
    $proxyCapability = Get-MediaCapability $proxyMediaURL
    $proxyRemaining = @(Get-Field $proxyRefresh.Payload 'remaining_transports')
    $proxySubtitles = @(Get-Field $proxyTransport 'subtitles')
    $proxyTextSubtitle = $proxySubtitles |
        Where-Object { (Get-Field $_ 'format') -eq 'vtt' } |
        Select-Object -First 1
    $proxyPgsSubtitle = $proxySubtitles |
        Where-Object { (Get-Field $_ 'format') -eq 'pgs' } |
        Select-Object -First 1
    $proxySubtitleURL = [string](Get-Field $proxyTextSubtitle 'url')
    $subtitleCapability = Get-SubtitleCapability $proxySubtitleURL
    Check 'refresh claims server-proxy transport' (
        $proxyRefresh.Status -eq 200 -and
        (Get-Field $proxyRefresh.Payload 'state') -eq 'consuming' -and
        (Get-Field $proxyTransport 'kind') -eq 'server-proxy' -and
        $null -ne (Get-Field $proxyTransport 'media_session') -and
        $proxyStreams.Count -eq 1 -and
        (Get-Field $proxyStream 'role') -eq 'muxed' -and
        (Get-Field $proxyStream 'range') -eq $true -and
        $proxyRemaining -contains 'server-hls'
    )
    Check 'proxy issuance is capability-only and query-free' (
        $proxyCapability.StartsWith('ms_') -and
        $proxyMediaURL -eq "https://media.example/api/v1/playback/media-sessions/$proxyCapability/stream" -and
        ([Uri]$proxyMediaURL).Query -eq ''
    )
    Test-NoSensitiveValues -Name 'proxy issuance does not expose source URL or source secret' `
        -Text $proxyRefresh.Body -ExtraValues @($sourceSecret)
    if ($proxyRefresh.Status -ne 200) {
        throw "proxy refresh failed: $(Get-ResponseSummary $proxyRefresh)"
    }
    if (-not $proxyCapability) {
        throw 'proxy refresh did not return a media capability'
    }
    Add-SensitiveValue $proxyCapability
    Add-SensitiveValue $proxySubtitleURL
    Add-SensitiveValue $subtitleCapability
    Check 'server-proxy refresh issues a text subtitle capability' (
        $proxySubtitles.Count -ge 1 -and
        $null -ne $proxyTextSubtitle -and
        $subtitleCapability.StartsWith('ss_') -and
        ([Uri]$proxySubtitleURL).Query -eq '' -and
        $proxySubtitleURL.Contains('/api/v1/playback/subtitle-sessions/ss_') -and
        $proxySubtitleURL.EndsWith('/subtitle.vtt', [System.StringComparison]::Ordinal)
    )
    Check 'PGS subtitle is described without a URL' (
        $null -ne $proxyPgsSubtitle -and
        [string]::IsNullOrEmpty([string](Get-Field $proxyPgsSubtitle 'url'))
    )
    if (-not $subtitleCapability) {
        throw 'proxy refresh did not return a text subtitle capability'
    }
    $localSubtitleURL = "$base$(([Uri]$proxySubtitleURL).PathAndQuery)"

    $consuming = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base$proxyTicketPath" `
        -Bearer $deviceOne.Access
    Check 'consuming status remains safe' (
        $consuming.Status -eq 200 -and
        (Get-Field $consuming.Payload 'state') -eq 'consuming' -and
        $null -eq (Get-Field $consuming.Payload 'transport') -and
        $consuming.Body.IndexOf('media-sessions/ms_', [System.StringComparison]::Ordinal) -lt 0
    )
    Test-NoSensitiveValues -Name 'consuming status contains no source secret or capability' `
        -Text $consuming.Body -ExtraValues @($sourceSecret, $proxyCapability)
    Check 'consuming status hides subtitles, URLs, and capability prefixes' (
        $null -eq (Get-Field $consuming.Payload 'transport') -and
        $null -eq (Get-Field $consuming.Payload 'subtitles') -and
        $consuming.Body.IndexOf('ss_', [System.StringComparison]::Ordinal) -lt 0 -and
        $consuming.Body.IndexOf('ms_', [System.StringComparison]::Ordinal) -lt 0 -and
        $consuming.Body -notmatch '(?i)"url"\s*:'
    )

    $hitCount = 0
    foreach ($header in $consuming.Headers.Keys) {
        if ($header -eq 'Cache-Control' -and $consuming.Headers[$header] -match 'no-store') {
            $hitCount++
        }
    }
    Check 'ticket status is not cacheable' ($hitCount -gt 0)

    $bodyRejected = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$proxyTicketPath/refresh" `
        -Bearer $deviceOne.Access `
        -Body @{ unexpected = $true }
    Check 'ticket refresh rejects a request body' (
        $bodyRejected.Status -eq 400 -and
        (Get-ErrorCode $bodyRejected.Payload) -eq 'invalid_request'
    )

    $crossDeviceStatus = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base$proxyTicketPath" `
        -Bearer $deviceTwo.Access
    Check 'cross-device ticket status is hidden as 404' (
        $crossDeviceStatus.Status -eq 404 -and
        (Get-ErrorCode $crossDeviceStatus.Payload) -eq 'ticket_not_found'
    )
    $crossDeviceRefresh = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$proxyTicketPath/refresh" `
        -Bearer $deviceTwo.Access
    Check 'cross-device ticket refresh is hidden as 404' (
        $crossDeviceRefresh.Status -eq 404 -and
        (Get-ErrorCode $crossDeviceRefresh.Payload) -eq 'ticket_not_found'
    )

    $mediaClient = New-WebClient
    $localMediaURL = "$base$(([Uri]$proxyMediaURL).PathAndQuery)"
    $rangeStart = 4096
    $rangeLength = 64
    $rangeEnd = $rangeStart + $rangeLength - 1
    $range = "bytes=$rangeStart-$rangeEnd"
    $expected = [byte[]]::new($rangeLength)
    [Array]::Copy($mediaBytes, $rangeStart, $expected, 0, $rangeLength)
    $rangeResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range $range
    $actualMatches = (
        $rangeResponse.Bytes.Length -eq $expected.Length -and
        [Convert]::ToBase64String($rangeResponse.Bytes) -eq [Convert]::ToBase64String($expected)
    )
    $script:mediaRequestMade = $true
    Check 'media capability GET supports byte Range' (
        $rangeResponse.Status -eq 206 -and
        $actualMatches -and
        (Get-ResponseHeader $rangeResponse 'Content-Range') -eq "bytes $rangeStart-$rangeEnd/$($mediaBytes.Length)"
    )
    Check 'media response is private and does not expose source headers' (
        (Get-ResponseHeader $rangeResponse 'Cache-Control') -match 'no-store' -and
        (Get-ResponseHeader $rangeResponse 'Location') -eq '' -and
        (Get-ResponseHeader $rangeResponse 'Authorization') -eq ''
    )
    $rangeHeaderText = ($rangeResponse.Headers.GetEnumerator() | ForEach-Object {
        "$($_.Key): $($_.Value)"
    }) -join "`n"
    Test-NoSensitiveValues -Name 'media Range body and headers contain no source secret' `
        -Text ($rangeResponse.Body + "`n" + $rangeHeaderText) -ExtraValues @($sourceSecret)

    $headResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Head
    $headLength = [long]-1
    $headLengthRaw = Get-ResponseHeader $headResponse 'Content-Length'
    if (-not [long]::TryParse($headLengthRaw, [ref]$headLength)) {
        $headLength = -1
    }
    Check 'media capability HEAD is bodyless with the exact full length' (
        $headResponse.Status -eq 200 -and
        $headResponse.Bytes.Length -eq 0 -and
        $headLength -eq $mediaBytes.Length -and
        (Get-ResponseHeader $headResponse 'Accept-Ranges') -eq 'bytes'
    )
    $mediaValidator = Get-ResponseHeader $headResponse 'ETag'
    Check 'media capability HEAD exposes a strong ETag validator' (
        $mediaValidator.StartsWith('"', [System.StringComparison]::Ordinal) -and
        -not $mediaValidator.StartsWith('W/', [System.StringComparison]::Ordinal)
    )

    $firstByteResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-0'
    $firstByteLength = [long]-1
    $firstByteLengthRaw = Get-ResponseHeader $firstByteResponse 'Content-Length'
    if (-not [long]::TryParse($firstByteLengthRaw, [ref]$firstByteLength)) {
        $firstByteLength = -1
    }
    Check 'media capability GET bytes=0-0 returns exactly the first byte' (
        $firstByteResponse.Status -eq 206 -and
        $firstByteResponse.Bytes.Length -eq 1 -and
        $firstByteResponse.Bytes[0] -eq $mediaBytes[0] -and
        $firstByteLength -eq 1 -and
        (Get-ResponseHeader $firstByteResponse 'Content-Range') -eq
            "bytes 0-0/$($mediaBytes.Length)"
    )

    $unsatisfiableRange = "bytes=$($mediaBytes.Length)-"
    $unsatisfiableResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range $unsatisfiableRange
    Check 'media capability rejects an unsatisfiable range with exact Content-Range' (
        $unsatisfiableResponse.Status -eq 416 -and
        (Get-ResponseHeader $unsatisfiableResponse 'Content-Range') -eq
            "bytes */$($mediaBytes.Length)"
    )

    $ifRangeMatch = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-0' -IfRange $mediaValidator
    Check 'media capability honors a matching If-Range validator' (
        $ifRangeMatch.Status -eq 206 -and
        $ifRangeMatch.Bytes.Length -eq 1 -and
        (Get-ResponseHeader $ifRangeMatch 'Content-Range') -eq
            "bytes 0-0/$($mediaBytes.Length)" -and
        (Get-ResponseHeader $ifRangeMatch 'ETag') -eq $mediaValidator
    )

    $ifRangeMismatch = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-0' -IfRange '"playback-e2e-stale"'
    $ifRangeMismatchLength = [long]-1
    $ifRangeMismatchLengthRaw = Get-ResponseHeader $ifRangeMismatch 'Content-Length'
    if (-not [long]::TryParse($ifRangeMismatchLengthRaw, [ref]$ifRangeMismatchLength)) {
        $ifRangeMismatchLength = -1
    }
    Check 'media capability ignores a stale If-Range and returns the full representation' (
        $ifRangeMismatch.Status -eq 200 -and
        $ifRangeMismatch.Bytes.Length -eq $mediaBytes.Length -and
        $ifRangeMismatchLength -eq $mediaBytes.Length -and
        (Get-ResponseHeader $ifRangeMismatch 'Content-Range') -eq '' -and
        (Get-ResponseHeader $ifRangeMismatch 'ETag') -eq $mediaValidator
    )

    $forgedQuery = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "${localMediaURL}?signature=forged"
    Check 'media capability rejects query parameters' (
        $forgedQuery.Status -eq 400 -and
        (Get-ErrorCode $forgedQuery.Payload) -eq 'invalid_request'
    )

    $subtitleGet = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $localSubtitleURL
    $script:subtitleRequestMade = $true
    $subtitleGetAgain = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $localSubtitleURL
    $subtitleBody = [string]$subtitleGet.Body
    $subtitleByteLength = [System.Text.Encoding]::UTF8.GetByteCount($subtitleBody)
    Check 'text subtitle GET returns a stable WebVTT timeline' (
        $subtitleGet.Status -eq 200 -and
        (Get-ResponseHeader $subtitleGet 'Content-Type').ToLowerInvariant() -eq
            'text/vtt; charset=utf-8' -and
        (Get-ResponseHeader $subtitleGet 'Cache-Control') -match 'no-store' -and
        (Get-ResponseHeader $subtitleGet 'X-Content-Type-Options') -eq 'nosniff' -and
        $subtitleBody.StartsWith('WEBVTT', [System.StringComparison]::Ordinal) -and
        $subtitleBody.Contains("00:00:07.000 --> 00:00:09.000") -and
        $subtitleBody.Contains("00:00:12.500 --> 00:00:14.000") -and
        $subtitleBody.Contains('Playback subtitle first line') -and
        $subtitleGetAgain.Status -eq 200 -and
        $subtitleGetAgain.Body -ceq $subtitleGet.Body
    )
    Check 'text subtitle body omits local source paths' (
        $subtitleBody.IndexOf($mediaDir, [System.StringComparison]::OrdinalIgnoreCase) -lt 0 -and
        $subtitleBody.IndexOf($subtitleSrtPath, [System.StringComparison]::OrdinalIgnoreCase) -lt 0
    )
    Test-NoSensitiveValues -Name 'text subtitle body contains no credentials or capability' `
        -Text $subtitleBody -ExtraValues @($sourceSecret)

    $subtitleHead = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Head) `
        -Uri $localSubtitleURL
    $subtitleHeadLength = [long]0
    $subtitleHeadLengthRaw = Get-ResponseHeader $subtitleHead 'Content-Length'
    if ( -not [long]::TryParse($subtitleHeadLengthRaw, [ref]$subtitleHeadLength)) {
        $subtitleHeadLength = -1
    }
    Check 'text subtitle HEAD returns VTT metadata without a body' (
        $subtitleHead.Status -eq 200 -and
        $subtitleHead.Body.Length -eq 0 -and
        (Get-ResponseHeader $subtitleHead 'Content-Type').ToLowerInvariant() -eq
            'text/vtt; charset=utf-8' -and
        $subtitleHeadLength -eq $subtitleByteLength
    )

    $subtitleQueryTamper = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "${localSubtitleURL}?start=99&capability=forged"
    Check 'subtitle capability rejects query parameters' (
        $subtitleQueryTamper.Status -eq 400 -and
        (Get-ErrorCode $subtitleQueryTamper.Payload) -eq 'invalid_request'
    )
    Test-NoSensitiveValues -Name 'subtitle query rejection leaks no capability' `
        -Text $subtitleQueryTamper.Body -ExtraValues @($sourceSecret)

    $replacement = 'A'
    if ($subtitleCapability.EndsWith('A', [System.StringComparison]::Ordinal)) {
        $replacement = 'B'
    }
    $wrongSubtitleCapability =
        $subtitleCapability.Substring(0, $subtitleCapability.Length - 1) + $replacement
    $wrongSubtitleURL = "$base/api/v1/playback/subtitle-sessions/" +
        "$wrongSubtitleCapability/subtitle.vtt"
    $wrongSubtitle = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $wrongSubtitleURL
    Check 'unknown subtitle capability is rejected as not found' (
        $wrongSubtitle.Status -eq 404 -and
        (Get-ErrorCode $wrongSubtitle.Payload) -eq 'media_session_not_found'
    )

    $subtitleAsMedia = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base/api/v1/playback/media-sessions/$subtitleCapability/stream"
    $mediaAsSubtitle = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base/api/v1/playback/subtitle-sessions/$proxyCapability/subtitle.vtt"
    Check 'capability domains are not interchangeable across paths' (
        $subtitleAsMedia.Status -eq 404 -and
        (Get-ErrorCode $subtitleAsMedia.Payload) -eq 'media_session_not_found' -and
        $mediaAsSubtitle.Status -eq 404 -and
        (Get-ErrorCode $mediaAsSubtitle.Payload) -eq 'media_session_not_found'
    )

    $hlsRefresh = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$proxyTicketPath/refresh" `
        -Bearer $deviceOne.Access
    $hlsTransport = Get-Field $hlsRefresh.Payload 'transport'
    $hlsStreams = @(Get-Field $hlsTransport 'streams')
    $hlsStream = $hlsStreams | Select-Object -First 1
    $hlsMediaURL = [string](Get-Field $hlsStream 'url')
    $hlsCapability = Get-MediaCapability $hlsMediaURL
    $hlsLocalURL = "$base/api/v1/playback/media-sessions/$hlsCapability/index.m3u8"
    Check 'second refresh falls back to server-hls ticket state' (
        $hlsRefresh.Status -eq 200 -and
        (Get-Field $hlsRefresh.Payload 'state') -eq 'consuming' -and
        (Get-Field $hlsTransport 'kind') -eq 'server-hls' -and
        $hlsMediaURL.EndsWith('/index.m3u8', [System.StringComparison]::Ordinal) -and
        @(Get-Field $hlsRefresh.Payload 'remaining_transports').Count -eq 0
    )
    Test-NoSensitiveValues -Name 'HLS issuance does not expose source URL or source secret' `
        -Text $hlsRefresh.Body -ExtraValues @($sourceSecret)
    Add-SensitiveValue $hlsCapability

    $hlsHead = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Head) `
        -Uri $hlsLocalURL
    Check 'server-HLS playlist rejects HEAD without a body' (
        $hlsHead.Status -eq 405 -and
        $hlsHead.Body.Length -eq 0 -and
        (Get-ResponseHeader $hlsHead 'Allow') -eq 'GET'
    )

    $hlsPlaylist = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $hlsLocalURL
    $hlsSegmentName = ''
    foreach ($line in ($hlsPlaylist.Body -split "`r?`n")) {
        $candidate = $line.Trim()
        if (
            $candidate -and
            -not $candidate.StartsWith('#') -and
            $candidate.EndsWith('.ts', [System.StringComparison]::Ordinal)
        ) {
            $hlsSegmentName = $candidate
            break
        }
    }
    Check 'server-HLS playlist is capability-addressed and lists a segment' (
        $hlsPlaylist.Status -eq 200 -and
        (Get-ResponseHeader $hlsPlaylist 'Content-Type') -eq
            'application/vnd.apple.mpegurl' -and
        $hlsPlaylist.Body.StartsWith('#EXTM3U', [System.StringComparison]::Ordinal) -and
        -not [string]::IsNullOrWhiteSpace($hlsSegmentName) -and
        $hlsPlaylist.Body.IndexOf($sourceSecret, [System.StringComparison]::Ordinal) -lt 0
    )

    $hlsSegmentURL = ''
    if ($hlsSegmentName) {
        $hlsSegmentURL = ([System.Uri]::new(
            [System.Uri]$hlsLocalURL, $hlsSegmentName)).AbsoluteUri
    }
    $hlsSegmentHead = $null
    if ($hlsSegmentURL) {
        $hlsSegmentHead = Invoke-Json -WebClient $mediaClient `
            -Method ([System.Net.Http.HttpMethod]::Head) `
            -Uri $hlsSegmentURL
    }
    Check 'server-HLS segment rejects HEAD without a body' (
        $null -ne $hlsSegmentHead -and
        $hlsSegmentHead.Status -eq 405 -and
        $hlsSegmentHead.Body.Length -eq 0 -and
        (Get-ResponseHeader $hlsSegmentHead 'Allow') -eq 'GET'
    )

    if ($script:hasDecodableMediaFixture -and $hlsSegmentURL) {
        $hlsSegment = Invoke-Bytes -WebClient $mediaClient `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri $hlsSegmentURL
        Check 'server-HLS segment returns non-empty MPEG-TS bytes' (
            $hlsSegment.Status -eq 200 -and
            [string]$hlsSegment.ContentHeaders['Content-Type'] -eq
                'video/mp2t' -and
            $hlsSegment.Bytes.Length -gt 188 -and
            ($hlsSegment.Bytes[0] -eq 0x47)
        )
        [System.IO.File]::WriteAllBytes($hlsSegmentPath, $hlsSegment.Bytes)
        & $ffmpegCommand.Source -hide_banner -loglevel error `
            -i $hlsSegmentPath -f null - *> $hlsDecodeLog
        $decodeExit = $LASTEXITCODE
        Check 'server-HLS segment decodes with ffmpeg' ($decodeExit -eq 0)
    } else {
        Skip 'real HLS segment bytes and decode (ffmpeg or decodable fixture unavailable)'
    }

    $exhausted = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$proxyTicketPath/refresh" `
        -Bearer $deviceOne.Access
    $exhaustedDetails = Get-ErrorDetails $exhausted.Payload
    Check 'refresh after all candidates reports exhausted terminal error' (
        $exhausted.Status -eq 410 -and
        (Get-ErrorCode $exhausted.Payload) -eq 'ticket_expired' -and
        (Get-Field $exhaustedDetails 'state') -eq 'exhausted' -and
        (Get-Field $exhaustedDetails 'action') -eq 'request_new_ticket' -and
        $null -eq (Get-Field $exhausted.Payload 'transport') -and
        $null -eq (Get-Field $exhausted.Payload 'remaining_transports')
    )

    $directCreate = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceOne.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceOne.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    $directTicketID = [string](Get-Field $directCreate.Payload 'ticket_id')
    $directTicketPath = "/api/v1/playback/tickets/$([uri]::EscapeDataString($directTicketID))"
    Check 'direct ticket enters probing state' (
        $directCreate.Status -eq 202 -and
        (Get-Field $directCreate.Payload 'state') -eq 'probing' -and
        -not [string]::IsNullOrEmpty($directTicketID)
    )
    if ($directCreate.Status -ne 202 -or -not $directTicketID) {
        throw "direct ticket creation failed: $(Get-ResponseSummary $directCreate)"
    }
    $directReady = Wait-TicketReady -Device $deviceOne -TicketPath $directTicketPath
    if ((Get-Field $directReady.Payload 'state') -ne 'ready') {
        throw "direct ticket did not become ready: $(Get-ResponseSummary $directReady)"
    }

    $directRefresh = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$directTicketPath/refresh" `
        -Bearer $deviceOne.Access
    $directTransport = Get-Field $directRefresh.Payload 'transport'
    $directStreams = @(Get-Field $directTransport 'streams')
    $directStream = $directStreams | Select-Object -First 1
    $directRemaining = @(Get-Field $directRefresh.Payload 'remaining_transports')
    Check 'direct-http transport is issued for an HTTPS source' (
        $directRefresh.Status -eq 200 -and
        (Get-Field $directRefresh.Payload 'state') -eq 'consuming' -and
        (Get-Field $directTransport 'kind') -eq 'direct-http' -and
        $null -eq (Get-Field $directTransport 'media_session') -and
        $directStreams.Count -eq 1 -and
        (Get-Field $directStream 'url') -eq $directTarget -and
        (Get-Field $directStream 'range') -eq $true -and
        $directRemaining -contains 'server-proxy' -and
        $directRemaining -contains 'server-hls'
    )
    $directStatus = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base$directTicketPath" `
        -Bearer $deviceOne.Access
    Check 'direct ticket status never repeats the source URL' (
        $directStatus.Status -eq 200 -and
        $null -eq (Get-Field $directStatus.Payload 'transport') -and
        $directStatus.Body.IndexOf($sourceSecret, [System.StringComparison]::Ordinal) -lt 0
    )

    $deleteDeviceOne = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Delete) `
        -Uri "$base/api/v1/devices/$([uri]::EscapeDataString($deviceOne.DeviceID))" `
        -Bearer $deviceTwo.Access
    Check 'second device can revoke the first device' (
        $deleteDeviceOne.Status -eq 200 -and
        (Get-Field $deleteDeviceOne.Payload 'device_id') -eq $deviceOne.DeviceID -and
        (Get-Field $deleteDeviceOne.Payload 'state') -eq 'revoked'
    )
    $revokedMedia = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-15'
    Check 'device revocation invalidates outstanding media capability' (
        $revokedMedia.Status -eq 403 -and
        (Get-ErrorCode $revokedMedia.Payload) -eq 'forbidden'
    )
    $revokedSubtitle = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $localSubtitleURL
    Check 'device revocation invalidates outstanding subtitle capability' (
        $revokedSubtitle.Status -eq 403 -and
        (Get-ErrorCode $revokedSubtitle.Payload) -eq 'transport_disabled'
    )
    Test-NoSensitiveValues -Name 'revoked subtitle response leaks no credential' `
        -Text $revokedSubtitle.Body -ExtraValues @($sourceSecret)

    Stop-Server
    Write-Config -TicketEnabled $true -DirectEnabled $false -ProxyEnabled $true -HLSEnabled $true
    Check 'server restart: direct disabled, proxy enabled' (
        Start-Server -Phase 'direct-disabled'
    )
    if ($null -eq $script:serverProcess -or $script:serverProcess.HasExited) {
        throw 'server did not restart with direct disabled'
    }

    $fallbackCreate = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceTwo.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceTwo.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    $fallbackTransport = Get-Field $fallbackCreate.Payload 'transport'
    $fallbackStreams = @(Get-Field $fallbackTransport 'streams')
    $fallbackStream = $fallbackStreams | Select-Object -First 1
    $fallbackURL = [string](Get-Field $fallbackStream 'url')
    $fallbackCapability = Get-MediaCapability $fallbackURL
    $fallbackRemaining = @(Get-Field $fallbackCreate.Payload 'remaining_transports')
    Check 'disabling direct-http falls back to server-proxy' (
        $fallbackCreate.Status -eq 201 -and
        (Get-Field $fallbackCreate.Payload 'state') -eq 'consuming' -and
        (Get-Field $fallbackTransport 'kind') -eq 'server-proxy' -and
        $fallbackCapability.StartsWith('ms_') -and
        $fallbackRemaining -contains 'server-hls'
    )
    Test-NoSensitiveValues -Name 'fallback issuance hides the stale direct source URL' `
        -Text $fallbackCreate.Body -ExtraValues @($sourceSecret)
    Add-SensitiveValue $fallbackCapability

    Stop-Server
    Write-Config -TicketEnabled $true -DirectEnabled $false -ProxyEnabled $false -HLSEnabled $false
    Check 'server restart: all playback transports disabled' (
        Start-Server -Phase 'all-transports-disabled'
    )
    if ($null -eq $script:serverProcess -or $script:serverProcess.HasExited) {
        throw 'server did not restart with all transports disabled'
    }

    $unsupported = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceTwo.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceTwo.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    Check 'ticket fails closed when every transport flag is off' (
        $unsupported.Status -eq 201 -and
        (Get-Field $unsupported.Payload 'state') -eq 'unsupported' -and
        $null -eq (Get-Field $unsupported.Payload 'transport') -and
        @(Get-Field $unsupported.Payload 'remaining_transports').Count -eq 0
    )

    Stop-Server
    Write-Config -TicketEnabled $false -DirectEnabled $false -ProxyEnabled $false -HLSEnabled $false
    Check 'server restart: playback ticket feature disabled' (
        Start-Server -Phase 'ticket-disabled'
    )
    if ($null -eq $script:serverProcess -or $script:serverProcess.HasExited) {
        throw 'server did not restart with playback tickets disabled'
    }

    $featureDisabled = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceTwo.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceTwo.DeviceID `
            -PreferredTransports @('server-proxy'))
    Check 'playback ticket feature gate fails closed' (
        $featureDisabled.Status -eq 404 -and
        (Get-ErrorCode $featureDisabled.Payload) -eq 'feature_disabled'
    )
    Test-NoSensitiveValues -Name 'feature-disabled response leaks nothing sensitive' `
        -Text $featureDisabled.Body -ExtraValues @($sourceSecret)

} catch {
    $safeMessage = ConvertTo-SafeMessage $_.Exception.Message
    $safeLine = [int]$_.InvocationInfo.ScriptLineNumber
    Write-Output "[FAIL] playback e2e completed without an unexpected error (line $safeLine): $safeMessage"
    $script:failCount++
} finally {
    Stop-Server
    foreach ($entry in $script:clients) {
        try {
            $entry.Client.Dispose()
            $entry.Handler.Dispose()
        } catch {
        }
    }

    $logs = @($script:serverLogs | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf })
    Check 'server logs were captured' ($logs.Count -gt 0)
    if ($logs.Count -gt 0) {
        $logText = ($logs | ForEach-Object {
            [System.IO.File]::ReadAllText($_)
        }) -join "`n"
        Test-NoSensitiveValues -Name 'server logs contain no tokens, source signature, or capability' `
            -Text $logText -ExtraValues @($sourceSecret, $proxyCapability, $hlsCapability, $fallbackCapability)
        Check 'server logs omit the source host and local absolute media path' (
            $logText.IndexOf('media.invalid', [System.StringComparison]::OrdinalIgnoreCase) -lt 0 -and
            $logText.IndexOf($proxyMediaPath, [System.StringComparison]::OrdinalIgnoreCase) -lt 0
        )
        if ($script:mediaRequestMade) {
            Check 'media-session capability is redacted in request logs' (
                $logText.Contains('ms_redacted/stream')
            )
        }
        if ($script:subtitleRequestMade) {
            Check 'subtitle capability is redacted in request logs' (
                $logText.Contains('ss_redacted/subtitle.vtt')
            )
        }
    }

    if ($null -eq $oldLocalEnv) {
        Remove-Item Env:DORIYAMEDIA_LOCAL -ErrorAction SilentlyContinue
    } else {
        $env:DORIYAMEDIA_LOCAL = $oldLocalEnv
    }

    $resolvedScratch = [System.IO.Path]::GetFullPath($scratch)
    if (
        $resolvedScratch.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase) -and
        $resolvedScratch -ne $tempBase
    ) {
        for ($attempt = 0; $attempt -lt 3 -and (Test-Path -LiteralPath $resolvedScratch); $attempt++) {
            Remove-Item -LiteralPath $resolvedScratch -Recurse -Force -ErrorAction SilentlyContinue
            if (Test-Path -LiteralPath $resolvedScratch) {
                Start-Sleep -Milliseconds 200
            }
        }
        Check 'temporary test directory cleaned up' (
            -not (Test-Path -LiteralPath $resolvedScratch)
        )
    } else {
        Check 'temporary test directory stayed inside the system temp root' $false
    }

    Write-Output ''
    Write-Output ("SUMMARY passed={0} failed={1} skipped={2}" -f `
        $script:passCount, $script:failCount, $script:skipCount)
}

if ($script:failCount -gt 0) {
    exit 1
}
exit 0
