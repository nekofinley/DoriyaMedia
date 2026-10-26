[CmdletBinding()]
param(
  [switch]$DryRun,
  [string]$ManifestPath = "",
  [string]$OutputDirectory = "",
  [Alias("Sample")]
  [string[]]$Samples = @(),
  [object]$FileIds = $null,
  [string]$NetworkLabel = "",
  [string]$EgressRegion = "",
  [ValidateRange(1, 120)]
  [int]$TimeoutSeconds = 20,
  [ValidateRange(1, 65536)]
  [int]$RangeBytes = 1024,
  [ValidateRange(10, 3600)]
  [int]$MockLifetimeSeconds = 300,
  [switch]$SkipS5Mock
)

$ErrorActionPreference = "Stop"
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8
Add-Type -AssemblyName System.Net.Http

$script:Root = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($ManifestPath)) {
  $ManifestPath = Join-Path $script:Root "protocol\fixtures\media-samples.json"
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
  $OutputDirectory = Join-Path $script:Root "docs\playback-spikes\results"
}

$script:StageNames = @(
  "ticket",
  "direct_dns",
  "connect",
  "http",
  "range",
  "header",
  "decode",
  "seek",
  "fallback"
)

function Get-ObjectProperty {
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

function Test-ObjectProperty {
  param($Object, [string]$Name)

  if ($null -eq $Object) {
    return $false
  }
  return ($null -ne $Object.PSObject.Properties[$Name])
}

function Get-EnvironmentValue {
  param([string]$Name)

  if ([string]::IsNullOrWhiteSpace($Name)) {
    return ""
  }
  $value = [System.Environment]::GetEnvironmentVariable($Name, "Process")
  if ($null -eq $value) {
    return ""
  }
  return $value
}

function Test-EnvironmentName {
  param([string]$Name)

  return ($Name -match '^[A-Z][A-Z0-9_]{2,127}$')
}

function Assert-StringValue {
  param([string]$Path, $Value)

  if ($null -eq $Value -or [string]::IsNullOrWhiteSpace([string]$Value)) {
    throw "manifest: $Path must be a non-empty string"
  }
}

function Assert-ObjectStringArray {
  param([string]$Path, $Owner, [string]$Name)

  if (-not (Test-ObjectProperty $Owner $Name)) {
    throw "manifest: $Path must be an array"
  }
  $Value = $Owner.PSObject.Properties[$Name].Value
  if ($null -eq $Value -or -not ($Value -is [System.Array])) {
    throw "manifest: $Path must be an array"
  }
  foreach ($item in @($Value)) {
    if ($null -eq $item -or [string]::IsNullOrWhiteSpace([string]$item)) {
      throw "manifest: $Path contains an empty value"
    }
  }
}

function Read-MatrixManifest {
  param([string]$Path)

  if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
    throw "manifest not found: $Path"
  }

  try {
    $manifest = Get-Content -LiteralPath $Path -Raw -ErrorAction Stop | ConvertFrom-Json -ErrorAction Stop
  } catch {
    throw "manifest is not valid JSON"
  }

  if ([int](Get-ObjectProperty $manifest "schema_version") -ne 1) {
    throw "manifest: schema_version must be 1"
  }

  $samples = @(Get-ObjectProperty $manifest "samples")
  if ($samples.Count -eq 0) {
    throw "manifest: samples must be a non-empty array"
  }

  $idSet = @{}
  foreach ($sample in $samples) {
    $id = [string](Get-ObjectProperty $sample "id")
    Assert-StringValue "samples[].id" $id
    if ($id -notmatch '^[A-Za-z0-9._-]{1,64}$') {
      throw "manifest: sample id '$id' contains unsupported characters"
    }
    if ($idSet.ContainsKey($id)) {
      throw "manifest: duplicate sample id '$id'"
    }
    $idSet[$id] = $true

    Assert-StringValue "$id.kind" (Get-ObjectProperty $sample "kind")
    Assert-StringValue "$id.container" (Get-ObjectProperty $sample "container")
    Assert-StringValue "$id.video" (Get-ObjectProperty $sample "video")
    Assert-ObjectStringArray "$id.audio" $sample "audio"
    Assert-ObjectStringArray "$id.features" $sample "features"
    Assert-ObjectStringArray "$id.subtitles" $sample "subtitles"
    Assert-StringValue "$id.secret_ref" (Get-ObjectProperty $sample "secret_ref")
    Assert-StringValue "$id.coverage" (Get-ObjectProperty $sample "coverage")
    Assert-StringValue "$id.coverage_status" (Get-ObjectProperty $sample "coverage_status")

    if (-not (Test-ObjectProperty $sample "source_url")) {
      throw "manifest: $id.source_url is required"
    }
    $sourceUrl = Get-ObjectProperty $sample "source_url"
    if ($null -ne $sourceUrl) {
      throw "manifest: $id.source_url must be null; put source URLs in process environment secrets"
    }

    $secretRef = [string](Get-ObjectProperty $sample "secret_ref")
    if (-not (Test-EnvironmentName $secretRef)) {
      throw "manifest: $id.secret_ref must be an environment variable name"
    }

    $headersRef = Get-ObjectProperty $sample "headers_ref"
    if ($null -ne $headersRef -and -not (Test-EnvironmentName ([string]$headersRef))) {
      throw "manifest: $id.headers_ref must be null or an environment variable name"
    }

    $fileIdEnv = Get-ObjectProperty $sample "file_id_env"
    if ($null -ne $fileIdEnv -and -not (Test-EnvironmentName ([string]$fileIdEnv))) {
      throw "manifest: $id.file_id_env must be null or an environment variable name"
    }

    $fileId = Get-ObjectProperty $sample "file_id"
    if ($null -ne $fileId) {
      $fileIdText = [string]$fileId
      if ($fileIdText -notmatch '^[A-Za-z0-9._:-]{1,128}$') {
        throw "manifest: $id.file_id contains unsupported characters"
      }
    }

    if ([string](Get-ObjectProperty $sample "kind") -eq "mock" -and $id -eq "S5") {
      Assert-StringValue "S5.mock_profile" (Get-ObjectProperty $sample "mock_profile")
    }
  }

  foreach ($requiredId in @("S1", "S2", "S3", "S4", "S5")) {
    if (-not $idSet.ContainsKey($requiredId)) {
      throw "manifest: required sample '$requiredId' is missing"
    }
  }

  $devices = @(Get-ObjectProperty $manifest "devices")
  if ($devices.Count -eq 0) {
    throw "manifest: devices must be a non-empty array"
  }
  $deviceIds = @{}
  foreach ($device in $devices) {
    $deviceId = [string](Get-ObjectProperty $device "id")
    Assert-StringValue "devices[].id" $deviceId
    Assert-StringValue "$deviceId.platform" (Get-ObjectProperty $device "platform")
    Assert-StringValue "$deviceId.version" (Get-ObjectProperty $device "version")
    if ($deviceIds.ContainsKey($deviceId)) {
      throw "manifest: duplicate device id '$deviceId'"
    }
    $deviceIds[$deviceId] = $true
  }

  $networks = @(Get-ObjectProperty $manifest "networks")
  if ($networks.Count -eq 0) {
    throw "manifest: networks must be a non-empty array"
  }
  $networkIds = @{}
  foreach ($network in $networks) {
    $networkId = [string](Get-ObjectProperty $network "id")
    $networkLabel = [string](Get-ObjectProperty $network "label")
    Assert-StringValue "networks[].id" $networkId
    Assert-StringValue "$networkId.label" $networkLabel
    if ($networkIds.ContainsKey($networkId)) {
      throw "manifest: duplicate network id '$networkId'"
    }
    $networkIds[$networkId] = $true
  }

  return $manifest
}

function ConvertTo-FileIdMap {
  param($InputValue, $Manifest)

  $map = @{}
  if ($null -eq $InputValue) {
    return $map
  }

  $entries = New-Object System.Collections.Generic.List[string]
  if ($InputValue -is [System.Collections.IDictionary]) {
    foreach ($key in $InputValue.Keys) {
      [void]$entries.Add(([string]$key) + "=" + ([string]$InputValue[$key]))
    }
  } elseif ($InputValue -is [string] -or $InputValue -is [System.Collections.IEnumerable]) {
    foreach ($entry in @($InputValue)) {
      if ($null -ne $entry) {
        foreach ($part in ([string]$entry -split '[;,]')) {
          if (-not [string]::IsNullOrWhiteSpace($part)) {
            [void]$entries.Add($part.Trim())
          }
        }
      }
    }
  } else {
    foreach ($property in $InputValue.PSObject.Properties) {
      [void]$entries.Add($property.Name + "=" + [string]$property.Value)
    }
  }

  $knownIds = @{}
  foreach ($sample in @(Get-ObjectProperty $Manifest "samples")) {
    $knownIds[[string]$sample.id] = $true
  }

  foreach ($entry in $entries) {
    if ($entry -notmatch '^([^=]+)=(.+)$') {
      throw "FileIds entries must use SAMPLE_ID=FILE_ID"
    }
    $sampleId = $Matches[1].Trim()
    $fileId = $Matches[2].Trim()
    if (-not $knownIds.ContainsKey($sampleId)) {
      throw "FileIds refers to unknown sample '$sampleId'"
    }
    if ($fileId -notmatch '^[A-Za-z0-9._:-]{1,128}$') {
      throw "FileIds value for '$sampleId' contains unsupported characters"
    }
    $map[$sampleId] = $fileId
  }

  return $map
}

function Resolve-SampleFileId {
  param($Sample, [hashtable]$FileIdMap)

  $id = [string]$Sample.id
  if ($FileIdMap.ContainsKey($id)) {
    return @{
      Value = [string]$FileIdMap[$id]
      Source = "parameter"
      Status = "provided"
    }
  }

  $fileIdEnv = [string](Get-ObjectProperty $Sample "file_id_env")
  if (-not [string]::IsNullOrWhiteSpace($fileIdEnv)) {
    $envValue = Get-EnvironmentValue $fileIdEnv
    if (-not [string]::IsNullOrWhiteSpace($envValue)) {
      $trimmed = $envValue.Trim()
      if ($trimmed -notmatch '^[A-Za-z0-9._:-]{1,128}$') {
        return @{
          Value = $null
          Source = "environment:$fileIdEnv"
          Status = "invalid"
        }
      }
      return @{
        Value = $trimmed
        Source = "environment:$fileIdEnv"
        Status = "provided"
      }
    }
  }

  $manifestValue = Get-ObjectProperty $Sample "file_id"
  if ($null -ne $manifestValue -and -not [string]::IsNullOrWhiteSpace([string]$manifestValue)) {
    return @{
      Value = [string]$manifestValue
      Source = "manifest"
      Status = "provided"
    }
  }

  return @{
    Value = $null
    Source = "missing"
    Status = "missing"
  }
}

