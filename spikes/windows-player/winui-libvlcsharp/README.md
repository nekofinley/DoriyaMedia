# WinUI 3 + LibVLCSharp W0 spike

Status: experimental candidate only. This spike does not select a Windows
player candidate and does not close W0.

## Scope

- Unpackaged WinUI 3 desktop window on .NET 8.
- Official `LibVLCSharp.WinUI` video surface.
- LibVLC initialization from the installed VLC 3.x directory.
- HTTPS-only URL validation (plain HTTP only for an explicit exact-loopback
  test mode), open, stop, and safe playback state display.
- One optional audio slave URI, merged by LibVLC with
  `Media.AddSlave(MediaSlaveType.Audio, ...)`.
- Allowlisted `Referer` and `User-Agent` media options. LibVLC 3.0.19 media
  options apply to both the main source and audio slave; unsupported headers or
  per-stream scopes fail closed.
- No authentication, ticket API integration, persistence, installer, or
  distribution packaging.

The project deliberately does not reference `VideoLAN.LibVLC.Windows`, and no
VLC DLL or plugin is copied into this repository. The process probes explicit
configuration, `DORIYA_VLC_HOME`, `%ProgramFiles%\VideoLAN\VLC`, then
`%ProgramFiles(x86)%\VideoLAN\VLC`.

## Verified toolchain

Recorded on 2026-10-28:

| Component | Version |
| --- | --- |
| .NET SDK | 8.0.427 |
| Windows App SDK NuGet | 1.8.260921001 |
| Windows SDK BuildTools NuGet | 10.0.26100.7175 |
| LibVLCSharp.WinUI | 3.10.2 |
| LibVLCSharp (headless probe) | 3.10.2 |
| LibVLCSharp.WinUI declared minimum Windows App SDK | 1.7.250909003 |
| xUnit | 2.9.3 |
| xUnit Visual Studio runner | 3.1.5 |
| Microsoft.NET.Test.Sdk | 17.14.1 |
| System VLC | 3.0.19 |

The local spike build uses Windows App SDK self-contained deployment. A
framework-dependent diagnostic build was also produced, but it exited before
window creation with `0x80070005` (Access denied) when launched from the Codex
desktop host. The self-contained build launched normally and showed the
LibVLCSharp WinUI surface as ready. This is a host-specific workaround, not a
claim that framework-dependent deployment is broken on a normal desktop.

Only Windows App SDK output is self-contained; the .NET runtime remains
framework-dependent on the installed .NET 8 runtime.

## Reproduce

The verification script keeps NuGet packages, HTTP cache, CLI state, and
temporary data under this spike directory. The playback probe additionally
requires an installed VLC 3.x runtime and `python` on `PATH`:

```powershell
.\verify.ps1
```

Equivalent explicit commands:

```powershell
dotnet restore .\DoriyaMedia.WinUISpike.sln --configfile .\NuGet.Config
dotnet build .\DoriyaMedia.WinUISpike.sln -c Debug --no-restore
dotnet test .\tests\DoriyaMedia.WinUISpike.Core.Tests\DoriyaMedia.WinUISpike.Core.Tests.csproj -c Debug --arch x64 --no-build --no-restore
dotnet test .\tests\DoriyaMedia.WinUISpike.PlaybackProbe.Tests\DoriyaMedia.WinUISpike.PlaybackProbe.Tests.csproj -c Debug --arch x64 --no-build --no-restore
dotnet run --project .\tools\DoriyaMedia.WinUISpike.VlcProbe\DoriyaMedia.WinUISpike.VlcProbe.csproj -c Debug --no-build --no-restore
```

Launch the experimental window with:

```powershell
.\src\DoriyaMedia.WinUISpike.App\bin\x64\Debug\net8.0-windows10.0.19041.0\win-x64\DoriyaMedia.WinUISpike.App.exe
```

`dotnet run` does not resolve this WinUI project's solution-mapped `x64` output
path, so use the executable path above.

## Actual verification results

Recorded on 2026-11-01 on Windows 10 22H2 (`10.0.19045`, x64), with the
deterministic dual-source playback probe added on 2026-11-03 and the current
LibVLC resume race mitigated on 2026-11-04:

- `dotnet restore`: PASS, exit code 0.
- `dotnet build -c Debug`: PASS, 0 warnings and 0 errors.
- `dotnet test --arch x64`: PASS, 91 passed, 0 failed.
- LibVLC probe: PASS, runtime version `3.0.19 Vetinari`; the probe also
  constructs a media object with an audio slave and `Referer`/`User-Agent`
  media options.
- App smoke: PASS in Windows App SDK self-contained mode. The window opened,
  reported `VLC 3.0.19 initialized with LibVLCSharp.WinUI.`, and showed
  `Ready` / `Player is ready.`
- Synthetic loopback dual-source smoke: PASS through an Explorer-launched
  interactive process. A video-only H.264 MP4 and audio-only AAC MP4 were served
  by separate loopback fixture servers with exact `Referer`/`User-Agent`
  requirements; the UI reached `Opening` then `Playing`. This proves mechanism
  wiring only, not real-source, synchronization, seek, or codec acceptance.
