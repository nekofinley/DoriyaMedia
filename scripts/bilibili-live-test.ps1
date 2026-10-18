# Live end-to-end check for the bilibili (番剧 / PGC) source.
#
#   pwsh scripts/bilibili-live-test.ps1                       # anonymous, free episodes
#   pwsh scripts/bilibili-live-test.ps1 -Cookie "SESSDATA=…"  # with an account
#   pwsh scripts/bilibili-live-test.ps1 -SessionPackager      # force the other HLS path
#
# It starts a DoriyaMedia of its own on a spare port with a throwaway data dir,
# adds a bilibili source, then walks the whole chain: browse a season, register an
# episode, ask for the playback decision, pull the HLS playlist and one segment,
# and fetch the 弹幕. Nothing it touches is the real data dir.
#
# Requires network access to api.bilibili.com; refuses to run against the real
# data directory or a busy port.

param(
  [string]$Cookie = "",
  [int]$Port = 5699,
  [string]$Series = "/热门番剧/鬼灭之刃 [series 26801]",
  [string]$Season = "/热门番剧/鬼灭之刃 [series 26801]/立志篇 [season 26801]",
  [switch]$SessionPackager,
  [int]$SegmentIndex = 3,
  [switch]$KeepRunning
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
$exe = Join-Path $root "doriyamedia.exe"
if (-not (Test-Path $exe)) { throw "先构建:$exe 不存在(scripts\build.ps1)" }

# Per port, so two copies of this test can run side by side without
# fighting over one SQLite file.
$scratch = Join-Path $root ".tmp\bili-e2e-$Port"
$dataDir = Join-Path $scratch "data"
New-Item -ItemType Directory -Force -Path $dataDir | Out-Null
$cfgPath = Join-Path $scratch "config.json"

# A throwaway config: its own port, its own data dir, and the HLS setting under
# test. Never the repository's own config.json.
$cfg = @{
  port = $Port
  data_dir = $dataDir
  secret = "bili-e2e-secret"
  ffmpeg = "ffmpeg"
  ffprobe = "ffprobe"
  playback = @{
    force_mode = "auto"; segment_seconds = 4; transcode_height = 1080
    probe_size_mb = 8; probe_analyze_sec = 4
    apple_hevc_copy = $false; hls_session_packager = [bool]$SessionPackager
    hls_cache_mb = 256
  }
  users = @{ registration = "closed"; min_password = 4 }
  scan_concurrency = 2
}
$json = $cfg | ConvertTo-Json -Depth 6
# No BOM: Go's JSON reader rejects one, and Windows PowerShell's own UTF8 encoder
# writes one.
[System.IO.File]::WriteAllText($cfgPath, $json, (New-Object System.Text.UTF8Encoding($false)))

if (Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue) {
  throw "端口 $Port 已被占用,换一个 -Port"
}

$logOut = Join-Path $scratch "server.out.log"
$logErr = Join-Path $scratch "server.err.log"
Write-Host "==> 启动服务(端口 $Port,数据目录 $dataDir,HLS 打包会话 = $([bool]$SessionPackager))" -ForegroundColor Cyan
$proc = Start-Process -FilePath $exe -ArgumentList @("-config", $cfgPath) -PassThru -NoNewWindow `
  -RedirectStandardOutput $logOut -RedirectStandardError $logErr

function Wait-Port {
  param([int]$P, [int]$TimeoutSec = 20)
  $deadline = (Get-Date).AddSeconds($TimeoutSec)
  while ((Get-Date) -lt $deadline) {
    if (Get-NetTCPConnection -LocalPort $P -State Listen -ErrorAction SilentlyContinue) { return $true }
    Start-Sleep -Milliseconds 250
  }
  return $false
}

$base = "http://127.0.0.1:$Port"

# Windows PowerShell sends a -Body <string> in the local code page, so any Chinese
# in it arrives as "???" — a library path of "/热门番剧" becomes "/????" and the
# source is scanned nowhere. Every JSON body goes out as UTF-8 bytes instead.
function PostJson {
  param([string]$Uri, $Object, $Session)
  $json = $Object | ConvertTo-Json -Depth 6
  $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
  $args = @{ Uri = $Uri; Method = "Post"; Body = $bytes; ContentType = "application/json; charset=utf-8" }
  if ($Session) { $args.WebSession = $Session }
  Invoke-RestMethod @args
}
$fails = New-Object System.Collections.Generic.List[string]
function Check {
  param([string]$What, [bool]$Ok, [string]$Detail = "")
  if ($Ok) { Write-Host ("  [ok]   " + $What + $(if ($Detail) { " — $Detail" } else { "" })) -ForegroundColor Green }
  else {
    Write-Host ("  [FAIL] " + $What + $(if ($Detail) { " — $Detail" } else { "" })) -ForegroundColor Red
    $fails.Add($What)
  }
}

try {
  if (-not (Wait-Port -P $Port)) {
    Write-Host "服务没有起来,日志:" -ForegroundColor Red
    Get-Content $logErr -ErrorAction SilentlyContinue | Select-Object -Last 20
    throw "server did not start"
  }

  # ---- admin + source ----
  # A second run finds the scratch data dir already initialised; log in instead,
  # so the test can be re-run without deleting anything.
  $sess = $null
  try {
    $null = Invoke-RestMethod -Uri "$base/api/setup" -Method Post `
      -Body ([System.Text.Encoding]::UTF8.GetBytes((@{ name = "e2e"; password = "e2e-pass" } | ConvertTo-Json))) `
      -ContentType "application/json; charset=utf-8" -SessionVariable sess
    Check "初始化管理员" ($null -ne $sess)
  } catch {
    $null = Invoke-RestMethod -Uri "$base/api/auth/login" -Method Post `
      -Body ([System.Text.Encoding]::UTF8.GetBytes((@{ name = "e2e"; password = "e2e-pass" } | ConvertTo-Json))) `
      -ContentType "application/json; charset=utf-8" -SessionVariable sess
    Check "复用已有实例并登录" ($null -ne $sess)
  }

  # 上一轮留下的库/源会让数量断言不准:先清空。
  foreach ($old in @(Invoke-RestMethod -Uri "$base/api/libraries" -WebSession $sess)) {
    Invoke-RestMethod -Uri "$base/api/libraries/$($old.id)" -Method Delete -WebSession $sess | Out-Null
  }
  foreach ($old in @(Invoke-RestMethod -Uri "$base/api/sources" -WebSession $sess)) {
    Invoke-RestMethod -Uri "$base/api/sources/$($old.id)" -Method Delete -WebSession $sess | Out-Null
  }

  $src = PostJson "$base/api/sources" @{
    name = "B站番剧"; type = "bilibili"
    config = @{ bili_cookie = $Cookie; bili_quality = 0; bili_codec = "avc" }
  } $sess
  Check "添加 bilibili 源" ($src.id -gt 0) "id=$($src.id) type=$($src.type)"
  $sid = $src.id

  function Browse([string]$Path) {
    $u = "$base/api/browse/$sid" + "?path=" + [uri]::EscapeDataString($Path)
    Invoke-RestMethod -Uri $u -WebSession $sess
  }

  # ---- browse ----
  $rootL = Browse "/"
  Check "根目录" ($rootL.entries.Count -ge 2) (($rootL.entries | ForEach-Object { $_.name }) -join ", ")

  # 一个系列目录下列出它的季度:bilibili 自己就叫它们第一季/第二季/游郭篇…
  $seasons = Browse $Series
  Check "系列目录列出季度" ($seasons.entries.Count -ge 2) `
    (($seasons.entries | ForEach-Object { $_.name }) -join ", ")
  $named = $seasons.entries | Where-Object { $_.name -match '游郭篇' }
  Check "季度目录带 season id 且能反查" ([bool]$named -and $named.name -match '\[season \d+\]') $named.name
  $spy = Browse "/热门番剧/间谍过家家 [series 41410]"
  $numbered = @($spy.entries | Where-Object { $_.name -match '^第[一二三]季' })
  Check "季度用 bilibili 自己的季名(第一季/第二季…)" ($numbered.Count -ge 2) `
    (($spy.entries | ForEach-Object { $_.name }) -join ", ")

  $eps = Browse $Season
  Check "季目录列出剧集" ($eps.entries.Count -gt 0) "$($eps.entries.Count) 集"
  if ($eps.entries.Count -eq 0) { throw "no episodes" }

  $ep1 = $eps.entries[0]
  Write-Host "      第一集: $($ep1.name)" -ForegroundColor DarkGray

  # ---- playback decision ----
  $playUrl = "$base/api/browseplay/$sid" + "?path=" + [uri]::EscapeDataString($ep1.path)
  $play = Invoke-RestMethod -Uri $playUrl -WebSession $sess
  Check "拿到播放决策" ($play.state -eq "ready") "state=$($play.state)"
  Check "模式为服务端合流(remux)" ($play.mode -eq "remux") "mode=$($play.mode) reason=$($play.reason)"
  Check "走 HLS" ($play.kind -eq "hls" -and $play.url) "kind=$($play.kind)"
  Check "时长已识别" ($play.duration -gt 60) "duration=$([math]::Round($play.duration))s"
  Check "编解码已由源说明" ($play.video_codec -eq "h264" -and $play.audio_codec -eq "aac") `
    "$($play.video_codec)/$($play.audio_codec) $($play.resolution)"
  Check "给了弹幕地址" ([bool]$play.danmaku_url) $play.danmaku_url

  # ---- HLS ----
  # Written to a file and read back: Windows PowerShell hands back a byte array
  # for a content type it does not recognise as text, and .m3u8 is one of those.
  #
  # The playlist is an EVENT one: ffmpeg adds to it as it packages, so a playlist
  # read the instant playback starts legitimately holds one segment. Wait for the
  # few this test needs rather than treating a young playlist as a failure.
  $plPath = Join-Path $scratch "index.m3u8"
  $plText = ""
  $segLines = @()
  $tries = 0
  for ($tries = 0; $tries -lt 40; $tries++) {
    Invoke-WebRequest -Uri ($base + $play.url) -WebSession $sess -OutFile $plPath
    $plText = Get-Content $plPath -Raw
    $segLines = @(($plText -split "`n") | Where-Object { $_ -match '^/api/hls/' })
    if ($segLines.Count -gt ($SegmentIndex + 2)) { break }
    Start-Sleep -Milliseconds 750
  }
  $lines = $plText -split "`n"
  Check "播放列表有分片" ($segLines.Count -gt ($SegmentIndex + 1)) "$($segLines.Count) 段(轮询 $tries 次)"
  Check "播放列表是分片式 fMP4" ([bool]($plText -match '#EXT-X-MAP:'))
  if ($segLines.Count -le $SegmentIndex) { throw "分片不足:$($segLines.Count) 段" }
  $seg = $segLines[$SegmentIndex].Trim()

  # A media segment carries no moov of its own, so it is joined with the init
  # segment before ffprobe — exactly what a player does with #EXT-X-MAP.
  $initUri = ""
  foreach ($l in $lines) { if ($l -match '^#EXT-X-MAP:URI="([^"]+)"') { $initUri = $Matches[1]; break } }
  Check "播放列表给出 init 分片" ([bool]$initUri) $initUri

  $initPath = Join-Path $scratch "init.mp4"
  $segPath = Join-Path $scratch "seg$SegmentIndex.m4s"
  $joinedPath = Join-Path $scratch "joined.mp4"
  Invoke-WebRequest -Uri ($base + $initUri) -WebSession $sess -OutFile $initPath
  Invoke-WebRequest -Uri ($base + $seg) -WebSession $sess -OutFile $segPath
  Check "分片已下载" ((Get-Item $segPath).Length -gt 20000) "$([math]::Round((Get-Item $segPath).Length/1KB))KB"
  [System.IO.File]::WriteAllBytes($joinedPath, `
    ([System.IO.File]::ReadAllBytes($initPath) + [System.IO.File]::ReadAllBytes($segPath)))

  # The segment has to carry both streams: a video-only segment is exactly the
  # failure this whole change exists to prevent. ffprobe prints codec_name first.
  $probe = & ffprobe -v error -show_entries stream=codec_type,codec_name -of csv=p=0 $joinedPath 2>&1
  $probeText = ($probe -join " ")
  Check "分片含视频流" ([bool]($probeText -match 'h264')) $probeText
  Check "分片含音频流" ([bool]($probeText -match 'aac')) $probeText
  Check "分片确实有两路流" ([bool](($probeText -match 'video') -and ($probeText -match 'audio')))

  # Consecutive segments must not overlap in time: overlapping timestamps are what
  # a player reports as "Parsed buffers not in DTS sequence" and refuses to play.
  if ($segLines.Count -gt ($SegmentIndex + 1)) {
    $nextPath = Join-Path $scratch "next.m4s"
    Invoke-WebRequest -Uri ($base + $segLines[$SegmentIndex + 1].Trim()) -WebSession $sess -OutFile $nextPath
    $initBytes = [System.IO.File]::ReadAllBytes($initPath)
    $spanOf = {
      param($path)
      $j = Join-Path $scratch "probe-join.mp4"
      [System.IO.File]::WriteAllBytes($j, ($initBytes + [System.IO.File]::ReadAllBytes($path)))
      $t = & ffprobe -v error -show_entries packet=stream_index,dts_time -of csv=p=0 $j 2>&1
      $v = @($t | Where-Object { $_ -match '^0,' } | ForEach-Object { [double]($_ -split ',')[1] })
      if ($v.Count -eq 0) { return @{ start = 0.0; end = 0.0 } }
      return @{ start = ($v | Measure-Object -Minimum).Minimum; end = ($v | Measure-Object -Maximum).Maximum }
    }
    $a = & $spanOf $segPath
    $b = & $spanOf $nextPath
    Check "相邻分片不重叠" ($b.start -ge ($a.end - 1.0)) `
      ("第 {0} 段 [{1:N2}..{2:N2}] → 第 {3} 段 [{4:N2}..{5:N2}]" -f `
        $SegmentIndex, $a.start, $a.end, ($SegmentIndex + 1), $b.start, $b.end)
  }

  # ---- 弹幕 ----
  if ($play.danmaku_url) {
    $dm = Invoke-RestMethod -Uri ($base + $play.danmaku_url) -WebSession $sess
    Check "弹幕接口返回条目" ($dm.count -gt 0) "$($dm.count) 条"
    if ($dm.count -gt 0) {
      $c = $dm.comments[0]
      Check "弹幕字段完整" ($null -ne $c.t -and $c.mode -gt 0 -and $c.text) "t=$($c.t) mode=$($c.mode) text=$($c.text)"
      $sorted = $true
      for ($i = 1; $i -lt [Math]::Min($dm.count, 200); $i++) {
        if ($dm.comments[$i].t -lt $dm.comments[$i-1].t) { $sorted = $false; break }
      }
      Check "弹幕按时间排序" $sorted
    }
  }

  # ---- a membership episode must be refused, not played silent ----
  if ($eps.entries.Count -gt 1) {
    $vip = $eps.entries | Where-Object { $_.name -match '第2话' } | Select-Object -First 1
    if ($vip) {
      try {
        $r = Invoke-RestMethod -Uri ("$base/api/browseplay/$sid" + "?path=" + [uri]::EscapeDataString($vip.path)) -WebSession $sess
        Check "会员集给出可读理由" ($r.mode -eq "remux") "(该集免费或账号有会员:mode=$($r.mode))"
      } catch {
        $msg = $_.ErrorDetails.Message
        Check "会员集被拒绝且有中文理由" ($msg -match '大会员' -or $msg -match '没有可播放') $msg
      }
    }
  }
  # ---- 媒体库:一个系列 = 一部剧,季度 = 季 ----
  # 这是这套目录结构存在的全部意义:鬼灭之刃在 B 站是十几个独立条目,进了媒体库
  # 要变成一部剧 + 十几个季度,而不是十几部互不相干的番。
  $lib = PostJson "$base/api/libraries" @{
    name = "追番"; type = "series"; source_id = $sid; path = "/热门番剧"; providers = "[]"
  } $sess
  Invoke-RestMethod -Uri "$base/api/libraries/$($lib.id)/scan" -Method Post -WebSession $sess | Out-Null
  $st = $null
  for ($i = 0; $i -lt 300; $i++) {
    Start-Sleep -Seconds 1
    $all = Invoke-RestMethod -Uri "$base/api/scan/status" -WebSession $sess
    $st = @($all) | Where-Object { $_.library_id -eq $lib.id } | Select-Object -First 1
    if ($st -and -not $st.running) { break }
  }
  Check "媒体库扫描完成" ($st -and -not $st.running -and -not $st.error) "phase=$($st.phase) error=$($st.error)"

  # limit 上限是 500,而这个库有三千多集;按类型分别取,别指望一页拿全。
  function Items([string]$Type, [int]$Limit = 500) {
    @( (Invoke-RestMethod -Uri "$base/api/items?library_id=$($lib.id)&type=$Type&limit=$Limit" -WebSession $sess).items )
  }
  $shows = Items "show"
  $seasons = Items "season"
  $items = @($shows) + @($seasons) + (Items "episode")
  $km = @($shows | Where-Object { $_.name -like "鬼灭之刃*" })
  Check "鬼灭之刃在库里只有一部" ($km.Count -eq 1) `
    ("匹配到 " + $km.Count + " 部: " + (($km | ForEach-Object { $_.name }) -join " / "))
  Check "库里没有季度混进剧列表" `
    (@($shows | Where-Object { $_.name -match '\[(season|series)' }).Count -eq 0) `
    ("show=" + $shows.Count + " 条目总数=" + $items.Count)

  if ($km.Count -eq 1) {
    $seasonItems = @($seasons | Where-Object { $_.parent_id -eq $km[0].id })
    Check "鬼灭之刃下面挂着多个季度" ($seasonItems.Count -ge 5) `
      ("$($seasonItems.Count) 季: " + (($seasonItems | ForEach-Object { $_.name }) -join " / "))
    $youguo = @($seasonItems | Where-Object { $_.name -match '游郭篇' })
    if ($youguo.Count -gt 0) {
      # 季条目的名字不该带着 [season …]:那是路径用的,不该出现在界面上。
      Check "季度名去掉了方括号标记" ($youguo[0].name -notmatch '\[season') $youguo[0].name
      # 走界面自己用的那个接口:剧集是季节的子条目,翻不了页也无所谓。
      $detail = Invoke-RestMethod -Uri "$base/api/items/$($youguo[0].id)" -WebSession $sess
      $epItems = @($detail.episodes)
      Check "季度下面挂着剧集" ($epItems.Count -ge 3) `
        ("游郭篇 $($epItems.Count) 集,第一集: " + (@($epItems | Sort-Object episode)[0].name))
    } else {
      Check "季度下面挂着剧集" $false "季度里没有游郭篇"
    }
  }
}
finally {
  if (-not $KeepRunning) {
    if ($proc -and -not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue }
  } else {
    Write-Host "==> 服务仍在运行:$base (Ctrl+C 或 Stop-Process -Id $($proc.Id))" -ForegroundColor Yellow
  }
}

Write-Host ""
if ($fails.Count -eq 0) {
  Write-Host "全部通过 ✓" -ForegroundColor Green
  exit 0
} else {
  Write-Host "$($fails.Count) 项失败:" -ForegroundColor Red
  $fails | ForEach-Object { Write-Host "  - $_" -ForegroundColor Red }
  exit 1
}
