# Doriya Windows W0: Tauri 2 + mpv

> Status: mechanism spike implemented and locally buildable. W0 is **not closed**. Synthetic-media embedded-window, fullscreen/resize, and external-audio merge smokes have passed, but no 30-minute playback, 20-seek synchronization on real media, DPI/display, GPU, Windows 11, or network matrix has been executed.

This is an isolated feasibility prototype. It must not be copied into `clients/windows/` or used as a distributable installer.

## Scope

The frontend is a control and status surface only. Rust owns:

- mpv sidecar startup and bounded shutdown.
- An absolute `DORIYA_W0_MPV_PATH` plus a required SHA-256 pin from
  `DORIYA_W0_MPV_SHA256`. The file is opened without write/delete sharing,
  hashed, and kept locked until `CreateProcessW` has accepted the image.
- An optional `DORIYA_W0_MPV_SIGNER_SHA256` publisher pin. When configured,
  the same locked file handle must also pass embedded Authenticode verification
  and expose exactly that SHA-256 leaf-signer certificate thumbprint.
- A host-created, single-instance Windows named pipe with an ACL that grants
  only the current user, remote clients rejected, and a 128-bit CSPRNG name.
  The pipe is preconnected before mpv starts. `STARTUPINFOEX` restricts the
  child handle list to the client stdin and NUL output/error handles; mpv
  receives `--input-ipc-client=fd://0`.
- Newline-delimited mpv JSON IPC over a synchronous wrapper around the required
  overlapped named-pipe I/O.
- A fixed command allowlist and numeric bounds.
- HTTPS-only media URL validation, with plain HTTP limited to explicitly enabled loopback test mode.
  Public HTTPS IP literals are required to be globally routable; loopback,
  private, link-local, documentation, CGNAT, multicast, selected special-use, and
  `localhost` targets are rejected. In strict mode, mpv uses a loopback
  `CONNECT` egress guard whose proxy URL contains no credentials. The guard
  accepts only the exact HTTPS CONNECT authority selected for the current playback,
  performs one single-flight DNS resolution outside the global relay lock with
  a five-second deadline, rejects mixed public/non-public answers, connects
  only to the pinned numeric addresses, and requires every accepted TCP
  connection to be owned by the already verified mpv PID plus its original
  process creation time. The listener uses `SO_EXCLUSIVEADDRUSE`; starting a
  new playback or disarming the guard actively shuts down registered tunnels.
  A 512-connection budget counts accepted connections within one playback,
  resets when a new playback starts, and is not consumed by malformed or
  cross-origin `CONNECT` requests. The independent 16-connection cap bounds
  simultaneous tunnels.
- Header validation for only `Accept`, `Accept-Language`, `Origin`, `Referer`, and `User-Agent`.
- A primary video URL with an optional `audio-add` secondary stream. Both URLs
  use the same validated Header map; identical URLs, Header mismatches, and
  cross-origin audio in strict relay mode fail closed. A dual-stream open first
  sets `pause=yes`, starts `loadfile` only after that acknowledgement, waits for
  the primary track, attaches and observes the selected external audio track,
  then clears pause. Failures observed while `audio-add` is pending retain
  `mpv_media_audio_load_failed`; after attachment, mpv does not identify which
  stream failed, so later terminal stream errors remain generic media-load
  failures.
- Coded, non-sensitive errors and allowlisted diagnostic fields.
- A dedicated Win32 child HWND owned by the Tauri main window. mpv is started
  with `--wid=<surface>` and renders only inside the WebView-reported video
  stage; HTML controls remain outside that rectangle because independent native
  HWNDs cannot overlay the WebView with CSS.
- Host-window fullscreen through Tauri. The video stage and its mpv child are
  resized from the same DOM geometry, while a compact control strip remains
  outside the native video surface. `F11` toggles fullscreen; `Escape` exits it.

