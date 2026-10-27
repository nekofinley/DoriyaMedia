# Doriya Windows W0: Tauri 2 + mpv

> Status: mechanism spike implemented and locally buildable. W0 is **not closed**. No 30-minute playback, 20-seek synchronization, DPI/display, GPU, Windows 11, or network matrix has been executed.

This is an isolated feasibility prototype. It must not be copied into `clients/windows/` or used as a distributable installer.

## Scope

The frontend is a control and status surface only. Rust owns:

- mpv sidecar startup and bounded shutdown.
- A unique Windows named pipe and newline-delimited mpv JSON IPC.
- A fixed command allowlist and numeric bounds.
- HTTPS-only media URL validation, with plain HTTP limited to explicitly enabled loopback test mode.
- Header validation for only `Accept`, `Accept-Language`, `Origin`, `Referer`, and `User-Agent`.
- Coded, non-sensitive errors and allowlisted diagnostic fields.

Media URLs and Header values are never placed in mpv command-line arguments, Tauri capabilities, local storage, or application log messages. The media URL and Header values are sent to mpv through a randomly named local pipe after Rust validation. The pipe name uses a 128-bit CSPRNG suffix, and the host verifies that the pipe server PID matches the mpv process it just launched. This validation does not make the pipe a security boundary against a hostile process already running as the same Windows user.

## Run

From this directory:

```powershell
pnpm install
$env:DORIYA_W0_MPV_PATH = 'C:\Program Files\MPV Player\mpv.exe'
$env:DORIYA_W0_ALLOW_LOOPBACK_HTTP = '1' # only for a local fixture
pnpm tauri dev
```

The application does not auto-load media. Use the masked URL input, or provide an environment input before starting:

```powershell
$env:DORIYA_W0_MEDIA_URL = 'https://media.example.invalid/path'
$env:DORIYA_W0_MEDIA_REFERER = 'https://www.example.invalid/'
$env:DORIYA_W0_MEDIA_USER_AGENT = 'Doriya-W0/0.1'
$env:DORIYA_W0_MEDIA_ORIGIN = 'https://www.example.invalid'
```

Environment values remain in the parent process environment and are not written by this spike. Prefer terminal-local environment variables and do not use shell history for production credentials.

## Verification performed

Run from this directory:

```powershell
pnpm install --frozen-lockfile
pnpm build
Set-Location .\src-tauri
cargo fmt --all -- --check
cargo clippy --all-targets --locked -- -D warnings
cargo test --locked
```

The Rust tests cover:

- Ignoring mpv event frames while waiting for the matching request response.
- Strict request-ID and protocol response validation.
- Distinguishing `property unavailable` from command rejection.
- Parsing `start-file`, `file-loaded`, and `end-file`, including old-generation
  event isolation across a reopen.
- HTTPS, user-info, fragment, loopback, and non-loopback URL policy.
- Header allowlisting, injected control characters, case-insensitive duplicates, and invalid URL values.
- Reaping an exited child and releasing its IPC handle.
- Bounded termination of a still-running child.
- Enforcing a fixed five-second response deadline without blocking the controller
  thread, then forcing sidecar termination and state cleanup on timeout.
- Proving sidecar arguments do not contain media URL, Header, or Cookie material.
- Clearing the inherited environment and passing only an explicit allowlist to mpv.
- Generating collision-resistant random pipe names and rejecting a pipe owned by another process.
- Keeping failed starts/open operations from publishing loaded or stale-session state.
- Replacing a reaped runtime on a later start and clearing state after control transport failure.
- Reading the current pause property before toggling instead of deriving it from stale UI metrics.

The installed-mpv integration test is opt-in because it starts a real process:

```powershell
cargo test --locked installed_mpv_accepts_json_ipc_and_shuts_down -- --ignored --nocapture
```

It opens an mpv sidecar with no media, verifies the `mpv-version` response through JSON IPC, and verifies process and pipe cleanup.

The full Tauri executable build requires an MSVC environment. From this directory:

```powershell
cmd.exe /d /s /c '"C:\Program Files\Microsoft Visual Studio\2022\Community\Common7\Tools\VsDevCmd.bat" -arch=x64 -host_arch=x64 >NUL && set "CARGO_PROFILE_DEV_DEBUG=0" && pnpm tauri build --debug --no-bundle'
```

