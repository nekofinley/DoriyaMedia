# DoriyaMedia Android A0 probe

Minimal Kotlin + Jetpack Compose + Media3 ExoPlayer prototype for the A0
playback-engine spike. This directory is intentionally not a production app.

## Scope

- `minSdk=28`, `targetSdk=37`, application ID `com.doriyamedia.spike`.
- Single HTTPS URL with optional allowlisted request headers and container MIME.
- Separate video and audio URLs merged locally with Media3
  `MergingMediaSource`, with independent per-stream Headers and MP4/WebM MIME
  hints.
- Each stream uses its own `OkHttpClient`, `OkHttpDataSource.Factory`, and
  `MediaSource`. Redirects are allowed only within the original scheme/host/
  effective-port origin; a cross-origin redirect is rejected before the target
  origin receives a request.
- Play/pause, seek, position/duration, phase and sanitized Media3 error-code
  display.
- Play intent (`playWhenReady`), stale-source teardown on prepare failure,
  lifecycle pause, and `PlayerView` unbinding on release.
- No account, library, persistence, telemetry, media session, HLS fallback or
  production UI.

The temporary URL and Header inputs are kept only in Compose memory. They are
not persisted across Activity recreation.

## Validation and logging

JVM tests cover:

- HTTPS-only, absolute URL validation; rejection of whitespace, fragments and
  URL user information; distinct video/audio URLs.
- Exact Header allowlist: `Referer`, `Origin`, `User-Agent`, `Accept`,
  `Accept-Language`; duplicate, malformed, injected and oversized values are
  rejected.
- Separate video/audio Header maps and MIME hints, including rejection of a MIME
  hint for the wrong stream role.
- Same-origin redirects retaining allowlisted Headers, plus a cross-origin
  redirect test proving the target server receives zero requests.
- Log summaries expose only URL origins (scheme/host/optional port), Header
  names, and Media3 error-code identifiers. Full query strings, Header values,
  credentials and exception messages are not logged.

Instrumented tests include:

- An Activity/UI smoke test.
- A Media3 merge test using separate synthetic H.264 video and AAC audio MP4
  assets, checking `STATE_READY`, presence of both track types, duration, seek
  discontinuity, position advancement after seek, play and pause. The test
  prefers software codecs because the API 37 ATD
  system image's `c2.goldfish.h264.decoder` currently fails at the Codec2
  buffer layer on this host; this workaround isolates the merge mechanism and
  is not evidence for a product decode path.

## Commands

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\spikes\android-player
.\gradlew.bat :app:lintDebug :app:testDebugUnitTest :app:assembleDebug :app:assembleDebugAndroidTest
.\gradlew.bat :app:connectedDebugAndroidTest
```

With enough host disk space, an AOSP ATD emulator can be started and stopped
by:

```powershell
.\scripts\run-emulator-smoke.ps1
```

The script creates its AVD under the ignored `artifacts/` directory, installs
the app, launches `MainActivity`, captures the UI/screenshot, runs connected
tests, and calls `adb emu kill` in `finally`.

## Current results (2026-10-28)

Mechanism validation only:

| Check | Result |
| --- | --- |
| `:app:lintDebug` | Passed: 0 errors, 10 version warnings |
| `:app:testDebugUnitTest` | Passed: 24 tests, 0 failures |
| `:app:assembleDebug` | Passed; debug APK generated |
| `:app:assembleDebugAndroidTest` | Passed; test APK compiled |
| API 37 x86_64 ATD `connectedDebugAndroidTest` | Passed: 4 tests, 0 failures; Ready, dual tracks, seek discontinuity, position advancement, play/pause, separate per-stream factory wiring, merge UI, and video-surface assertions |
| APK install and cold `MainActivity` launch | Passed: `topResumedActivity` set, UI text found, no app-level runtime errors |
| APK metadata | `minSdk=28`, `targetSdk=37`, package `com.doriyamedia.spike` |

The connected run used the isolated `A0_API_37_ATD` AVD under `artifacts/`.
It booted successfully with about 7.7 GB free on C:. The screenshot from the
headless ATD was black, so UI presence is based on `uiautomator` output rather
than screenshot inspection.

Not executed:

- Media3 merged playback on any real Android device.
- API 28, 31 and 34 real-device matrix; Qualcomm/MediaTek coverage.
- Wi-Fi, 5G, hotspot, VPN and different source egress matrix.
- Real Bilibili `direct-dash` URLs/Headers and audio/video synchronization.
- Real CDN redirect behavior. The current policy intentionally rejects every
  cross-origin redirect; only a local MockWebServer mechanism test exists.
- Per-stream Range behavior and real source response variants. Range is left to
  Media3/OkHttp and is not directly asserted by this spike.
- Media3 + FFmpeg extension and libmpv comparison.
- CPU, battery, memory, subtitle and long-duration measurements.

## Acceptance boundary

This validates input handling, per-stream Header/MIME wiring, same-origin
redirect enforcement, log redaction, Media3 merge API wiring, Android
build/lint/unit tests, API 37 x86_64 ATD installation, and synthetic local
merge tests. It does **not** close A0 or S5. Real-device capability, actual
Bilibili DASH merge, audio/video sync, real codec behavior, Range behavior and
the device/network matrix remain unverified. The ADR is
`docs/adr/0003-android-player-engine.md` and intentionally remains
`Proposed / 待真机验证`.

## Toolchain observed

- JDK 17.0.21
- Android SDK Platform 37.0, Build Tools 36.0.0
- Android CLI 1.1.16717390
- Gradle 9.1.0, Android Gradle Plugin 9.0.1
- Kotlin 2.3.20, Compose BOM 2026.03.01
- Media3 1.11.1