Media URLs and Header values are never placed in mpv command-line arguments,
Tauri capabilities, local storage, or application log messages. The pipe name
is not passed to mpv. The sole preconnected client endpoint is inherited as
stdin after launch, while the host retains the server endpoint. The host also
verifies the child's user SID and session and assigns it atomically to a
`KILL_ON_JOB_CLOSE` Job Object through `PROC_THREAD_ATTRIBUTE_JOB_LIST`. This
reduces cross-process pipe exposure, but it
does not make the spike resistant to a hostile process with the same user's
debug/injection rights.

## Run

Strict HTTPS mode (do not set the loopback fixture override):

```powershell
pnpm install
$env:DORIYA_W0_MPV_PATH = 'C:\Program Files\MPV Player\mpv.exe'
$env:DORIYA_W0_MPV_SHA256 = (Get-FileHash -LiteralPath $env:DORIYA_W0_MPV_PATH -Algorithm SHA256).Hash.ToLowerInvariant()
# $env:DORIYA_W0_MPV_SIGNER_SHA256 = '<64 hex characters from a trusted release>'
Remove-Item Env:DORIYA_W0_ALLOW_LOOPBACK_HTTP -ErrorAction SilentlyContinue
pnpm tauri dev
```

`DORIYA_W0_MPV_PATH` must be absolute. Startup fails closed when the SHA-256
value is missing, malformed, or does not match the file. The signer pin is
optional for local development because the installed development mpv is
unsigned; when it is set, a missing, invalid, untrusted, or mismatched
Authenticode signer also fails startup closed.

`DORIYA_W0_ALLOW_LOOPBACK_HTTP=1` is a separate isolated fixture mode honored
only by debug builds. While enabled, the runtime does not create the strict
HTTPS egress guard, adds `http` to the mpv process-wide Lavf protocol allowlist,
and allows the initial URL to load directly from loopback. Nested redirects and
playlist resources can therefore use HTTP for that process. Do not use this flag
with production tickets, and do not count direct-fixture results as relay
evidence. Dual-stream requests in this mode must use loopback HTTP for both
streams; mixing a loopback-HTTP primary with public HTTPS audio is rejected
before `loadfile`.

The sidecar uses `--tls-verify=yes`, `--ytdl=no`, and
`--network-timeout=15`. It disables config scripts, automatic local file
loading, external-subtitle autoload, audio-file autoload, and cover-art
autoload. Candidate Lavf limits are applied with
`--stream-lavf-o-append` and `--demuxer-lavf-o-append` for the protocol
allowlist and `max_redirects=3`. These append forms were exercised with the real
installed mpv and do not break `fd://0` JSON IPC, but a positive/negative
HTTPS HLS/DASH subresource matrix is still required before treating redirect
and nested-resource policy as complete.

Runtime startup retains its own bounded startup budget. Once the runtime is
ready, each media open receives a fresh independent 30-second deadline rather
than borrowing the five-second control-operation budget from an already running
sidecar. Relay configuration, `stop`, Header setup, `loadfile`, primary-track
wait, `audio-add`, selected-audio wait, and final unpause all share that
media-open deadline. Expiry reports `mpv_media_open_timeout` before a
per-request IPC timeout. `status` polling also enforces the same deadline, then
stops the sidecar and transitions to a stable error instead of remaining in
`Opening` indefinitely.

The application does not auto-load media by default. Use the masked URL input,
or provide an environment input before starting:

```powershell
$env:DORIYA_W0_MEDIA_URL = 'https://media.example.invalid/path'
$env:DORIYA_W0_MEDIA_REFERER = 'https://www.example.invalid/'
$env:DORIYA_W0_MEDIA_USER_AGENT = 'Doriya-W0/0.1'
$env:DORIYA_W0_MEDIA_ORIGIN = 'https://www.example.invalid'
# Optional external audio; omit it to use the primary source's own tracks.
$env:DORIYA_W0_AUDIO_URL = 'https://media.example.invalid/audio'
```

Environment values remain in the parent process environment and are not written by this spike. Prefer terminal-local environment variables and do not use shell history for production credentials.

Debug builds also accept a strict loopback fixture shorthand and an explicit
GUI automation hook:

```powershell
$env:DORIYA_W0_LOOPBACK_FIXTURE = '18431/s3-hevc10-flac-ass.mkv'
$env:DORIYA_W0_AUTO_OPEN = '1'
$env:DORIYA_W0_DEBUG_TRACE_PATH = 'C:\temp\doriya-w0-events.log'
```

`DORIYA_W0_LOOPBACK_FIXTURE` is accepted only in debug builds and is still
validated by `DORIYA_W0_ALLOW_LOOPBACK_HTTP`. It constructs a loopback URL from
the fixed host, numeric port, and a path restricted to safe filename segments.
`DORIYA_W0_AUTO_OPEN` waits for one successful video-surface sync before opening
the environment media. `DORIYA_W0_DEBUG_TRACE_PATH` records fixed event names
and geometry-safe outcomes only; URLs, query strings, and Header values are not
written.

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
- Rejecting invalid video target handles instead of silently launching a
  non-embedded mpv window, and marshalling all Win32 surface operations onto
  Tauri's main event-loop thread through a bounded result channel. Blocking
  mpv startup and IPC work is dispatched to Tauri's blocking pool.
- Clearing the inherited environment and passing only an explicit allowlist to mpv.
- Generating collision-resistant random pipe names, creating a one-ACE
  current-user DACL, rejecting remote clients, and proving a second client
  cannot attach to the preconnected single-instance pipe.
- Proving an arbitrary inheritable parent file handle is absent from the
  restricted child handle list.
- Verifying the launched process user SID and session, and failing closed on a
  mismatch using the original process handle rather than reopening a PID.
- Asserting Job Object `KILL_ON_JOB_CLOSE`, job membership, and child termination
  when the job handle closes.
- Enforcing one shared five-second deadline across multi-request `control` and
  `status` operations.
- Giving cold process launch and the first IPC acknowledgement one shared
  fifteen-second startup deadline. An auto-starting `control` uses a bounded
  startup-plus-operation budget; after runtime readiness, `open` starts a fresh
  thirty-second media deadline and warm runtime control operations remain
  limited to five seconds.
- Requiring an absolute mpv path and matching SHA-256 pin before process
  creation, then holding the verification handle without write/delete sharing
  until the image has been passed to `CreateProcessW`.
- Verifying an optional embedded Authenticode leaf-signer SHA-256 thumbprint
  through the same locked handle before process creation.
- Enforcing the sidecar script/autoload switches, Lavf protocol allowlist and
  redirect limit in command-line tests, and exercising that argument set against
  the real installed mpv over `fd://0` IPC.
- Releasing the sidecar and reporting a stable error when `status` polling sees
  the 30-second `file-loaded` deadline expire.
- Loading a Header-protected loopback MP4 through the full Tauri controller,
  pausing, issuing 20 relative seeks, resuming playback, and verifying no stale
  player error or leftover sidecar state.
- Loading synthetic E-AC-3 and HEVC Main10/FLAC/ASS MKV fixtures through the
  same controller, asserting decoded codecs, 10-bit pixel format, embedded ASS
  track selection, and 20 relative seeks per fixture.
- Pausing before a dual-stream `loadfile`, waiting for both selected tracks,
  resuming only after attachment, and mapping failures seen while `audio-add`
  is pending to `mpv_media_audio_load_failed`.
- Proving a primary-track delay beyond five seconds still succeeds under the
  independent 30-second media-open deadline, while deadline expiry reports
  `mpv_media_open_timeout`; runtime startup is completed before that media
  deadline begins.
- Deterministic relay-mode `PlayerController::open()` command-path coverage for
  same-origin dual-stream acceptance and cross-origin failure, plus
  debug-loopback rejection of mixed loopback-HTTP and public-HTTPS streams. This
  is not a real two-stream CONNECT/transport test.
- Keeping failed starts/open operations from publishing loaded or stale-session state.
- Replacing a reaped runtime on a later start and clearing state after control transport failure.
- Reading the current pause property before toggling instead of deriving it from stale UI metrics.
- Strict HTTP/1.1 `CONNECT` parsing, exact origin matching, one-time DNS
  resolution and pinning per playback, mixed private/public answer rejection,
  absolute header deadlines, bounded read/write tunnel timeouts, and
  generation-based tunnel shutdown.
