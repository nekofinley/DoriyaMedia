[CmdletBinding()]
param(
  [string]$MpvPath = $(if ($env:DORIYA_W0_MPV_PATH) { $env:DORIYA_W0_MPV_PATH } else { "mpv" }),
  [switch]$SkipMpv
)

$ErrorActionPreference = "Stop"
$root = $PSScriptRoot

$fixtures = @(
  @{
    Name = "s1-h264-aac.mp4"
    Codecs = @("h264", "aac")
  },
  @{
    Name = "s2-h264-eac3.mkv"
    Codecs = @("h264", "eac3")
  },
  @{
    Name = "s3-hevc10-flac.mkv"
    Codecs = @("hevc", "flac")
  }
)

foreach ($fixture in $fixtures) {
  $path = Join-Path $root $fixture.Name
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
    throw "Missing synthetic fixture: $path"
  }

  $probeJson =
    & ffprobe -v error -show_entries stream=codec_name -of json $path
  if ($LASTEXITCODE -ne 0) {
    throw "ffprobe failed for $($fixture.Name) with exit code $LASTEXITCODE."
  }

  $actual =
    @(
      ($probeJson | ConvertFrom-Json).streams |
        ForEach-Object { $_.codec_name }
    )
  $expected = @($fixture.Codecs)
  if (($actual -join ",") -ne ($expected -join ",")) {
    throw "Codec mismatch for $($fixture.Name): expected $($expected -join ', '), got $($actual -join ', ')."
  }

  if (-not $SkipMpv) {
    & $MpvPath --no-config --vo=null --ao=null --really-quiet --frames=1 `
      --force-window=no $path
    if ($LASTEXITCODE -ne 0) {
      throw "mpv decode failed for $($fixture.Name) with exit code $LASTEXITCODE."
    }
  }

  Write-Output "SYNTHETIC_FIXTURE|PASS|$($fixture.Name)|codecs=$($actual -join ',')"
}

Write-Output "SYNTHETIC_FIXTURES|PASS|count=$($fixtures.Count)|realS2=NOT_VERIFIED"
