package media

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// keyframe.go: where a video stream's keyframes are, near one point.
//
// A session that starts mid-file copies the video stream rather than re-encoding
// it, so it can only begin on a keyframe. Input-stage `-ss` is not enough here:
// even when asked for a keyframe's PTS, ffmpeg's copy seek can start at the
// previous keyframe while the independent audio input starts at the requested
// time. Asking for 50.042s on a 5s-GOP bilibili source was measured starting the
// picture at 45.038s and the sound at 50.042s, a permanent five-second split.
//
// The reliable shape is a coarse input seek to the preceding keyframe followed by
// an output-stage seek for the timestamp distance between the two keyframes. That
// makes ffmpeg choose the intended keyframe while both inputs are being muxed, so
// the picture and sound cross the cut together. Keyframes are ordered by PTS for
// selection, but seek positions come from DTS: input-stage seeking operates on
// decode timestamps.

// PackWindow is the pair of coarse and precise seeks used to start a two-input
// packaging session on one target keyframe.
type PackWindow struct {
	// StartSeconds is the target keyframe's DTS and becomes the stream's absolute
	// timeline origin. It is what /api/play reports as stream_start.
	StartSeconds float64
	// InputSeekSeconds is the preceding keyframe's DTS. Both inputs seek here,
	// then ffmpeg applies StartSeconds-InputSeekSeconds at the output stage.
	InputSeekSeconds float64
}

type videoKeyframe struct {
	pts float64
	dts float64
}

type keyframeWindow struct {
	start PackWindow
	// pts identifies the target frame in display order, which is what the caller
	// requested. It is kept for the existing public probe and its tests.
	pts float64
}

// KeyframeAtOrBefore returns the position of the last video keyframe at or before
// `pos`, in seconds, or an error when none could be read.
//
// `window` is how far back to look. It must cover at least one GOP; a source whose
// GOP is longer than the window answers with the keyframe at the window's start
// (or nothing), and the caller keeps its unaligned start.
func (p *Prober) KeyframeAtOrBefore(ctx context.Context, target string, headers map[string]string, proxy string, pos, window float64) (float64, error) {
	if pos <= 0 {
		return 0, nil
	}
	found, err := p.keyframeWindowAtOrBefore(ctx, target, headers, proxy, pos, window)
	if err != nil {
		return 0, err
	}
	return found.pts, nil
}

// keyframeWindowAtOrBefore returns the last keyframe at or before pos and the
// keyframe before it. The latter gives the copy seek a safe coarse starting point;
// the distance between their DTS values is then applied at the output stage.
func (p *Prober) keyframeWindowAtOrBefore(ctx context.Context, target string, headers map[string]string, proxy string, pos, window float64) (keyframeWindow, error) {
	if window <= 0 {
		window = 30
	}
	// One retry: -read_intervals against a CDN occasionally comes back empty for a
	// window that has keyframes in it, and a second read costs a fraction of a
	// second next to the wait this alignment saves.
	found, err := p.keyframeRead(ctx, target, headers, proxy, pos, window)
	if err == nil {
		return found, nil
	}
	if ctx.Err() != nil {
		return keyframeWindow{}, err
	}
	if found, err2 := p.keyframeRead(ctx, target, headers, proxy, pos, window); err2 == nil {
		return found, nil
	}
	return keyframeWindow{}, err
}

