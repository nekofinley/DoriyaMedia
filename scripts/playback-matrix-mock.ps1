[CmdletBinding()]
param(
  [Parameter(Mandatory = $true)]
  [int]$Port1,
  [Parameter(Mandatory = $true)]
  [int]$Port2,
  [Parameter(Mandatory = $true)]
  [string]$AuthToken,
  [Parameter(Mandatory = $true)]
  [string]$RefererValue,
  [int]$LifetimeSeconds = 300,
  [int]$BodyLength = 1048576
)

$ErrorActionPreference = "Stop"

function Write-MockResponse {
  param(
    [System.IO.Stream]$Stream,
    [int]$StatusCode,
    [string]$Reason,
    [hashtable]$Headers,
    [byte[]]$Body,
    [switch]$SendBody
  )

  $sb = New-Object System.Text.StringBuilder
  [void]$sb.Append("HTTP/1.1 $StatusCode $Reason`r`n")
  foreach ($name in $Headers.Keys) {
    [void]$sb.Append("${name}: $($Headers[$name])`r`n")
  }
  [void]$sb.Append("Connection: close`r`n")
  [void]$sb.Append("Cache-Control: no-store`r`n`r`n")

  $head = [System.Text.Encoding]::ASCII.GetBytes($sb.ToString())
  $Stream.Write($head, 0, $head.Length)
  if ($SendBody -and $null -ne $Body -and $Body.Length -gt 0) {
    $Stream.Write($Body, 0, $Body.Length)
  }
  $Stream.Flush()
}

function Read-MockRequest {
  param([System.IO.Stream]$Stream)

  $reader = New-Object System.IO.StreamReader($Stream, [System.Text.Encoding]::ASCII, $false, 1024, $true)
  $requestLine = $reader.ReadLine()
  if ([string]::IsNullOrWhiteSpace($requestLine)) {
    return $null
  }

  $parts = $requestLine -split " "
  if ($parts.Length -lt 2) {
    return $null
  }

  $headers = @{}
  while ($true) {
    $line = $reader.ReadLine()
    if ($null -eq $line -or $line -eq "") {
      break
    }
    $colon = $line.IndexOf(":")
    if ($colon -gt 0) {
      $name = $line.Substring(0, $colon).Trim().ToLowerInvariant()
      $value = $line.Substring($colon + 1).Trim()
      $headers[$name] = $value
    }
  }

  $target = $parts[1]
  $path = ($target -split "\?", 2)[0]
  return @{
    Method = $parts[0].ToUpperInvariant()
    Target = $target
    Path = $path
    Headers = $headers
  }
}

function Test-MockHeaders {
  param([hashtable]$Headers)

  $auth = ""
  $referer = ""
  if ($Headers.ContainsKey("authorization")) {
    $auth = [string]$Headers["authorization"]
  }
  if ($Headers.ContainsKey("referer")) {
    $referer = [string]$Headers["referer"]
  }
  return ($auth -ceq ("Bearer " + $AuthToken) -and $referer -ceq $RefererValue)
}

function New-MockBody {
  param([int]$Length)

  $body = New-Object byte[] $Length
  for ($i = 0; $i -lt $Length; $i++) {
    $body[$i] = [byte](($i + 17) % 251)
  }
  return ,$body
}

function Send-Empty {
  param(
    [System.IO.Stream]$Stream,
    [int]$StatusCode,
    [string]$Reason,
    [hashtable]$Extra = @{}
  )

  $headers = @{
    "Content-Length" = "0"
    "Content-Type" = "text/plain"
  }
  foreach ($key in $Extra.Keys) {
    $headers[$key] = $Extra[$key]
  }
  Write-MockResponse -Stream $Stream -StatusCode $StatusCode -Reason $Reason -Headers $headers
}