function ConvertTo-SafeLabel {
  param([string]$Value, [string]$Fallback)

  if ([string]::IsNullOrWhiteSpace($Value)) {
    return $Fallback
  }
  $clean = ([regex]'[^\p{L}\p{N}._:/ -]+').Replace($Value.Trim(), "-")
  $clean = $clean.Trim()
  if ($clean.Length -gt 80) {
    $clean = $clean.Substring(0, 80)
  }
  if ([string]::IsNullOrWhiteSpace($clean)) {
    return $Fallback
  }
  return $clean
}

function ConvertTo-SafeUriInfo {
  param([string]$Value)

  if ([string]::IsNullOrWhiteSpace($Value)) {
    return @{
      Ok = $false
      FailureCategory = "secret_missing"
      Uri = $null
    }
  }

  try {
    $uri = New-Object System.Uri($Value.Trim(), [System.UriKind]::Absolute)
  } catch {
    return @{
      Ok = $false
      FailureCategory = "invalid_url"
      Uri = $null
    }
  }

  if ($uri.Scheme -ne "http" -and $uri.Scheme -ne "https") {
    return @{
      Ok = $false
      FailureCategory = "invalid_url_scheme"
      Uri = $null
    }
  }
  if (-not [string]::IsNullOrEmpty($uri.UserInfo)) {
    return @{
      Ok = $false
      FailureCategory = "url_userinfo_forbidden"
      Uri = $null
    }
  }
  if ([string]::IsNullOrWhiteSpace($uri.DnsSafeHost)) {
    return @{
      Ok = $false
      FailureCategory = "invalid_url_host"
      Uri = $null
    }
  }

  return @{
    Ok = $true
    FailureCategory = $null
    Uri = $uri
  }
}

function Get-SafeSource {
  param([System.Uri]$Uri, [string]$Role)

  return [ordered]@{
    role = $Role
    transport = $Uri.Scheme
    host = $Uri.DnsSafeHost
    port = $Uri.Port
  }
}

function Get-OriginKey {
  param([System.Uri]$Uri)

  return ($Uri.Scheme.ToLowerInvariant() + "://" + $Uri.DnsSafeHost.ToLowerInvariant() + ":" + [string]$Uri.Port)
}

function Get-ExceptionFailureCategory {
  param([System.Exception]$Exception)

  $current = $Exception
  while ($null -ne $current) {
    if ($current -is [System.Net.Sockets.SocketException]) {
      switch ($current.SocketErrorCode) {
        "TimedOut" { return "connect_timeout" }
        "ConnectionRefused" { return "connect_refused" }
        "NetworkUnreachable" { return "network_unreachable" }
        "HostUnreachable" { return "host_unreachable" }
        "NameResolutionFailure" { return "dns_failure" }
        "AddressNotAvailable" { return "address_unavailable" }
        default { return "socket_failure" }
      }
    }
    if ($current -is [System.Security.Authentication.AuthenticationException]) {
      return "tls_failure"
    }
    if ($current -is [System.Threading.Tasks.TaskCanceledException] -or
        $current -is [System.OperationCanceledException]) {
      return "timeout"
    }
    $current = $current.InnerException
  }
  return "request_failed"
}

function Get-AddressScope {
  param([System.Net.IPAddress]$Address)

  if ([System.Net.IPAddress]::IsLoopback($Address)) {
    return "loopback"
  }
  $bytes = $Address.GetAddressBytes()
  if ($Address.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetwork) {
    if ($bytes[0] -eq 10) {
      return "private"
    }
    if ($bytes[0] -eq 172 -and $bytes[1] -ge 16 -and $bytes[1] -le 31) {
      return "private"
    }
    if ($bytes[0] -eq 192 -and $bytes[1] -eq 168) {
      return "private"
    }
    if ($bytes[0] -eq 169 -and $bytes[1] -eq 254) {
      return "link-local"
    }
    if ($bytes[0] -ge 224) {
      return "multicast"
    }
    return "public"
  }
  if ($Address.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetworkV6) {
    if (($bytes[0] -band 0xFE) -eq 0xFC) {
      return "private"
    }
    if ($bytes[0] -eq 0xFE -and ($bytes[1] -band 0xC0) -eq 0x80) {
      return "link-local"
    }
    if ($bytes[0] -eq 0xFF) {
      return "multicast"
    }
    return "public"
  }
  return "unknown"
}

function Resolve-HostAddresses {
  param([System.Uri]$Uri, [int]$TimeoutMilliseconds)

  $watch = [System.Diagnostics.Stopwatch]::StartNew()
  $hostName = $Uri.DnsSafeHost
  $literal = $null
  if ([System.Net.IPAddress]::TryParse($hostName, [ref]$literal)) {
    $watch.Stop()
    return @{
      Ok = $true
      Addresses = @($literal)
      Scope = @(Get-AddressScope $literal)
      AddressCount = 1
      ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
      FailureCategory = $null
    }
  }

  try {
    $task = [System.Net.Dns]::GetHostAddressesAsync($hostName)
    if (-not $task.Wait($TimeoutMilliseconds)) {
      $watch.Stop()
      return @{
        Ok = $false
        Addresses = @()
        Scope = @()
        AddressCount = 0
        ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
        FailureCategory = "dns_timeout"
      }
    }
    $addresses = @($task.Result | Where-Object { $null -ne $_ })
    $watch.Stop()
    if ($addresses.Count -eq 0) {
      return @{
        Ok = $false
        Addresses = @()
        Scope = @()
        AddressCount = 0
        ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
        FailureCategory = "dns_no_address"
      }
    }
    $scopes = @($addresses | ForEach-Object { Get-AddressScope $_ } | Sort-Object -Unique)
    return @{
      Ok = $true
      Addresses = $addresses
      Scope = $scopes
      AddressCount = $addresses.Count
      ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
      FailureCategory = $null
    }
  } catch {
    $watch.Stop()
    return @{
      Ok = $false
      Addresses = @()
      Scope = @()
      AddressCount = 0
      ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
      FailureCategory = "dns_failure"
    }
  }
}

function Test-TcpConnection {
  param(
    [System.Net.IPAddress[]]$Addresses,
    [int]$Port,
    [int]$TimeoutMilliseconds
  )

  $watch = [System.Diagnostics.Stopwatch]::StartNew()
  $deadline = (Get-Date).AddMilliseconds($TimeoutMilliseconds)
  $attempts = 0
  $lastCategory = "connect_failed"

  foreach ($address in $Addresses) {
    $remaining = [int][Math]::Max(1, ($deadline - (Get-Date)).TotalMilliseconds)
    if ($remaining -le 0) {
      $lastCategory = "connect_timeout"
      break
    }

    $attempts++
    $client = New-Object System.Net.Sockets.TcpClient
    try {
      $async = $client.BeginConnect($address, $Port, $null, $null)
      if (-not $async.AsyncWaitHandle.WaitOne($remaining, $false)) {
        $lastCategory = "connect_timeout"
        continue
      }
      $client.EndConnect($async)
      $watch.Stop()
      return @{
        Ok = $true
        Attempts = $attempts
        ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
        FailureCategory = $null
      }
    } catch {
      $lastCategory = Get-ExceptionFailureCategory $_.Exception
    } finally {
      try {
        $client.Close()
      } catch {
      }
    }
  }

  $watch.Stop()
  return @{
    Ok = $false
    Attempts = $attempts
    ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
    FailureCategory = $lastCategory
  }
}

function Get-HttpFailureCategory {
  param([int]$StatusCode)

  switch ($StatusCode) {
    400 { return "http_400" }
    401 { return "http_401" }
    403 { return "http_403" }
    404 { return "http_404" }
    408 { return "http_408" }
    410 { return "http_410" }
    416 { return "http_416" }
    429 { return "http_429" }
    default {
      if ($StatusCode -ge 300 -and $StatusCode -lt 400) {
        return "unexpected_redirect"
      }
      if ($StatusCode -ge 500) {
        return "http_5xx"
      }
      return "http_status_$StatusCode"
    }
  }
}

