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
$webDAVFixtureSource = Join-Path $toolsDir 'webdav-fixture.go'
$webDAVFixtureExe = Join-Path $toolsDir 'webdav-fixture.exe'
$buildLog = Join-Path $scratch 'build.log'
$fakeProbeBuildLog = Join-Path $scratch 'fake-probe-build.log'
$webDAVFixtureBuildLog = Join-Path $scratch 'webdav-fixture-build.log'
$webDAVFixtureOutLog = Join-Path $scratch 'webdav-fixture.out.log'
$webDAVFixtureErrLog = Join-Path $scratch 'webdav-fixture.err.log'
$mediaFixtureLog = Join-Path $scratch 'media-fixture.log'
$hlsDecodeLog = Join-Path $scratch 'hls-decode.log'
$hlsSegmentPath = Join-Path $scratch 'playback-segment.ts'
$dashModePath = Join-Path $scratch 'direct-dash-mode.txt'
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
$script:webDAVFixtureProcess = $null
$script:serverRun = 0
$script:serverLogs = [System.Collections.Generic.List[string]]::new()
$script:clients = [System.Collections.Generic.List[object]]::new()
$script:sensitiveValues = [System.Collections.Generic.List[string]]::new()
$script:mediaRequestMade = $false
$script:subtitleRequestMade = $false
$script:dashTicketIDs = [System.Collections.Generic.List[string]]::new()
$oldLocalEnv = $env:DORIYAMEDIA_LOCAL
$oldDashModeFileEnv = $env:PLAYBACK_E2E_DASH_MODE_FILE
$oldDashVideoURLEnv = $env:PLAYBACK_E2E_DASH_VIDEO_URL
$oldDashAudioURLEnv = $env:PLAYBACK_E2E_DASH_AUDIO_URL
$oldDashCookieEnv = $env:PLAYBACK_E2E_DASH_UNSAFE_COOKIE
$oldDashAuthorizationEnv = $env:PLAYBACK_E2E_DASH_UNSAFE_AUTHORIZATION

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

function Test-FieldAbsent {
    param($Object, [string]$Name)

    if ($null -eq $Object) {
        return $false
    }
    return $null -eq $Object.PSObject.Properties[$Name]
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

function Get-AllResponseHeaderText {
    param($Response)

    if ($null -eq $Response) {
        return ''
    }
    $lines = [System.Collections.Generic.List[string]]::new()
    foreach ($headers in @($Response.Headers, $Response.ContentHeaders)) {
        if ($null -eq $headers) {
            continue
        }
        foreach ($name in @($headers.Keys | Sort-Object)) {
            $lines.Add("${name}: $([string]$headers[$name])")
        }
    }
    return ($lines -join "`n")
}

function Get-ForbiddenResponseHeader {
    param(
        $Response,
        [string[]]$AllowedLocations = @()
    )

    if ($null -eq $Response) {
        return 'missing response'
    }
    $forbidden = @(
        'authorization',
        'proxy-authorization',
        'cookie',
        'set-cookie',
        'www-authenticate'
    )
    foreach ($headers in @($Response.Headers, $Response.ContentHeaders)) {
        if ($null -eq $headers) {
            continue
        }
        foreach ($name in @($headers.Keys)) {
            $headerName = [string]$name
            $lowerName = $headerName.ToLowerInvariant()
            if ($forbidden -contains $lowerName) {
                return $headerName
            }
            if ($lowerName.StartsWith('x-source-', [System.StringComparison]::Ordinal)) {
                return $headerName
            }
            if ($lowerName -eq 'location') {
                $value = [string]$headers[$name]
                $allowed = $false
                foreach ($candidate in $AllowedLocations) {
                    if ([string]::Equals(
                        $value,
                        $candidate,
                        [System.StringComparison]::Ordinal
                    )) {
                        $allowed = $true
                        break
                    }
                }
                if (-not $allowed) {
                    return $headerName
                }
            }
        }
    }
    return ''
}

function Test-ResponseHeaderSafety {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        $Response,
        [string[]]$AllowedLocations = @(),
        [string[]]$ExtraValues = @()
    )

    $forbiddenHeader = Get-ForbiddenResponseHeader `
        -Response $Response -AllowedLocations $AllowedLocations
    $headerText = Get-AllResponseHeaderText $Response
    $secret = Find-SensitiveValue -Text $headerText -ExtraValues $ExtraValues
    Check "$Name response headers are sanitized" (
        -not $forbiddenHeader -and $null -eq $secret
    )
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

function Set-DASHMode {
    param(
        [Parameter(Mandatory = $true)]
        [ValidateSet(
            'valid',
            'single',
            'bad-header',
            'valid-probe-and-select-then-bad-header'
        )]
        [string]$Mode
    )

    Write-Utf8File -Path $dashModePath -Text ($Mode + "`n")
}

function Restore-EnvironmentValue {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [AllowNull()][AllowEmptyString()][string]$Value
    )

    if ($null -eq $Value) {
        Remove-Item -Path "Env:$Name" -ErrorAction SilentlyContinue
        return
    }
    Set-Item -Path "Env:$Name" -Value $Value
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

function Initialize-WebDAVFixture {
    $source = @'
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	payloadSize  = 2 * 1024 * 1024
	remoteETag   = `"webdav-fixture-v1"`
	lastModified = "Mon, 26 Oct 2026 10:00:00 GMT"
)

var (
	payload = func() []byte {
		out := make([]byte, payloadSize)
		for i := range out {
			out[i] = byte(i % 251)
		}
		return out
	}()
	modified = parseTime(lastModified)
)

