[CmdletBinding()]
param(
  [string]$File = (Join-Path $PSScriptRoot "synthetic\s1-h264-aac.mp4"),
  [string]$MpvPath = $(if ($env:DORIYA_W0_MPV_PATH) { $env:DORIYA_W0_MPV_PATH } else { "mpv" })
)

$ErrorActionPreference = "Stop"
$mediaPath = (Resolve-Path -LiteralPath $File).Path
$serverScript = Join-Path $PSScriptRoot "header-range-server.py"
$python = (Get-Command python).Source
$id = [Guid]::NewGuid().ToString("N")
$stdoutPath = Join-Path $env:TEMP "doriya-header-range-$id.stdout"
$stderrPath = Join-Path $env:TEMP "doriya-header-range-$id.stderr"
$serverProcess = $null

try {
  $serverProcess = Start-Process `
    -FilePath $python `
    -ArgumentList @("-u", $serverScript, "--file", $mediaPath) `
    -WindowStyle Hidden `
    -PassThru `
    -RedirectStandardOutput $stdoutPath `
    -RedirectStandardError $stderrPath

  $deadline = (Get-Date).AddSeconds(10)
  $fixture = $null
  while ((Get-Date) -lt $deadline) {
    if ($serverProcess.HasExited) {
      throw "Fixture server exited with code $($serverProcess.ExitCode)."
    }
    if (Test-Path -LiteralPath $stdoutPath) {
      $line =
        Get-Content -LiteralPath $stdoutPath -ErrorAction SilentlyContinue |
          Select-Object -First 1
      if ($line) {
        $fixture = $line | ConvertFrom-Json
        break
      }
    }
    Start-Sleep -Milliseconds 100
  }
  if ($null -eq $fixture) {
    throw "Fixture server did not publish its URL within 10 seconds."
  }

  $headers = @{
    Referer = [string]$fixture.referer
    "User-Agent" = [string]$fixture.user_agent
  }
  $noHeaderStatus =
    try {
      (Invoke-WebRequest -Uri $fixture.url -Method Head -SkipHttpErrorCheck).StatusCode
    }
    catch {
      [int]$_.Exception.Response.StatusCode
    }
  if ($noHeaderStatus -ne 403) {
    throw "Expected missing Header status 403, got $noHeaderStatus."
  }

  $head = Invoke-WebRequest -Uri $fixture.url -Method Head -Headers $headers
  if ($head.StatusCode -ne 200) {
    throw "Expected HEAD status 200, got $($head.StatusCode)."
  }
  $headLength = [long](@($head.Headers["Content-Length"])[0])
  if ($headLength -ne [long]$fixture.size) {
    throw "HEAD Content-Length did not match the fixture size."
  }

  $range =
    Invoke-WebRequest `
      -Uri $fixture.url `
      -Headers ($headers + @{ Range = "bytes=0-99" })
  if ($range.StatusCode -ne 206 -or $range.RawContentLength -ne 100) {
    throw "Expected a 100-byte 206 response."
  }
  $expectedContentRange = "bytes 0-99/$($fixture.size)"
  $actualContentRange = [string](@($range.Headers["Content-Range"])[0])
  if ($actualContentRange -ne $expectedContentRange) {
    throw "Unexpected Content-Range."
  }

  $unsatisfiedStatus =
    try {
      (
        Invoke-WebRequest `
          -Uri $fixture.url `
          -Headers ($headers + @{ Range = "bytes=$($fixture.size)-" }) `
          -SkipHttpErrorCheck
      ).StatusCode
    }
    catch {
      [int]$_.Exception.Response.StatusCode
    }
  if ($unsatisfiedStatus -ne 416) {
    throw "Expected unsatisfied Range status 416, got $unsatisfiedStatus."
  }

  & $MpvPath `
    --no-config `
    --vo=null `
    --ao=null `
    --really-quiet `
    --frames=1 `
    --force-window=no `
    "--referrer=$($fixture.referer)" `
    "--user-agent=$($fixture.user_agent)" `
    $fixture.url
  if ($LASTEXITCODE -ne 0) {
    throw "mpv failed to decode the Header-protected loopback fixture."
  }

  Write-Output "HEADER_RANGE_FIXTURE|PASS|size=$($fixture.size)|head=200|range=206|unsatisfied=416|mpv=PASS"
}
finally {
  if ($serverProcess -and -not $serverProcess.HasExited) {
    Stop-Process -Id $serverProcess.Id -Force
    Wait-Process -Id $serverProcess.Id -Timeout 10 -ErrorAction SilentlyContinue
  }
  foreach ($path in @($stdoutPath, $stderrPath)) {
    if (Test-Path -LiteralPath $path) {
      Remove-Item -LiteralPath $path -Force
    }
  }
}