- A 512-connection per-playback budget that resets on a new playback, remains
  unaffected by 512 cross-origin rejections, and is distinct from the
  16-connection simultaneous-tunnel cap.
- Windows TCP-table ownership lookup for the client-side ESTABLISHED loopback
  row, including byte-order decoding and rejection when the connection owner is
  not the bound mpv PID or its verified creation time. The loopback listener
  rejects same-port rebinding with `SO_EXCLUSIVEADDRUSE`.
- A real installed-mpv HTTPS probe that requires the relay to complete an
  owner-bound CONNECT response, not merely accept the client socket.

The installed-mpv integration test is opt-in because it starts a real process:

```powershell
cargo test --locked installed_mpv_accepts_json_ipc_and_shuts_down -- --ignored --nocapture
```

It opens an mpv sidecar with no media, verifies the `mpv-version` response through
JSON IPC, and verifies process and pipe cleanup. The installed mpv binary uses
the `fd://0` IPC-client form; the client pipe is inherited as stdin.

The header/seek fixture integration test is also opt-in and requires Python:

```powershell
cargo test --locked installed_mpv_loads_header_protected_loopback_fixture_and_seeks -- --ignored --nocapture
```

It starts `fixtures/header-range-server.py` on `127.0.0.1`, loads
`fixtures/synthetic/s1-h264-aac.mp4` through the real Tauri player controller,
requires matching `Referer` and `User-Agent`, performs 20 seeks while paused,
and resumes playback. This is mechanism evidence only; it does not replace real
S1-S5 media, network, timing, or device acceptance. The test enables the direct
loopback HTTP fixture mode, so it does not exercise or validate the CONNECT
egress guard.

The synthetic codec coverage test is also opt-in and requires Python:

```powershell
cargo test --locked installed_mpv_decodes_synthetic_eac3_and_hevc10_fixtures_and_seeks -- --ignored --nocapture
```

It loads `s2-h264-eac3.mkv` and `s3-hevc10-flac-ass.mkv` through the full
controller with deterministic software decoding. It asserts `eac3`, `flac`,
HEVC Main 10, the exact `yuv420p`/`yuv420p10` format, a decoded video frame,
48 kHz mono audio, advancing audio PTS, and the embedded ASS track selected by
its actual track ID. It then performs 20 controller seeks per fixture, waiting
for each seek to settle and verifying `time-pos`, with at least 12 distinct
landing positions. Subtitle selection is rechecked after seeks and resume.
This proves synthetic demux/decode/track/seek wiring only; it does not replace a
real S2 E-AC-3 sample, S3 ASS/PGS rendering, 30-minute play, or device
acceptance.

The synthetic dual-stream test is also opt-in and requires Python:

```powershell
cargo test --locked installed_mpv_merges_synthetic_video_and_audio_streams_and_seeks -- --ignored --nocapture
```

It starts independent loopback origins for `s1-video-only.mp4` and
`s1-audio-only.mp4`, confirms the real mpv clock stays below 0.75 seconds during
an injected pause-gate wait, then verifies both selected tracks, performs 20
position-checked seeks, resumes playback, and records a diagnostic `time-pos`
versus `audio-pts` sample. The controller pauses before `loadfile`, waits for
the primary track before `audio-add`, and does not unpause until the selected
external audio track is visible. The injected delay is test-only and makes the
"paused while waiting" assertion deterministic without changing production
timing.

```powershell
cargo test --locked installed_mpv_rejects_missing_external_audio_and_cleans_up -- --ignored --nocapture
```

The negative test points the validated audio URL at a missing fixture path and
requires `mpv_media_audio_load_failed` plus complete runtime cleanup.

The public HTTPS relay probe is opt-in and requires network access:

```powershell
cargo test --locked installed_mpv_routes_public_https_through_connect_relay -- --ignored --nocapture
```

