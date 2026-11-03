# WinUI + LibVLCSharp dual-source playback probe (2026-11-03, updated 2026-11-04)

Status: synthetic mechanism evidence only. This does not close W0, S1-S5,
G3, or any real-media, synchronization, DPI, GPU, or 30-minute gate.

## Scope

The WinUI spike now has an automatic playback probe in addition to its
existing GUI smoke. The probe:

- starts two independent `127.0.0.1` fixture servers with exact `Referer` and
  `User-Agent` requirements;
- validates the URLs and shared Header text through the production
  `MediaLoadPlanBuilder`;
- loads `s1-video-only.mp4` as the main media and attaches
  `s1-audio-only.mp4` through `Media.AddSlave(MediaSlaveType.Audio, ...)`;
- requires `Opening -> Playing`, one playable video track and one playable
  external audio track;
- pauses and performs 20 position-checked seeks, rechecking both tracks after
  every seek;
- resumes and requires forward position progress; and
- bounds `MediaPlayer.Stop()` plus both fixture-process cleanup paths.

The probe uses LibVLC `--vout=dummy` and `--aout=dummy`. These options isolate
demux/decode/track/seek wiring and do not validate the WinUI swap chain, an
audio device, rendering quality, or audio/video synchronization.

## Results

- `.\\verify.ps1`: pass, exit code 0.
- Solution build: 0 warnings, 0 errors.
- Existing core xUnit tests: 91 passed, 0 failed.
- Relay tests: 94 passed, 0 failed.
- Dual-source playback probe: 1 passed, 0 failed.
- LibVLC initialization and audio-slave construction probe: pass.
- Focused playback probe: 10/10 consecutive passes on a clean build.
- Each run reported 20/20 distinct seek positions and 250 ms resume progress.
- Last three `MediaPlayer.Stop()` observations: 113 ms, 117 ms, 120 ms.
- No fixture Python process remained after the runs.

During diagnosis, the probe intermittently and then consistently crashed with
`0xc0000005` in `D3D11.DLL_unloaded` after clearing pause. A native exception
checkpoint narrowed the fault to high-frequency `State`/`Time` polling while
LibVLC resumed its video output. Waiting 100 ms after `SetPause(false)` before
querying player properties removed the crash without changing production
playback behavior. The backend also no longer queries LibVLC from its
`Stopped` event callback.

The raw LibVLC track list also exposes selection placeholders. The probe
therefore filters to non-negative playable IDs and requires exactly one video
and one external audio ID, with both selected. The final output labels those
values as `playableVideoTrackIds` and `playableAudioTrackIds`.

## Boundaries

This is a three-second H.264/AAC synthetic mechanism result. It does not prove
real Bilibili dual-stream URLs, per-stream Headers, 30-minute stability,
100 ms audio/video offset, real seek recovery, network switching, DPI/multi-
monitor behavior, hardware decode, actual WinUI composition, audio devices, or
license/distribution readiness. W0 remains open.
