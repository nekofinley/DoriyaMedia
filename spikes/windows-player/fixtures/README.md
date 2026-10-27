# Windows player fixtures

These fixtures are local mechanism inputs only. They do not represent real
Bilibili/OpenList media and cannot close S1-S5 acceptance gates.

## Synthetic codecs

`synthetic/` contains small H.264/AAC, H.264/E-AC-3 and HEVC Main10/FLAC
files. Run `synthetic/check-fixtures.ps1` for codec metadata and mpv null-output
decode checks. The E-AC-3 file is synthetic; the real S2 sample is still
required.

## Header and Range server

`header-range-server.py` binds only `127.0.0.1`, uses a random token path, and
requires exact `Referer` and `User-Agent` values before serving media. It
supports HEAD, full GET, single-range 206 and 416 responses.

```powershell
python .\header-range-server.py --file .\synthetic\s1-h264-aac.mp4
```

The process prints one JSON line containing the URL and required Header values.
Request logging is disabled so media URLs and Header values are not persisted.
This is a test fixture, not a production relay.

Run the end-to-end fixture check with:

```powershell
.\check-header-range.ps1
```

It validates missing-Header 403, HEAD 200, 100-byte Range 206, unsatisfied
Range 416, and mpv decoding through the loopback HTTP server with the required
Referer/User-Agent.
