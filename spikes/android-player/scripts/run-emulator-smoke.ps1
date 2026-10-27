$ErrorActionPreference = "Stop"

$projectRoot = Split-Path -Parent $PSScriptRoot
$artifacts = Join-Path $projectRoot "artifacts"
New-Item -ItemType Directory -Force $artifacts | Out-Null

$androidUserHome = Join-Path $artifacts "android-user-home"
$androidAvdHome = Join-Path $artifacts "android-avd"
$avdName = "A0_API_37_ATD"
$avdPath = Join-Path $androidAvdHome "$avdName.avd"
New-Item -ItemType Directory -Force $androidUserHome, $androidAvdHome | Out-Null
$env:ANDROID_USER_HOME = $androidUserHome
$env:ANDROID_AVD_HOME = $androidAvdHome

if (-not (Test-Path (Join-Path $avdPath "config.ini"))) {
  "no" |
    avdmanager create avd `
      --name $avdName `
      --package "system-images;android-37.0;aosp_atd;x86_64" `
      --device pixel_7 `
      --path $avdPath `
      --force
  if ($LASTEXITCODE -ne 0) {
    throw "avdmanager failed with exit code $LASTEXITCODE."
  }

  $configPath = Join-Path $avdPath "config.ini"
  $config = Get-Content $configPath
  $config = $config -replace "^avd\.id=.*$", "avd.id=$avdName"
  $config = $config -replace "^avd\.name=.*$", "avd.name=$avdName"
  $config = $config -replace "^disk\.cachePartition\.size=.*$", "disk.cachePartition.size=64MB"
  $config = $config -replace "^disk\.dataPartition\.size=.*$", "disk.dataPartition.size=1G"
  $config = $config -replace "^fastboot\.forceColdBoot=.*$", "fastboot.forceColdBoot=yes"
  $config = $config -replace "^fastboot\.forceFastBoot=.*$", "fastboot.forceFastBoot=no"
  $config = $config -replace "^firstboot\.bootFromDownloadableSnapshot=.*$", "firstboot.bootFromDownloadableSnapshot=no"
  $config = $config -replace "^firstboot\.bootFromLocalSnapshot=.*$", "firstboot.bootFromLocalSnapshot=no"
  $config = $config -replace "^firstboot\.saveToLocalSnapshot=.*$", "firstboot.saveToLocalSnapshot=no"
  $config = $config -replace "^hw\.cpu\.ncore=.*$", "hw.cpu.ncore=2"
  $config = $config -replace "^hw\.gpu\.enabled=.*$", "hw.gpu.enabled=yes"
  $config = $config -replace "^hw\.gpu\.mode=.*$", "hw.gpu.mode=swiftshader_indirect"
  $config = $config -replace "^hw\.keyboard=.*$", "hw.keyboard=yes"
  $config = $config -replace "^hw\.ramSize=.*$", "hw.ramSize=1024"
  $config = $config -replace "^sdcard\.size=.*$", "sdcard.size=64M"
  $config = $config -replace "^showDeviceFrame=.*$", "showDeviceFrame=no"
  Set-Content -Path $configPath -Value $config -Encoding utf8NoBOM
}

$emulator = (Get-Command emulator).Source
$stdoutLog = Join-Path $artifacts "emulator.stdout.log"
$stderrLog = Join-Path $artifacts "emulator.stderr.log"
$emulatorProcess = Start-Process `
  -FilePath $emulator `
  -ArgumentList @(
    "-avd", $avdName,
    "-no-window",
    "-no-audio",
    "-no-boot-anim",
    "-gpu", "swiftshader_indirect",
    "-no-snapshot",
    "-no-snapstorage",
    "-partition-size", "512",
    "-cache-size", "32",
    "-memory", "1024",
    "-no-metrics"
  ) `
  -WindowStyle Hidden `
  -PassThru `
  -RedirectStandardOutput $stdoutLog `
  -RedirectStandardError $stderrLog

$serial = $null
$bootCompleted = $false
try {
  $deadline = (Get-Date).AddMinutes(4)
  while ((Get-Date) -lt $deadline) {
    if ($emulatorProcess.HasExited) {
      throw "Emulator exited early with code $($emulatorProcess.ExitCode). See $stderrLog."
    }

    $serialLine =
      adb devices |
        Select-String -Pattern "^(emulator-\d+)\s+device$" |
        Select-Object -First 1
    if ($serialLine) {
      $serial = $serialLine.Matches[0].Groups[1].Value
      $boot = (adb -s $serial shell getprop sys.boot_completed 2>$null).Trim()
      if ($boot -eq "1") {
        $bootCompleted = $true
        break
      }
    }
    Start-Sleep -Seconds 3
  }
  if (-not $bootCompleted) {
    throw "Emulator did not finish booting within four minutes."
  }

  Write-Output "EMULATOR_SERIAL=$serial"
  adb -s $serial logcat -c

  # The dedicated ATD can retain APKs signed by an older debug key.
  foreach ($package in @("com.doriyamedia.spike.test", "com.doriyamedia.spike")) {
    & adb -s $serial uninstall $package 2>$null | Out-Null
  }

  Push-Location $projectRoot
  try {
    $env:JAVA_TOOL_OPTIONS =
      "-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=7890 " +
      "-Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=7890"
    $env:HTTP_PROXY = "http://127.0.0.1:7890"
    $env:HTTPS_PROXY = "http://127.0.0.1:7890"

    & .\gradlew.bat :app:connectedDebugAndroidTest --stacktrace
    if ($LASTEXITCODE -ne 0) {
      throw "connectedDebugAndroidTest failed with exit code $LASTEXITCODE."
    }

    & adb -s $serial install -r "app\build\outputs\apk\debug\app-debug.apk"
    if ($LASTEXITCODE -ne 0) {
      throw "adb install failed with exit code $LASTEXITCODE."
    }

    & adb -s $serial shell am force-stop com.doriyamedia.spike
    & adb -s $serial shell am start -W -n com.doriyamedia.spike/.MainActivity
    if ($LASTEXITCODE -ne 0) {
      throw "Activity launch failed with exit code $LASTEXITCODE."
    }
    Start-Sleep -Seconds 3

    Write-Output "--- PID ---"
    & adb -s $serial shell pidof com.doriyamedia.spike
    Write-Output "--- RESUMED ---"
    & adb -s $serial shell dumpsys activity activities |
      Select-String -Pattern "mResumedActivity|topResumedActivity" |
      Select-Object -First 5

    Write-Output "--- UI DUMP ---"
    & adb -s $serial shell uiautomator dump /sdcard/a0-window.xml | Out-Null
    $uiDump = (& adb -s $serial shell cat /sdcard/a0-window.xml) -join "`n"
    if ($uiDump -match "Playback mechanism probe") {
      Write-Output "UI_TEXT_FOUND=Playback mechanism probe"
    } else {
      Write-Output "UI_TEXT_FOUND=false"
      $uiDump | Select-String -Pattern "Playback|A0|MEDIA3" | Select-Object -First 20
    }

    & adb -s $serial shell screencap -p /sdcard/a0-smoke.png
    & adb -s $serial pull /sdcard/a0-smoke.png (Join-Path $artifacts "emulator-smoke.png") |
      Out-Null

    Write-Output "--- APP ERRORS ---"
    & adb -s $serial logcat -d -s AndroidRuntime:E DoriyaA0:W DoriyaA0:I |
      Select-Object -Last 60
  } finally {
    Pop-Location
  }
} finally {
  if ($serial) {
    & adb -s $serial emu kill 2>$null | Out-Null
    $stopDeadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $stopDeadline -and -not $emulatorProcess.HasExited) {
      Start-Sleep -Seconds 1
    }
  }
  Write-Output "EMULATOR_EXITED=$($emulatorProcess.HasExited)"
}
