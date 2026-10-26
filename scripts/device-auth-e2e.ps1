[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'

Add-Type -AssemblyName System.Net.Http

$root = Split-Path -Parent $PSScriptRoot
$tempBase = [System.IO.Path]::GetFullPath([System.IO.Path]::GetTempPath())
$scratch = Join-Path $tempBase ("doriyamedia-device-auth-e2e-" + [guid]::NewGuid().ToString('N'))
$dataDir = Join-Path $scratch 'data'
$configPath = Join-Path $scratch 'config.json'
$exePath = Join-Path $scratch 'doriyamedia-e2e.exe'
$buildLog = Join-Path $scratch 'build.log'
$serverOut1 = Join-Path $scratch 'server-1.out.log'
$serverErr1 = Join-Path $scratch 'server-1.err.log'
$serverOut2 = Join-Path $scratch 'server-2.out.log'
$serverErr2 = Join-Path $scratch 'server-2.err.log'
$failures = [System.Collections.Generic.List[string]]::new()
$serverProcess = $null
$oldLocalEnv = $env:DORIYAMEDIA_LOCAL

function Check {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][bool]$Ok
    )

    if ($Ok) {
        Write-Output "[PASS] $Name"
        return
    }
    Write-Output "[FAIL] $Name"
    $failures.Add($Name)
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