function Invoke-SafeHttpProbe {
  param(
    [System.Uri]$Uri,
    [string]$Method,
    [hashtable]$Headers,
    [string]$RangeValue,
    [int]$TimeoutSeconds
  )

  $watch = [System.Diagnostics.Stopwatch]::StartNew()
  $handler = New-Object System.Net.Http.HttpClientHandler
  $handler.AllowAutoRedirect = $false
  $handler.UseCookies = $false
  $handler.AutomaticDecompression = [System.Net.DecompressionMethods]::None
  $client = New-Object System.Net.Http.HttpClient($handler)
  $client.Timeout = [TimeSpan]::FromSeconds($TimeoutSeconds)

  $current = $Uri
  $allowSourceHeaders = $true
  $redirectCount = 0
  $crossOriginRedirect = $false
  $sourceHeadersStripped = $false
  $headerNames = @()
  if ($null -ne $Headers) {
    $headerNames = @($Headers.Keys | Sort-Object)
  }

  try {
    for ($hop = 0; $hop -le 5; $hop++) {
      $request = $null
      $response = $null
      try {
        $request = New-Object System.Net.Http.HttpRequestMessage(
          (New-Object System.Net.Http.HttpMethod($Method)),
          $current
        )

        if ($allowSourceHeaders -and $null -ne $Headers) {
          foreach ($name in $Headers.Keys) {
            if (-not $request.Headers.TryAddWithoutValidation([string]$name, [string]$Headers[$name])) {
              $watch.Stop()
              return @{
                Ok = $false
                StatusCode = $null
                FailureCategory = "invalid_header"
                ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
                Method = $Method
                RedirectCount = $redirectCount
                CrossOriginRedirect = $crossOriginRedirect
                SourceHeadersStripped = $sourceHeadersStripped
                HeaderNames = $headerNames
                FinalUri = $current
                AcceptRanges = ""
                ContentRange = ""
                ContentLength = $null
              }
            }
          }
        }

        if (-not [string]::IsNullOrWhiteSpace($RangeValue)) {
          [void]$request.Headers.TryAddWithoutValidation("Range", $RangeValue)
        }
        if (-not $request.Headers.Contains("User-Agent")) {
          [void]$request.Headers.TryAddWithoutValidation("User-Agent", "DoriyaMedia-PlaybackMatrix/1.0")
        }

        $response = $client.SendAsync(
          $request,
          [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead
        ).GetAwaiter().GetResult()

        $statusCode = [int]$response.StatusCode
        $location = $response.Headers.Location
        if ($statusCode -ge 300 -and $statusCode -lt 400 -and $null -ne $location) {
          if ($redirectCount -ge 5) {
            $watch.Stop()
            return @{
              Ok = $false
              StatusCode = $statusCode
              FailureCategory = "redirect_limit"
              ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
              Method = $Method
              RedirectCount = $redirectCount
              CrossOriginRedirect = $crossOriginRedirect
              SourceHeadersStripped = $sourceHeadersStripped
              HeaderNames = $headerNames
              FinalUri = $current
              AcceptRanges = ""
              ContentRange = ""
              ContentLength = $null
            }
          }

          if ($location.IsAbsoluteUri) {
            $next = $location
          } else {
            $next = New-Object System.Uri($current, $location)
          }
          if ($next.Scheme -ne "http" -and $next.Scheme -ne "https") {
            $watch.Stop()
            return @{
              Ok = $false
              StatusCode = $statusCode
              FailureCategory = "redirect_scheme_forbidden"
              ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
              Method = $Method
              RedirectCount = $redirectCount
              CrossOriginRedirect = $crossOriginRedirect
              SourceHeadersStripped = $sourceHeadersStripped
              HeaderNames = $headerNames
              FinalUri = $current
              AcceptRanges = ""
              ContentRange = ""
              ContentLength = $null
            }
          }
          if (-not [string]::IsNullOrEmpty($next.UserInfo)) {
            $watch.Stop()
            return @{
              Ok = $false
              StatusCode = $statusCode
              FailureCategory = "redirect_userinfo_forbidden"
              ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
              Method = $Method
              RedirectCount = $redirectCount
              CrossOriginRedirect = $crossOriginRedirect
              SourceHeadersStripped = $sourceHeadersStripped
              HeaderNames = $headerNames
              FinalUri = $current
              AcceptRanges = ""
              ContentRange = ""
              ContentLength = $null
            }
          }

          if ((Get-OriginKey $current) -ne (Get-OriginKey $next)) {
            $crossOriginRedirect = $true
            $allowSourceHeaders = $false
            $sourceHeadersStripped = $true
          }
          $redirectCount++
          $current = $next
          continue
        }

        $acceptRanges = ""
        if ($null -ne $response.Headers.AcceptRanges) {
          $acceptRanges = (@($response.Headers.AcceptRanges) -join ",")
        }
        $contentRange = ""
        if ($null -ne $response.Content.Headers.ContentRange) {
          $contentRange = $response.Content.Headers.ContentRange.ToString()
        }
        $contentLength = $null
        if ($null -ne $response.Content.Headers.ContentLength) {
          $contentLength = [long]$response.Content.Headers.ContentLength
        }

        $watch.Stop()
        $ok = ($statusCode -ge 200 -and $statusCode -lt 300)
        $failureCategory = $null
        if (-not $ok) {
          $failureCategory = Get-HttpFailureCategory $statusCode
        }
        return @{
          Ok = $ok
          StatusCode = $statusCode
          FailureCategory = $failureCategory
          ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
          Method = $Method
          RedirectCount = $redirectCount
          CrossOriginRedirect = $crossOriginRedirect
          SourceHeadersStripped = $sourceHeadersStripped
          HeaderNames = $headerNames
          FinalUri = $current
          AcceptRanges = $acceptRanges
          ContentRange = $contentRange
          ContentLength = $contentLength
        }
      } catch {
        $watch.Stop()
        return @{
          Ok = $false
          StatusCode = $null
          FailureCategory = (Get-ExceptionFailureCategory $_.Exception)
          ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
          Method = $Method
          RedirectCount = $redirectCount
          CrossOriginRedirect = $crossOriginRedirect
          SourceHeadersStripped = $sourceHeadersStripped
          HeaderNames = $headerNames
          FinalUri = $current
          AcceptRanges = ""
          ContentRange = ""
          ContentLength = $null
        }
      } finally {
        if ($null -ne $response) {
          try {
            $response.Dispose()
          } catch {
          }
        }
        if ($null -ne $request) {
          try {
            $request.Dispose()
          } catch {
          }
        }
      }
    }

    $watch.Stop()
    return @{
      Ok = $false
      StatusCode = $null
      FailureCategory = "redirect_limit"
      ElapsedMs = [Math]::Round($watch.Elapsed.TotalMilliseconds, 1)
      Method = $Method
      RedirectCount = $redirectCount
      CrossOriginRedirect = $crossOriginRedirect
      SourceHeadersStripped = $sourceHeadersStripped
      HeaderNames = $headerNames
      FinalUri = $current
      AcceptRanges = ""
      ContentRange = ""
      ContentLength = $null
    }
  } finally {
    $client.Dispose()
    $handler.Dispose()
  }
}

function ConvertTo-HeaderSet {
  param($Value)

  $headers = @{}
  if ($null -eq $Value) {
    return @{
      Ok = $true
      Headers = $headers
    }
  }

  $items = New-Object System.Collections.Generic.List[object]
  if ($Value -is [System.Collections.IDictionary]) {
    foreach ($key in $Value.Keys) {
      [void]$items.Add(@{ Name = [string]$key; Value = $Value[$key] })
    }
  } else {
    foreach ($property in $Value.PSObject.Properties) {
      [void]$items.Add(@{ Name = $property.Name; Value = $property.Value })
    }
  }

  foreach ($item in $items) {
    $name = [string]$item.Name
    $text = [string]$item.Value
    if ($name -notmatch '^[A-Za-z0-9-]{1,128}$') {
      return @{
        Ok = $false
        Headers = @{}
        FailureCategory = "invalid_header_name"
      }
    }
    if ($text.Contains("`r") -or $text.Contains("`n")) {
      return @{
        Ok = $false
        Headers = @{}
        FailureCategory = "invalid_header_value"
      }
    }
    $headers[$name] = $text
  }

  return @{
    Ok = $true
    Headers = $headers
  }
}

function New-SourceRole {
  param([string]$Role, $Value, [hashtable]$CommonHeaders)

  $roleName = $Role.ToLowerInvariant()
  $urlValue = $null
  $roleHeadersValue = $null
  if ($Value -is [string]) {
    $urlValue = [string]$Value
  } else {
    $urlValue = [string](Get-ObjectProperty $Value "url")
    $roleHeadersValue = Get-ObjectProperty $Value "headers"
  }

  if ([string]::IsNullOrWhiteSpace($urlValue)) {
    return @{
      Ok = $false
      Role = $roleName
      FailureCategory = "invalid_secret_bundle_url"
      Uri = $null
      Headers = @{}
      HeaderNames = @()
    }
  }

  $uriInfo = ConvertTo-SafeUriInfo $urlValue
  if (-not $uriInfo.Ok) {
    return @{
      Ok = $false
      Role = $roleName
      FailureCategory = $uriInfo.FailureCategory
      Uri = $null
      Headers = @{}
      HeaderNames = @()
    }
  }

  $headerResult = ConvertTo-HeaderSet $roleHeadersValue
  if (-not $headerResult.Ok) {
    return @{
      Ok = $false
      Role = $roleName
      FailureCategory = $headerResult.FailureCategory
      Uri = $null
      Headers = @{}
      HeaderNames = @()
    }
  }

  $mergedHeaders = @{}
  foreach ($key in $CommonHeaders.Keys) {
    $mergedHeaders[$key] = $CommonHeaders[$key]
  }
  foreach ($key in $headerResult.Headers.Keys) {
    $mergedHeaders[$key] = $headerResult.Headers[$key]
  }

  return @{
    Ok = $true
    Role = $roleName
    FailureCategory = $null
    Uri = $uriInfo.Uri
    Headers = $mergedHeaders
    HeaderNames = @($mergedHeaders.Keys | Sort-Object)
  }
}

function Read-SourceBundle {
  param($Sample)

  $secretRef = [string](Get-ObjectProperty $Sample "secret_ref")
  $raw = Get-EnvironmentValue $secretRef
  if ([string]::IsNullOrWhiteSpace($raw)) {
    return @{
      Ok = $false
      FailureCategory = "secret_missing"
      Roles = @()
      Warnings = @("secret-ref-not-set")
    }
  }

  $commonHeaders = @{}
  $warnings = New-Object System.Collections.Generic.List[string]
  $headersRefMissing = $false
  $headersRef = [string](Get-ObjectProperty $Sample "headers_ref")
  if (-not [string]::IsNullOrWhiteSpace($headersRef)) {
    $headersRaw = Get-EnvironmentValue $headersRef
    if ([string]::IsNullOrWhiteSpace($headersRaw)) {
      $headersRefMissing = $true
    } else {
      try {
        $headerObject = $headersRaw | ConvertFrom-Json -ErrorAction Stop
      } catch {
        return @{
          Ok = $false
          FailureCategory = "invalid_headers_json"
          Roles = @()
          Warnings = @($warnings)
        }
      }
      $headerResult = ConvertTo-HeaderSet $headerObject
      if (-not $headerResult.Ok) {
        return @{
          Ok = $false
          FailureCategory = $headerResult.FailureCategory
          Roles = @()
          Warnings = @($warnings)
        }
      }
      $commonHeaders = $headerResult.Headers
    }
  }

  $features = @(Get-ObjectProperty $Sample "features")
  $dualRequired = ($features -contains "dual-url")
  $trimmed = $raw.Trim()
  $parsed = $null
  $isJson = ($trimmed.StartsWith("{") -or $trimmed.StartsWith("["))

  if ($isJson) {
    try {
      $parsed = $trimmed | ConvertFrom-Json -ErrorAction Stop
    } catch {
      return @{
        Ok = $false
        FailureCategory = "invalid_secret_bundle_json"
        Roles = @()
        Warnings = @($warnings)
      }
    }
  }

  $roleList = New-Object System.Collections.Generic.List[object]

  if ($isJson) {
    if ($parsed -is [System.Array]) {
      foreach ($entry in @($parsed)) {
        $role = [string](Get-ObjectProperty $entry "role")
        if ([string]::IsNullOrWhiteSpace($role)) {
          $role = "muxed"
        }
        [void]$roleList.Add((New-SourceRole $role $entry $commonHeaders))
      }
    } else {
      $bundleHeaders = Get-ObjectProperty $parsed "headers"
      if ($null -ne $bundleHeaders) {
        $bundleHeaderResult = ConvertTo-HeaderSet $bundleHeaders
        if (-not $bundleHeaderResult.Ok) {
          return @{
            Ok = $false
            FailureCategory = $bundleHeaderResult.FailureCategory
            Roles = @()
            Warnings = @($warnings)
          }
        }
        foreach ($key in $bundleHeaderResult.Headers.Keys) {
          $commonHeaders[$key] = $bundleHeaderResult.Headers[$key]
        }
      }

      $streams = Get-ObjectProperty $parsed "streams"
      if ($null -ne $streams) {
        foreach ($entry in @($streams)) {
          $role = [string](Get-ObjectProperty $entry "role")
          if ([string]::IsNullOrWhiteSpace($role)) {
            $role = "muxed"
          }
          [void]$roleList.Add((New-SourceRole $role $entry $commonHeaders))
        }
      } else {
        $video = Get-ObjectProperty $parsed "video"
        $audio = Get-ObjectProperty $parsed "audio"
        $videoUrl = Get-ObjectProperty $parsed "video_url"
        $audioUrl = Get-ObjectProperty $parsed "audio_url"
        $url = Get-ObjectProperty $parsed "url"

        if ($null -ne $video) {
          [void]$roleList.Add((New-SourceRole "video" $video $commonHeaders))
        }
        if ($null -ne $audio) {
          [void]$roleList.Add((New-SourceRole "audio" $audio $commonHeaders))
        }
        if ($null -ne $videoUrl) {
          [void]$roleList.Add((New-SourceRole "video" @{ url = $videoUrl } $commonHeaders))
        }
        if ($null -ne $audioUrl) {
          [void]$roleList.Add((New-SourceRole "audio" @{ url = $audioUrl } $commonHeaders))
        }
        if ($null -ne $url -and $roleList.Count -eq 0) {
          [void]$roleList.Add((New-SourceRole "muxed" $parsed $commonHeaders))
        }
      }
    }
  } else {
    if ($dualRequired) {
      return @{
        Ok = $false
        FailureCategory = "dual_url_secret_required"
        Roles = @()
        Warnings = @($warnings)
      }
    }
    [void]$roleList.Add((New-SourceRole "muxed" $trimmed $commonHeaders))
  }

  $roles = @($roleList.ToArray())
  if ($roles.Count -eq 0) {
    return @{
      Ok = $false
      FailureCategory = "secret_bundle_has_no_streams"
      Roles = @()
      Warnings = @($warnings)
    }
  }

  foreach ($role in $roles) {
    if (-not $role.Ok) {
      return @{
        Ok = $false
        FailureCategory = $role.FailureCategory
        Roles = $roles
        Warnings = @($warnings)
      }
    }
  }

  if ($dualRequired) {
    $roleNames = @($roles | ForEach-Object { $_.Role })
    if (-not ($roleNames -contains "video") -or -not ($roleNames -contains "audio")) {
      return @{
        Ok = $false
        FailureCategory = "dual_url_roles_missing"
        Roles = $roles
        Warnings = @($warnings)
      }
    }
  }

  if ($headersRefMissing) {
    $rolesWithHeaders = @($roles | Where-Object { @($_.HeaderNames).Count -gt 0 })
    if ($rolesWithHeaders.Count -eq 0) {
      [void]$warnings.Add("headers-secret-missing")
    }
  }

  return @{
    Ok = $true
    FailureCategory = $null
    Roles = $roles
    Warnings = @($warnings)
  }
}

function New-StageResult {
  param(
    [string]$Role,
    [string]$Name,
    [string]$Status,
    [double]$ElapsedMs,
    [string]$FailureCategory,
    [string]$Summary,
    $Evidence
  )

  if ($null -eq $Evidence) {
    $Evidence = [ordered]@{}
  }
  $failure = $null
  if (-not [string]::IsNullOrWhiteSpace($FailureCategory)) {
    $failure = $FailureCategory
  }
  return [ordered]@{
    role = $Role
    name = $Name
    status = $Status
    elapsed_ms = [Math]::Round($ElapsedMs, 1)
    failure_category = $failure
    summary = $Summary
    evidence = $Evidence
  }
}

function New-SkippedStageSet {
  param(
    [string]$Role,
    [string]$FailureCategory,
    [string]$Summary
  )

  $stages = New-Object System.Collections.Generic.List[object]
  foreach ($name in $script:StageNames) {
    [void]$stages.Add((New-StageResult $Role $name "skip" 0 $FailureCategory $Summary ([ordered]@{})))
  }
  return @($stages.ToArray())
}

function Parse-ContentRange {
  param([string]$Value)

  if ([string]::IsNullOrWhiteSpace($Value)) {
    return $null
  }
  if ($Value -notmatch '^bytes (\d+)-(\d+)/(\d+|\*)$') {
    return $null
  }
  $total = $null
  if ($Matches[3] -ne "*") {
    $total = [long]$Matches[3]
  }
  return @{
    Start = [long]$Matches[1]
    End = [long]$Matches[2]
    Total = $total
  }
}

function Get-HttpStatusSummary {
  param($Result)

  if ($null -ne $Result.FailureCategory) {
    return ("Request failed: " + $Result.FailureCategory)
  }
  return ("HTTP " + [string]$Result.StatusCode)
}

function Invoke-RemoteRoleProbe {
  param(
    $Role,
    $Sample,
    $FileIdResult,
    [int]$TimeoutSeconds,
    [int]$RangeBytes
  )

  $stages = New-Object System.Collections.Generic.List[object]
  $roleName = [string]$Role.Role
  $uri = $Role.Uri
  $headers = $Role.Headers
  $features = @(Get-ObjectProperty $Sample "features")
  $requiresHeaders = (($features -contains "requires-referer") -or ($features -contains "requires-authorization"))

  if ($FileIdResult.Status -eq "provided") {
    [void]$stages.Add((New-StageResult $roleName "ticket" "pending" 0 "ticket_api_missing" `
      "S0 has no v1 ticket API; the file ID is recorded for later ticket runs." ([ordered]@{})))
  } elseif ($FileIdResult.Status -eq "invalid") {
    [void]$stages.Add((New-StageResult $roleName "ticket" "pending" 0 "invalid_file_id" `
      "The supplied file ID is invalid, so ticket and fallback runs were not attempted." ([ordered]@{})))
  } else {
    [void]$stages.Add((New-StageResult $roleName "ticket" "pending" 0 "file_id_missing" `
      "No file ID was supplied; direct transport can continue, but ticket attribution is pending." ([ordered]@{})))
  }

  $dns = Resolve-HostAddresses $uri ([int]($TimeoutSeconds * 1000))
  if ($dns.Ok) {
    [void]$stages.Add((New-StageResult $roleName "direct_dns" "pass" $dns.ElapsedMs "" `
      "Host resolution succeeded." ([ordered]@{ address_count = $dns.AddressCount; address_scope = $dns.Scope })))
  } else {
    [void]$stages.Add((New-StageResult $roleName "direct_dns" "fail" $dns.ElapsedMs $dns.FailureCategory `
      "Host resolution failed." ([ordered]@{ address_count = 0 })))
  }

  $connectOk = $false
  if ($dns.Ok) {
    $connect = Test-TcpConnection $dns.Addresses $uri.Port ([int]($TimeoutSeconds * 1000))
    if ($connect.Ok) {
      $connectOk = $true
      [void]$stages.Add((New-StageResult $roleName "connect" "pass" $connect.ElapsedMs "" `
        "TCP connection succeeded." ([ordered]@{ attempts = $connect.Attempts; port = $uri.Port })))
    } else {
      [void]$stages.Add((New-StageResult $roleName "connect" "fail" $connect.ElapsedMs $connect.FailureCategory `
        "TCP connection failed." ([ordered]@{ attempts = $connect.Attempts; port = $uri.Port })))
    }
  } else {
    [void]$stages.Add((New-StageResult $roleName "connect" "skip" 0 "blocked_by_direct_dns" `
      "Skipped because host resolution failed." ([ordered]@{})))
  }

  $httpPass = $false
  $httpResult = $null
  if ($connectOk) {
    $httpResult = Invoke-SafeHttpProbe $uri "HEAD" $headers "" $TimeoutSeconds
    if ($null -ne $httpResult.FailureCategory -and
        ($httpResult.FailureCategory -eq "http_status_405" -or $httpResult.FailureCategory -eq "http_status_501")) {
      $httpResult = Invoke-SafeHttpProbe $uri "GET" $headers "bytes=0-0" $TimeoutSeconds
    }
    if ($httpResult.Ok) {
      $httpPass = $true
      [void]$stages.Add((New-StageResult $roleName "http" "pass" $httpResult.ElapsedMs "" `
        "HTTP request succeeded." ([ordered]@{
          status_code = $httpResult.StatusCode
          method = $httpResult.Method
          redirects = $httpResult.RedirectCount
          cross_origin_redirect = $httpResult.CrossOriginRedirect
          source_headers_stripped = $httpResult.SourceHeadersStripped
          accept_ranges = $httpResult.AcceptRanges
        })))
    } else {
      $category = $httpResult.FailureCategory
      if ([string]::IsNullOrWhiteSpace($category)) {
        $category = "http_failure"
      }
      [void]$stages.Add((New-StageResult $roleName "http" "fail" $httpResult.ElapsedMs $category `
        (Get-HttpStatusSummary $httpResult) ([ordered]@{
          status_code = $httpResult.StatusCode
          redirects = $httpResult.RedirectCount
          cross_origin_redirect = $httpResult.CrossOriginRedirect
          source_headers_stripped = $httpResult.SourceHeadersStripped
        })))
    }
  } else {
    [void]$stages.Add((New-StageResult $roleName "http" "skip" 0 "blocked_by_connect" `
      "Skipped because TCP connection failed." ([ordered]@{})))
  }

  $rangePass = $false
  $rangeInfo = $null
  if ($httpPass) {
    $rangeValue = "bytes=0-" + [string]($RangeBytes - 1)
    $rangeResult = Invoke-SafeHttpProbe $uri "GET" $headers $rangeValue $TimeoutSeconds
    if ($rangeResult.Ok -and $rangeResult.StatusCode -eq 206) {
      $rangeInfo = Parse-ContentRange $rangeResult.ContentRange
      if ($null -ne $rangeInfo -and $rangeInfo.Start -eq 0) {
        $rangePass = $true
        [void]$stages.Add((New-StageResult $roleName "range" "pass" $rangeResult.ElapsedMs "" `
          "Byte-range request returned 206." ([ordered]@{
            status_code = $rangeResult.StatusCode
            range_start = $rangeInfo.Start
            range_end = $rangeInfo.End
            range_total = $rangeInfo.Total
            accept_ranges = $rangeResult.AcceptRanges
            redirects = $rangeResult.RedirectCount
            cross_origin_redirect = $rangeResult.CrossOriginRedirect
          })))
      } else {
        [void]$stages.Add((New-StageResult $roleName "range" "fail" $rangeResult.ElapsedMs "invalid_content_range" `
          "Range response did not include a valid range starting at byte 0." ([ordered]@{
            status_code = $rangeResult.StatusCode
          })))
      }
    } else {
      $category = $rangeResult.FailureCategory
      if ($rangeResult.StatusCode -eq 200) {
        $category = "range_ignored"
      } elseif ($rangeResult.StatusCode -eq 416) {
        $category = "range_not_satisfiable"
      } elseif ([string]::IsNullOrWhiteSpace($category)) {
        $category = "range_failed"
      }
      [void]$stages.Add((New-StageResult $roleName "range" "fail" $rangeResult.ElapsedMs $category `
        "Byte-range request failed." ([ordered]@{
          status_code = $rangeResult.StatusCode
          accept_ranges = $rangeResult.AcceptRanges
        })))
    }
  } else {
    [void]$stages.Add((New-StageResult $roleName "range" "skip" 0 "blocked_by_http" `
      "Skipped because the HTTP stage did not pass." ([ordered]@{})))
  }

  if ($Role.HeaderNames.Count -eq 0 -and $requiresHeaders) {
    [void]$stages.Add((New-StageResult $roleName "header" "fail" 0 "header_missing" `
      "The sample requires source headers, but none were supplied." ([ordered]@{})))
  } elseif ($Role.HeaderNames.Count -eq 0) {
    [void]$stages.Add((New-StageResult $roleName "header" "not_applicable" 0 "" `
      "No source header was configured for this sample." ([ordered]@{ header_names = @() })))
  } elseif ($httpPass) {
    [void]$stages.Add((New-StageResult $roleName "header" "pass" 0 "" `
      "Configured headers were accepted by the authorized request; values are not recorded." ([ordered]@{
        header_names = $Role.HeaderNames
      })))
  } elseif ($null -eq $httpResult -or $null -eq $httpResult.StatusCode) {
    [void]$stages.Add((New-StageResult $roleName "header" "skip" 0 "blocked_by_http" `
      "The authorized request was not completed, so header behavior could not be evaluated." ([ordered]@{
        header_names = $Role.HeaderNames
      })))
  } else {
    $category = "header_rejected"
    if ($httpResult.StatusCode -ne 401 -and $httpResult.StatusCode -ne 403) {
      $category = $httpResult.FailureCategory
    }
    [void]$stages.Add((New-StageResult $roleName "header" "fail" 0 $category `
      "The authorized request did not succeed; header values are not recorded." ([ordered]@{
        header_names = $Role.HeaderNames
      })))
  }

  [void]$stages.Add((New-StageResult $roleName "decode" "pending" 0 "client_decode_runner_missing" `
    "Decode requires a real Windows/Android client runner; this host probe does not claim a decode result." ([ordered]@{})))

  if (-not $rangePass) {
    [void]$stages.Add((New-StageResult $roleName "seek" "skip" 0 "blocked_by_range" `
      "Skipped because the Range stage did not pass." ([ordered]@{})))
  } elseif ($null -eq $rangeInfo -or $null -eq $rangeInfo.Total) {
    [void]$stages.Add((New-StageResult $roleName "seek" "pending" 0 "range_length_unknown" `
      "The total file length was unknown, so an HTTP midpoint-range seek could not be selected." ([ordered]@{})))
  } else {
    $midpoint = [long][Math]::Floor($rangeInfo.Total / 2)
    $seekEnd = [Math]::Min($rangeInfo.Total - 1, $midpoint + $RangeBytes - 1)
    $seekRange = "bytes=$midpoint-$seekEnd"
    $seekResult = Invoke-SafeHttpProbe $uri "GET" $headers $seekRange $TimeoutSeconds
    $seekInfo = Parse-ContentRange $seekResult.ContentRange
    if ($seekResult.StatusCode -eq 206 -and $null -ne $seekInfo -and $seekInfo.Start -eq $midpoint) {
      [void]$stages.Add((New-StageResult $roleName "seek" "pass" $seekResult.ElapsedMs "" `
        "HTTP midpoint range succeeded; this is not a player-seek result." ([ordered]@{
          probe_kind = "http-range"
          range_start = $seekInfo.Start
          range_end = $seekInfo.End
          range_total = $seekInfo.Total
        })))
    } else {
      $category = "seek_range_failed"
      if ($seekResult.StatusCode -eq 200) {
        $category = "range_ignored"
      } elseif ($null -ne $seekResult.FailureCategory) {
        $category = $seekResult.FailureCategory
      }
      [void]$stages.Add((New-StageResult $roleName "seek" "fail" $seekResult.ElapsedMs $category `
        "HTTP midpoint range failed; this is not a player-seek result." ([ordered]@{
          probe_kind = "http-range"
          status_code = $seekResult.StatusCode
          expected_start = $midpoint
        })))
    }
  }

  [void]$stages.Add((New-StageResult $roleName "fallback" "pending" 0 "fallback_runner_missing" `
    "Fallback validation requires the v1 server-proxy/HLS endpoints or a client runner." ([ordered]@{})))

  return @{
    Stages = @($stages.ToArray())
    Source = Get-SafeSource $uri $roleName
  }
}