function Send-Media {
  param(
    [System.IO.Stream]$Stream,
    [hashtable]$RequestHeaders,
    [string]$Method
  )

  if (-not (Test-MockHeaders -Headers $RequestHeaders)) {
    Send-Empty -Stream $Stream -StatusCode 403 -Reason "Forbidden"
    return
  }

  $range = ""
  if ($RequestHeaders.ContainsKey("range")) {
    $range = [string]$RequestHeaders["range"]
  }

  if ($range -eq "") {
    $headers = @{
      "Content-Length" = [string]$BodyLength
      "Content-Type" = "video/mp4"
      "Accept-Ranges" = "bytes"
    }
    if ($Method -eq "HEAD") {
      Write-MockResponse -Stream $Stream -StatusCode 200 -Reason "OK" -Headers $headers
    } else {
      $body = New-MockBody -Length ([Math]::Min(1024, $BodyLength))
      $headers["Content-Length"] = [string]$body.Length
      Write-MockResponse -Stream $Stream -StatusCode 200 -Reason "OK" -Headers $headers -Body $body -SendBody
    }
    return
  }

  if ($range -notmatch '^bytes=(\d+)-(\d*)$') {
    Send-Empty -Stream $Stream -StatusCode 416 -Reason "Range Not Satisfiable" -Extra @{
      "Content-Range" = "bytes */$BodyLength"
      "Accept-Ranges" = "bytes"
    }
    return
  }

  $start = [long]$Matches[1]
  $endText = [string]$Matches[2]
  if ($start -lt 0 -or $start -ge $BodyLength) {
    Send-Empty -Stream $Stream -StatusCode 416 -Reason "Range Not Satisfiable" -Extra @{
      "Content-Range" = "bytes */$BodyLength"
      "Accept-Ranges" = "bytes"
    }
    return
  }

  if ($endText -eq "") {
    $end = [Math]::Min([long]$BodyLength - 1, $start + 1023)
  } else {
    $end = [long]$endText
  }
  if ($end -lt $start -or $end -ge $BodyLength) {
    if ($end -lt $start) {
      Send-Empty -Stream $Stream -StatusCode 416 -Reason "Range Not Satisfiable" -Extra @{
        "Content-Range" = "bytes */$BodyLength"
        "Accept-Ranges" = "bytes"
      }
      return
    }
    $end = [long]$BodyLength - 1
  }

  $count = [int]([Math]::Min(65536, $end - $start + 1))
  $body = New-MockBody -Length $count
  $headers = @{
    "Content-Length" = [string]($end - $start + 1)
    "Content-Range" = "bytes $start-$end/$BodyLength"
    "Content-Type" = "video/mp4"
    "Accept-Ranges" = "bytes"
  }
  if ($Method -eq "HEAD") {
    Write-MockResponse -Stream $Stream -StatusCode 206 -Reason "Partial Content" -Headers $headers
  } else {
    Write-MockResponse -Stream $Stream -StatusCode 206 -Reason "Partial Content" -Headers $headers -Body $body -SendBody
  }
}

function Send-Hop {
  param([System.IO.Stream]$Stream)

  Send-Empty -Stream $Stream -StatusCode 200 -Reason "OK" -Extra @{
    "X-Doriya-Mock" = "hop"
  }
}

$listener1 = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $Port1)
$listener2 = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Loopback, $Port2)

try {
  $listener1.Start()
  $listener2.Start()
  $deadline = (Get-Date).AddSeconds($LifetimeSeconds)

  while ((Get-Date) -lt $deadline) {
    $work = @(
      @{ Listener = $listener1; Port = $Port1 },
      @{ Listener = $listener2; Port = $Port2 }
    )
    $didWork = $false

    foreach ($item in $work) {
      $listener = $item.Listener
      if (-not $listener.Pending()) {
        continue
      }
      $didWork = $true
      $client = $null
      $stream = $null
      try {
        $client = $listener.AcceptTcpClient()
        $stream = $client.GetStream()
        $request = Read-MockRequest -Stream $stream
        if ($null -eq $request) {
          continue
        }

        if ($item.Port -eq $Port2) {
          if ($request.Path -eq "/hop") {
            $sensitive = @("authorization", "cookie", "referer", "origin")
            $leaked = $false
            foreach ($name in $sensitive) {
              if ($request.Headers.ContainsKey($name)) {
                $leaked = $true
                break
              }
            }
            if ($leaked) {
              Send-Empty -Stream $stream -StatusCode 400 -Reason "Bad Request"
            } else {
              Send-Hop -Stream $stream
            }
          } else {
            Send-Empty -Stream $stream -StatusCode 404 -Reason "Not Found"
          }
          continue
        }

        if (-not (Test-MockHeaders -Headers $request.Headers)) {
          Send-Empty -Stream $stream -StatusCode 403 -Reason "Forbidden"
          continue
        }

        switch ($request.Path) {
          "/media.mp4" {
            Send-Media -Stream $stream -RequestHeaders $request.Headers -Method $request.Method
          }
          "/expired.mp4" {
            Send-Empty -Stream $stream -StatusCode 410 -Reason "Gone"
          }
          "/redirect" {
            Write-MockResponse -Stream $stream -StatusCode 302 -Reason "Found" -Headers @{
              "Content-Length" = "0"
              "Location" = "http://127.0.0.1:$Port2/hop"
            }
          }
          default {
            Send-Empty -Stream $stream -StatusCode 404 -Reason "Not Found"
          }
        }
      } catch {
        if ($null -ne $stream) {
          try {
            Send-Empty -Stream $stream -StatusCode 400 -Reason "Bad Request"
          } catch {
          }
        }
      } finally {
        if ($null -ne $client) {
          try {
            $client.Close()
          } catch {
          }
        }
      }
    }

    if (-not $didWork) {
      Start-Sleep -Milliseconds 20
    }
  }
} finally {
  try {
    $listener1.Stop()
  } catch {
  }
  try {
    $listener2.Stop()
  } catch {
  }
}