func parseTime(raw string) time.Time {
	value, err := http.ParseTime(raw)
	if err != nil {
		panic(err)
	}
	return value
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address")
	user := flag.String("user", "", "WebDAV user")
	pass := flag.String("pass", "", "WebDAV password")
	canary := flag.String("canary", "fixture-canary", "sensitive header canary")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		if !ok || gotUser != *user || gotPass != *pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="webdav-fixture"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		log.Printf("method=%s path=%s range=%q if_range=%q",
			r.Method, r.URL.Path, r.Header.Get("Range"), r.Header.Get("If-Range"))

		path := strings.TrimSuffix(r.URL.Path, "/")
		if r.Method == "PROPFIND" && path == "/dav" {
			writeMultistatus(w)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		switch path {
		case "/dav/remote.mp4":
			addSourceCanaries(w, *canary)
			w.Header().Set("ETag", remoteETag)
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "remote.mp4", modified, bytes.NewReader(payload))
		case "/dav/ignored-range.mp4":
			addSourceCanaries(w, *canary)
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("ETag", remoteETag)
			w.Header().Set("Last-Modified", lastModified)
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(payload)
			}
		case "/dav/bad-partial.mp4":
			addSourceCanaries(w, *canary)
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Range", "bytes malformed-upstream")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte("junk"))
			}
		case "/dav/source-fail.mp4":
			addSourceCanaries(w, *canary)
			body := "upstream source stage body " + *canary
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusInternalServerError)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(body))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
	mux.HandleFunc("/dav", handler)
	mux.HandleFunc("/dav/", handler)

	log.Printf("webdav fixture listening on %s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

func addSourceCanaries(w http.ResponseWriter, canary string) {
	w.Header().Set("Authorization", "remote-authorization-"+canary)
	w.Header().Set("Proxy-Authorization", "remote-proxy-authorization-"+canary)
	w.Header().Set("Cookie", "remote-cookie-"+canary)
	w.Header().Add("Set-Cookie", "remote-session-"+canary+"=1; Path=/; HttpOnly")
	w.Header().Set("WWW-Authenticate", `Basic realm="remote-www-`+canary+`"`)
	w.Header().Set("Location", "http://127.0.0.1:9/remote-location-"+canary)
	w.Header().Set("X-Source-Secret", "remote-x-source-secret-"+canary)
	w.Header().Set("X-Source-Token", "remote-x-source-token-"+canary)
	w.Header().Set("Content-Disposition",
		`attachment; filename="remote-content-disposition-`+canary+`"`)
}

func writeMultistatus(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	responses := []struct {
		name string
		dir  bool
	}{
		{name: "/dav/", dir: true},
		{name: "/dav/remote.mp4"},
		{name: "/dav/ignored-range.mp4"},
		{name: "/dav/bad-partial.mp4"},
		{name: "/dav/source-fail.mp4"},
	}
	var out strings.Builder
	out.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	out.WriteString(`<D:multistatus xmlns:D="DAV:">`)
	for _, entry := range responses {
		out.WriteString("<D:response><D:href>")
		out.WriteString(entry.name)
		out.WriteString("</D:href><D:propstat><D:prop>")
		if entry.dir {
			out.WriteString("<D:resourcetype><D:collection/></D:resourcetype>")
		} else {
			out.WriteString("<D:resourcetype/>")
			out.WriteString("<D:getcontentlength>")
			out.WriteString(strconv.Itoa(len(payload)))
			out.WriteString("</D:getcontentlength>")
			out.WriteString("<D:getlastmodified>")
			out.WriteString(lastModified)
			out.WriteString("</D:getlastmodified>")
		}
		out.WriteString(`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	}
	out.WriteString("</D:multistatus>")
	_, _ = fmt.Fprint(w, out.String())
}
'@
    Write-Utf8File -Path $webDAVFixtureSource -Text $source
}

function Start-WebDAVFixture {
    param(
        [Parameter(Mandatory = $true)][string]$BaseURL,
        [Parameter(Mandatory = $true)][string]$User,
        [Parameter(Mandatory = $true)][string]$Pass,
        [Parameter(Mandatory = $true)][string]$Canary
    )

    $uri = [Uri]$BaseURL
    $listen = "$($uri.Host):$($uri.Port)"
    $healthURL = "$($uri.Scheme)://$($uri.Authority)/health"
    $script:webDAVFixtureProcess = Start-Process -FilePath $webDAVFixtureExe `
        -ArgumentList @(
            '-listen', $listen,
            '-user', $User,
            '-pass', $Pass,
            '-canary', $Canary
        ) `
        -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $webDAVFixtureOutLog `
        -RedirectStandardError $webDAVFixtureErrLog

    $probe = New-WebClient
    try {
        $deadline = [DateTime]::UtcNow.AddSeconds(20)
        while ([DateTime]::UtcNow -lt $deadline) {
            if ($script:webDAVFixtureProcess.HasExited) {
                return $false
            }
            try {
                $response = $probe.Client.GetAsync($healthURL).GetAwaiter().GetResult()
                try {
                    if ([int]$response.StatusCode -eq 204) {
                        return $true
                    }
                } finally {
                    $response.Dispose()
                }
            } catch {
                # The fixture listener is not ready yet.
            }
            Start-Sleep -Milliseconds 100
        }
        return $false
    } finally {
        $probe.Client.Dispose()
        $probe.Handler.Dispose()
    }
}

function Stop-WebDAVFixture {
    if ($null -ne $script:webDAVFixtureProcess -and
        -not $script:webDAVFixtureProcess.HasExited) {
        Stop-Process -Id $script:webDAVFixtureProcess.Id -Force -ErrorAction SilentlyContinue
        $null = $script:webDAVFixtureProcess.WaitForExit(5000)
    }
    $script:webDAVFixtureProcess = $null
}

function Write-Config {
    param(
        [bool]$TicketEnabled = $true,
        [bool]$DirectEnabled = $true,
        [bool]$ProxyEnabled = $true,
        [bool]$HLSEnabled = $true,
        [bool]$DirectDASHEnabled = $false
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
            direct_dash_enabled = $DirectDASHEnabled
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
        [string[]]$PreferredTransports,
        [ValidateSet('omit', 'false', 'true')]
        [string]$DASHMerge = 'omit'
    )

    $body = @{
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
    if ($DASHMerge -ne 'omit') {
        $body.client.supports_dash_merge = $DASHMerge -eq 'true'
    }
    return $body
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

function Open-DASHTicket {
    param(
        [Parameter(Mandatory = $true)]$Device,
        [Parameter(Mandatory = $true)][long]$FileID,
        [Parameter(Mandatory = $true)][string]$Label,
        [ValidateSet('omit', 'false', 'true')]
        [string]$DASHMerge = 'omit',
        [string[]]$PreferredTransports = @('direct-dash', 'server-hls')
    )

    $create = Invoke-Json -WebClient $Device.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $Device.Access `
        -Body (New-TicketBody -FileID $FileID -DeviceID $Device.DeviceID `
            -PreferredTransports $PreferredTransports -DASHMerge $DASHMerge)
    $ticketID = [string](Get-Field $create.Payload 'ticket_id')
    if (-not $ticketID) {
        throw "$Label ticket creation returned no ticket id: $(Get-ResponseSummary $create)"
    }
    $script:dashTicketIDs.Add($ticketID)
    $ticketPath = "/api/v1/playback/tickets/$([uri]::EscapeDataString($ticketID))"
    Test-ResponseHeaderSafety -Name "$Label ticket creation" -Response $create `
        -AllowedLocations @($ticketPath)
    $createState = [string](Get-Field $create.Payload 'state')
    Check "$Label ticket creation is accepted" (
        @(201, 202) -contains $create.Status -and
        @('probing', 'ready', 'consuming', 'unsupported', 'failed') -contains $createState -and
        (Get-ResponseHeader $create 'Cache-Control') -eq 'no-store'
    )
    if (@(201, 202) -notcontains $create.Status) {
        throw "$Label ticket creation failed: $(Get-ResponseSummary $create)"
    }

    $issued = $create
    if ($createState -eq 'probing') {
        $issued = Wait-TicketReady -Device $Device -TicketPath $ticketPath
    }
    $issuedState = [string](Get-Field $issued.Payload 'state')
    if ($issuedState -eq 'ready') {
        $issued = Invoke-Json -WebClient $Device.Client `
            -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base$ticketPath/refresh" `
            -Bearer $Device.Access
        Test-ResponseHeaderSafety -Name "$Label ticket refresh" -Response $issued
    }

    return [pscustomobject]@{
        Create = $create
        Issued = $issued
        TicketID = $ticketID
        TicketPath = $ticketPath
    }
}

function Open-ServerProxySession {
    param(
        [Parameter(Mandatory = $true)]$Device,
        [Parameter(Mandatory = $true)][long]$FileID,
        [Parameter(Mandatory = $true)][string]$Label
    )

    $create = Invoke-Json -WebClient $Device.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $Device.Access `
        -Body (New-TicketBody -FileID $FileID -DeviceID $Device.DeviceID `
            -PreferredTransports @('server-proxy'))
    $ticketID = [string](Get-Field $create.Payload 'ticket_id')
    if (-not $ticketID) {
        throw "$Label ticket creation returned no ticket id: $(Get-ResponseSummary $create)"
    }
    $ticketPath = "/api/v1/playback/tickets/$([uri]::EscapeDataString($ticketID))"
    Test-ResponseHeaderSafety -Name "$Label ticket creation" -Response $create `
        -AllowedLocations @($ticketPath)
    $createState = [string](Get-Field $create.Payload 'state')
    Check "$Label ticket creation is accepted" (
        @(201, 202) -contains $create.Status -and
        @('probing', 'ready', 'consuming') -contains $createState -and
        (Get-ResponseHeader $create 'Cache-Control') -eq 'no-store'
    )
    if (@(201, 202) -notcontains $create.Status) {
        throw "$Label ticket creation failed: $(Get-ResponseSummary $create)"
    }

    $ready = $create
    if ($createState -eq 'probing') {
        $ready = Wait-TicketReady -Device $Device -TicketPath $ticketPath
    }
    $readyState = [string](Get-Field $ready.Payload 'state')
    $readyStatusOk = (
        $ready.Status -eq 200 -or
        ($createState -eq 'consuming' -and $ready.Status -eq 201)
    )
    Check "$Label ticket becomes ready" (
        $readyStatusOk -and
        @('ready', 'consuming') -contains $readyState
    )
    if (@('ready', 'consuming') -notcontains $readyState) {
        throw "$Label ticket did not become ready: $(Get-ResponseSummary $ready)"
    }

    $issued = $create
    if ($createState -ne 'consuming') {
        $issued = Invoke-Json -WebClient $Device.Client `
            -Method ([System.Net.Http.HttpMethod]::Post) `
            -Uri "$base$ticketPath/refresh" `
            -Bearer $Device.Access
        Test-ResponseHeaderSafety -Name "$Label ticket refresh" -Response $issued
    }
    $transport = Get-Field $issued.Payload 'transport'
    $streams = @(Get-Field $transport 'streams')
    $stream = $streams | Select-Object -First 1
    $mediaURL = [string](Get-Field $stream 'url')
    $capability = Get-MediaCapability $mediaURL
    Check "$Label issues a server-proxy media capability" (
        $issued.Status -eq 200 -and
        (Get-Field $issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $transport 'kind') -eq 'server-proxy' -and
        $streams.Count -eq 1 -and
        $capability.StartsWith('ms_', [System.StringComparison]::Ordinal)
    )
    if (-not $capability) {
        throw "$Label did not issue a media capability: $(Get-ResponseSummary $issued)"
    }
    Add-SensitiveValue $capability
    return [pscustomobject]@{
        TicketID = $ticketID
        TicketPath = $ticketPath
        Create = $create
        Ready = $ready
        Issued = $issued
        MediaURL = $mediaURL
        LocalURL = "$base$(([Uri]$mediaURL).PathAndQuery)"
        Capability = $capability
    }
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

function Get-MediaResponseRecords {
    param([AllowNull()][AllowEmptyString()][string]$Text)

    $records = [System.Collections.Generic.List[object]]::new()
    if ([string]::IsNullOrEmpty($Text)) {
        return @($records)
    }
    $pattern = [regex]::new(
        'event=media_response surface=(\S+) transport=(\S+) asset=(\S+) ' +
        'outcome=(\S+) failure_stage=(\S+) status=(\d+) response_bytes=(\d+) ' +
        'server_media_bytes=(\d+) duration_ms=(\d+)\s*$',
        [System.Text.RegularExpressions.RegexOptions]::CultureInvariant
    )
    $lineNumber = 0
    foreach ($line in ($Text -split "`r?`n")) {
        $lineNumber++
        if ($line.IndexOf(
            'event=media_response',
            [System.StringComparison]::Ordinal
        ) -lt 0) {
            continue
        }
        $match = $pattern.Match($line)
        if (-not $match.Success) {
            $records.Add([pscustomobject]@{
                Valid = $false
                LineNumber = $lineNumber
                Raw = $line
            })
            continue
        }
        try {
            $records.Add([pscustomobject]@{
                Valid = $true
                LineNumber = $lineNumber
                Raw = $line
                Surface = $match.Groups[1].Value
                Transport = $match.Groups[2].Value
                Asset = $match.Groups[3].Value
                Outcome = $match.Groups[4].Value
                FailureStage = $match.Groups[5].Value
                Status = [int]$match.Groups[6].Value
                ResponseBytes = [long]$match.Groups[7].Value
                ServerMediaBytes = [long]$match.Groups[8].Value
                DurationMS = [long]$match.Groups[9].Value
            })
        } catch {
            $records.Add([pscustomobject]@{
                Valid = $false
                LineNumber = $lineNumber
                Raw = $line
            })
        }
    }
    return @($records)
}

function Test-MediaResponseRecord {
    param(
        $Records,
        [string]$Surface = '',
        [string]$Transport = '',
        [string]$Asset = '',
        [string]$Outcome = '',
        [string]$FailureStage = '',
        [int]$Status = -1,
        [long]$ResponseBytes = -1,
        [long]$ServerMediaBytes = -1,
        [long]$MinResponseBytes = -1,
        [long]$MinServerMediaBytes = -1
    )

    foreach ($record in @($Records)) {
        if (-not $record.Valid) {
            continue
        }
        if ($Surface -and $record.Surface -ne $Surface) {
            continue
        }
        if ($Transport -and $record.Transport -ne $Transport) {
            continue
        }
        if ($Asset -and $record.Asset -ne $Asset) {
            continue
        }
        if ($Outcome -and $record.Outcome -ne $Outcome) {
            continue
        }
        if ($FailureStage -and $record.FailureStage -ne $FailureStage) {
            continue
        }
        if ($Status -ge 0 -and $record.Status -ne $Status) {
            continue
        }
        if ($ResponseBytes -ge 0 -and $record.ResponseBytes -ne $ResponseBytes) {
            continue
        }
        if ($ServerMediaBytes -ge 0 -and
            $record.ServerMediaBytes -ne $ServerMediaBytes) {
            continue
        }
        if ($MinResponseBytes -ge 0 -and
            $record.ResponseBytes -lt $MinResponseBytes) {
            continue
        }
        if ($MinServerMediaBytes -ge 0 -and
            $record.ServerMediaBytes -lt $MinServerMediaBytes) {
            continue
        }
        return $true
    }
    return $false
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

function Test-PatternBytes {
    param(
        [byte[]]$Bytes,
        [long]$Offset,
        [int]$Count
    )

    if ($null -eq $Bytes -or $Bytes.Length -ne $Count) {
        return $false
    }
    for ($i = 0; $i -lt $Count; $i++) {
        if ($Bytes[$i] -ne [byte](($Offset + $i) % 251)) {
            return $false
        }
    }
    return $true
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
    $dashVideoSecret = 'dashv_' + (New-RandomBase64Url -ByteCount 32)
    $dashAudioSecret = 'dasha_' + (New-RandomBase64Url -ByteCount 32)
    $dashVideoURL = "https://video.playback-e2e.invalid/video.m4s?token=$dashVideoSecret"
    $dashAudioURL = "https://audio.playback-e2e.invalid/audio.m4s?token=$dashAudioSecret"
    $dashCookieCanary = 'dash-cookie-' + (New-RandomBase64Url -ByteCount 32)
    $dashAuthorizationCanary = 'Bearer dash-auth-' + (New-RandomBase64Url -ByteCount 32)
    Add-SensitiveValue $dashVideoSecret
    Add-SensitiveValue $dashAudioSecret
    Add-SensitiveValue $dashVideoURL
    Add-SensitiveValue $dashAudioURL
    Add-SensitiveValue 'video.playback-e2e.invalid'
    Add-SensitiveValue 'audio.playback-e2e.invalid'
    Add-SensitiveValue $dashCookieCanary
    Add-SensitiveValue $dashAuthorizationCanary
    Set-DASHMode valid
    $env:PLAYBACK_E2E_DASH_MODE_FILE = $dashModePath
    $env:PLAYBACK_E2E_DASH_VIDEO_URL = $dashVideoURL
    $env:PLAYBACK_E2E_DASH_AUDIO_URL = $dashAudioURL
    $env:PLAYBACK_E2E_DASH_UNSAFE_COOKIE = $dashCookieCanary
    $env:PLAYBACK_E2E_DASH_UNSAFE_AUTHORIZATION = $dashAuthorizationCanary
    $remoteWebDAVUser = 'e2e-webdav-user'
    $remoteWebDAVPass = New-RandomBase64Url -ByteCount 32
    $remoteBasicAuth = 'Basic ' + [Convert]::ToBase64String(
        [System.Text.Encoding]::UTF8.GetBytes(
            "${remoteWebDAVUser}:${remoteWebDAVPass}"))
    $remoteCanary = New-RandomBase64Url -ByteCount 24
    $remoteCanaryValues = @(
        "remote-authorization-$remoteCanary",
        "remote-proxy-authorization-$remoteCanary",
        "remote-cookie-$remoteCanary",
        "remote-session-$remoteCanary=1; Path=/; HttpOnly",
        "remote-www-$remoteCanary",
        "remote-location-$remoteCanary",
        "remote-x-source-secret-$remoteCanary",
        "remote-x-source-token-$remoteCanary",
        "remote-content-disposition-$remoteCanary",
        "upstream source stage body $remoteCanary"
    )
    Add-SensitiveValue $remoteWebDAVPass
    Add-SensitiveValue $remoteBasicAuth
    foreach ($value in $remoteCanaryValues) {
        Add-SensitiveValue $value
    }

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
    Initialize-WebDAVFixture
    Push-Location -LiteralPath $root
    try {
        & go build -tags playbacke2e -o $exePath ./cmd/doriyamedia *> $buildLog
        $buildExit = $LASTEXITCODE
        & go build -o $fakeProbeExe $fakeProbeSource *> $fakeProbeBuildLog
        $fakeProbeBuildExit = $LASTEXITCODE
        & go build -o $webDAVFixtureExe $webDAVFixtureSource *> $webDAVFixtureBuildLog
        $webDAVFixtureBuildExit = $LASTEXITCODE
    } finally {
        Pop-Location
    }
    Check 'server build' ($buildExit -eq 0 -and (Test-Path -LiteralPath $exePath -PathType Leaf))
    Check 'deterministic ffprobe helper build' (
        $fakeProbeBuildExit -eq 0 -and (Test-Path -LiteralPath $fakeProbeExe -PathType Leaf)
    )
    Check 'remote WebDAV fixture build' (
        $webDAVFixtureBuildExit -eq 0 -and
        (Test-Path -LiteralPath $webDAVFixtureExe -PathType Leaf)
    )
    if ($buildExit -ne 0 -or $fakeProbeBuildExit -ne 0 -or
        $webDAVFixtureBuildExit -ne 0) {
        throw 'required test binary build failed'
    }

    do {
        $remoteWebDAVPort = Get-FreeLoopbackPort
    } while ($remoteWebDAVPort -eq $script:port)
    $remoteWebDAVBase = "http://127.0.0.1:$remoteWebDAVPort/dav"
    Check 'remote WebDAV fixture starts' (
        Start-WebDAVFixture -BaseURL $remoteWebDAVBase `
            -User $remoteWebDAVUser -Pass $remoteWebDAVPass -Canary $remoteCanary
    )
    if ($null -eq $script:webDAVFixtureProcess -or
        $script:webDAVFixtureProcess.HasExited) {
        throw 'remote WebDAV fixture did not start'
    }

    Write-Config -TicketEnabled $true -DirectEnabled $true -ProxyEnabled $true `
        -HLSEnabled $true -DirectDASHEnabled $true
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
        (Get-Field $features 'server_hls_v1_enabled') -eq $true -and
        (Get-Field $features 'direct_dash_enabled') -eq $true
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
    $createDASHSource = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/sources" `
        -Body @{
            name = 'playback-e2e-direct-dash'
            type = 'playbacke2e-dash'
            config = @{}
        }
    $dashSourceID = [long](Get-Field $createDASHSource.Payload 'id')
    Check 'direct-dash fixture source created through the real admin API' (
        $createDASHSource.Status -eq 200 -and $dashSourceID -gt 0
    )
    if ($dashSourceID -le 0) {
        throw "direct-dash source creation failed: $(Get-ResponseSummary $createDASHSource)"
    }

    $createDASHLibrary = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries" `
        -Body @{
            name = 'playback-e2e-direct-dash-library'
            type = 'movie'
            source_id = $dashSourceID
            path = '/'
            providers = '[]'
        }
    $dashLibraryID = [long](Get-Field $createDASHLibrary.Payload 'id')
    Check 'direct-dash fixture library created' (
        $createDASHLibrary.Status -eq 200 -and $dashLibraryID -gt 0
    )
    if ($dashLibraryID -le 0) {
        throw "direct-dash library creation failed: $(Get-ResponseSummary $createDASHLibrary)"
    }

    $dashScan = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries/$dashLibraryID/scan"
    Check 'direct-dash fixture scan started' (
        $dashScan.Status -eq 200 -and (Get-Field $dashScan.Payload 'ok') -eq $true
    )
    if ($dashScan.Status -ne 200) {
        throw "direct-dash scan start failed: $(Get-ResponseSummary $dashScan)"
    }
    $dashScanState = Wait-ScanDone -LibraryID $dashLibraryID
    Check 'direct-dash fixture scan reached the done phase' (
        (Get-Field $dashScanState 'phase') -eq 'done' -and
        (Get-Field $dashScanState 'running') -eq $false
    )
    $dashLibraryFiles = @(Find-LibraryFiles -LibraryID $dashLibraryID)
    $dashFile = $dashLibraryFiles |
        Where-Object { $_.Path -eq '/episode.mp4' } |
        Select-Object -First 1
    Check 'direct-dash fixture exposes the multipart episode' (
        $null -ne $dashFile -and [long]$dashFile.ID -gt 0
    )
    if ($null -eq $dashFile) {
        throw 'direct-dash fixture was not indexed'
    }

    $createRemoteSource = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/sources" `
        -Body @{
            name = 'playback-e2e-webdav'
            type = 'webdav'
            config = @{
                webdav_url = $remoteWebDAVBase
                webdav_user = $remoteWebDAVUser
                webdav_pass = $remoteWebDAVPass
            }
        }
    $remoteSourceID = [long](Get-Field $createRemoteSource.Payload 'id')
    Check 'remote WebDAV source created' (
        $createRemoteSource.Status -eq 200 -and $remoteSourceID -gt 0
    )
    if ($remoteSourceID -le 0) {
        throw "remote WebDAV source creation failed: $(Get-ResponseSummary $createRemoteSource)"
    }

    $createRemoteLibrary = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries" `
        -Body @{
            name = 'playback-e2e-remote-library'
            type = 'movie'
            source_id = $remoteSourceID
            path = '/'
            providers = '[]'
        }
    $remoteLibraryID = [long](Get-Field $createRemoteLibrary.Payload 'id')
    Check 'remote WebDAV movie library created' (
        $createRemoteLibrary.Status -eq 200 -and $remoteLibraryID -gt 0
    )
    if ($remoteLibraryID -le 0) {
        throw "remote WebDAV library creation failed: $(Get-ResponseSummary $createRemoteLibrary)"
    }

    $remoteScan = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/libraries/$remoteLibraryID/scan"
    Check 'remote WebDAV library scan started' (
        $remoteScan.Status -eq 200 -and (Get-Field $remoteScan.Payload 'ok') -eq $true
    )
    if ($remoteScan.Status -ne 200) {
        throw "remote WebDAV scan start failed: $(Get-ResponseSummary $remoteScan)"
    }
    $remoteScanState = Wait-ScanDone -LibraryID $remoteLibraryID
    Check 'remote WebDAV library scan reached the done phase' (
        (Get-Field $remoteScanState 'phase') -eq 'done' -and
        (Get-Field $remoteScanState 'running') -eq $false
    )
    $remoteLibraryFiles = @(Find-LibraryFiles -LibraryID $remoteLibraryID)
    $remoteFile = $remoteLibraryFiles |
        Where-Object { $_.Path -eq '/remote.mp4' } |
        Select-Object -First 1
    $ignoredRangeFile = $remoteLibraryFiles |
        Where-Object { $_.Path -eq '/ignored-range.mp4' } |
        Select-Object -First 1
    $badPartialFile = $remoteLibraryFiles |
        Where-Object { $_.Path -eq '/bad-partial.mp4' } |
        Select-Object -First 1
    $sourceFailureFile = $remoteLibraryFiles |
        Where-Object { $_.Path -eq '/source-fail.mp4' } |
        Select-Object -First 1
    Check 'remote WebDAV library exposes the range fixture' (
        $null -ne $remoteFile -and [long]$remoteFile.ID -gt 0
    )
    Check 'remote WebDAV library exposes ignored-range and malformed-206 fixtures' (
        $null -ne $ignoredRangeFile -and [long]$ignoredRangeFile.ID -gt 0 -and
        $null -ne $badPartialFile -and [long]$badPartialFile.ID -gt 0
    )
    Check 'remote WebDAV library exposes the source-failure fixture' (
        $null -ne $sourceFailureFile -and [long]$sourceFailureFile.ID -gt 0
    )
    if ($null -eq $remoteFile -or $null -eq $ignoredRangeFile -or
        $null -eq $badPartialFile -or $null -eq $sourceFailureFile) {
        $indexedPaths = @($remoteLibraryFiles | ForEach-Object { $_.Path }) -join ', '
        Write-Output "[INFO] remote WebDAV indexed paths: $indexedPaths"
        Write-Output ("[INFO] remote WebDAV scan: total={0} walk_errors={1} first_error={2}" -f `
            (Get-Field $remoteScanState 'total'),
            (Get-Field $remoteScanState 'walk_errors'),
            (Get-Field $remoteScanState 'first_error'))
        if (Test-Path -LiteralPath $webDAVFixtureErrLog -PathType Leaf) {
            Write-Output '[INFO] remote WebDAV fixture requests:'
            Get-Content -LiteralPath $webDAVFixtureErrLog |
                ForEach-Object { Write-Output "  $_" }
        }
        throw 'remote WebDAV fixtures were not indexed as expected'
    }

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
    Test-ResponseHeaderSafety -Name 'playback ticket creation' -Response $ticketCreate `
        -AllowedLocations @($proxyTicketPath)
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
    Test-ResponseHeaderSafety -Name 'playback ticket status' -Response $ready
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
    Test-ResponseHeaderSafety -Name 'playback ticket refresh' -Response $proxyRefresh
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
    $rangeHeaderText = Get-AllResponseHeaderText $rangeResponse
    Test-NoSensitiveValues -Name 'media Range body and headers contain no source secret' `
        -Text ($rangeResponse.Body + "`n" + $rangeHeaderText) -ExtraValues @($sourceSecret)
    Test-ResponseHeaderSafety -Name 'media Range' -Response $rangeResponse `
        -ExtraValues @($sourceSecret)

    $headResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Head
    Test-ResponseHeaderSafety -Name 'media HEAD' -Response $headResponse `
        -ExtraValues @($sourceSecret)
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
    Test-ResponseHeaderSafety -Name 'media bytes=0-0' -Response $firstByteResponse `
        -ExtraValues @($sourceSecret)
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
    Test-ResponseHeaderSafety -Name 'media unsatisfiable Range' `
        -Response $unsatisfiableResponse -ExtraValues @($sourceSecret)
    Check 'media capability rejects an unsatisfiable range with exact Content-Range' (
        $unsatisfiableResponse.Status -eq 416 -and
        (Get-ResponseHeader $unsatisfiableResponse 'Content-Range') -eq
            "bytes */$($mediaBytes.Length)"
    )

    $ifRangeMatch = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-0' -IfRange $mediaValidator
    Test-ResponseHeaderSafety -Name 'media matching If-Range' `
        -Response $ifRangeMatch -ExtraValues @($sourceSecret)
    Check 'media capability honors a matching If-Range validator' (
        $ifRangeMatch.Status -eq 206 -and
        $ifRangeMatch.Bytes.Length -eq 1 -and
        (Get-ResponseHeader $ifRangeMatch 'Content-Range') -eq
            "bytes 0-0/$($mediaBytes.Length)" -and
        (Get-ResponseHeader $ifRangeMatch 'ETag') -eq $mediaValidator
    )

    $ifRangeMismatch = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $localMediaURL -Range 'bytes=0-0' -IfRange '"playback-e2e-stale"'
    Test-ResponseHeaderSafety -Name 'media stale If-Range' `
        -Response $ifRangeMismatch -ExtraValues @($sourceSecret)
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
    Test-ResponseHeaderSafety -Name 'subtitle GET' -Response $subtitleGet `
        -ExtraValues @($sourceSecret)
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
    Test-ResponseHeaderSafety -Name 'subtitle HEAD' -Response $subtitleHead `
        -ExtraValues @($sourceSecret)
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
    Test-ResponseHeaderSafety -Name 'server-HLS ticket refresh' -Response $hlsRefresh
    $hlsTransport = Get-Field $hlsRefresh.Payload 'transport'
    $hlsStreams = @(Get-Field $hlsTransport 'streams')
    $hlsStream = $hlsStreams | Select-Object -First 1
    $hlsMediaURL = [string](Get-Field $hlsStream 'url')
    $hlsCapability = Get-MediaCapability $hlsMediaURL
    $hlsLocalURL = "$base/api/v1/playback/media-sessions/$hlsCapability/index.m3u8"
    $hlsMediaURI = $null
    try {
        $hlsMediaURI = [Uri]$hlsMediaURL
    } catch {
    }
    Check 'second refresh falls back to server-hls ticket state' (
        $hlsRefresh.Status -eq 200 -and
        (Get-Field $hlsRefresh.Payload 'state') -eq 'consuming' -and
        (Get-Field $hlsTransport 'kind') -eq 'server-hls' -and
        $hlsMediaURL.EndsWith('/index.m3u8', [System.StringComparison]::Ordinal) -and
        @(Get-Field $hlsRefresh.Payload 'remaining_transports').Count -eq 0
    )
    Check 'server-HLS issuance is capability-addressed without query or fragment' (
        $hlsCapability.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $null -ne $hlsMediaURI -and
        $hlsMediaURI.AbsolutePath -eq
            "/api/v1/playback/media-sessions/$hlsCapability/index.m3u8" -and
        $hlsMediaURI.Query -eq '' -and
        $hlsMediaURI.Fragment -eq ''
    )
    Test-NoSensitiveValues -Name 'HLS issuance does not expose source URL or source secret' `
        -Text $hlsRefresh.Body -ExtraValues @($sourceSecret)
    Add-SensitiveValue $hlsCapability

    $hlsPlaylistQueryTamper = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "${hlsLocalURL}?m=remux-aac&a=2&start=99"
    Check 'server-HLS playlist capability rejects query parameters' (
        $hlsPlaylistQueryTamper.Status -eq 400 -and
        (Get-ErrorCode $hlsPlaylistQueryTamper.Payload) -eq 'invalid_request'
    )

    $hlsHead = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Head) `
        -Uri $hlsLocalURL
    Test-ResponseHeaderSafety -Name 'server-HLS playlist HEAD' -Response $hlsHead `
        -ExtraValues @($sourceSecret)
    Check 'server-HLS playlist rejects HEAD without a body' (
        $hlsHead.Status -eq 405 -and
        $hlsHead.Body.Length -eq 0 -and
        (Get-ResponseHeader $hlsHead 'Allow') -eq 'GET'
    )

    $hlsPlaylist = Invoke-Json -WebClient $mediaClient `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri $hlsLocalURL
    Test-ResponseHeaderSafety -Name 'server-HLS playlist' -Response $hlsPlaylist `
        -ExtraValues @($sourceSecret)
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
    $hlsSegmentURI = $null
    if ($hlsSegmentName) {
        $hlsSegmentURI = [System.Uri]::new(
            [System.Uri]$hlsLocalURL, $hlsSegmentName)
        $hlsSegmentURL = $hlsSegmentURI.AbsoluteUri
    }
    $hlsLocalURI = [System.Uri]$hlsLocalURL
    $hlsSegmentPathPattern = '^/api/v1/playback/media-sessions/' +
        [regex]::Escape($hlsCapability) + '/\d+\.ts$'
    Check 'server-HLS segment URI stays on the issued capability without query or fragment' (
        $null -ne $hlsSegmentURI -and
        (Get-MediaCapability $hlsSegmentURL) -eq $hlsCapability -and
        $hlsSegmentURI.Scheme -eq $hlsLocalURI.Scheme -and
        $hlsSegmentURI.Authority -eq $hlsLocalURI.Authority -and
        $hlsSegmentURI.AbsolutePath -match $hlsSegmentPathPattern -and
        $hlsSegmentURI.Query -eq '' -and
        $hlsSegmentURI.Fragment -eq ''
    )
    $hlsSegmentQueryTamper = $null
    if ($hlsSegmentURL) {
        $hlsSegmentQueryTamper = Invoke-Json -WebClient $mediaClient `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri "${hlsSegmentURL}?start=99&m=remux"
    }
    Check 'server-HLS segment capability rejects query parameters' (
        $null -ne $hlsSegmentQueryTamper -and
        $hlsSegmentQueryTamper.Status -eq 400 -and
        (Get-ErrorCode $hlsSegmentQueryTamper.Payload) -eq 'invalid_request'
    )
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
    if ($null -ne $hlsSegmentHead) {
        Test-ResponseHeaderSafety -Name 'server-HLS segment HEAD' `
            -Response $hlsSegmentHead -ExtraValues @($sourceSecret)
    }

    if ($script:hasDecodableMediaFixture -and $hlsSegmentURL) {
        $hlsSegment = Invoke-Bytes -WebClient $mediaClient `
            -Method ([System.Net.Http.HttpMethod]::Get) `
            -Uri $hlsSegmentURL
        Test-ResponseHeaderSafety -Name 'server-HLS segment' `
            -Response $hlsSegment -ExtraValues @($sourceSecret)
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

    $remotePayloadSize = 2 * 1024 * 1024
    $fixtureClient = New-WebClient
    $fixtureHead = Invoke-Json -WebClient $fixtureClient `
        -Method ([System.Net.Http.HttpMethod]::Head) `
        -Uri "$remoteWebDAVBase/remote.mp4" `
        -Headers @{ Authorization = $remoteBasicAuth }
    $missingCanaryHeaders = @(
        'Authorization',
        'Proxy-Authorization',
        'Cookie',
        'Set-Cookie',
        'WWW-Authenticate',
        'Location',
        'X-Source-Secret',
        'X-Source-Token',
        'Content-Disposition'
    ) | Where-Object {
        [string]::IsNullOrEmpty((Get-ResponseHeader $fixtureHead $_))
    }
    Check 'remote WebDAV fixture emits the complete sensitive-header canary set' (
        $fixtureHead.Status -eq 200 -and
        $fixtureHead.Body.Length -eq 0 -and
        @($missingCanaryHeaders).Count -eq 0
    )

    $remoteSession = Open-ServerProxySession -Device $deviceTwo `
        -FileID $remoteFile.ID -Label 'remote WebDAV range fixture'
    $remoteFull = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL
    $remoteFullLength = [long]-1
    $remoteFullLengthRaw = Get-ResponseHeader $remoteFull 'Content-Length'
    if (-not [long]::TryParse($remoteFullLengthRaw, [ref]$remoteFullLength)) {
        $remoteFullLength = -1
    }
    Check 'remote server-proxy full GET returns a sanitized 200 representation' (
        $remoteFull.Status -eq 200 -and
        $remoteFull.Bytes.Length -eq $remotePayloadSize -and
        $remoteFullLength -eq $remotePayloadSize -and
        (Get-ResponseHeader $remoteFull 'Content-Range') -eq '' -and
        (Test-PatternBytes -Bytes $remoteFull.Bytes -Offset 0 -Count $remotePayloadSize)
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy full GET' `
        -Response $remoteFull

    $remoteHead = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Head
    $remoteHeadLength = [long]-1
    $remoteHeadLengthRaw = Get-ResponseHeader $remoteHead 'Content-Length'
    if (-not [long]::TryParse($remoteHeadLengthRaw, [ref]$remoteHeadLength)) {
        $remoteHeadLength = -1
    }
    $remoteValidator = Get-ResponseHeader $remoteHead 'ETag'
    Check 'remote server-proxy HEAD is bodyless with validator and full length' (
        $remoteHead.Status -eq 200 -and
        $remoteHead.Bytes.Length -eq 0 -and
        $remoteHeadLength -eq $remotePayloadSize -and
        $remoteValidator -eq '"webdav-fixture-v1"' -and
        (Get-ResponseHeader $remoteHead 'Accept-Ranges') -eq 'bytes' -and
        (Get-ResponseHeader $remoteHead 'Last-Modified') -ne ''
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy HEAD' `
        -Response $remoteHead

    $remoteRangeStart = 65536
    $remoteRangeLength = 128
    $remoteRangeEnd = $remoteRangeStart + $remoteRangeLength - 1
    $remoteRange = "bytes=$remoteRangeStart-$remoteRangeEnd"
    $remoteRangeResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Range $remoteRange
    Check 'remote server-proxy forwards Range and returns exact 206 bytes' (
        $remoteRangeResponse.Status -eq 206 -and
        (Get-ResponseHeader $remoteRangeResponse 'Content-Range') -eq
            "bytes $remoteRangeStart-$remoteRangeEnd/$remotePayloadSize" -and
        (Get-ResponseHeader $remoteRangeResponse 'Content-Length') -eq
            "$remoteRangeLength" -and
        (Test-PatternBytes -Bytes $remoteRangeResponse.Bytes `
            -Offset $remoteRangeStart -Count $remoteRangeLength)
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy Range' `
        -Response $remoteRangeResponse

    $remoteFirstByte = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Range 'bytes=0-0'
    Check 'remote server-proxy bytes=0-0 returns one byte' (
        $remoteFirstByte.Status -eq 206 -and
        $remoteFirstByte.Bytes.Length -eq 1 -and
        $remoteFirstByte.Bytes[0] -eq 0 -and
        (Get-ResponseHeader $remoteFirstByte 'Content-Range') -eq
            "bytes 0-0/$remotePayloadSize"
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy bytes=0-0' `
        -Response $remoteFirstByte

    $remoteUnsatisfiable = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Range "bytes=$remotePayloadSize-"
    Check 'remote server-proxy maps an unsatisfiable source range to 416' (
        $remoteUnsatisfiable.Status -eq 416 -and
        (Get-ErrorCode $remoteUnsatisfiable.Payload) -eq 'invalid_request' -and
        (Get-ResponseHeader $remoteUnsatisfiable 'Content-Range') -eq
            "bytes */$remotePayloadSize"
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy unsatisfiable Range' `
        -Response $remoteUnsatisfiable

    $remoteIfRangeMatch = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Range 'bytes=0-15' -IfRange $remoteValidator
    Check 'remote server-proxy forwards a matching If-Range and returns 206' (
        $remoteIfRangeMatch.Status -eq 206 -and
        $remoteIfRangeMatch.Bytes.Length -eq 16 -and
        (Test-PatternBytes -Bytes $remoteIfRangeMatch.Bytes -Offset 0 -Count 16) -and
        (Get-ResponseHeader $remoteIfRangeMatch 'ETag') -eq $remoteValidator
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy matching If-Range' `
        -Response $remoteIfRangeMatch

    $remoteIfRangeStale = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $remoteSession.LocalURL -Range 'bytes=0-15' -IfRange '"stale-remote"'
    $remoteIfRangeStaleLength = [long]-1
    $remoteIfRangeStaleLengthRaw = Get-ResponseHeader $remoteIfRangeStale 'Content-Length'
    if (-not [long]::TryParse(
        $remoteIfRangeStaleLengthRaw,
        [ref]$remoteIfRangeStaleLength
    )) {
        $remoteIfRangeStaleLength = -1
    }
    Check 'remote server-proxy passes through stale If-Range as a full 200' (
        $remoteIfRangeStale.Status -eq 200 -and
        $remoteIfRangeStale.Bytes.Length -eq $remotePayloadSize -and
        $remoteIfRangeStaleLength -eq $remotePayloadSize -and
        (Get-ResponseHeader $remoteIfRangeStale 'Content-Range') -eq '' -and
        (Test-PatternBytes -Bytes $remoteIfRangeStale.Bytes `
            -Offset 0 -Count $remotePayloadSize)
    )
    Test-ResponseHeaderSafety -Name 'remote server-proxy stale If-Range' `
        -Response $remoteIfRangeStale

    $ignoredRangeSession = Open-ServerProxySession -Device $deviceTwo `
        -FileID $ignoredRangeFile.ID -Label 'remote ignored-Range fixture'
    $ignoredRangeStart = 1234
    $ignoredRangeLength = 67
    $ignoredRangeEnd = $ignoredRangeStart + $ignoredRangeLength - 1
    $ignoredRangeResponse = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $ignoredRangeSession.LocalURL `
        -Range "bytes=$ignoredRangeStart-$ignoredRangeEnd"
    Check 'remote server-proxy safely slices an upstream 200 that ignores Range' (
        $ignoredRangeResponse.Status -eq 206 -and
        (Get-ResponseHeader $ignoredRangeResponse 'Content-Range') -eq
            "bytes $ignoredRangeStart-$ignoredRangeEnd/$remotePayloadSize" -and
        (Test-PatternBytes -Bytes $ignoredRangeResponse.Bytes `
            -Offset $ignoredRangeStart -Count $ignoredRangeLength)
    )
    Test-ResponseHeaderSafety -Name 'remote ignored-Range response' `
        -Response $ignoredRangeResponse

    $badPartialSession = Open-ServerProxySession -Device $deviceTwo `
        -FileID $badPartialFile.ID -Label 'remote malformed-206 fixture'
    $badPartial = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $badPartialSession.LocalURL -Range 'bytes=0-0'
    Check 'remote malformed upstream 206 maps to sanitized 502' (
        $badPartial.Status -eq 502 -and
        (Get-ErrorCode $badPartial.Payload) -eq 'source_unavailable' -and
        $badPartial.Body.IndexOf(
            $remoteCanary,
            [System.StringComparison]::Ordinal
        ) -lt 0
    )
    Test-ResponseHeaderSafety -Name 'remote malformed upstream 206' `
        -Response $badPartial

    $sourceFailureSession = Open-ServerProxySession -Device $deviceTwo `
        -FileID $sourceFailureFile.ID -Label 'remote source-error fixture'
    $sourceFailure = Invoke-MediaRange -WebClient $mediaClient `
        -Uri $sourceFailureSession.LocalURL
    Check 'remote source-stage failure maps to sanitized 502' (
        $sourceFailure.Status -eq 502 -and
        (Get-ErrorCode $sourceFailure.Payload) -eq 'source_unavailable' -and
        $sourceFailure.Body.IndexOf(
            $remoteCanary,
            [System.StringComparison]::Ordinal
        ) -lt 0
    )
    Test-ResponseHeaderSafety -Name 'remote source-stage failure' `
        -Response $sourceFailure
    Test-NoSensitiveValues -Name 'remote failure bodies contain no source canary' `
        -Text ($badPartial.Body + "`n" + $sourceFailure.Body)

    Stop-WebDAVFixture
    $fixtureLogText = ''
    if (Test-Path -LiteralPath $webDAVFixtureErrLog -PathType Leaf) {
        $fixtureLogText = [System.IO.File]::ReadAllText($webDAVFixtureErrLog)
    }
    Check 'remote WebDAV fixture observed proxied Range, HEAD, and If-Range requests' (
        $fixtureLogText.Contains('method=HEAD path=/dav/remote.mp4') -and
        $fixtureLogText.Contains("method=GET path=/dav/remote.mp4 range=`"$remoteRange`"") -and
        $fixtureLogText.Contains('if_range="\"webdav-fixture-v1\""') -and
        $fixtureLogText.Contains(
            "method=GET path=/dav/ignored-range.mp4 range=`"bytes=$ignoredRangeStart-$ignoredRangeEnd`""
        ) -and
        $fixtureLogText.Contains('method=GET path=/dav/bad-partial.mp4') -and
        $fixtureLogText.Contains('method=GET path=/dav/source-fail.mp4')
    )

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
    Test-ResponseHeaderSafety -Name 'direct ticket creation' -Response $directCreate `
        -AllowedLocations @($directTicketPath)
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
    Test-ResponseHeaderSafety -Name 'direct ticket refresh' -Response $directRefresh
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

    Set-DASHMode valid
    $dashNoCapability = Open-DASHTicket -Device $deviceOne -FileID $dashFile.ID `
        -Label 'DASH merge omitted' -DASHMerge omit
    $dashNoCapabilityTransport = Get-Field $dashNoCapability.Issued.Payload 'transport'
    $dashNoCapabilityStreams = @(Get-Field $dashNoCapabilityTransport 'streams')
    $dashNoCapabilityStream = $dashNoCapabilityStreams | Select-Object -First 1
    $dashNoCapabilityURL = [string](Get-Field $dashNoCapabilityStream 'url')
    $dashNoCapabilityMedia = Get-MediaCapability $dashNoCapabilityURL
    $dashNoCapabilityRemaining = @(
        Get-Field $dashNoCapability.Issued.Payload 'remaining_transports')
    Check 'omitting supports_dash_merge cannot issue direct-dash' (
        $dashNoCapability.Issued.Status -eq 201 -and
        (Get-Field $dashNoCapability.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashNoCapabilityTransport 'kind') -eq 'server-hls' -and
        $null -ne (Get-Field $dashNoCapabilityTransport 'media_session') -and
        $dashNoCapabilityStreams.Count -eq 1 -and
        (Get-Field $dashNoCapabilityStream 'role') -eq 'muxed' -and
        $dashNoCapabilityMedia.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $dashNoCapabilityURL.EndsWith(
            '/index.m3u8', [System.StringComparison]::Ordinal) -and
        $dashNoCapabilityRemaining.Count -eq 0
    )
    Test-NoSensitiveValues -Name 'omitted-capability HLS response hides DASH URLs and headers' `
        -Text $dashNoCapability.Issued.Body
    Add-SensitiveValue $dashNoCapabilityMedia

    $dashExplicitFalse = Open-DASHTicket -Device $deviceOne -FileID $dashFile.ID `
        -Label 'DASH merge false' -DASHMerge false
    $dashExplicitFalseTransport = Get-Field $dashExplicitFalse.Issued.Payload 'transport'
    $dashExplicitFalseStreams = @(Get-Field $dashExplicitFalseTransport 'streams')
    $dashExplicitFalseStream = $dashExplicitFalseStreams | Select-Object -First 1
    $dashExplicitFalseURL = [string](Get-Field $dashExplicitFalseStream 'url')
    $dashExplicitFalseMedia = Get-MediaCapability $dashExplicitFalseURL
    $dashExplicitFalseRemaining = @(
        Get-Field $dashExplicitFalse.Issued.Payload 'remaining_transports')
    Check 'supports_dash_merge=false cannot issue direct-dash' (
        $dashExplicitFalse.Issued.Status -eq 201 -and
        (Get-Field $dashExplicitFalse.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashExplicitFalseTransport 'kind') -eq 'server-hls' -and
        $null -ne (Get-Field $dashExplicitFalseTransport 'media_session') -and
        $dashExplicitFalseStreams.Count -eq 1 -and
        (Get-Field $dashExplicitFalseStream 'role') -eq 'muxed' -and
        $dashExplicitFalseMedia.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $dashExplicitFalseURL.EndsWith(
            '/index.m3u8', [System.StringComparison]::Ordinal) -and
        $dashExplicitFalseRemaining.Count -eq 0
    )
    Test-NoSensitiveValues -Name 'explicit-false HLS response hides DASH URLs and headers' `
        -Text $dashExplicitFalse.Issued.Body
    Add-SensitiveValue $dashExplicitFalseMedia

    $dashDeviceFeaturePath = '/api/admin/client-devices/' +
        [uri]::EscapeDataString($deviceTwo.DeviceID) + '/features'
    $dashDeviceDisabled = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Put) `
        -Uri "$base$dashDeviceFeaturePath" `
        -Body @{ direct_dash_disabled = $true }
    Check 'admin can disable direct-dash for one client device' (
        $dashDeviceDisabled.Status -eq 200 -and
        (Get-Field (Get-Field $dashDeviceDisabled.Payload 'overrides') 'direct_dash_disabled') -eq $true
    )
    $dashDeviceOverride = Open-DASHTicket -Device $deviceTwo -FileID $dashFile.ID `
        -Label 'device direct-dash disabled' -DASHMerge true
    $dashDeviceOverrideTransport = Get-Field $dashDeviceOverride.Issued.Payload 'transport'
    $dashDeviceOverrideStreams = @(Get-Field $dashDeviceOverrideTransport 'streams')
    $dashDeviceOverrideStream = $dashDeviceOverrideStreams | Select-Object -First 1
    $dashDeviceOverrideURL = [string](Get-Field $dashDeviceOverrideStream 'url')
    $dashDeviceOverrideMedia = Get-MediaCapability $dashDeviceOverrideURL
    $dashDeviceOverrideRemaining = @(
        Get-Field $dashDeviceOverride.Issued.Payload 'remaining_transports')
    Check 'device override forces server-HLS even with DASH merge support' (
        $dashDeviceOverride.Issued.Status -eq 201 -and
        (Get-Field $dashDeviceOverride.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashDeviceOverrideTransport 'kind') -eq 'server-hls' -and
        $null -ne (Get-Field $dashDeviceOverrideTransport 'media_session') -and
        $dashDeviceOverrideStreams.Count -eq 1 -and
        (Get-Field $dashDeviceOverrideStream 'role') -eq 'muxed' -and
        $dashDeviceOverrideMedia.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $dashDeviceOverrideURL.EndsWith(
            '/index.m3u8', [System.StringComparison]::Ordinal) -and
        $dashDeviceOverrideRemaining.Count -eq 0
    )
    Test-NoSensitiveValues -Name 'device-override HLS response hides DASH URLs and headers' `
        -Text $dashDeviceOverride.Issued.Body
    Add-SensitiveValue $dashDeviceOverrideMedia

    Set-DASHMode valid
    $dashValid = Open-DASHTicket -Device $deviceOne -FileID $dashFile.ID `
        -Label 'direct-dash valid' -DASHMerge true
    $dashValidTransport = Get-Field $dashValid.Issued.Payload 'transport'
    $dashValidStreams = @(Get-Field $dashValidTransport 'streams')
    $dashValidRemaining = @(Get-Field $dashValid.Issued.Payload 'remaining_transports')
    $dashVideoStream = $dashValidStreams | Select-Object -First 1
    $dashAudioStream = $dashValidStreams | Select-Object -Skip 1 -First 1
    $dashVideoIssuedURL = [string](Get-Field $dashVideoStream 'url')
    $dashAudioIssuedURL = [string](Get-Field $dashAudioStream 'url')
    $dashHeaderNames = @(
        $dashValidStreams | ForEach-Object {
            $headers = Get-Field $_ 'headers'
            if ($null -ne $headers) {
                $headers.PSObject.Properties.Name
            }
        }
    )
    $allowedDASHHeaders = @(
        'Referer', 'Origin', 'User-Agent', 'Accept', 'Accept-Language'
    )
    $unexpectedDASHHeaders = @($dashHeaderNames |
        Where-Object { $allowedDASHHeaders -notcontains $_ })
    Check 'direct-dash still works on an unaffected device while another is overridden' (
        $dashValid.Issued.Status -eq 201 -and
        (Get-Field $dashValid.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashValidTransport 'kind') -eq 'direct-dash' -and
        (Test-FieldAbsent $dashValidTransport 'media_session') -and
        $dashValidStreams.Count -eq 2 -and
        (Get-Field $dashVideoStream 'role') -eq 'video' -and
        (Get-Field $dashAudioStream 'role') -eq 'audio' -and
        (Get-Field $dashVideoStream 'mime') -eq 'video/mp4' -and
        (Get-Field $dashAudioStream 'mime') -eq 'audio/mp4' -and
        (Get-Field $dashVideoStream 'range') -eq $false -and
        (Get-Field $dashAudioStream 'range') -eq $false -and
        ([uri]$dashVideoIssuedURL).Scheme -eq 'https' -and
        ([uri]$dashAudioIssuedURL).Scheme -eq 'https' -and
        $dashVideoIssuedURL -eq $dashVideoURL -and
        $dashAudioIssuedURL -eq $dashAudioURL -and
        $dashVideoIssuedURL -ne $dashAudioIssuedURL -and
        $dashValidRemaining.Count -eq 1 -and
        $dashValidRemaining -contains 'server-hls'
    )
    Check 'direct-dash exposes only the reviewed Bilibili request headers' (
        $unexpectedDASHHeaders.Count -eq 0 -and
        $dashHeaderNames.Count -eq 4 -and
        (Get-Field (Get-Field $dashVideoStream 'headers') 'Referer') -eq 'https://www.bilibili.com/' -and
        (Get-Field (Get-Field $dashAudioStream 'headers') 'Referer') -eq 'https://www.bilibili.com/' -and
        [string](Get-Field (Get-Field $dashVideoStream 'headers') 'User-Agent') -like 'Mozilla/5.0*' -and
        [string](Get-Field (Get-Field $dashAudioStream 'headers') 'User-Agent') -like 'Mozilla/5.0*'
    )
    Check 'direct-dash response never contains Cookie or Authorization credentials' (
        $dashValid.Issued.Body.IndexOf(
            $dashCookieCanary, [System.StringComparison]::Ordinal) -lt 0 -and
        $dashValid.Issued.Body.IndexOf(
            $dashAuthorizationCanary, [System.StringComparison]::Ordinal) -lt 0
    )
    $dashValidStatus = Invoke-Json -WebClient $deviceOne.Client `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base$($dashValid.TicketPath)" `
        -Bearer $deviceOne.Access
    Check 'direct-dash ticket status omits the one-time transport' (
        $dashValidStatus.Status -eq 200 -and
        (Test-FieldAbsent $dashValidStatus.Payload 'transport')
    )
    Test-NoSensitiveValues -Name 'direct-dash status hides source URLs and source headers' `
        -Text $dashValidStatus.Body

    $dashDeviceRestored = Invoke-Json -WebClient $web `
        -Method ([System.Net.Http.HttpMethod]::Put) `
        -Uri "$base$dashDeviceFeaturePath" `
        -Body @{}
    Check 'admin can clear the direct-dash device override' (
        $dashDeviceRestored.Status -eq 200 -and
        (Get-Field (Get-Field $dashDeviceRestored.Payload 'overrides') 'direct_dash_disabled') -eq $false
    )
    $dashRestored = Open-DASHTicket -Device $deviceTwo -FileID $dashFile.ID `
        -Label 'direct-dash device override restored' -DASHMerge true
    $dashRestoredTransport = Get-Field $dashRestored.Issued.Payload 'transport'
    Check 'clearing the device override restores direct-dash on that device' (
        $dashRestored.Issued.Status -eq 201 -and
        (Get-Field $dashRestored.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashRestoredTransport 'kind') -eq 'direct-dash' -and
        (Test-FieldAbsent $dashRestoredTransport 'media_session')
    )

    Set-DASHMode valid-probe-and-select-then-bad-header
    $dashFallback = Open-DASHTicket -Device $deviceOne -FileID $dashFile.ID `
        -Label 'direct-dash claim-time unsafe-header fallback' -DASHMerge true
    $dashFallbackTransport = Get-Field $dashFallback.Issued.Payload 'transport'
    $dashFallbackStreams = @(Get-Field $dashFallbackTransport 'streams')
    $dashFallbackStream = $dashFallbackStreams | Select-Object -First 1
    $dashFallbackURL = [string](Get-Field $dashFallbackStream 'url')
    $dashFallbackMedia = Get-MediaCapability $dashFallbackURL
    $dashFallbackRemaining = @(Get-Field $dashFallback.Issued.Payload 'remaining_transports')
    Check 'claim-time direct-dash rejection falls back atomically to server-HLS' (
        $dashFallback.Issued.Status -eq 201 -and
        (Get-Field $dashFallback.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashFallbackTransport 'kind') -eq 'server-hls' -and
        $null -ne (Get-Field $dashFallbackTransport 'media_session') -and
        $dashFallbackStreams.Count -eq 1 -and
        (Get-Field $dashFallbackStream 'role') -eq 'muxed' -and
        $dashFallbackMedia.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $dashFallbackURL.EndsWith('/index.m3u8', [System.StringComparison]::Ordinal) -and
        $dashFallbackRemaining.Count -eq 0
    )
    Test-NoSensitiveValues -Name 'direct-dash fallback hides invalid source headers and URLs' `
        -Text $dashFallback.Issued.Body
    Add-SensitiveValue $dashFallbackMedia

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

    Set-DASHMode valid
    $dashFlagOff = Open-DASHTicket -Device $deviceTwo -FileID $dashFile.ID `
        -Label 'direct-dash runtime flag off' -DASHMerge true
    $dashFlagOffTransport = Get-Field $dashFlagOff.Issued.Payload 'transport'
    $dashFlagOffStreams = @(Get-Field $dashFlagOffTransport 'streams')
    $dashFlagOffStream = $dashFlagOffStreams | Select-Object -First 1
    $dashFlagOffURL = [string](Get-Field $dashFlagOffStream 'url')
    $dashFlagOffMedia = Get-MediaCapability $dashFlagOffURL
    $dashFlagOffRemaining = @(Get-Field $dashFlagOff.Issued.Payload 'remaining_transports')
    Check 'direct-dash runtime flag off forces server-HLS' (
        $dashFlagOff.Issued.Status -eq 201 -and
        (Get-Field $dashFlagOff.Issued.Payload 'state') -eq 'consuming' -and
        (Get-Field $dashFlagOffTransport 'kind') -eq 'server-hls' -and
        $null -ne (Get-Field $dashFlagOffTransport 'media_session') -and
        $dashFlagOffStreams.Count -eq 1 -and
        (Get-Field $dashFlagOffStream 'role') -eq 'muxed' -and
        $dashFlagOffMedia.StartsWith('ms_', [System.StringComparison]::Ordinal) -and
        $dashFlagOffURL.EndsWith('/index.m3u8', [System.StringComparison]::Ordinal) -and
        $dashFlagOffRemaining.Count -eq 0
    )
    Test-NoSensitiveValues -Name 'flag-off HLS response hides DASH URLs and headers' `
        -Text $dashFlagOff.Issued.Body
    Add-SensitiveValue $dashFlagOffMedia

    $fallbackCreate = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceTwo.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceTwo.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    if ((Get-Field $fallbackCreate.Payload 'ticket_id')) {
        $fallbackTicketPath = '/api/v1/playback/tickets/' +
            [uri]::EscapeDataString([string](Get-Field $fallbackCreate.Payload 'ticket_id'))
        Test-ResponseHeaderSafety -Name 'fallback ticket issuance' `
            -Response $fallbackCreate -AllowedLocations @($fallbackTicketPath)
    } else {
        Test-ResponseHeaderSafety -Name 'fallback ticket issuance' `
            -Response $fallbackCreate
    }
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
    Write-Config -TicketEnabled $true -DirectEnabled $false -ProxyEnabled $false `
        -HLSEnabled $false -DirectDASHEnabled $true
    Check 'server restart: all playback transports disabled' (
        Start-Server -Phase 'all-transports-disabled'
    )
    if ($null -eq $script:serverProcess -or $script:serverProcess.HasExited) {
        throw 'server did not restart with all transports disabled'
    }

    Set-DASHMode single
    $dashUnsupported = Open-DASHTicket -Device $deviceTwo -FileID $dashFile.ID `
        -Label 'direct-dash without server HLS' -DASHMerge true
    Check 'direct-dash without two streams and without server-HLS fails closed' (
        $dashUnsupported.Issued.Status -eq 201 -and
        (Get-Field $dashUnsupported.Issued.Payload 'state') -eq 'unsupported' -and
        (Get-Field $dashUnsupported.Issued.Payload 'failure_code') -eq 'unsupported_media' -and
        -not [string]::IsNullOrWhiteSpace(
            [string](Get-Field $dashUnsupported.Issued.Payload 'message')) -and
        (Test-FieldAbsent $dashUnsupported.Issued.Payload 'transport') -and
        @(Get-Field $dashUnsupported.Issued.Payload 'remaining_transports').Count -eq 0
    )
    $dashUnsupportedRefresh = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base$($dashUnsupported.TicketPath)/refresh" `
        -Bearer $deviceTwo.Access
    $dashUnsupportedDetails = Get-ErrorDetails $dashUnsupportedRefresh.Payload
    Check 'unsupported direct-dash ticket is terminal and requests a new ticket' (
        $dashUnsupportedRefresh.Status -eq 410 -and
        (Get-ErrorCode $dashUnsupportedRefresh.Payload) -eq 'ticket_expired' -and
        (Get-Field $dashUnsupportedDetails 'state') -eq 'unsupported' -and
        (Get-Field $dashUnsupportedDetails 'action') -eq 'request_new_ticket' -and
        (Get-Field $dashUnsupportedDetails 'ticket_id') -eq $dashUnsupported.TicketID -and
        (Test-FieldAbsent $dashUnsupportedRefresh.Payload 'transport')
    )
    $dashUnsupportedStatus = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Get) `
        -Uri "$base$($dashUnsupported.TicketPath)" `
        -Bearer $deviceTwo.Access
    Check 'unsupported ticket status remains queryable without a transport' (
        $dashUnsupportedStatus.Status -eq 200 -and
        (Get-Field $dashUnsupportedStatus.Payload 'state') -eq 'unsupported' -and
        (Get-Field $dashUnsupportedStatus.Payload 'failure_code') -eq 'unsupported_media' -and
        -not [string]::IsNullOrWhiteSpace(
            [string](Get-Field $dashUnsupportedStatus.Payload 'message')) -and
        (Test-FieldAbsent $dashUnsupportedStatus.Payload 'transport') -and
        @(Get-Field $dashUnsupportedStatus.Payload 'remaining_transports').Count -eq 0
    )
    Test-NoSensitiveValues -Name 'unsupported direct-dash responses hide source details' `
        -Text (
            $dashUnsupported.Issued.Body + "`n" +
            $dashUnsupportedRefresh.Body + "`n" +
            $dashUnsupportedStatus.Body
        )

    $unsupported = Invoke-Json -WebClient $deviceTwo.Client `
        -Method ([System.Net.Http.HttpMethod]::Post) `
        -Uri "$base/api/v1/playback/tickets" `
        -Bearer $deviceTwo.Access `
        -Body (New-TicketBody -FileID $directFile.ID -DeviceID $deviceTwo.DeviceID `
            -PreferredTransports @('direct-http', 'server-proxy', 'server-hls'))
    if ((Get-Field $unsupported.Payload 'ticket_id')) {
        $unsupportedTicketPath = '/api/v1/playback/tickets/' +
            [uri]::EscapeDataString([string](Get-Field $unsupported.Payload 'ticket_id'))
        Test-ResponseHeaderSafety -Name 'unsupported ticket issuance' `
            -Response $unsupported -AllowedLocations @($unsupportedTicketPath)
    } else {
        Test-ResponseHeaderSafety -Name 'unsupported ticket issuance' `
            -Response $unsupported
    }
    Check 'ticket fails closed when every transport flag is off' (
        $unsupported.Status -eq 201 -and
        (Get-Field $unsupported.Payload 'state') -eq 'unsupported' -and
        (Test-FieldAbsent $unsupported.Payload 'transport') -and
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
    Stop-WebDAVFixture
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
        Check 'claim-time direct-dash failure is observed before atomic HLS fallback' (
            $logText.Contains(
                'event=transport_attempt_failed transport=direct-dash failure_stage=source request_issuance_fallback_count=0') -and
            $logText.Contains(
                'event=transport_issued transport=server-hls request_issuance_fallback_count=1')
        )
        Test-NoSensitiveValues -Name 'server logs contain no tokens, source signature, or capability' `
            -Text $logText -ExtraValues @($sourceSecret, $proxyCapability, $hlsCapability, $fallbackCapability)
        Check 'server logs omit the source host and local absolute media path' (
            $logText.IndexOf('media.invalid', [System.StringComparison]::OrdinalIgnoreCase) -lt 0 -and
            $logText.IndexOf(
                'video.playback-e2e.invalid',
                [System.StringComparison]::OrdinalIgnoreCase
            ) -lt 0 -and
            $logText.IndexOf(
                'audio.playback-e2e.invalid',
                [System.StringComparison]::OrdinalIgnoreCase
            ) -lt 0 -and
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

        $mediaResponseRecords = @(Get-MediaResponseRecords -Text $logText)
        $invalidMediaResponseRecords = @($mediaResponseRecords |
            Where-Object { -not $_.Valid })
        $validMediaResponseRecords = @($mediaResponseRecords |
            Where-Object { $_.Valid })
        Check 'media_response logs use the fixed telemetry field contract' (
            $mediaResponseRecords.Count -gt 0 -and
            $invalidMediaResponseRecords.Count -eq 0 -and
            @($validMediaResponseRecords | Where-Object {
                $_.Surface -ne 'v1' -or
                @('direct-http', 'direct-dash', 'server-proxy', 'server-hls', 'unknown') -notcontains
                    $_.Transport -or
                @('stream', 'playlist', 'segment', 'subtitle') -notcontains $_.Asset -or
                @('success', 'error', 'aborted') -notcontains $_.Outcome -or
                @(
                    'complete', 'body', 'request', 'authorization', 'session',
                    'rate_limit', 'source', 'internal', 'response'
                ) -notcontains $_.FailureStage -or
                $_.Status -lt 100 -or $_.Status -gt 599 -or
                $_.ResponseBytes -lt 0 -or
                $_.ServerMediaBytes -lt 0 -or
                $_.DurationMS -lt 0
            }).Count -eq 0
        )

        $expectedProxyMediaBytes = -1
        if ($null -ne $mediaBytes) {
            $expectedProxyMediaBytes = [long]$mediaBytes.Length
        }
        Check 'server-proxy stream Range telemetry reports client and server bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-proxy' -Asset 'stream' `
                -Outcome 'success' -FailureStage 'complete' -Status 206 `
                -ResponseBytes ([long]$rangeLength) `
                -ServerMediaBytes ([long]$rangeLength)
        )
        Check 'server-proxy stream HEAD telemetry reports zero response bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-proxy' -Asset 'stream' `
                -Outcome 'success' -FailureStage 'complete' -Status 200 `
                -ResponseBytes 0 -ServerMediaBytes 0
        )
        Check 'server-proxy full stream telemetry reports the complete body' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-proxy' -Asset 'stream' `
                -Outcome 'success' -FailureStage 'complete' -Status 200 `
                -ResponseBytes $expectedProxyMediaBytes `
                -ServerMediaBytes $expectedProxyMediaBytes
        )

        $expectedSubtitleBytes = -1
        if ($null -ne $subtitleGet) {
            $expectedSubtitleBytes = [long][System.Text.Encoding]::UTF8.GetByteCount(
                [string]$subtitleGet.Body)
        }
        Check 'subtitle GET telemetry reports response bytes but no server media bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-proxy' -Asset 'subtitle' `
                -Outcome 'success' -FailureStage 'complete' -Status 200 `
                -ResponseBytes $expectedSubtitleBytes -ServerMediaBytes 0
        )
        Check 'subtitle HEAD telemetry reports zero response bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-proxy' -Asset 'subtitle' `
                -Outcome 'success' -FailureStage 'complete' -Status 200 `
                -ResponseBytes 0 -ServerMediaBytes 0
        )

        $expectedPlaylistBytes = -1
        if ($null -ne $hlsPlaylist) {
            $expectedPlaylistBytes = [long][System.Text.Encoding]::UTF8.GetByteCount(
                [string]$hlsPlaylist.Body)
        }
        Check 'server-HLS playlist telemetry reports no server media bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-hls' -Asset 'playlist' `
                -Outcome 'success' -FailureStage 'complete' -Status 200 `
                -ResponseBytes $expectedPlaylistBytes -ServerMediaBytes 0
        )
        Check 'every HLS playlist telemetry record reports zero server media bytes' (
            @($validMediaResponseRecords | Where-Object {
                $_.Asset -eq 'playlist' -and $_.ServerMediaBytes -ne 0
            }).Count -eq 0
        )
        Check 'server-HLS playlist HEAD telemetry reports zero response bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-hls' -Asset 'playlist' `
                -Outcome 'error' -FailureStage 'request' -Status 405 `
                -ResponseBytes 0 -ServerMediaBytes 0
        )
        Check 'server-HLS segment HEAD telemetry reports zero response bytes' (
            Test-MediaResponseRecord -Records $validMediaResponseRecords `
                -Surface 'v1' -Transport 'server-hls' -Asset 'segment' `
                -Outcome 'error' -FailureStage 'request' -Status 405 `
                -ResponseBytes 0 -ServerMediaBytes 0
        )
        $expectedSegmentBytes = -1
        if ($null -ne $hlsSegment) {
            $expectedSegmentBytes = [long]$hlsSegment.Bytes.Length
        }
        if ($expectedSegmentBytes -ge 0) {
            Check 'server-HLS segment telemetry reports served media bytes' (
                Test-MediaResponseRecord -Records $validMediaResponseRecords `
                    -Surface 'v1' -Transport 'server-hls' -Asset 'segment' `
                    -Outcome 'success' -FailureStage 'complete' -Status 200 `
                    -ResponseBytes $expectedSegmentBytes `
                    -ServerMediaBytes $expectedSegmentBytes
            )
        } else {
            Skip 'server-HLS segment telemetry byte counts (decodable fixture unavailable)'
        }

        $leakedTicketIDs = [System.Collections.Generic.List[string]]::new()
        $trackedTicketIDs = @([string]$proxyTicketID, [string]$directTicketID) +
            @($script:dashTicketIDs)
        foreach ($ticketID in $trackedTicketIDs) {
            if (
                -not [string]::IsNullOrEmpty($ticketID) -and
                $logText.IndexOf($ticketID, [System.StringComparison]::Ordinal) -ge 0
            ) {
                $leakedTicketIDs.Add($ticketID)
            }
        }
        Check 'server logs omit proxy, direct, and direct-dash ticket IDs' (
            $leakedTicketIDs.Count -eq 0
        )
    }

    if ($null -eq $oldLocalEnv) {
        Remove-Item Env:DORIYAMEDIA_LOCAL -ErrorAction SilentlyContinue
    } else {
        $env:DORIYAMEDIA_LOCAL = $oldLocalEnv
    }
    Restore-EnvironmentValue 'PLAYBACK_E2E_DASH_MODE_FILE' $oldDashModeFileEnv
    Restore-EnvironmentValue 'PLAYBACK_E2E_DASH_VIDEO_URL' $oldDashVideoURLEnv
    Restore-EnvironmentValue 'PLAYBACK_E2E_DASH_AUDIO_URL' $oldDashAudioURLEnv
    Restore-EnvironmentValue 'PLAYBACK_E2E_DASH_UNSAFE_COOKIE' $oldDashCookieEnv
    Restore-EnvironmentValue 'PLAYBACK_E2E_DASH_UNSAFE_AUTHORIZATION' $oldDashAuthorizationEnv

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
