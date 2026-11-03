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
    Sha256 = "40fca3ce084434f32c6f9c335c01acc49e9b083e843f1f793f26865560460b89"
    Streams = @(
      @{
        Index = 0
        CodecType = "video"
        CodecName = "h264"
        Profile = "Constrained Baseline"
        PixFmt = "yuv420p"
      },
      @{
        Index = 1
        CodecType = "audio"
        CodecName = "aac"
        Profile = "LC"
      }
    )
  },
  @{
    Name = "s1-video-only.mp4"
    Sha256 = "29d5ccb3d795312eb72b0402069829ddae0dc80e85d22fd0bbe4cff791c67509"
    Streams = @(
      @{
        Index = 0
        CodecType = "video"
        CodecName = "h264"
        Profile = "Constrained Baseline"
        PixFmt = "yuv420p"
      }
    )
  },
  @{
    Name = "s1-audio-only.mp4"
    Sha256 = "a285fc728bd5d7bd4df4acbf20b2cfb264833f87503e94539055deaf23ded104"
    Streams = @(
      @{
        Index = 0
        CodecType = "audio"
        CodecName = "aac"
        Profile = "LC"
      }
    )
  },
  @{
    Name = "s2-h264-eac3.mkv"
    Sha256 = "218c9eb551fe36b3cde75c415efa4c13cb21337e5ef73e0eda195561d68afcdb"
    Streams = @(
      @{
        Index = 0
        CodecType = "video"
        CodecName = "h264"
        Profile = "Constrained Baseline"
        PixFmt = "yuv420p"
      },
      @{
        Index = 1
        CodecType = "audio"
        CodecName = "eac3"
        Profile = $null
      }
    )
  },
  @{
    Name = "s3-hevc10-flac.mkv"
    Sha256 = "b8d21a9b53d4482717c1495c64f7804904b359740b1fed8e20f8eea69f9e1595"
    Streams = @(
      @{
        Index = 0
        CodecType = "video"
        CodecName = "hevc"
        Profile = "Main 10"
        PixFmt = "yuv420p10le"
      },
      @{
        Index = 1
        CodecType = "audio"
        CodecName = "flac"
        Profile = $null
      }
    )
  },
  @{
    Name = "s3-hevc10-flac-ass.mkv"
    Sha256 = "cb295cf642b75964772ac9a713f3ed714265957ee6107c377f955786c6e03eb7"
    Streams = @(
      @{
        Index = 0
        CodecType = "video"
        CodecName = "hevc"
        Profile = "Main 10"
        PixFmt = "yuv420p10le"
      },
      @{
        Index = 1
        CodecType = "audio"
        CodecName = "flac"
        Profile = $null
      },
      @{
        Index = 2
        CodecType = "subtitle"
        CodecName = "ass"
        Profile = $null
      }
    )
  }
)

$probeFieldNames = @{
  Index = "index"
  CodecType = "codec_type"
  CodecName = "codec_name"
  Profile = "profile"
  PixFmt = "pix_fmt"
}

function Get-StreamField {
  param(
    [object]$Stream,
    [string]$Name
  )

  $probeFieldName =
    if ($probeFieldNames.ContainsKey($Name)) { $probeFieldNames[$Name] } else { $Name }
  $property = $Stream.PSObject.Properties[$probeFieldName]
  if ($null -eq $property) {
    return $null
  }

  return $property.Value
}

function Assert-StreamField {
  param(
    [object]$ActualStream,
    [hashtable]$ExpectedStream,
    [string]$FieldName,
    [string]$FixtureName,
    [int]$StreamPosition
  )

  if (-not $ExpectedStream.ContainsKey($FieldName)) {
    return
  }

  $actual = Get-StreamField $ActualStream $FieldName
  $expected = $ExpectedStream[$FieldName]
  if ($actual -ne $expected) {
    throw (
      "Stream {0} {1} mismatch for {2}: expected '{3}', got '{4}'." -f
      $StreamPosition,
      $FieldName,
      $FixtureName,
      $expected,
      $actual
    )
  }
}

foreach ($fixture in $fixtures) {
  $path = Join-Path $root $fixture.Name
  if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
    throw "Missing synthetic fixture: $path"
  }

  $actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash
  if ($actualHash -ne $fixture.Sha256) {
    throw "SHA-256 mismatch for $($fixture.Name): expected $($fixture.Sha256), got $actualHash."
  }

  $probeJson = & ffprobe `
    -v error `
    -show_entries stream=index,codec_type,codec_name,profile,pix_fmt `
    -of json `
    $path
  if ($LASTEXITCODE -ne 0) {
    throw "ffprobe failed for $($fixture.Name) with exit code $LASTEXITCODE."
  }

  $probe = ($probeJson | Out-String) | ConvertFrom-Json
  $actualStreams = @($probe.streams)
  $expectedStreams = @($fixture.Streams)
  if ($actualStreams.Count -ne $expectedStreams.Count) {
    throw (
      "Stream count mismatch for {0}: expected {1}, got {2}." -f
      $fixture.Name,
      $expectedStreams.Count,
      $actualStreams.Count
    )
  }

  for ($i = 0; $i -lt $expectedStreams.Count; $i++) {
    $expectedStream = $expectedStreams[$i]
    $actualStream = $actualStreams[$i]

    Assert-StreamField $actualStream $expectedStream "Index" $fixture.Name $i
    Assert-StreamField $actualStream $expectedStream "CodecType" $fixture.Name $i
    Assert-StreamField $actualStream $expectedStream "CodecName" $fixture.Name $i
    Assert-StreamField $actualStream $expectedStream "Profile" $fixture.Name $i
    Assert-StreamField $actualStream $expectedStream "PixFmt" $fixture.Name $i
  }

  if (-not $SkipMpv) {
    $mpvProcess = Start-Process `
      -FilePath $MpvPath `
      -ArgumentList @(
        "--no-config",
        "--vo=null",
        "--ao=null",
        "--really-quiet",
        "--frames=1",
        "--force-window=no",
        $path
      ) `
      -WindowStyle Hidden `
      -Wait `
      -PassThru
    if ($mpvProcess.ExitCode -ne 0) {
      throw "mpv decode failed for $($fixture.Name) with exit code $($mpvProcess.ExitCode)."
    }
  }

  $codecs =
    @($actualStreams | ForEach-Object { Get-StreamField $_ "CodecName" }) -join ","
  $profiles =
    @($actualStreams | ForEach-Object { Get-StreamField $_ "Profile" }) -join ","
  Write-Output (
    "SYNTHETIC_FIXTURE|PASS|{0}|sha256={1}|streams={2}|codecs={3}|profiles={4}" -f
    $fixture.Name,
    $actualHash.ToLowerInvariant(),
    $actualStreams.Count,
    $codecs,
    $profiles
  )
}

$mpvStatus = if ($SkipMpv) { "SKIPPED" } else { "PASS" }
Write-Output (
  "SYNTHETIC_FIXTURES|PASS|count={0}|mpv={1}|realS2=NOT_VERIFIED" -f
  $fixtures.Count,
  $mpvStatus
)