function New-WebClient {
    $handler = [System.Net.Http.HttpClientHandler]::new()
    $handler.UseCookies = $true
    $handler.CookieContainer = [System.Net.CookieContainer]::new()
    $handler.UseProxy = $false
    $client = [System.Net.Http.HttpClient]::new($handler)
    $client.Timeout = [TimeSpan]::FromSeconds(15)
    return [pscustomobject]@{ Client = $client; Handler = $handler }
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
            $json = $Body | ConvertTo-Json -Depth 10 -Compress
            $content = [System.Net.Http.StringContent]::new(
                $json,
                [System.Text.Encoding]::UTF8,
                'application/json'
            )
            $request.Content = $content
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
            return [pscustomobject]@{
                Status = $status
                Payload = $payload
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

function Write-Config {
    param([bool]$DeviceAuthEnabled)

    $existing = $null
    if (Test-Path -LiteralPath $configPath -PathType Leaf) {
        $existing = Get-Content -LiteralPath $configPath -Raw | ConvertFrom-Json
    }

    if ($null -eq $existing) {
        $config = [ordered]@{
            port = $script:port
            data_dir = $dataDir
            secret = (New-RandomBase64Url -ByteCount 32)
            users = [ordered]@{
                registration = 'closed'
                min_password = 4
                base_url = $script:base
            }
            client = [ordered]@{
                v1_enabled = $true
                device_auth_enabled = $DeviceAuthEnabled
            }
        }
    } else {
        $existing.port = $script:port
        $existing.data_dir = $dataDir
        if ($null -eq $existing.client) {
            $existing | Add-Member -NotePropertyName client -NotePropertyValue (
                [pscustomobject]@{ v1_enabled = $true; device_auth_enabled = $DeviceAuthEnabled }
            )
        } else {
            $existing.client.v1_enabled = $true
            $existing.client.device_auth_enabled = $DeviceAuthEnabled
        }
        $config = $existing
    }

    $json = $config | ConvertTo-Json -Depth 12
    [System.IO.File]::WriteAllText($configPath, $json, [System.Text.UTF8Encoding]::new($false))
}

function Start-Server {
    param([string]$StdOut, [string]$StdErr)

    $script:serverProcess = Start-Process -FilePath $exePath `
        -ArgumentList @('-config', $configPath) `
        -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $StdOut -RedirectStandardError $StdErr

    $probe = New-WebClient
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(20)
        while ([DateTime]::UtcNow -lt $deadline) {
            if ($serverProcess.HasExited) {
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
}

try {
    New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
    $resolvedScratch = [System.IO.Path]::GetFullPath($scratch)
    if (-not $resolvedScratch.StartsWith($tempBase, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'scratch path escaped the temporary directory'
    }

    $port = Get-FreeLoopbackPort
    $base = "http://127.0.0.1:$port"

    $go = Get-Command go -ErrorAction SilentlyContinue
    Check 'go toolchain available' ($null -ne $go)
    if ($null -eq $go) {
        throw 'go is required'
    }

    Push-Location -LiteralPath $root
    try {
        & go build -o $exePath ./cmd/doriyamedia *> $buildLog
        $buildExit = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    Check 'server build' ($buildExit -eq 0 -and (Test-Path -LiteralPath $exePath -PathType Leaf))
    if ($buildExit -ne 0) {
        throw 'server build failed'
    }

    Write-Config -DeviceAuthEnabled $true
    $env:DORIYAMEDIA_LOCAL = '1'
    Check 'server start with device auth enabled' (Start-Server -StdOut $serverOut1 -StdErr $serverErr1)
    if ($null -eq $serverProcess -or $serverProcess.HasExited) {
        throw 'server did not start'
    }

    $web = New-WebClient
    $deviceOne = New-WebClient
    $deviceTwo = New-WebClient
    try {
        $password = New-RandomBase64Url -ByteCount 24
        $setup = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/setup" -Body @{ name = 'e2e'; password = $password }
        $cookies = $web.Handler.CookieContainer.GetCookies([Uri]$base)
        Check 'setup' ($setup.Status -eq 200 -and -not [string]::IsNullOrEmpty([string](Get-Field $setup.Payload 'token')) -and $cookies.Count -gt 0)

        $login = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/auth/login" -Body @{ name = 'e2e'; password = $password }
        Check 'login' ($login.Status -eq 200 -and -not [string]::IsNullOrEmpty([string](Get-Field $login.Payload 'token')))

        $pkceOne = New-PKCEPair
        $startBody = @{
            device_name = 'E2E Windows Desktop'
            platform = 'windows'
            client_version = '0.1.0'
            code_challenge = $pkceOne.Challenge
            code_challenge_method = 'S256'
        }
        $startOne = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations" -Body $startBody
        $authorizationOne = [string](Get-Field $startOne.Payload 'authorization_id')
        $deviceCodeOne = [string](Get-Field $startOne.Payload 'device_code')
        $userCodeOne = [string](Get-Field $startOne.Payload 'user_code')
        $verificationOne = [string](Get-Field $startOne.Payload 'verification_uri')
        $completeOne = [string](Get-Field $startOne.Payload 'verification_uri_complete')
        Check 'device authorization start contract' (
            $startOne.Status -eq 201 -and
            -not [string]::IsNullOrEmpty($authorizationOne) -and
            -not [string]::IsNullOrEmpty($deviceCodeOne) -and
            -not [string]::IsNullOrEmpty($userCodeOne) -and
            (Get-Field $startOne.Payload 'expires_in') -eq 600 -and
            (Get-Field $startOne.Payload 'interval') -eq 5 -and
            $verificationOne -eq "$base/device" -and
            $completeOne -eq "$base/device?code=$userCodeOne"
        )
        if (-not $authorizationOne -or -not $deviceCodeOne -or -not $userCodeOne) {
            throw 'start response omitted a required value'
        }

        $statusOne = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))"
        Check 'authorization pending status' (
            $statusOne.Status -eq 200 -and (Get-Field $statusOne.Payload 'state') -eq 'pending' -and
            -not [string]::IsNullOrEmpty([string](Get-Field $statusOne.Payload 'expires_at'))
        )

        $anonymousLookup = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/auth/device/authorizations/lookup?user_code=$([uri]::EscapeDataString($userCodeOne))"
        Check 'lookup requires Web session' (
            $anonymousLookup.Status -eq 401 -and (Get-ErrorCode $anonymousLookup.Payload) -eq 'unauthenticated'
        )

        $lookupOne = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/auth/device/authorizations/lookup?user_code=$([uri]::EscapeDataString($userCodeOne))"
        $csrfOne = [string](Get-Field $lookupOne.Payload 'csrf_token')
        $lookupLeaked = $null -ne (Get-Field $lookupOne.Payload 'device_code') -or
            $null -ne (Get-Field $lookupOne.Payload 'user_code') -or
            $null -ne (Get-Field $lookupOne.Payload 'user_id')
        Check 'Web session lookup' (
            $lookupOne.Status -eq 200 -and
            (Get-Field $lookupOne.Payload 'authorization_id') -eq $authorizationOne -and
            (Get-Field $lookupOne.Payload 'state') -eq 'pending' -and
            -not [string]::IsNullOrEmpty($csrfOne) -and
            -not $lookupLeaked
        )
        if (-not $csrfOne) {
            throw 'lookup response omitted the approval token'
        }

        $missingCSRF = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))/decision" `
            -Body @{ decision = 'approve' } -Headers @{ Origin = $base }
        Check 'approval requires CSRF token' (
            $missingCSRF.Status -eq 403 -and (Get-ErrorCode $missingCSRF.Payload) -eq 'forbidden'
        )

        $wrongCSRF = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))/decision" `
            -Body @{ decision = 'approve' } `
            -Headers @{ Origin = $base; 'X-CSRF-Token' = ('csrf_' + ('A' * 43)) }
        Check 'incorrect CSRF token rejected' (
            $wrongCSRF.Status -eq 403 -and (Get-ErrorCode $wrongCSRF.Payload) -eq 'forbidden'
        )

        $crossSite = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))/decision" `
            -Body @{ decision = 'approve' } `
            -Headers @{
                Origin = $base
                'Sec-Fetch-Site' = 'cross-site'
                'X-CSRF-Token' = $csrfOne
            }
        Check 'cross-site approval rejected' (
            $crossSite.Status -eq 403 -and (Get-ErrorCode $crossSite.Payload) -eq 'forbidden'
        )

        $approveOne = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))/decision" `
            -Body @{ decision = 'approve' } `
            -Headers @{
                Origin = $base
                'Sec-Fetch-Site' = 'same-origin'
                'X-CSRF-Token' = $csrfOne
            }
        Check 'Web session approval' (
            $approveOne.Status -eq 200 -and (Get-Field $approveOne.Payload 'state') -eq 'authorized'
        )

        $replayedDecision = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationOne))/decision" `
            -Body @{ decision = 'approve' } `
            -Headers @{
                Origin = $base
                'X-CSRF-Token' = $csrfOne
            }
        Check 'replayed approval token rejected' (
            $replayedDecision.Status -eq 409 -and
            (Get-ErrorCode $replayedDecision.Payload) -eq 'authorization_consumed'
        )

        $tokenOne = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/token" `
            -Body @{ device_code = $deviceCodeOne; code_verifier = $pkceOne.Verifier }
        $accessOne = [string](Get-Field $tokenOne.Payload 'access_token')
        $refreshOne = [string](Get-Field $tokenOne.Payload 'refresh_token')
        $deviceOneID = [string](Get-Field (Get-Field $tokenOne.Payload 'device') 'id')
        $userOne = Get-Field $tokenOne.Payload 'user'
        $tokenLeaked = $null -ne (Get-Field $userOne 'pass_hash') -or $null -ne (Get-Field $userOne 'salt')
        Check 'device token exchange' (
            $tokenOne.Status -eq 200 -and
            -not [string]::IsNullOrEmpty($accessOne) -and
            -not [string]::IsNullOrEmpty($refreshOne) -and
            -not [string]::IsNullOrEmpty($deviceOneID) -and
            (Get-Field $tokenOne.Payload 'token_type') -eq 'Bearer' -and
            (Get-Field $tokenOne.Payload 'expires_in') -eq 900 -and
            (Get-Field $tokenOne.Payload 'refresh_expires_in') -eq 2592000 -and
            -not $tokenLeaked
        )
        if (-not $accessOne -or -not $refreshOne -or -not $deviceOneID) {
            throw 'token response omitted a required value'
        }

        $refresh = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/refresh" -Body @{ refresh_token = $refreshOne }
        $rotatedAccess = [string](Get-Field $refresh.Payload 'access_token')
        $rotatedRefresh = [string](Get-Field $refresh.Payload 'refresh_token')
        Check 'refresh rotation' (
            $refresh.Status -eq 200 -and
            -not [string]::IsNullOrEmpty($rotatedAccess) -and
            -not [string]::IsNullOrEmpty($rotatedRefresh) -and
            $rotatedAccess -ne $accessOne -and
            $rotatedRefresh -ne $refreshOne
        )
        if (-not $rotatedAccess) {
            throw 'refresh response omitted an access token'
        }

        $oldAccessAfterRefresh = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $accessOne
        Check 'refresh invalidates old access token' (
            $oldAccessAfterRefresh.Status -eq 401 -and
            (Get-ErrorCode $oldAccessAfterRefresh.Payload) -eq 'unauthenticated'
        )

        $listBeforeReplay = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $rotatedAccess
        Check 'device list with rotated access token' (
            $listBeforeReplay.Status -eq 200 -and @(Get-Field $listBeforeReplay.Payload 'devices').Count -eq 1
        )

        $pkceTwo = New-PKCEPair
        $startTwo = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations" `
            -Body @{
                device_name = 'E2E Android Phone'
                platform = 'android'
                client_version = '0.1.0'
                code_challenge = $pkceTwo.Challenge
                code_challenge_method = 'S256'
            }
        $authorizationTwo = [string](Get-Field $startTwo.Payload 'authorization_id')
        $deviceCodeTwo = [string](Get-Field $startTwo.Payload 'device_code')
        $userCodeTwo = [string](Get-Field $startTwo.Payload 'user_code')
        if ($startTwo.Status -ne 201 -or -not $authorizationTwo -or -not $deviceCodeTwo -or -not $userCodeTwo) {
            throw 'second device authorization failed'
        }
        $lookupTwo = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/auth/device/authorizations/lookup?user_code=$([uri]::EscapeDataString($userCodeTwo))"
        $csrfTwo = [string](Get-Field $lookupTwo.Payload 'csrf_token')
        if ($lookupTwo.Status -ne 200 -or -not $csrfTwo) {
            throw 'second device lookup failed'
        }
        $approveTwo = Invoke-Json -WebClient $web -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations/$([uri]::EscapeDataString($authorizationTwo))/decision" `
            -Body @{ decision = 'approve' } `
            -Headers @{ Origin = $base; 'X-CSRF-Token' = $csrfTwo }
        $tokenTwo = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/token" `
            -Body @{ device_code = $deviceCodeTwo; code_verifier = $pkceTwo.Verifier }
        $accessTwo = [string](Get-Field $tokenTwo.Payload 'access_token')
        Check 'second device authorization' (
            $approveTwo.Status -eq 200 -and $tokenTwo.Status -eq 200 -and
            -not [string]::IsNullOrEmpty($accessTwo)
        )
        if (-not $accessTwo) {
            throw 'second device token missing'
        }

        $listTwo = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $accessTwo
        $listedDevices = @(Get-Field $listTwo.Payload 'devices')
        $listedIDs = @($listedDevices | ForEach-Object { [string](Get-Field $_ 'id') })
        Check 'device list' (
            $listTwo.Status -eq 200 -and $listedDevices.Count -eq 2 -and
            $listedIDs -contains $deviceOneID
        )

        $replay = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/refresh" -Body @{ refresh_token = $refreshOne }
        Check 'old refresh replay rejected' (
            $replay.Status -eq 409 -and (Get-ErrorCode $replay.Payload) -eq 'refresh_reuse_detected'
        )

        $accessAfterReplay = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $rotatedAccess
        Check 'refresh replay revokes device' (
            $accessAfterReplay.Status -eq 401 -and
            (Get-ErrorCode $accessAfterReplay.Payload) -eq 'device_revoked'
        )

        $listAfterReplay = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $accessTwo
        $revokedOne = @(Get-Field $listAfterReplay.Payload 'devices') |
            Where-Object { [string](Get-Field $_ 'id') -eq $deviceOneID }
        Check 'device list exposes replay revocation' (
            $listAfterReplay.Status -eq 200 -and $null -ne $revokedOne -and
            $null -ne (Get-Field $revokedOne 'revoked_at')
        )

        $delete = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Delete) `
            -Uri "$base/api/v1/devices/$([uri]::EscapeDataString($deviceOneID))" -Bearer $accessTwo
        Check 'device delete' (
            $delete.Status -eq 200 -and
            (Get-Field $delete.Payload 'device_id') -eq $deviceOneID -and
            (Get-Field $delete.Payload 'state') -eq 'revoked'
        )

        $repeatDelete = Invoke-Json -WebClient $deviceTwo -Method ([System.Net.Http.HttpMethod]::Delete) `
            -Uri "$base/api/v1/devices/$([uri]::EscapeDataString($deviceOneID))" -Bearer $accessTwo
        Check 'device delete idempotent' (
            $repeatDelete.Status -eq 200 -and
            (Get-Field $repeatDelete.Payload 'revoked_at') -eq (Get-Field $delete.Payload 'revoked_at')
        )

        $revokedAccess = Invoke-Json -WebClient $deviceOne -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/devices" -Bearer $rotatedAccess
        Check 'revoked device access rejected' (
            $revokedAccess.Status -eq 401 -and (Get-ErrorCode $revokedAccess.Payload) -eq 'device_revoked'
        )
    } finally {
        $web.Client.Dispose()
        $web.Handler.Dispose()
        $deviceOne.Client.Dispose()
        $deviceOne.Handler.Dispose()
        $deviceTwo.Client.Dispose()
        $deviceTwo.Handler.Dispose()
    }

    Stop-Server
    Write-Config -DeviceAuthEnabled $false
    Check 'server start with device auth disabled' (Start-Server -StdOut $serverOut2 -StdErr $serverErr2)
    if ($null -eq $serverProcess -or $serverProcess.HasExited) {
        throw 'server did not restart'
    }

    $disabledClient = New-WebClient
    try {
        $meta = Invoke-Json -WebClient $disabledClient -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "$base/api/v1/meta"
        Check 'meta remains available while device auth is disabled' (
            $meta.Status -eq 200 -and
            (Get-Field (Get-Field $meta.Payload 'features') 'device_auth_enabled') -eq $false
        )

        $disabledPKCE = New-PKCEPair
        $disabledStart = Invoke-Json -WebClient $disabledClient -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base/api/v1/auth/device/authorizations" `
            -Body @{
                device_name = 'Disabled E2E Device'
                platform = 'windows'
                client_version = '0.1.0'
                code_challenge = $disabledPKCE.Challenge
                code_challenge_method = 'S256'
            }
        Check 'device auth feature flag fails closed' (
            $disabledStart.Status -eq 404 -and (Get-ErrorCode $disabledStart.Payload) -eq 'feature_disabled'
        )
    } finally {
        $disabledClient.Client.Dispose()
        $disabledClient.Handler.Dispose()
    }
} catch {
    Check 'e2e execution completed without an unexpected error' $false
} finally {
    Stop-Server
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
        Remove-Item -LiteralPath $resolvedScratch -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if ($failures.Count -gt 0) {
    exit 1
}
exit 0