`--no-bundle` is intentional: this Spike does not create a distributable installer and does not copy or package mpv, FFmpeg, VLC, or their codecs.

Local verification on 2026-10-28:

| Gate | Result |
| --- | --- |
| `pnpm install --frozen-lockfile` | Pass |
| `pnpm build` | Pass |
| `cargo fmt --all -- --check` | Pass |
| `cargo clippy --all-targets --locked -- -D warnings` | Pass |
| `cargo test --locked` | Pass: 32 passed, 1 opt-in test ignored |
| Installed-mpv IPC smoke test | Pass: 1/1 |
| `pnpm tauri build --debug --no-bundle` | Pass |
| Explorer-launched GUI smoke | Pass: window created, process responsive, clean process exit |

The first Tauri build attempt exhausted the host's free disk while producing full debug symbols. The successful final build set `CARGO_PROFILE_DEV_DEBUG=0`; this changes debug information, not application behavior, and produced the executable at `src-tauri/target/debug/doriya-w0-tauri-mpv.exe`. Generated build directories are ignored and are not part of the deliverable.

Directly launching the generated GUI from the restricted Codex automation token made WebView2 return OS error 5 (`Access denied`). Launching the same executable through Explorer in the interactive desktop session succeeded. Use an interactive desktop session for GUI smoke tests; this is a harness constraint, not an application capability failure.

## Not executed

The following remain **open W0 gates** and must not be inferred from passing unit tests or the integration smoke test:

- Real S1-S5 media playback, including MKV, HEVC 10-bit, FLAC, ASS/PGS, Range and Header behavior.
- Any 30-minute continuous playback run.
- 20 random seeks per sample, seek recovery, or audio/video offset measurement against the 100 ms gate.
- Fullscreen, 100%/150%/200% DPI, single/dual display, display hot-plug, sleep/resume, or window crossing.
- Windows 11, Intel/NVIDIA/AMD device matrix, Wi-Fi/hotspot/VPN/region changes.
- Real Bilibili dual-stream `direct-dash`, audio/video merge, or expiry/fallback behavior.
- License/distribution review for the installed mpv, FFmpeg and codec builds.
- Production ticket storage, authentication, refresh, or relay.

## Toolchain notes

- Built with Tauri 2.5.1 and a Rust 1.87-compatible lockfile.
- `Cargo.toml` pins the Tauri 2.5.x family used at this Tauri release. `Cargo.lock` is committed for reproducibility.
- Tauri icon assets are checked-in source artifacts required for the build; generated ACL schemas under `src-tauri/gen` are ignored and regenerated by Tauri.
- Generated directories (`node_modules`, `dist`, `src-tauri/target`, plus common build outputs) are ignored by the local `.gitignore`.

## Security limitations

- The control UI is not authenticated. A production shell needs a device authorization design and a trusted ticket source.
- The named pipe is randomly named, local-only, and PID-checked, but it does not yet have a complete DACL/SID/session peer-trust policy. Same-user malicious processes may still inspect or connect to it.
- Blocking pipe I/O runs on a worker thread. Every request has a fixed five-second
  deadline; a timeout is treated as fatal and triggers bounded forced termination
  of the sidecar. The detached worker can only exit after the closed pipe unblocks.
- `loadfile` acceptance is not treated as proof that media loaded; `file-loaded`
  and `end-file` now drive the stable state. mpv events do not carry the
  application request ID, so old-generation isolation still depends on the
  `start-file` barrier and mpv event ordering.
- Forced child wait after a kill attempt is bounded, but a Windows Job Object is not yet used to guarantee sidecar cleanup after a host crash.
- The sidecar executable identity and redirect policy are not yet pinned or verified beyond the configured path/PATH lookup.
- URL and Header values necessarily exist in process memory when handed to mpv; this prototype minimizes persistence and logging, not memory exposure.
- mpv is chosen from `DORIYA_W0_MPV_PATH`, otherwise `mpv` from `PATH`. Production needs a signed, versioned sidecar and explicit license review.