It starts the real mpv sidecar in strict mode, injects the credential-free
loopback proxy URL through JSON IPC, loads `https://example.com/`, and requires
the relay to write the successful CONNECT response for a tunnel whose Windows
TCP-table owner PID and process creation time match the verified mpv sidecar.
FFmpeg sends `Host` as the proxy loopback authority rather than the target
authority; the parser accepts only that exact proxy authority or the exact
CONNECT target. On 2026-11-01 this probe passed 3/3 after the hardening
changes. It is egress guard mechanism evidence only and does not establish
product `local-relay`.

The full Tauri executable build requires an MSVC environment. From this directory:

```powershell
cmd.exe /d /s /c '"C:\Program Files\Microsoft Visual Studio\2022\Community\Common7\Tools\VsDevCmd.bat" -arch=x64 -host_arch=x64 >NUL && set "CARGO_PROFILE_DEV_DEBUG=0" && pnpm tauri build --debug --no-bundle'
```

`--no-bundle` is intentional: this Spike does not create a distributable installer and does not copy or package mpv, FFmpeg, VLC, or their codecs.

Local verification through 2026-11-04:

| Gate | Result |
| --- | --- |
| `pnpm install --frozen-lockfile` | Pass |
| `pnpm build` | Pass |
| `cargo fmt --all -- --check` | Pass |
| `cargo clippy --all-targets --locked -- -D warnings` | Pass |
| `cargo test --locked` | Pass: 97 passed, 6 opt-in tests ignored |
| Installed-mpv IPC smoke test | Pass: 3/3 consecutive runs |
| Header-protected loopback MP4 + 20 seeks | Pass: 3/3 consecutive runs |
| Synthetic S2 E-AC-3 + S3 HEVC10/FLAC/ASS + 20 position-verified seeks each | Pass: 3/3 consecutive runs |
| Synthetic external-audio merge + 20 position-verified seeks | Pass: 3/3 consecutive runs |
| Missing external audio fails closed and releases runtime | Pass: 3/3 consecutive runs |
| Current pause-gate regression | Pass: real mpv held the clock during a 1.2-second injected wait; three consecutive runs opened in 1.541-1.574 s and paused at 0-83 ms, with PTS deltas 0.027-0.031 s |
| Current missing-audio regression | Pass with `mpv_media_audio_load_failed` and no runtime residue after the startup/deadline refactor |
| Current single-stream open regression | Header-protected loopback MP4 + 20 position-checked seeks passed after the startup/deadline refactor |
| Relay quota regression | Pass: 512 accepted connections are allowed per playback, the 513th fails, a new playback resets the budget, and 512 cross-origin rejections consume none |
| Public HTTPS CONNECT with mpv owner-PID binding | Pass: 3/3 consecutive runs |
| Latest combined opt-in invocation | 5/6; the public HTTPS probe had no upstream tunnel in that run (`raw=1`, `session=0`), while both dual-stream tests passed |
| Current public HTTPS relay probe | Not established on 2026-11-04: `raw=1`, `session=0`; treated as environment failure, not relay success |
| `pnpm tauri build --debug --no-bundle` | Pass |
| Dual-stream GUI auto-open | Pass on the `7be9a184...` build with the restricted-session WebView2 workaround; the page reported both URLs configured, attached mpv, and closed with no host/mpv residue |
| Current-build embedded synthetic S3 geometry | Pass on `7be9a184...`: normal, 1920x1080 fullscreen, 640x480 and 1542x931 client sizes produced matching video/mpv rectangles with correct parent/PID ownership and `WS_CHILD` state |
| Current-build UI responsiveness during auto-open | Pass on `7be9a184...`: 120/120 `SendMessageTimeout` probes completed; maximum 13.62 ms, average 0.61 ms |
| Host fullscreen with embedded video | Pass on `7be9a184...`: `F11` entered 1920x1080 fullscreen with a 1920x1017 video surface; `F11` and `Escape` exited |
| Resize with embedded video | Pass on `7be9a184...`: 640x480 and 1542x931 client sizes produced matching 601x171 and 1380x391 video/mpv rectangles |
| Current-build stop lifecycle | Pass on `7be9a184...`: normal close and forced host termination both left no mpv process |

