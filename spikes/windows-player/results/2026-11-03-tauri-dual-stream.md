# Tauri dual-stream mechanism smoke (2026-11-03)

Status: local synthetic mechanism evidence only. This does not close W0, S4,
S5, G3, or the 100 ms audio/video offset gate.

## Scope

The Tauri prototype now accepts a primary video URL plus an optional external
audio URL. Both URLs pass the existing HTTPS/loopback and Header allowlists.
The request fails closed when:

- both URLs are identical;
- the two streams do not use exactly the same Header map;
- strict relay mode would require two different HTTPS origins; or
- mpv rejects `audio-add`.

mpv receives the primary source through `loadfile`, then the secondary source
through `audio-add <url> select`. The controller waits for the primary track to
appear before adding audio, avoiding an observed startup race where an
immediate `audio-add` intermittently returned `error running command`.

## Fixtures

The two new fixtures are derived from the existing synthetic S1 MP4:

| File | Streams | SHA-256 |
| --- | --- | --- |
| `s1-video-only.mp4` | H.264 Constrained Baseline | `29d5ccb3d795312eb72b0402069829ddae0dc80e85d22fd0bbe4cff791c67509` |
| `s1-audio-only.mp4` | AAC LC | `a285fc728bd5d7bd4df4acbf20b2cfb264833f87503e94539055deaf23ded104` |

Each stream is served by a separate `127.0.0.1` fixture origin with the same
required `Referer` and `User-Agent` headers. The test therefore exercises two
independent URLs, Header validation, external-track loading and
`direct-loopback` mode. It does not exercise strict HTTPS relay dual-origin
support, which remains intentionally unsupported.

## Results

- `pnpm build`: pass.
- `cargo fmt --all -- --check`: pass.
- `cargo clippy --all-targets --locked -- -D warnings`: pass.
- `cargo test --locked`: 86 passed, 0 failed, 6 opt-in tests ignored.
- Dual-stream success test, three consecutive runs: 3/3 pass.
- Missing external-audio test, three consecutive runs: 3/3 pass with
  `mpv_media_audio_load_failed` and no retained runtime.
- Each successful run selected one video track and one external, selected AAC
  audio track; both tracks remained selected after 20 position-checked seeks
  and resume.
- PTS diagnostic samples:
  - run 1: video `1.375`, audio `1.348`, absolute delta `0.027` seconds;
  - run 2: video `1.375`, audio `1.349`, absolute delta `0.026` seconds;
  - run 3: video `1.375`, audio `1.341`, absolute delta `0.034` seconds.
- `pnpm tauri build --debug --no-bundle`: pass.
- Debug executable SHA-256:
  `7be9a18465c4ef76578e16a0c06339a8e53a8b9454f4acda6b9d53600a91ea8c`.
- Tauri GUI auto-open smoke: pass with the documented restricted-session
  `WEBVIEW2_USER_DATA_FOLDER` and `WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS`
  settings (`--disable-gpu`). The page reported the primary and external audio
  URLs as configured, attached the mpv child, and logged successful
  environment-media submission. Closing the host left no
  `doriya-w0-tauri-mpv.exe` or `mpv.exe` process.

The PTS values are diagnostic samples from a three-second synthetic fixture.
They are not a 30-minute drift measurement and must not be used as evidence
for the 100 ms real-media gate.

## Boundaries

The combined opt-in invocation produced 5/6 passes. The unrelated public HTTPS
relay probe failed in that run because the local machine did not establish an
upstream tunnel (`raw=1`, `session=0`); its earlier 3/3 result and the dual
stream result are separate evidence sets. No real Bilibili URL, credential,
30-minute playback, audio/video sync matrix, GPU path, DPI matrix, signed mpv
release or production relay is covered.
