# Tauri current-build embedded-window recheck (2026-11-04)

Status: local synthetic mechanism evidence only. This does not close W0, G4,
DPI acceptance, or any real-media gate.

## Scope

This run rechecks the executable identified by SHA-256
`7be9a18465c4ef76578e16a0c06339a8e53a8b9454f4acda6b9d53600a91ea8c`.
It supersedes the embedded-window interaction geometry and responsiveness
results previously recorded against `da851673...`.

The test used the checked-out synthetic S3 HEVC Main10/FLAC/ASS fixture
`cb295cf642b75964772ac9a713f3ed714265957ee6107c377f955786c6e03eb7`
served only from `127.0.0.1:18431` with exact loopback test Headers. The
restricted desktop session used:

```powershell
$env:WEBVIEW2_USER_DATA_FOLDER = 'C:\Users\Administrator\DoriyaMedia\.tmp-webview-media-2'
$env:WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = '--disable-gpu'
```

This is a harness workaround, not evidence for normal WebView2 GPU
acceleration or a production deployment configuration.

## Environment

- Windows 10 Pro 22H2, build 19045, x64.
- One active display: `1920x1080`, 100% scale, origin `0,0`.
- WebView2 Runtime `156.0.4314.58`.
- NVIDIA RTX 3070 Laptop GPU driver `32.0.16.1742`.
- AMD Radeon Graphics driver `31.0.21923.11000`.
- mpv SHA-256
  `1645e57569c0af58a70c61257f3716f2ee6eb31bb170edc51f3f25a0b8d0d99c`.

No Windows 11, Intel GPU, second display, or non-100% DPI evidence was
produced.

## Auto-open state

The current executable loaded the synthetic fixture through the complete
Tauri controller. The accessibility tree reported:

- `STATE=PAUSED`;
- `POSITION=0:03` with `duration=0:03`;
- one attached mpv sidecar PID;
- both the primary media environment URL and two allowlisted Header names were
  configured;
- the page recorded `环境媒体已提交到受控命令层`.

The GUI used a separate video-surface HWND. No second top-level mpv window was
observed.

## Geometry

An external Win32 read-only probe enumerated the Tauri main window, the
`DoriyaW0MpvSurface` child, and the mpv child. The current build produced:

| State | Tauri main outer/client | Video surface outer/client | mpv outer/client |
| --- | ---: | ---: | ---: |
| Normal | 1196x859 / 1180x820 | 1117x344 / 1117x344 | 1117x344 / 1117x344 |
| `F11` fullscreen | 1920x1080 / 1920x1080 | 1920x1017 / 1920x1017 | 1920x1017 / 1920x1017 |
| Minimum resize | 656x519 / 640x480 | 601x171 / 601x171 | 601x171 / 601x171 |
| Larger resize | 1558x970 / 1542x931 | 1380x391 / 1380x391 | 1380x391 / 1380x391 |

In every state:

- the Tauri window was a visible top-level window;
- `DoriyaW0MpvSurface` was a visible `WS_CHILD` owned by the Tauri process;
- the mpv window was a visible `WS_CHILD` owned by the verified mpv PID;
- the mpv parent was the video-surface HWND;
- the video-surface and mpv outer/client dimensions matched exactly.

`F11` entered fullscreen and exited it. A separate run entered fullscreen with
`F11` and exited with `Escape`; the UI event stream recorded both transitions.

## Responsiveness

A concurrent probe sent 120 `WM_NULL` messages with
`SendMessageTimeout(..., SMTO_ABORTIFHUNG, 500 ms, ...)` while the host was
starting and auto-opening the synthetic fixture.

- Completed probes: `120/120`.
- Failed or timed-out probes: `0`.
- Maximum latency: `13.62 ms`.
- Average latency: `0.61 ms`.
- Probe duration: `7553 ms`.

This is a window-message responsiveness check, not a rendering-framerate or
user-perceived input-latency benchmark.

## Reproducible probe

`tauri-mpv/scripts/window-smoke.ps1` now attaches to exactly one running
`doriya-w0-tauri-mpv` process, verifies the host/surface/mpv parent chain and
both `WS_CHILD` flags, and can run the resize matrix and a bounded
`WM_NULL` responsiveness probe. It fails closed if either process count is not
exactly one, if a rectangle differs, or if any response probe times out.

The checked-in probe was validated after media load:

- PowerShell 7: four geometry snapshots, `120/120` responses, maximum
  `18.08 ms`, average `0.52 ms`.
- Windows PowerShell 5.1: four geometry snapshots, `20/20` responses, maximum
  `12.35 ms`, average `0.89 ms`.

These loaded/idle responsiveness values are separate from the
startup/auto-open values in the preceding section.

## Lifecycle

Forced termination of host PID `13848` while mpv PID `30528` was attached
released the Job Object. A follow-up process query found neither process. A
normal `Alt+F4` close in another run also left no host or mpv process.

## Not proven

- Real S1-S5 media, real E-AC-3, ASS/PGS rendering quality, 30-minute playback,
  real-media random seek recovery, or audio/video offset below 100 ms.
- 150%/200% DPI, mixed DPI, dual display, display hot-plug, sleep/resume,
  window crossing, Windows 11, Intel GPU, or a hardware-decoding matrix.
- Transparent WebView2 controls over the native mpv surface.
- Process-level network egress fail-closed. The Lavf nested `tcp://` risk
  remains open.
- Product `local-relay`, authentication, ticketing, packaging, signing, or
  redistribution/license approval.