Detailed current-build window, keyboard, responsiveness, and cleanup evidence is
recorded in
`../results/2026-11-04-tauri-current-build-window.md`.
The dual-stream pause gate, independent media deadline, error mapping, and
relay-budget review evidence is recorded in
`../results/2026-11-04-tauri-dual-stream-review-fixes.md`.

With the Tauri host and mpv sidecar running, the reusable Win32 probe is:

```powershell
.\scripts\window-smoke.ps1 `
    -RunResizeMatrix `
    -RunResponsiveness `
    -OutputPath C:\temp\doriya-w0-window-smoke.json
```

It requires exactly one `doriya-w0-tauri-mpv` process and one `mpv` process,
validates the parent chain, `WS_CHILD`, visibility, and outer/client geometry,
and exits nonzero on any mismatch or response timeout. It runs on Windows
PowerShell 5.1 and PowerShell 7. Run the resize matrix while the host is in
windowed mode; the script restores the original rectangle but does not manage
fullscreen state.

The first Tauri build attempt exhausted the host's free disk while producing full debug symbols. The current successful build set `CARGO_PROFILE_DEV_DEBUG=0`; this changes debug information, not application behavior, and produced the executable at `src-tauri/target/debug/doriya-w0-tauri-mpv.exe` with SHA-256 `7be9a18465c4ef76578e16a0c06339a8e53a8b9454f4acda6b9d53600a91ea8c`. Generated build directories are ignored and are not part of the deliverable.

The restricted automation session needed a writable WebView2 user-data folder
and a software-rendered WebView2 UI before the page could load:

```powershell
$env:WEBVIEW2_USER_DATA_FOLDER = 'C:\temp\doriya-w0-ebwebview'
$env:WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS = '--disable-gpu'
```

This affects only the WebView2 control UI. mpv rendered the synthetic video in
its own native HWND. The workaround is a harness constraint, not evidence for
the normal user profile or a supported deployment configuration.

## Not executed

The following remain **open W0 gates** and must not be inferred from passing unit tests or the integration smoke test:

- Real S1-S5 media playback, including MKV, HEVC 10-bit, FLAC, ASS/PGS, Range and Header behavior.
- Any 30-minute continuous playback run.
- 20 random seeks on each real S1-S5 sample, seek recovery, or audio/video offset measurement against the 100 ms gate. Synthetic seek and PTS diagnostics cover controller mechanics only.
- 150%/200% DPI, mixed DPI, dual display, display hot-plug, sleep/resume, or window crossing. Fullscreen and resize were exercised only on one 1920x1080 display at 100%.
- Windows 11, Intel/NVIDIA/AMD device matrix, Wi-Fi/hotspot/VPN/region changes.
- Real Bilibili dual-stream `direct-dash`, audio/video merge, or expiry/fallback behavior.
- License/distribution review for the installed mpv, FFmpeg and codec builds.
- Production ticket storage, authentication, refresh, or the product
  `local-relay` transport. The W0 loopback egress guard is not that transport.

Current fullscreen/display constraints:

- The Tauri main window now drives fullscreen. The dedicated video surface and
  its mpv child resize together, and the HTML control strip remains outside the
  native video rectangle. CSS overlays over the video are intentionally not
  supported by this prototype.
- The current harness has one active 1920x1080 display at 100% scaling. It
  cannot produce 150%, 200%, mixed-DPI, or dual-display evidence.
- The configured minimum window is now 640x480 logical pixels, removing the
  previous 620-pixel minimum-height contradiction at a hypothetical 200% scale.
  This is a layout mitigation, not DPI acceptance evidence.

## Toolchain notes

- Built with Tauri 2.5.1 and a Rust 1.87-compatible lockfile.
- `Cargo.toml` pins the Tauri 2.5.x family used at this Tauri release. `Cargo.lock` is committed for reproducibility.
- Tauri icon assets are checked-in source artifacts required for the build; generated ACL schemas under `src-tauri/gen` are ignored and regenerated by Tauri.
- Generated directories (`node_modules`, `dist`, `src-tauri/target`, plus common build outputs) are ignored by the local `.gitignore`.

## Security limitations

- The control UI is not authenticated. A production shell needs a device authorization design and a trusted ticket source.
- The pipe's DACL, single-instance preconnection, inherited client handle,
  process identity check, and Job Object replace the old PID-only path. This is
  still not anti-tamper protection against a same-user process with debug or
  injection rights.
- Pending overlapped I/O is cancelled and synchronously reaped before its
  `OVERLAPPED` storage is released; preconnection participates in the shared
  fifteen-second startup deadline. Blocking pipe I/O still runs on a worker thread. Multi-request
  operations share one five-second operation deadline, while a blocked worker
  can remain detached until the pipe closes.
- `PROC_THREAD_ATTRIBUTE_JOB_LIST` assigns the child to the Job Object during
  process creation, and the child handle list contains only stdin and NUL.
- An IPC timeout is treated as fatal and triggers bounded forced termination of
  the sidecar. The detached worker can only exit after the closed pipe unblocks.
- `loadfile` acceptance is not treated as proof that media loaded; `file-loaded`
  and `end-file` now drive the stable state. mpv events do not carry the
  application request ID, so old-generation isolation still depends on the
  `start-file` barrier and mpv event ordering.
- The sidecar supports an optional Authenticode leaf-signer thumbprint pin, but
  no signed mpv distribution, signed version manifest, production-enforced
  publisher gate, signer rotation/revocation policy, or update channel exists
  yet.
- Lavf stream/demuxer protocol allowlists and a redirect limit are configured,
  and the strict W0 egress guard now resolves and pins one exact HTTPS CONNECT
  authority per playback. It compares only host and port; after the tunnel is
  established it does not inspect TLS SNI, HTTP Host, path, query, method,
  Header or response content. It is deliberately not a general local relay:
  cross-origin redirects, HLS/DASH child origins, per-stream origins and
  automatic URL refresh currently fail closed. DNS is not re-resolved within a
  playback; CDN/DNS rotation or URL refresh requires a new playback, and strict
  DNS/PID/relay failures do not fall back to direct connections. HTTPS
  HLS/DASH redirect, nested-resource, cross-origin Header propagation and
  compatibility tests are not complete.
- **Open P1 egress risk:** real HTTPS requires `tcp` in the Lavf protocol
  whitelist. Lavf accepts `tcp://` without using the HTTP proxy, so a malicious
  nested playlist may bypass the CONNECT guard and open a direct TCP connection.
  A local experiment that kept `tcp` in the stream whitelist but removed it from
  the demuxer whitelist still observed the nested `tcp://` connection, so
  splitting those allowlists is not a mitigation.
  The W0 guard therefore confines proxied HTTP/HTTPS traffic; it is **not** a
  process-level egress fail-closed boundary. A production security gate needs
  WFP, AppContainer, a process firewall, or an equivalent network layer.