- Deterministic headless dual-source playback probe: PASS on 2026-11-04. The
  automatic test starts the same two loopback fixture servers, applies the
  production `MediaLoadPlanBuilder` validation, attaches the audio-only MP4
  with `Media.AddSlave(MediaSlaveType.Audio, ...)`, and observes `Opening` and
  `Playing` with one selected H.264 video track and one selected AAC slave
  track. It performs 20 position-checked seeks while paused and rechecks both
  tracks after every seek, then resumes and checks forward progress. The probe
  also bounds `MediaPlayer.Stop()` and fixture-process cleanup. This remains
  synthetic mechanism evidence only; it does not measure audio/video sync.
  LibVLC 3.0.19 crashed in `D3D11.DLL_unloaded` when the probe polled
  `State`/`Time` immediately after clearing pause; a 100 ms resume-settle
  window before polling eliminated the failure. Ten consecutive clean-build
  runs passed with 20 distinct position samples each.
- Full `.\verify.ps1` on 2026-11-04: PASS, including fixture integrity, solution
  build with 0 warnings / 0 errors, 91 core tests, the dual-source playback
  probe, 94 relay tests, and the LibVLC initialization/slave construction probe.
- The framework-dependent diagnostic launch failed with `0x80070005` before
  window creation in the Codex host, as described above.

### Isolated loopback relay probe

`tools/DoriyaMedia.WinUISpike.LoopbackRelayProbe/` is a separate .NET console
project. It is **not referenced by the WinUI app**, is not used by LibVLC, and
does not implement a product `local-relay` transport. Its purpose is to test the
HTTP mechanism that a future relay would need.

The probe builds with 0 warnings / 0 errors and its separate xUnit project
passes 94 tests. It currently covers:

- `127.0.0.1`-only ephemeral binding, a 256-bit capability token, and fixed-time
  token comparison.
- Independent video/audio routes with exact-origin allowlists and per-route
  `Origin`, `Referer`, and `User-Agent` injection.
- GET/HEAD, Range/If-Range, 200/206/416 forwarding, response-header allowlisting,
  redirect fail-closed behavior, and non-identity `Content-Encoding` rejection.
- Header-phase timeout separate from response-body idle timeout, so a long
  media transfer is not cut off by a fixed total response deadline. If a
  started response times out or its body stream fails, the downstream
  connection is reset instead of appearing as a clean EOF.
- Public targets require HTTPS; only the exact `127.0.0.1` fixture permits
  plain HTTP. Connect-time checks use exact IANA RIR global-unicast CIDR
  prefixes and reject allocation gaps, private, loopback, documentation,
  reserved, NAT64, 6to4, Teredo, IPv4-compatible and IPv4-mapped addresses,
  plus query/Host/method/header rejection paths.
- Redacted audit outcomes that do not contain URLs, query values, headers,
  cookies, or capability tokens.

```powershell
Set-Location C:\Users\Administrator\DoriyaMedia\spikes\windows-player\winui-libvlcsharp\tools\DoriyaMedia.WinUISpike.LoopbackRelayProbe
dotnet build .\DoriyaMedia.WinUISpike.LoopbackRelayProbe.csproj -c Release
dotnet test ..\..\tests\DoriyaMedia.WinUISpike.LoopbackRelayProbe.Tests\DoriyaMedia.WinUISpike.LoopbackRelayProbe.Tests.csproj -c Release --no-restore
```

This is mechanism evidence only. No real LibVLC playback through the relay, URL
refresh, product ticket integration, TLS/token rotation, rate limiting, or
run/game/device policy has been verified. W0 and S5 remain open.

## Safety boundary

- The media URI is validated before entering the playback adapter.
- The application emits no URL, query token, Cookie, authorization value, or
  source header to its diagnostic sink.
- Unexpected exception messages pass through a conservative redactor before
  they reach `Debug.WriteLine`.
- LibVLC debug logging is disabled, and no persistent application log file is
  created.
- The text box is not persisted by this spike.
- Only `Referer` and `User-Agent` have a reliable LibVLC 3.x injection path.
  `Origin`, `Accept`, and `Accept-Language` are rejected. Both supported values
  are media-level options, so the spike rejects requests that need different
  headers per stream.
- Cross-origin redirect behavior, expired signed URLs, and per-stream Range
  policy have not been verified.

Unit tests cover HTTPS-only and exact-loopback URI validation, user-info,
fragment, escaped-separator, malformed-host and value-redaction cases; VLC
location success and failure branches through a fake filesystem; header
allowlisting and exact LibVLC option construction; audio-slave planning;
playback adapter state mapping; and diagnostic redaction. The synthetic
loopback playback probe is an integration test because it starts Python fixture
servers and a real LibVLC instance; the GUI smoke remains separate.

## Gates not executed

The following gates remain open and must not be inferred from a successful
build:

- S1-S5 end-to-end client scenarios were not executed.
- No 30-minute playback, memory, network-switch, real-source seek, or recovery
  soak was run. The 20 seeks above are synthetic mechanism evidence only.
- The LibVLC 3.0.19 resume-time D3D11 unload race was avoided in the probe by
  not calling player properties during the native resume transition. The WinUI
  surface itself uses backend events and still needs interactive
  open/pause/resume/seek/close soak testing.
- No per-monitor DPI, mixed-DPI, display hot-plug, or accessibility review was
  run. The manifest declares PerMonitorV2, but that is not DPI acceptance.
- No license/compliance gate was executed. LibVLCSharp is LGPL-2.1-or-later,
  LibVLC has its own licensing obligations, and no distribution package or
  notice bundle was produced.
- Real Bilibili DASH video/audio merge, real authenticated source headers,
  server ticket integration, cross-origin redirects, subtitle tracks, and
  fallback behavior were not tested.
- The automatic tests do not prove codec support or real playback behavior.

## License and distribution boundary

This directory is source and build output for a local engineering spike. It is
not an installer and must not be published as a distributable product build
until the license/compliance gate is completed.