function Get-FreeLoopbackPort {
  $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, 0)
  $listener.Start()
  try {
    return [int]([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
  } finally {
    $listener.Stop()
  }
}

function Start-MatrixMock {
  param(
    [string]$HelperPath,
    [int]$LifetimeSeconds
  )

  if (-not (Test-Path -LiteralPath $HelperPath -PathType Leaf)) {
    return @{
      Ok = $false
      FailureCategory = "mock_helper_missing"
      Shell = $null
      AsyncResult = $null
    }
  }

  $port1 = Get-FreeLoopbackPort
  $port2 = Get-FreeLoopbackPort
  while ($port2 -eq $port1) {
    $port2 = Get-FreeLoopbackPort
  }

  $token = [Guid]::NewGuid().ToString("N")
  $referer = "https://matrix.invalid/source"
  $helperText = Get-Content -LiteralPath $HelperPath -Raw -ErrorAction Stop
  $shell = [PowerShell]::Create()
  [void]$shell.AddScript($helperText)
  [void]$shell.AddArgument($port1)
  [void]$shell.AddArgument($port2)
  [void]$shell.AddArgument($token)
  [void]$shell.AddArgument($referer)
  [void]$shell.AddArgument($LifetimeSeconds)
  [void]$shell.AddArgument(1048576)
  $async = $shell.BeginInvoke()

  $deadline = (Get-Date).AddSeconds(5)
  $ready = $false
  while ((Get-Date) -lt $deadline) {
    if ($shell.InvocationStateInfo.State -eq [System.Management.Automation.PSInvocationState]::Failed -or
        $shell.InvocationStateInfo.State -eq [System.Management.Automation.PSInvocationState]::Completed) {
      break
    }
    $client = New-Object System.Net.Sockets.TcpClient
    try {
      $task = $client.ConnectAsync("127.0.0.1", $port1)
      if ($task.Wait(200) -and $client.Connected) {
        $ready = $true
        break
      }
    } catch {
    } finally {
      try {
        $client.Close()
      } catch {
      }
    }
    Start-Sleep -Milliseconds 50
  }

  if (-not $ready) {
    try {
      $shell.Stop()
    } catch {
    }
    try {
      $shell.Dispose()
    } catch {
    }
    return @{
      Ok = $false
      FailureCategory = "mock_start_failed"
      Shell = $null
      AsyncResult = $null
    }
  }

  return @{
    Ok = $true
    FailureCategory = $null
    Shell = $shell
    AsyncResult = $async
    Port1 = $port1
    Port2 = $port2
    Token = $token
    Referer = $referer
    Headers = @{
      Authorization = "Bearer " + $token
      Referer = $referer
    }
    MediaUri = New-Object System.Uri("http://127.0.0.1:$port1/media.mp4")
    ExpiredUri = New-Object System.Uri("http://127.0.0.1:$port1/expired.mp4")
    RedirectUri = New-Object System.Uri("http://127.0.0.1:$port1/redirect")
  }
}

function Stop-MatrixMock {
  param($Mock)

  if ($null -eq $Mock -or $null -eq $Mock.Shell) {
    return
  }
  try {
    $Mock.Shell.Stop()
  } catch {
  }
  try {
    if ($null -ne $Mock.AsyncResult) {
      [void]$Mock.Shell.EndInvoke($Mock.AsyncResult)
    }
  } catch {
  }
  try {
    $Mock.Shell.Dispose()
  } catch {
  }
}

function Invoke-MockSampleProbe {
  param(
    $Mock,
    $Sample,
    $FileIdResult,
    [int]$TimeoutSeconds,
    [int]$RangeBytes
  )

  $roleName = "mock"
  $stages = New-Object System.Collections.Generic.List[object]
  $headers = $Mock.Headers
  $source = [ordered]@{
    role = $roleName
    transport = "http"
    host = "127.0.0.1"
    port = $Mock.Port1
    loopback_only = $true
  }

  if ($FileIdResult.Status -eq "provided") {
    [void]$stages.Add((New-StageResult $roleName "ticket" "pending" 0 "ticket_api_missing" `
      "S0 has no v1 ticket API; the mock validates source behavior without a ticket." ([ordered]@{})))
  } else {
    [void]$stages.Add((New-StageResult $roleName "ticket" "pending" 0 "file_id_missing" `
      "No file ID was supplied; the mock source test does not require one." ([ordered]@{})))
  }

  $loopback = [System.Net.IPAddress]::Loopback
  [void]$stages.Add((New-StageResult $roleName "direct_dns" "pass" 0 "" `
    "Loopback literal used; no external DNS lookup was performed." ([ordered]@{ address_count = 1; address_scope = @("loopback") })))

  $connect = Test-TcpConnection @($loopback) $Mock.Port1 ([int]($TimeoutSeconds * 1000))
  if ($connect.Ok) {
    [void]$stages.Add((New-StageResult $roleName "connect" "pass" $connect.ElapsedMs "" `
      "Loopback TCP connection succeeded." ([ordered]@{ attempts = $connect.Attempts; port = $Mock.Port1 })))
  } else {
    [void]$stages.Add((New-StageResult $roleName "connect" "fail" $connect.ElapsedMs $connect.FailureCategory `
      "Loopback TCP connection failed." ([ordered]@{ attempts = $connect.Attempts; port = $Mock.Port1 })))
  }

  $httpPass = $false
  $authorizedHead = Invoke-SafeHttpProbe $Mock.MediaUri "HEAD" $headers "" $TimeoutSeconds
  if ($authorizedHead.StatusCode -eq 200) {
    $httpPass = $true
    [void]$stages.Add((New-StageResult $roleName "http" "pass" $authorizedHead.ElapsedMs "" `
      "Authorized media HEAD returned 200." ([ordered]@{ status_code = 200; method = "HEAD" })))
  } else {
    $category = $authorizedHead.FailureCategory
    if ([string]::IsNullOrWhiteSpace($category)) {
      $category = "mock_http_failed"
    }
    [void]$stages.Add((New-StageResult $roleName "http" "fail" $authorizedHead.ElapsedMs $category `
      "Authorized media HEAD did not return 200." ([ordered]@{ status_code = $authorizedHead.StatusCode })))
  }

  $rangeResult = Invoke-SafeHttpProbe $Mock.MediaUri "GET" $headers ("bytes=0-" + [string]($RangeBytes - 1)) $TimeoutSeconds
  $rangeInfo = Parse-ContentRange $rangeResult.ContentRange
  $rangePass = ($rangeResult.StatusCode -eq 206 -and $null -ne $rangeInfo -and $rangeInfo.Start -eq 0)
  if ($rangePass) {
    [void]$stages.Add((New-StageResult $roleName "range" "pass" $rangeResult.ElapsedMs "" `
      "Authorized byte-range request returned 206." ([ordered]@{
        status_code = 206
        range_start = $rangeInfo.Start
        range_end = $rangeInfo.End
        range_total = $rangeInfo.Total
      })))
  } else {
    $category = $rangeResult.FailureCategory
    if ([string]::IsNullOrWhiteSpace($category)) {
      $category = "mock_range_failed"
    }
    [void]$stages.Add((New-StageResult $roleName "range" "fail" $rangeResult.ElapsedMs $category `
      "Authorized byte-range request failed." ([ordered]@{ status_code = $rangeResult.StatusCode })))
  }

  $unauthorized = Invoke-SafeHttpProbe $Mock.MediaUri "HEAD" @{} "" $TimeoutSeconds
  $redirect = Invoke-SafeHttpProbe $Mock.RedirectUri "GET" $headers "" $TimeoutSeconds
  $expired = Invoke-SafeHttpProbe $Mock.ExpiredUri "GET" $headers "" $TimeoutSeconds
  $malformed = Invoke-SafeHttpProbe $Mock.MediaUri "GET" $headers "bytes=999999999-" $TimeoutSeconds
  $headerPass = (
    $unauthorized.StatusCode -eq 403 -and
    $authorizedHead.StatusCode -eq 200 -and
    $redirect.StatusCode -eq 200 -and
    $redirect.CrossOriginRedirect -and
    $redirect.SourceHeadersStripped
  )
  $headerElapsed = $unauthorized.ElapsedMs + $authorizedHead.ElapsedMs + $redirect.ElapsedMs + $expired.ElapsedMs + $malformed.ElapsedMs
  if ($headerPass) {
    [void]$stages.Add((New-StageResult $roleName "header" "pass" $headerElapsed "" `
      "Header enforcement and cross-origin stripping behaved as expected; header values are not recorded." ([ordered]@{
        unauthenticated_status = 403
        authorized_status = 200
        redirect_final_status = 200
        cross_origin_redirect = $true
        source_headers_stripped = $true
        header_names = @("Authorization", "Referer")
      })))
  } else {
    [void]$stages.Add((New-StageResult $roleName "header" "fail" $headerElapsed "mock_header_semantics_failed" `
      "Header enforcement or cross-origin stripping did not match the mock contract." ([ordered]@{
        unauthenticated_status = $unauthorized.StatusCode
        authorized_status = $authorizedHead.StatusCode
        redirect_final_status = $redirect.StatusCode
        cross_origin_redirect = $redirect.CrossOriginRedirect
        source_headers_stripped = $redirect.SourceHeadersStripped
        header_names = @("Authorization", "Referer")
      })))
  }

  [void]$stages.Add((New-StageResult $roleName "decode" "pending" 0 "client_decode_runner_missing" `
    "The mock proves transport behavior only; no client decoder was invoked." ([ordered]@{})))

  if (-not $rangePass -or $null -eq $rangeInfo -or $null -eq $rangeInfo.Total) {
    [void]$stages.Add((New-StageResult $roleName "seek" "skip" 0 "blocked_by_range" `
      "Skipped because the mock Range test did not pass." ([ordered]@{})))
  } else {
    $midpoint = [long][Math]::Floor($rangeInfo.Total / 2)
    $seekEnd = [Math]::Min($rangeInfo.Total - 1, $midpoint + $RangeBytes - 1)
    $seek = Invoke-SafeHttpProbe $Mock.MediaUri "GET" $headers ("bytes=$midpoint-$seekEnd") $TimeoutSeconds
    $seekInfo = Parse-ContentRange $seek.ContentRange
    if ($seek.StatusCode -eq 206 -and $null -ne $seekInfo -and $seekInfo.Start -eq $midpoint) {
      [void]$stages.Add((New-StageResult $roleName "seek" "pass" $seek.ElapsedMs "" `
        "HTTP midpoint range succeeded; this is not a player-seek result." ([ordered]@{
          probe_kind = "http-range"
          range_start = $seekInfo.Start
          range_end = $seekInfo.End
        })))
    } else {
      [void]$stages.Add((New-StageResult $roleName "seek" "fail" $seek.ElapsedMs "mock_seek_range_failed" `
        "HTTP midpoint range failed against the local mock." ([ordered]@{
          probe_kind = "http-range"
          status_code = $seek.StatusCode
          expected_start = $midpoint
        })))
    }
  }

  $rangeAnomalyPass = ($malformed.StatusCode -eq 416 -and $expired.StatusCode -eq 410)
  if (-not $rangeAnomalyPass) {
    [void]$stages.Add((New-StageResult $roleName "fallback" "pending" 0 "mock_anomaly_mismatch" `
      "S5 anomaly checks did not match 416/410 expectations; inspect the report before trusting S5 results." ([ordered]@{
        malformed_range_status = $malformed.StatusCode
        expired_status = $expired.StatusCode
      })))
  } else {
    [void]$stages.Add((New-StageResult $roleName "fallback" "pending" 0 "fallback_runner_missing" `
      "S5 malformed-range/expired behavior was observed; real fallback still needs a client or fallback endpoint." ([ordered]@{
        malformed_range_status = 416
        expired_status = 410
      })))
  }

  return @{
    Stages = @($stages.ToArray())
    Source = $source
  }
}

function Get-SampleOverallStatus {
  param($Stages)

  $stageList = @($Stages)
  if ($stageList.Count -eq 0) {
    return "pending"
  }
  $failCount = @($stageList | Where-Object { $_.status -eq "fail" }).Count
  if ($failCount -gt 0) {
    return "failed"
  }
  $passCount = @($stageList | Where-Object { $_.status -eq "pass" }).Count
  $pendingCount = @($stageList | Where-Object { $_.status -eq "pending" }).Count
  $skipCount = @($stageList | Where-Object { $_.status -eq "skip" }).Count
  if ($passCount -eq 0 -and $pendingCount -eq 0 -and $skipCount -gt 0) {
    return "skipped"
  }
  if ($pendingCount -gt 0) {
    return "partial"
  }
  if ($passCount -gt 0) {
    return "passed"
  }
  return "not_applicable"
}

function Get-SampleFailureCategories {
  param($Stages)

  return @(
    @($Stages) |
      Where-Object { $_.status -eq "fail" -and -not [string]::IsNullOrWhiteSpace($_.failure_category) } |
      ForEach-Object { $_.failure_category } |
      Sort-Object -Unique
  )
}

function Get-ClientMatrix {
  param($Manifest, [string]$CurrentNetworkLabel, [string]$CurrentEgressLabel, [hashtable]$SampleStatusMap)

  $matrix = New-Object System.Collections.Generic.List[object]
  foreach ($sample in @(Get-ObjectProperty $Manifest "samples")) {
    foreach ($device in @(Get-ObjectProperty $Manifest "devices")) {
      foreach ($network in @(Get-ObjectProperty $Manifest "networks")) {
        $egress = $null
        if ([string]$network.label -eq $CurrentNetworkLabel) {
          $egress = $CurrentEgressLabel
        }
        $sampleId = [string]$sample.id
        $transportStatus = $null
        if ($SampleStatusMap.ContainsKey($sampleId)) {
          $transportStatus = $SampleStatusMap[$sampleId]
        }
        [void]$matrix.Add([ordered]@{
          sample_id = $sampleId
          device_id = [string]$device.id
          platform = [string]$device.platform
          client_version = [string]$device.version
          soc = Get-ObjectProperty $device "soc"
          gpu = Get-ObjectProperty $device "gpu"
          network_id = [string]$network.id
          network_label = [string]$network.label
          egress_region_label = $egress
          transport_probe_status = $transportStatus
          status = "pending"
          failure_category = "client_runner_missing"
          summary = "Requires a real Windows/Android client run; transport probes do not count as playback results."
        })
      }
    }
  }
  return @($matrix.ToArray())
}

function ConvertTo-MarkdownText {
  param($Value)

  if ($null -eq $Value) {
    return "-"
  }
  $text = [string]$Value
  $text = $text.Replace("`r", " ").Replace("`n", " ")
  $text = $text.Replace("|", "\|")
  if ([string]::IsNullOrWhiteSpace($text)) {
    return "-"
  }
  return $text
}

function Format-SafeEvidence {
  param($Evidence)

  if ($null -eq $Evidence) {
    return "-"
  }
  $parts = New-Object System.Collections.Generic.List[string]
  $pairs = New-Object System.Collections.Generic.List[object]
  if ($Evidence -is [System.Collections.IDictionary]) {
    foreach ($key in $Evidence.Keys) {
      [void]$pairs.Add(@{ Name = [string]$key; Value = $Evidence[$key] })
    }
  } else {
    foreach ($property in $Evidence.PSObject.Properties) {
      [void]$pairs.Add(@{ Name = $property.Name; Value = $property.Value })
    }
  }
  foreach ($pair in $pairs) {
    $value = $pair.Value
    if ($null -eq $value) {
      continue
    }
    if ($value -is [System.Array]) {
      $text = (@($value) | ForEach-Object { [string]$_ }) -join ","
    } elseif ($value -is [bool]) {
      if ($value) {
        $text = "true"
      } else {
        $text = "false"
      }
    } else {
      $text = [string]$value
    }
    [void]$parts.Add($pair.Name + "=" + $text)
  }
  if ($parts.Count -eq 0) {
    return "-"
  }
  return (ConvertTo-MarkdownText ($parts -join "; "))
}

function New-MarkdownReport {
  param($Report)

  $sb = New-Object System.Text.StringBuilder
  [void]$sb.AppendLine("# Playback Capability Matrix")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("Generated: ``" + $Report.generated_at + "``")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("Transport results are host-level HTTP probes. They are not Windows/Android decoder, player-seek, or fallback results.")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Run Context")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("| Field | Value |")
  [void]$sb.AppendLine("| --- | --- |")
  [void]$sb.AppendLine("| Manifest | " + (ConvertTo-MarkdownText $Report.manifest.path) + " |")
  [void]$sb.AppendLine("| Network label | " + (ConvertTo-MarkdownText $Report.run_context.network_label) + " |")
  [void]$sb.AppendLine("| Egress region label | " + (ConvertTo-MarkdownText $Report.run_context.egress_region_label) + " |")
  [void]$sb.AppendLine("| Probe executor | " + (ConvertTo-MarkdownText ($Report.executor.platform + " " + $Report.executor.version)) + " |")
  [void]$sb.AppendLine("| PowerShell | " + (ConvertTo-MarkdownText $Report.executor.powershell) + " |")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Summary")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("| Status | Samples |")
  [void]$sb.AppendLine("| --- | ---: |")
  [void]$sb.AppendLine("| Partial transport | " + [string]$Report.summary.partial + " |")
  [void]$sb.AppendLine("| Failed | " + [string]$Report.summary.failed + " |")
  [void]$sb.AppendLine("| Skipped | " + [string]$Report.summary.skipped + " |")
  [void]$sb.AppendLine("| Passed | " + [string]$Report.summary.passed + " |")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Samples")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("| ID | Kind | Container | Video | Audio | Subtitles | File ID | Hosts | Status |")
  [void]$sb.AppendLine("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")

  foreach ($sample in @($Report.samples)) {
    $hosts = @($sample.sources | ForEach-Object { $_.host + ":" + [string]$_.port } | Sort-Object -Unique)
    [void]$sb.AppendLine(
      "| " + (ConvertTo-MarkdownText $sample.id) +
      " | " + (ConvertTo-MarkdownText $sample.kind) +
      " | " + (ConvertTo-MarkdownText $sample.container) +
      " | " + (ConvertTo-MarkdownText $sample.video) +
      " | " + (ConvertTo-MarkdownText ($sample.audio -join "/")) +
      " | " + (ConvertTo-MarkdownText ($sample.subtitles -join "/")) +
      " | " + (ConvertTo-MarkdownText $sample.file_id) +
      " | " + (ConvertTo-MarkdownText ($hosts -join ", ")) +
      " | " + (ConvertTo-MarkdownText $sample.status) +
      " |"
    )
  }

  foreach ($sample in @($Report.samples)) {
    [void]$sb.AppendLine("")
    [void]$sb.AppendLine("## " + $sample.id + ": " + $sample.source_label)
    [void]$sb.AppendLine("")
    [void]$sb.AppendLine("| Field | Value |")
    [void]$sb.AppendLine("| --- | --- |")
    [void]$sb.AppendLine("| Status | " + (ConvertTo-MarkdownText $sample.status) + " |")
    [void]$sb.AppendLine("| File ID | " + (ConvertTo-MarkdownText $sample.file_id) + " |")
    [void]$sb.AppendLine("| File ID source | " + (ConvertTo-MarkdownText $sample.file_id_source) + " |")
    [void]$sb.AppendLine("| Secret ref | " + (ConvertTo-MarkdownText $sample.secret_ref) + " |")
    [void]$sb.AppendLine("| Coverage | " + (ConvertTo-MarkdownText $sample.coverage_status) + " |")
    if (-not [string]::IsNullOrWhiteSpace([string]$sample.skip_reason)) {
      [void]$sb.AppendLine("| Skip reason | " + (ConvertTo-MarkdownText $sample.skip_reason) + " |")
    }
    if (-not [string]::IsNullOrWhiteSpace([string]$sample.coverage_gap)) {
      [void]$sb.AppendLine("| Coverage gap | " + (ConvertTo-MarkdownText $sample.coverage_gap) + " |")
    }
    [void]$sb.AppendLine("")
    if (@($sample.sources).Count -gt 0) {
      [void]$sb.AppendLine("Sources:")
      [void]$sb.AppendLine("")
      [void]$sb.AppendLine("| Role | Transport | Host | Port |")
      [void]$sb.AppendLine("| --- | --- | --- | ---: |")
      foreach ($source in @($sample.sources)) {
        [void]$sb.AppendLine(
          "| " + (ConvertTo-MarkdownText $source.role) +
          " | " + (ConvertTo-MarkdownText $source.transport) +
          " | " + (ConvertTo-MarkdownText $source.host) +
          " | " + [string]$source.port +
          " |"
        )
      }
      [void]$sb.AppendLine("")
    }

    [void]$sb.AppendLine("| Role | Stage | Status | Elapsed ms | Failure | Summary | Evidence |")
    [void]$sb.AppendLine("| --- | --- | --- | ---: | --- | --- | --- |")
    foreach ($stage in @($sample.stages)) {
      [void]$sb.AppendLine(
        "| " + (ConvertTo-MarkdownText $stage.role) +
        " | " + (ConvertTo-MarkdownText $stage.name) +
        " | " + (ConvertTo-MarkdownText $stage.status) +
        " | " + [string]$stage.elapsed_ms +
        " | " + (ConvertTo-MarkdownText $stage.failure_category) +
        " | " + (ConvertTo-MarkdownText $stage.summary) +
        " | " + (Format-SafeEvidence $stage.evidence) +
        " |"
      )
    }
  }

  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Device And Network Matrix")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("Every client cell is pending until W0/A0 records a real device run. The JSON report contains one row per sample/device/network cell.")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("| Device | Platform | Version | SoC | GPU | Network cells | Client status |")
  [void]$sb.AppendLine("| --- | --- | --- | --- | --- | --- | --- |")
  foreach ($device in @($Report.devices)) {
    $networkLabels = @($Report.networks | ForEach-Object { $_.label }) -join ", "
    [void]$sb.AppendLine(
      "| " + (ConvertTo-MarkdownText $device.id) +
      " | " + (ConvertTo-MarkdownText $device.platform) +
      " | " + (ConvertTo-MarkdownText $device.version) +
      " | " + (ConvertTo-MarkdownText $device.soc) +
      " | " + (ConvertTo-MarkdownText $device.gpu) +
      " | " + (ConvertTo-MarkdownText $networkLabels) +
      " | pending |"
    )
  }

  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Pending And Risks")
  [void]$sb.AppendLine("")
  foreach ($item in @($Report.pending_items)) {
    [void]$sb.AppendLine("- " + (ConvertTo-MarkdownText $item))
  }
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("## Log Safety")
  [void]$sb.AppendLine("")
  [void]$sb.AppendLine("This report intentionally omits source paths, queries, fragments, header values, tickets, tokens, cookies, and authorization values. Only transport, host, port, status, timing, header names, and failure categories are recorded.")
  [void]$sb.AppendLine("")

  return $sb.ToString()
}

function Write-Utf8File {
  param([string]$Path, [string]$Content)

  $encoding = New-Object System.Text.UTF8Encoding($false)
  [System.IO.File]::WriteAllText($Path, $Content, $encoding)
}

function ConvertTo-SafeErrorMessage {
  param([string]$Message)

  $safe = $Message
  $safe = ([regex]'(?i)https?://[^\s"'']+').Replace($safe, "<redacted-url>")
  $safe = ([regex]'(?i)(authorization|ticket|token|cookie)\s*[:=]\s*[^\s,;]+').Replace($safe, '$1=<redacted>')
  return $safe
}

function Show-DryRun {
  param(
    $Manifest,
    $SelectedSamples,
    [hashtable]$FileIdMap,
    [string]$NetworkLabel,
    [string]$EgressRegion
  )

  $sampleCount = @(Get-ObjectProperty $Manifest "samples").Count
  $deviceCount = @(Get-ObjectProperty $Manifest "devices").Count
  $networkCount = @(Get-ObjectProperty $Manifest "networks").Count

  Write-Host "[ok] manifest validation passed"
  Write-Host "     schema_version=1 samples=$sampleCount devices=$deviceCount networks=$networkCount"
  Write-Host ""

  Write-Host "Samples selected:"
  Write-Host ("  {0,-8} {1,-10} {2,-9} {3,-10} {4,-14} {5,-12} {6}" -f "ID", "KIND", "CONTAINER", "VIDEO", "AUDIO", "FILE_ID", "SECRET")
  foreach ($sample in $SelectedSamples) {
    $secretRef = [string]$sample.secret_ref
    $secretState = "missing"
    if ($sample.kind -eq "mock") {
      $secretState = "local-mock"
    } elseif (-not [string]::IsNullOrWhiteSpace((Get-EnvironmentValue $secretRef))) {
      $secretState = "configured"
    }
    $fileIdState = (Resolve-SampleFileId $sample $FileIdMap).Source
    Write-Host ("  {0,-8} {1,-10} {2,-9} {3,-10} {4,-14} {5,-12} {6}" -f `
      $sample.id, $sample.kind, $sample.container, $sample.video, (@($sample.audio) -join "/"), $fileIdState, $secretState)
  }
  Write-Host ""

  Write-Host "Planned samples x devices (client cells are pending until real device runs):"
  $deviceHeaders = @("SAMPLE") + @($Manifest.devices | ForEach-Object { [string]$_.id })
  Write-Host ("  " + ($deviceHeaders -join " | "))
  foreach ($sample in $SelectedSamples) {
    $cells = @($Manifest.devices | ForEach-Object { "planned" })
    Write-Host ("  " + (([string]$sample.id) + " | " + ($cells -join " | ")))
  }
  Write-Host ""

  Write-Host "Devices:"
  foreach ($device in @($Manifest.devices)) {
    Write-Host ("  {0,-24} platform={1,-8} version={2}" -f $device.id, $device.platform, $device.version)
  }
  Write-Host ""

  Write-Host "Networks:"
  foreach ($network in @($Manifest.networks)) {
    Write-Host ("  {0,-22} label={1}" -f $network.id, $network.label)
  }
  Write-Host "  current-run label=$NetworkLabel egress=$EgressRegion (labels only; no network call)"
  Write-Host ""

  Write-Host "Planned transport probes per network:"
  $networkHeaders = @("SAMPLE") + @($Manifest.networks | ForEach-Object { [string]$_.label })
  Write-Host ("  " + ($networkHeaders -join " | "))
  foreach ($sample in $SelectedSamples) {
    $secretState = "configured"
    if ($sample.kind -eq "mock") {
      $secretState = "local-mock"
    } elseif ([string]::IsNullOrWhiteSpace((Get-EnvironmentValue ([string]$sample.secret_ref)))) {
      $secretState = "skip:secret"
    }
    $cells = @($Manifest.networks | ForEach-Object { $secretState })
    Write-Host ("  " + (([string]$sample.id) + " | " + ($cells -join " | ")))
  }
  Write-Host ""

  Write-Host ("Stage set: " + ($script:StageNames -join ", "))
  foreach ($sample in $SelectedSamples) {
    $gap = [string](Get-ObjectProperty $sample "coverage_gap")
    if (-not [string]::IsNullOrWhiteSpace($gap)) {
      Write-Host ("[pending] {0}: {1}" -f $sample.id, $gap)
    }
  }
  Write-Host "[pending] decode, real player seek, and fallback require W0/A0 client runners or v1 endpoints."
  Write-Host "[dry-run] no network access and no report files written."
}

$manifest = $null
$selectedSamples = @()
$fileIdMap = @{}
$mock = $null

try {
  $manifest = Read-MatrixManifest $ManifestPath
  $fileIdMap = ConvertTo-FileIdMap $FileIds $manifest
  $allSamples = @(Get-ObjectProperty $manifest "samples")

  if ($Samples.Count -gt 0) {
    $selectedSamples = @($allSamples | Where-Object { $Samples -contains ([string]$_.id) })
    $requested = @($Samples | Sort-Object -Unique)
    $found = @($selectedSamples | ForEach-Object { [string]$_.id } | Sort-Object -Unique)
    $missingRequested = @($requested | Where-Object { $found -notcontains $_ })
    if ($missingRequested.Count -gt 0) {
      throw "unknown sample selection: " + ($missingRequested -join ",")
    }
  } else {
    $selectedSamples = @($allSamples)
  }

  if ($selectedSamples.Count -eq 0) {
    throw "no samples selected"
  }

  $networkLabel = ConvertTo-SafeLabel $NetworkLabel "unlabeled"
  if ([string]::IsNullOrWhiteSpace($NetworkLabel)) {
    $envNetworkLabel = Get-EnvironmentValue "DORIYA_MATRIX_NETWORK_LABEL"
    if (-not [string]::IsNullOrWhiteSpace($envNetworkLabel)) {
      $networkLabel = ConvertTo-SafeLabel $envNetworkLabel "unlabeled"
    }
  }
  $egressLabel = ConvertTo-SafeLabel $EgressRegion "unlabeled"
  if ([string]::IsNullOrWhiteSpace($EgressRegion)) {
    $envEgressLabel = Get-EnvironmentValue "DORIYA_MATRIX_EGRESS_REGION"
    if (-not [string]::IsNullOrWhiteSpace($envEgressLabel)) {
      $egressLabel = ConvertTo-SafeLabel $envEgressLabel "unlabeled"
    }
  }

  if ($DryRun) {
    Show-DryRun $manifest $selectedSamples $fileIdMap $networkLabel $egressLabel
    exit 0
  }

  $s5Selected = (@($selectedSamples | Where-Object { [string]$_.id -eq "S5" }).Count -gt 0)
  if ($s5Selected -and -not $SkipS5Mock) {
    $helperPath = Join-Path $PSScriptRoot "playback-matrix-mock.ps1"
    $mock = Start-MatrixMock $helperPath $MockLifetimeSeconds
  }

  $outputSamples = New-Object System.Collections.Generic.List[object]
  $sampleStatusMap = @{}

  foreach ($sample in $selectedSamples) {
    $sampleId = [string]$sample.id
    $fileIdResult = Resolve-SampleFileId $sample $fileIdMap
    $stages = @()
    $sources = @()
    $warnings = New-Object System.Collections.Generic.List[string]
    $skipReason = $null
    $transportMode = "http"

    if ($fileIdResult.Status -eq "missing") {
      [void]$warnings.Add("file-id-missing")
    } elseif ($fileIdResult.Status -eq "invalid") {
      [void]$warnings.Add("file-id-invalid")
    }

    if ($sample.kind -eq "mock") {
      if ($SkipS5Mock) {
        $transportMode = "local-mock-disabled"
        $skipReason = "s5_mock_disabled"
        $stages = New-SkippedStageSet "mock" $skipReason "S5 was selected with -SkipS5Mock."
      } elseif ($null -eq $mock -or -not $mock.Ok) {
        $transportMode = "local-mock-unavailable"
        $skipReason = "s5_mock_unavailable"
        $stages = New-SkippedStageSet "mock" $skipReason "The loopback mock did not start."
      } else {
        $transportMode = "local-loopback"
        try {
          $probe = Invoke-MockSampleProbe $mock $sample $fileIdResult $TimeoutSeconds $RangeBytes
          $stages = @($probe.Stages)
          $sources = @($probe.Source)
        } catch {
          $skipReason = "mock_probe_error"
          $stages = New-SkippedStageSet "mock" $skipReason "The loopback mock probe failed without exposing request details."
        }
      }
    } else {
      $bundle = Read-SourceBundle $sample
      foreach ($warning in @($bundle.Warnings)) {
        [void]$warnings.Add([string]$warning)
      }
      if (-not $bundle.Ok) {
        $skipReason = $bundle.FailureCategory
        $stages = New-SkippedStageSet "unavailable" $skipReason "No source request was made; inspect the source secret configuration."
      } else {
        $stageList = New-Object System.Collections.Generic.List[object]
        $sourceList = New-Object System.Collections.Generic.List[object]
        foreach ($role in @($bundle.Roles)) {
          try {
            $probe = Invoke-RemoteRoleProbe $role $sample $fileIdResult $TimeoutSeconds $RangeBytes
            foreach ($stage in @($probe.Stages)) {
              [void]$stageList.Add($stage)
            }
            [void]$sourceList.Add($probe.Source)
          } catch {
            foreach ($stage in @(New-SkippedStageSet ([string]$role.Role) "probe_error" "The role probe failed without exposing request details.")) {
              [void]$stageList.Add($stage)
            }
            [void]$sourceList.Add((Get-SafeSource $role.Uri ([string]$role.Role)))
          }
        }
        $stages = @($stageList.ToArray())
        $sources = @($sourceList.ToArray())
      }
    }

    $overallStatus = Get-SampleOverallStatus $stages
    if (-not [string]::IsNullOrWhiteSpace($skipReason)) {
      if ($skipReason -like "s5_mock*" -or $skipReason -eq "secret_missing") {
        $overallStatus = "skipped"
      } elseif ($overallStatus -eq "not_applicable") {
        $overallStatus = "failed"
      }
    }
    $sampleStatusMap[$sampleId] = $overallStatus

    $sampleRecord = [ordered]@{
      id = $sampleId
      kind = [string]$sample.kind
      source_label = [string](Get-ObjectProperty $sample "source_label")
      container = [string]$sample.container
      video = [string]$sample.video
      video_profile = Get-ObjectProperty $sample "video_profile"
      audio = @(Get-ObjectProperty $sample "audio")
      features = @(Get-ObjectProperty $sample "features")
      subtitles = @(Get-ObjectProperty $sample "subtitles")
      source_url = $null
      source_url_policy = "environment-only"
      secret_ref = [string]$sample.secret_ref
      file_id = $fileIdResult.Value
      file_id_source = $fileIdResult.Source
      file_id_status = $fileIdResult.Status
      coverage = [string]$sample.coverage
      coverage_status = [string]$sample.coverage_status
      coverage_gap = Get-ObjectProperty $sample "coverage_gap"
      transport_mode = $transportMode
      network_label = $networkLabel
      egress_region_label = $egressLabel
      status = $overallStatus
      skip_reason = $skipReason
      warnings = @($warnings.ToArray())
      failure_categories = @(Get-SampleFailureCategories $stages)
      sources = @($sources)
      stages = @($stages)
    }
    [void]$outputSamples.Add($sampleRecord)

    $hosts = @($sources | ForEach-Object { [string]$_.host } | Sort-Object -Unique)
    $hostSummary = "-"
    if ($hosts.Count -gt 0) {
      $hostSummary = $hosts -join ","
    }
    $stageSummary = @($stages | ForEach-Object { $_.name + ":" + $_.status }) -join ","
    Write-Host ("[{0}] status={1} file_id={2} host={3} stages={4}" -f `
      $sampleId, $overallStatus, $fileIdResult.Source, $hostSummary, $stageSummary)
    if ($warnings.Count -gt 0) {
      Write-Host ("       warnings=" + (@($warnings.ToArray()) -join ","))
    }
  }

  $samplesArray = @($outputSamples.ToArray())
  $statusCounts = @{
    partial = @($samplesArray | Where-Object { $_.status -eq "partial" }).Count
    failed = @($samplesArray | Where-Object { $_.status -eq "failed" }).Count
    skipped = @($samplesArray | Where-Object { $_.status -eq "skipped" }).Count
    passed = @($samplesArray | Where-Object { $_.status -eq "passed" }).Count
  }

  $clientMatrix = Get-ClientMatrix $manifest $networkLabel $egressLabel $sampleStatusMap
  $pendingItems = New-Object System.Collections.Generic.List[string]
  [void]$pendingItems.Add("Windows and Android decode/player-seek results are pending until W0/A0 device runs are recorded.")
  [void]$pendingItems.Add("Fallback through server-proxy/server-HLS is pending because the v1 playback endpoints do not exist in S0.")
  [void]$pendingItems.Add("The v1 ticket stage is pending; file IDs are recorded for the later ticket baseline.")
  foreach ($sample in $samplesArray) {
    if (-not [string]::IsNullOrWhiteSpace([string]$sample.coverage_gap)) {
      [void]$pendingItems.Add($sample.id + ": " + $sample.coverage_gap)
    }
  }

  $generatedAt = (Get-Date).ToUniversalTime().ToString("o")
  $osVersion = [System.Environment]::OSVersion.VersionString
  $report = [ordered]@{
    schema_version = 1
    generated_at = $generatedAt
    dry_run = $false
    manifest = [ordered]@{
      path = "protocol/fixtures/media-samples.json"
      schema_version = [int]$manifest.schema_version
    }
    executor = [ordered]@{
      platform = "windows-host-probe"
      version = $osVersion
      powershell = $PSVersionTable.PSVersion.ToString()
    }
    run_context = [ordered]@{
      network_label = $networkLabel
      egress_region_label = $egressLabel
      timeout_seconds = $TimeoutSeconds
      range_bytes = $RangeBytes
    }
    stage_names = @($script:StageNames)
    summary = $statusCounts
    devices = @(Get-ObjectProperty $manifest "devices")
    networks = @(Get-ObjectProperty $manifest "networks")
    samples = @($samplesArray)
    client_matrix = @($clientMatrix)
    pending_items = @($pendingItems.ToArray())
  }

  $reportDir = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($OutputDirectory)
  if (-not (Test-Path -LiteralPath $reportDir -PathType Container)) {
    New-Item -ItemType Directory -Path $reportDir -Force | Out-Null
  }
  $stamp = (Get-Date).ToUniversalTime().ToString("yyyyMMdd'T'HHmmss'Z'")
  $baseName = "playback-matrix-$stamp-$PID"
  $jsonPath = Join-Path $reportDir ($baseName + ".json")
  $markdownPath = Join-Path $reportDir ($baseName + ".md")

  $jsonText = $report | ConvertTo-Json -Depth 20
  $markdownText = New-MarkdownReport $report
  Write-Utf8File $jsonPath $jsonText
  Write-Utf8File $markdownPath $markdownText

  Write-Host ""
  Write-Host "[ok] report written"
  Write-Host "     JSON: $jsonPath"
  Write-Host "     Markdown: $markdownPath"
  Write-Host ("     summary: partial={0} failed={1} skipped={2} passed={3}" -f `
    $statusCounts.partial, $statusCounts.failed, $statusCounts.skipped, $statusCounts.passed)
  exit 0
} catch {
  $safeMessage = ConvertTo-SafeErrorMessage ([string]$_.Exception.Message)
  Write-Host ("[FAIL] " + $safeMessage) -ForegroundColor Red
  exit 1
} finally {
  Stop-MatrixMock $mock
}