- PID plus process-creation-time matching is process-ownership association,
  not cryptographic authentication. Creation-time binding closes ordinary PID
  reuse, but handle duplication, kernel compromise, and a hostile same-user
  process with debugging or injection rights remain outside this spike's
  threat model. The guard remains a W0 mechanism, not the product
  `local-relay` transport or a ticket/session implementation.
- `DORIYA_W0_ALLOW_LOOPBACK_HTTP=1` is honored only by debug builds. Release
  builds always stay on the strict HTTPS/relay path, but the W0 spike still has
  no production configuration or trust boundary for this environment switch.
- Disarming closes every registered tunnel, but shutdown is not a complete
  cancellation barrier for work still inside a non-cancellable DNS lookup or
  an already-started five-second upstream connect. That handler may remain
  detached briefly after `stop()`'s two-second wait; its next generation and
  shutdown checks prevent sustained tunneling or a later successful session.
- The five-second DNS deadline bounds how long a CONNECT handler waits, not the
  underlying `ToSocketAddrs` worker. Standard Windows resolution is not exposed
  here as a cancellable operation, so a stuck resolver worker may remain
  detached after the relay has disarmed.
- URL and Header values necessarily exist in process memory when handed to mpv; this prototype minimizes persistence and logging, not memory exposure.
- Production still needs a signed, versioned sidecar, a reproducible hash
  manifest, and explicit license review.
