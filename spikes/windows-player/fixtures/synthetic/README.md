# Synthetic Windows player fixtures

These files are generated local mechanism fixtures, not real-world media
samples. They may validate container/codec wiring, process loading, seek and
logging, but must not be used to close S2 or any real-source acceptance gate.

Generated on 2026-10-28, 2026-11-02 and 2026-11-03 with the locally installed
`ffmpeg 8.0.1`:

| File | Streams | Purpose |
| --- | --- | --- |
| `s1-h264-aac.mp4` | H.264 Constrained Baseline / AAC LC | basic MP4 loading and seek |
| `s1-video-only.mp4` | H.264 Constrained Baseline | video side of a synthetic external-audio pair |
| `s1-audio-only.mp4` | AAC | audio side of a synthetic external-audio pair |
| `s2-h264-eac3.mkv` | H.264 Constrained Baseline / E-AC-3 | synthetic E-AC-3 demux/decode mechanism only |
| `s3-hevc10-flac.mkv` | HEVC Main10 / FLAC | synthetic 10-bit HEVC and FLAC mechanism only |
| `s3-hevc10-flac-ass.mkv` | HEVC Main10 / FLAC / ASS | synthetic embedded ASS track discovery and selection |

Reproduction commands:

```powershell
ffmpeg -f lavfi -i "color=c=0x223344:s=320x180:r=24:d=3" `
  -f lavfi -i "sine=frequency=440:sample_rate=48000:duration=3" `
  -c:v libx264 -preset ultrafast -profile:v baseline -level 4.0 `
  -pix_fmt yuv420p -g 24 -c:a aac -b:a 96k `
  -movflags +faststart s1-h264-aac.mp4

ffmpeg -i s1-h264-aac.mp4 -map 0:v:0 -c copy `
  -movflags +faststart s1-video-only.mp4

ffmpeg -i s1-h264-aac.mp4 -map 0:a:0 -c copy s1-audio-only.mp4

ffmpeg -f lavfi -i "color=c=0x334455:s=320x180:r=24:d=3" `
  -f lavfi -i "sine=frequency=550:sample_rate=48000:duration=3" `
  -c:v libx264 -preset ultrafast -profile:v baseline -level 4.0 `
  -pix_fmt yuv420p -g 24 -c:a eac3 -b:a 192k s2-h264-eac3.mkv

ffmpeg -f lavfi -i "color=c=0x445566:s=320x180:r=24:d=3,format=yuv420p10le" `
  -f lavfi -i "sine=frequency=660:sample_rate=48000:duration=3" `
  -c:v libx265 -preset ultrafast -x265-params "log-level=error" `
  -pix_fmt yuv420p10le -g 24 -c:a flac s3-hevc10-flac.mkv

ffmpeg -i s3-hevc10-flac.mkv -i s3-probe.ass `
  -map 0 -map 1 -c:v copy -c:a copy -c:s ass `
  -metadata:s:s:0 language=chi -metadata:s:s:0 title="ASS probe" `
  s3-hevc10-flac-ass.mkv
```

The real S2 E-AC-3 sample, Bilibili dual-stream URLs/Headers, subtitles,
30-minute stability and the Windows device/DPI/GPU matrix remain outstanding.
The ASS fixture only proves embedded-track discovery and selection; it does not
prove production subtitle rendering quality or real-sample compatibility.

Validate the fixtures in this directory with:

```powershell
.\check-fixtures.ps1
```

The script verifies each file's fixed SHA-256 and its exact stream count, order,
codec type/name, profile, and video pixel format with `ffprobe`. It then opens
each file through the locally installed mpv with null video/audio output. Use
`-SkipMpv` when only fixture identity and metadata should be checked.
