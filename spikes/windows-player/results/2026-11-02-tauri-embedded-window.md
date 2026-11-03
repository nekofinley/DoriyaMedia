# Tauri embedded-window smoke (2026-11-02 to 2026-11-03)

Status: local mechanism evidence only. This does not close W0, G4, or any real
media gate.

The interaction geometry in this document belongs to the historical
`da851673...` build. Current-build geometry, responsiveness, keyboard
transitions, and forced-cleanup evidence is recorded in
`2026-11-04-tauri-current-build-window.md`.

## Environment

- Windows 10 Pro 10.0.19045, build 19045.
- Single active display: 1920x1080, 100% scale, 96 DPI.
- NVIDIA GeForce RTX 3070 Laptop GPU driver 32.0.16.1742 and AMD Radeon
  Graphics driver 31.0.21923.11000 are present. No Intel GPU or Windows 11
  evidence was produced.
- WebView2 Runtime 156.0.4314.58.
- mpv SHA-256:
  `1645e57569c0af58a70c61257f3716f2ee6eb31bb170edc51f3f25a0b8d0d99c`.
- Synthetic S3 fixture SHA-256:
  `cb295cf642b75964772ac9a713f3ed714265957ee6107c377f955786c6e03eb7`.

The restricted automation session required:

```powershell
$env:WEBVIEW2_USER_DATA_FOLDER = 'C:\temp\doriya-w0-ebwebview'
$env:WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = '--disable-gpu'
```

This made the WebView2 control UI load. It is a harness workaround, not evidence
for the normal user profile, hardware acceleration, or a deployment
configuration. mpv rendered video through its own native HWND.

## Build and tests

```powershell
pnpm install --frozen-lockfile
pnpm build
cargo fmt --all -- --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
pnpm tauri build --debug --no-bundle
```

Results:

- Rust tests: 80 passed, 0 failed, 4 opt-in tests ignored.
- Opt-in tests: 4 passed, 0 failed.
- Final debug executable SHA-256:
  `da8516735cd8b499d5a9790c84a20b6b131e93631327a706e814ad4f487c00b0`.

The final build marshals video-surface creation, bounds changes, visibility
changes, and native diagnostics onto Tauri's main event-loop thread. A worker
thread only waits on a bounded one-result channel. Invalid video targets are
rejected instead of silently removing mpv's `--wid` argument. Blocking process
startup, IPC, status, and stop work runs on Tauri's blocking pool rather than
the WebView/IPC callback thread. The 2026-11-03 final build also accepts `F11`
as a fullscreen toggle; `Escape` remains an exit-only shortcut.

## Prior-build interaction recheck (`da851673...`)

The final executable auto-opened the same synthetic S3 fixture. An external
Win32 reading observed the normal window, fullscreen, and two resize
geometries:

| State | Tauri main outer/client | Video surface outer/client | mpv outer/client |
| --- | ---: | ---: | ---: |
| Normal launch | 1196x859 / 1180x820 | 1117x344 / 1117x344 | 1117x344 / 1117x344 |
| `F11` fullscreen | 1920x1080 / 1920x1080 | 1920x1017 / 1920x1017 | 1920x1017 / 1920x1017 |
| Native Size, minimum | 656x519 / 640x480 | 601x171 / 601x171 | 601x171 / 601x171 |
| Native Size, larger | 1558x970 / 1542x931 | 1380x391 / 1380x391 | 1380x391 / 1380x391 |

The parent chain and exact geometry agreement remained valid after routing the
surface operations through the main thread. Both the fullscreen button path and
the new `F11` path produced the 1920x1017 video rectangle while retaining the
HTML control strip. Exiting with either `F11` or `Escape` returned to windowed
mode. During MPV startup and fixture loading, 120
`WM_NULL`/`SendMessageTimeout` probes all completed with no hung window;
observed maximum latency was 11 ms and average latency was 0.1 ms. Forced host
termination also released the Job Object and left no mpv process.

## GUI fixture

The fixture server bound only to `127.0.0.1:18431`. Debug-only environment
variables selected the checked-out synthetic S3 MKV and requested one automatic
open after a successful video-surface sync.

Prior-build fullscreen Win32 readings:

| Window | Owner PID | Parent | Style | WS_CHILD | Outer | Client |
| --- | ---: | ---: | --- | --- | ---: | ---: |
| Tauri main | 14144 | desktop | n/a | n/a | 1920x1080 | 1920x1080 |
| `DoriyaW0MpvSurface` | 14144 | Tauri main | `0x56000000` | yes | 1920x1017 | 1920x1017 |
| `mpv` | 9408 | video surface | `0x58000000` | yes | 1920x1017 | 1920x1017 |
| `WRY_WEBVIEW` | 14144 | Tauri main | `0x52000000` | yes | 1920x1080 | 1920x1080 |

The normal-window run used PIDs 3228/22000 for the host/mpv pair. The resize
run used PIDs 8548/21980. All three interaction runs retained the same parent
chain, both `WS_CHILD` styles, and exact container/mpv geometry agreement.

## Lifecycle

- The synthetic fixture reached a paused end state at `0:03`; video output was
  rendered by the mpv child HWND.
- The Tauri fullscreen command produced a 1920x1080 host window while preserving
  a 63-pixel HTML control strip below the 1920x1017 video rectangle.
- Stop removed the mpv process. A follow-up process query returned no mpv PID.
- No secondary top-level mpv window or double rendering was observed.

## Not proven

- Real S1-S5 samples, real E-AC-3, ASS/PGS rendering, 30-minute playback,
  20-seek recovery on real media, or audio/video offset below 100 ms.
- 150%/200%, mixed DPI, dual display, display hot-plug, sleep/resume, window
  crossing, Windows 11, Intel GPU, or a hardware-decoding matrix.
- Transparent WebView2 controls over video. The prototype deliberately keeps
  the native video rectangle and HTML controls disjoint.
- Process-level network egress fail-closed. The open Lavf `tcp://` W0 risk
  remains.
- Product `local-relay`, authentication, ticketing, packaging, signing, or
  redistribution/license approval.
