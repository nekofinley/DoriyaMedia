# Tauri dual-stream and open-deadline review fixes (2026-11-04)

Status: synthetic mechanism evidence only. W0 remains open.

## Fixed

- Dual-stream open now sets `pause=yes` before `loadfile`, waits for the
  primary track, adds the external audio stream with `select`, verifies the
  selected external track, and only then clears pause.
- Runtime startup keeps its bounded startup budget. After the runtime is ready,
  media open starts a fresh 30-second deadline instead of inheriting the
  remaining cold-start or five-second control budget.
- Failures observed while `audio-add` is pending map to
  `mpv_media_audio_load_failed`. After attachment, mpv events do not identify
  which stream failed, so terminal stream errors remain generic media errors.
- Debug loopback mode distinguishes mixed loopback-HTTP/public-HTTPS dual
  streams from an unsupported single public-HTTPS source and reports
  `mpv_egress_mode_unsupported` for the latter.
- The relay tracks a 512-connection budget per playback, resets it when a new
  playback begins, does not spend it on cross-origin rejects, and separately
  limits simultaneous tunnels to 16.

## Verification

- `cargo fmt --all -- --check`: pass.
- `cargo clippy --all-targets --locked -- -D warnings`: pass.
- `cargo test --locked`: 97 passed, 0 failed, 6 opt-in tests ignored.
- Real mpv synthetic dual stream: 3/3 consecutive passes after the fixes.
  - Open time: 1.541-1.574 seconds with the injected 1.2-second pause gate.
  - Immediate paused position: 0/83/0 ms.
  - `audio-pts` minus `time-pos`: 0.027-0.031 seconds.
- Missing external audio: passed with `mpv_media_audio_load_failed` and no
  runtime residue.
- Header-protected loopback MP4 plus 20 position-checked seeks: passed after
  the startup/deadline refactor.
- Relay quota tests: 512 connections accepted per playback, the 513th rejected,
  reset on new playback, and 512 cross-origin rejects consume no budget.

## Remaining Gates

- No full production relay dual-stream E2E has established two real CONNECT
  tunnels with Header injection and synchronized playback. The current
  controller command-path test does not exercise that data plane.
- The latest public HTTPS relay probe did not establish an upstream tunnel in
  this environment (`raw=1`, `session=0`), so it is not counted as a pass.
- Real Bilibili dual-stream URLs, real media, real 20-seek recovery, 100 ms
  audio/video offset, 30-minute stability, DPI/display, network switching,
  signed mpv distribution, and the `tcp://` egress bypass remain open.