// keyframeRead is one ffprobe pass over the window around pos.
func (p *Prober) keyframeRead(ctx context.Context, target string, headers map[string]string, proxy string, pos, window float64) (keyframeWindow, error) {
	from := pos - window
	if from < 0 {
		from = 0
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		// Read only the packets around the position, not the file's head. The
		// extra second includes a keyframe exactly at pos; ffprobe's end bound is
		// otherwise exclusive and selecting it would silently fall back one GOP.
		"-read_intervals", fmt.Sprintf("%.3f%%+%.3f", from, window+1),
		"-show_entries", "packet=pts_time,dts_time,flags",
		"-of", "csv=p=0",
	}
	if len(headers) > 0 {
		var b strings.Builder
		for k, v := range headers {
			b.WriteString(k)
			b.WriteString(": ")
			b.WriteString(v)
			b.WriteString("\r\n")
		}
		args = append(args, "-headers", b.String())
	}
	args = ProxyArgs(args, proxy)
	args = append(args, target)

	cmd := exec.CommandContext(cctx, p.FFProbe, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return keyframeWindow{}, fmt.Errorf("ffprobe keyframes: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	// Each row is "pts_time,dts_time,flags" with flags like "K__" for a keyframe.
	var frames []videoKeyframe
	r := csv.NewReader(strings.NewReader(string(out)))
	r.FieldsPerRecord = -1
	for {
		row, err := r.Read()
		if err != nil {
			break
		}
		if len(row) < 3 || !strings.Contains(row[2], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(strings.TrimSpace(row[0]), 64)
		if err != nil {
			continue
		}
		dts, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64)
		if err != nil {
			// Some formats expose only PTS. It is identical to DTS for the
			// all-intra/audio cases where this fallback is needed.
			dts = pts
		}
		frames = append(frames, videoKeyframe{pts: pts, dts: dts})
	}
	sort.SliceStable(frames, func(i, j int) bool { return frames[i].pts < frames[j].pts })

	last := -1
	for i, frame := range frames {
		if frame.pts <= pos+1e-6 {
			last = i
		}
	}
	if last < 0 {
		return keyframeWindow{}, fmt.Errorf("ffprobe keyframes: none at or before %.3fs", pos)
	}
	if last == 0 {
		return keyframeWindow{}, fmt.Errorf("ffprobe keyframes: no keyframe before %.3fs", frames[last].pts)
	}
	prev, current := frames[last-1], frames[last]
	if current.dts <= prev.dts {
		return keyframeWindow{}, fmt.Errorf("ffprobe keyframes: non-increasing DTS %.3f -> %.3f", prev.dts, current.dts)
	}
	return keyframeWindow{
		start: PackWindow{StartSeconds: current.dts, InputSeekSeconds: prev.dts},
		pts:   current.pts,
	}, nil
}

// PackStart answers where a session for this file may begin when the viewer is
// coming back at `pos`: the DTS of the last video keyframe at or before it, or 0
// when that cannot be determined. Callers that start ffmpeg should use PackWindowAt
// instead: the target DTS alone is not enough to make a copied video seek land on
// that keyframe alongside an independent audio input.
//
// Both halves of a playback call this — /api/play reports it as stream_start, and
// the HLS handler starts its packaging session there — and they must reach the
// same answer. They are two separate requests, so calling one function is not
// enough by itself: the alignment is cached, failures included, so the second
// caller reads the first one's answer instead of re-probing a CDN that may have
// hiccuped in between.
//
// 0 rather than `pos` on failure is deliberate. A session that begins between
// keyframes plays with its sound a GOP away from its picture for the whole
// playback, while starting at the beginning only costs the wait this path exists
// to avoid.
func (e *Engine) PackStart(ctx context.Context, file *models.MediaFile, drv source.Driver, pos float64) float64 {
	return e.PackWindowAt(ctx, file, drv, pos).StartSeconds
}

// PackWindowAt is PackStart with both seek positions. The public stream origin is
// the target keyframe's DTS; the input seek starts at the preceding keyframe so
// the precise output seek can land both independent inputs on the target frame.
func (e *Engine) PackWindowAt(ctx context.Context, file *models.MediaFile, drv source.Driver, pos float64) PackWindow {
	if pos <= 0 || file == nil || drv == nil {
		return PackWindow{}
	}
	if _, ok := drv.(source.MultiPart); !ok {
		return PackWindow{}
	}
	inputs, err := e.SegmentInputs(ctx, file, drv, "")
	if err != nil || len(inputs) == 0 {
		log.Printf("keyframe alignment for file %d at %.1fs: no inputs: %v", file.ID, pos, err)
		e.rememberAlignment(fmt.Sprintf("%d|%.3f", file.ID, pos), PackWindow{}, false)
		return PackWindow{}
	}
	return e.PackWindowFrom(ctx, file, inputs, pos)
}

// PackStartFrom is PackStart for a caller that has already resolved the session's
// streams. It exists so the one expensive part — the ffprobe read — can run in the
// background while the rest of a playback decision is computed: reading it costs
// seconds on a CDN, and the viewer is waiting for a first frame, not for it.
//
// Resolving the streams is the caller's business (and must not happen in another
// goroutine: a source's driver is not documented as safe for concurrent use).
func (e *Engine) PackStartFrom(ctx context.Context, file *models.MediaFile, inputs []Input, pos float64) float64 {
	return e.PackWindowFrom(ctx, file, inputs, pos).StartSeconds
}

// PackWindowFrom is PackStartFrom with the coarse input seek included.
func (e *Engine) PackWindowFrom(ctx context.Context, file *models.MediaFile, inputs []Input, pos float64) PackWindow {
	if pos <= 0 || file == nil || len(inputs) == 0 {
		return PackWindow{}
	}
	if window, ok := e.alignInputs(ctx, file, inputs, pos); ok {
		return window
	}
	return PackWindow{}
}

// AlignedStart returns the position a session for this file may start at, which is
// the DTS of the last video keyframe at or before `pos`, and whether that could be
// determined.
//
// A source whose media is one file hands ffmpeg both streams from one input, so a
// start between keyframes only makes the first frames late — the sound and the
// picture still agree. A two-stream source (bilibili DASH) is the case that needs
// the alignment: its picture and its sound are read from separate inputs, and a
// start that is not a keyframe puts the sound a GOP ahead of the picture (see the
// note at the top of this file).
//
// ok is false when the keyframe could not be read. The caller must then start at the
// beginning rather than at `pos`: a session that begins between keyframes plays with
// its sound a GOP out, and that is worse than packaging from zero. PackStart is the
// wrapper that turns this into that answer.
//
// Results are cached per file and position, failures included. The cache is
// deliberately not keyed by the resolved stream URL: the source mints a new signed
// link on every resolution, so a URL-keyed cache never hits — and, worse, an entry
// left by one resolution answers for another.
func (e *Engine) AlignedStart(ctx context.Context, file *models.MediaFile, drv source.Driver, pos float64) (float64, bool) {
	if pos <= 0 || file == nil || drv == nil {
		return pos, false
	}
	if _, ok := drv.(source.MultiPart); !ok {
		return pos, false
	}
	inputs, err := e.SegmentInputs(ctx, file, drv, "")
	if err != nil || len(inputs) == 0 {
		log.Printf("keyframe alignment for file %d at %.1fs: no inputs: %v", file.ID, pos, err)
		e.rememberAlignment(fmt.Sprintf("%d|%.3f", file.ID, pos), PackWindow{}, false)
		return pos, false
	}
	window, ok := e.alignInputs(ctx, file, inputs, pos)
	return window.StartSeconds, ok
}

// alignInputs is the measurement every entry point shares: the cached answer while
// it is fresh, the ffprobe read otherwise, and the cache write either way. It takes
// resolved streams rather than a driver so that a caller can resolve them on the
// request's own goroutine and do the slow part where it hurts least.
func (e *Engine) alignInputs(ctx context.Context, file *models.MediaFile, inputs []Input, pos float64) (PackWindow, bool) {
	cacheKey := fmt.Sprintf("%d|%.3f", file.ID, pos)

	e.keyMu.Lock()
	if ent, ok := e.keyCache[cacheKey]; ok && time.Since(ent.at) < ent.ttl() {
		e.keyMu.Unlock()
		return ent.window, ent.ok
	}
	e.keyMu.Unlock()

	video := inputs[0]
	found, err := e.prober.keyframeWindowAtOrBefore(ctx, video.Target, video.Headers, video.Proxy, pos, keyframeSearchWindow)
	if err != nil {
		log.Printf("keyframe alignment for file %d at %.1fs: %v", file.ID, pos, err)
		e.rememberAlignment(cacheKey, PackWindow{}, false)
		return PackWindow{}, false
	}
	if found.pts > pos || pos-found.pts > keyframeSearchWindow {
		// Outside what was read: a guess is worse than starting at the beginning.
		log.Printf("keyframe alignment for file %d: %.1fs is %.1fs from the nearest keyframe, too far",
			file.ID, pos, pos-found.pts)
		e.rememberAlignment(cacheKey, PackWindow{}, false)
		return PackWindow{}, false
	}
	window := found.start
	if window.StartSeconds < 0 || window.InputSeekSeconds < 0 ||
		window.StartSeconds <= window.InputSeekSeconds {
		log.Printf("keyframe alignment for file %d at %.1fs: invalid seek window %.3f -> %.3f",
			file.ID, pos, window.InputSeekSeconds, window.StartSeconds)
		e.rememberAlignment(cacheKey, PackWindow{}, false)
		return PackWindow{}, false
	}

	e.rememberAlignment(cacheKey, window, true)
	return window, true
}

// rememberAlignment caches one answer, success or failure. A failure is remembered
// for a much shorter time: it only has to hold for the other caller of the same
// playback, while a success describes the media and is worth keeping.
func (e *Engine) rememberAlignment(cacheKey string, window PackWindow, ok bool) {
	e.keyMu.Lock()
	if len(e.keyCache) > 256 {
		e.keyCache = map[string]keyframeEntry{}
	}
	e.keyCache[cacheKey] = keyframeEntry{window: window, ok: ok, at: time.Now()}
	e.keyMu.Unlock()
}

// keyframeEntry is one resolved alignment.
type keyframeEntry struct {
	window PackWindow
	ok     bool
	at     time.Time
}

// ttl is how long this answer may be reused. A successful alignment is a property
// of the media, while a failure is only trusted long enough for the request that
// follows the one that failed: a caller allowed to retry it could start packaging
// at a keyframe the player was never told about, and every position the player
// computes would then be off by that distance.
func (k keyframeEntry) ttl() time.Duration {
	if k.ok {
		return keyframeTTL
	}
	return keyframeFailTTL
}

// keyframeTTL bounds how long one successful alignment is reused. It is keyed by
// the file and the requested position, so a new resume point is measured afresh;
// the source's signed links rotate every few minutes, and the answer does not
// depend on which one was used.
const keyframeTTL = 10 * time.Minute

// keyframeFailTTL is how long a failed alignment is reused. /api/play and the
// playlist request that follows it are seconds apart, and both have to answer the
// same thing (see PackStart).
const keyframeFailTTL = time.Minute

// keyframeSearchWindow is how far back the alignment looks for a keyframe. Bilibili
// publishes 5s GOPs, so this covers several of them; a longer GOP falls back to the
// beginning rather than starting the session minutes early.
const keyframeSearchWindow = 30.0
