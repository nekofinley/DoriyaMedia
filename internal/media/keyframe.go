package media

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// keyframe.go: where a video stream's keyframes are, near one point.
//
// A session that starts mid-file copies the video stream rather than re-encoding
// it, so it can only begin on a keyframe: ffmpeg rewinds to the nearest one at or
// before the requested position. The audio input has no such constraint and starts
// exactly where it was told, so a start that is not a keyframe leaves the sound a
// whole GOP behind the picture — measured on a 5s-GOP source, asking for 21s began
// the picture at 20.08s and the sound at 21.07s, and that offset never goes away.
//
// Aligning the start to the keyframe is what makes the two agree, so this asks the
// source where they are. It reads only the packets around the requested position
// (-read_intervals), which against a bilibili CDN costs about 0.7s.

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
	if window <= 0 {
		window = 30
	}
	// One retry: -read_intervals against a CDN occasionally comes back empty for a
	// window that has keyframes in it, and a second read costs a fraction of a
	// second next to the wait this alignment saves.
	key, err := p.keyframeRead(ctx, target, headers, proxy, pos, window)
	if err == nil {
		return key, nil
	}
	if ctx.Err() != nil {
		return 0, err
	}
	if key, err2 := p.keyframeRead(ctx, target, headers, proxy, pos, window); err2 == nil {
		return key, nil
	}
	return 0, err
}

// keyframeRead is one ffprobe pass over the window around pos.
func (p *Prober) keyframeRead(ctx context.Context, target string, headers map[string]string, proxy string, pos, window float64) (float64, error) {
	from := pos - window
	if from < 0 {
		from = 0
	}

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		// Read only the packets around the position, not the file's head.
		"-read_intervals", fmt.Sprintf("%.3f%%+%.3f", from, window),
		"-show_entries", "packet=pts_time,flags",
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
		return 0, fmt.Errorf("ffprobe keyframes: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	// Each row is "pts_time,flags" with flags like "K__" for a keyframe.
	best := -1.0
	r := csv.NewReader(strings.NewReader(string(out)))
	r.FieldsPerRecord = -1
	for {
		row, err := r.Read()
		if err != nil {
			break
		}
		if len(row) < 2 || !strings.Contains(row[1], "K") {
			continue
		}
		pts, err := strconv.ParseFloat(strings.TrimSpace(row[0]), 64)
		if err != nil {
			continue
		}
		if pts <= pos+1e-6 && pts > best {
			best = pts
		}
	}
	if best < 0 {
		return 0, fmt.Errorf("ffprobe keyframes: none at or before %.3fs", pos)
	}
	return best, nil
}

// PackStart answers where a session for this file may begin when the viewer is
// coming back at `pos`: the last video keyframe at or before it, or 0 when that
// cannot be determined.
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
	if pos <= 0 || file == nil || drv == nil {
		return 0
	}
	if _, ok := drv.(source.MultiPart); !ok {
		return 0
	}
	inputs, err := e.SegmentInputs(ctx, file, drv, "")
	if err != nil || len(inputs) == 0 {
		log.Printf("keyframe alignment for file %d at %.1fs: no inputs: %v", file.ID, pos, err)
		e.rememberAlignment(fmt.Sprintf("%d|%.3f", file.ID, pos), pos, false)
		return 0
	}
	return e.PackStartFrom(ctx, file, inputs, pos)
}

// PackStartFrom is PackStart for a caller that has already resolved the session's
// streams. It exists so the one expensive part — the ffprobe read — can run in the
// background while the rest of a playback decision is computed: reading it costs
// seconds on a CDN, and the viewer is waiting for a first frame, not for it.
//
// Resolving the streams is the caller's business (and must not happen in another
// goroutine: a source's driver is not documented as safe for concurrent use).
func (e *Engine) PackStartFrom(ctx context.Context, file *models.MediaFile, inputs []Input, pos float64) float64 {
	if pos <= 0 || file == nil || len(inputs) == 0 {
		return 0
	}
	if key, ok := e.alignInputs(ctx, file, inputs, pos); ok {
		return key
	}
	return 0
}

// AlignedStart returns the position a session for this file may start at, which is
// the last video keyframe at or before `pos`, and whether that could be determined.
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
		e.rememberAlignment(fmt.Sprintf("%d|%.3f", file.ID, pos), pos, false)
		return pos, false
	}
	return e.alignInputs(ctx, file, inputs, pos)
}

// alignInputs is the measurement every entry point shares: the cached answer while
// it is fresh, the ffprobe read otherwise, and the cache write either way. It takes
// resolved streams rather than a driver so that a caller can resolve them on the
// request's own goroutine and do the slow part where it hurts least.
func (e *Engine) alignInputs(ctx context.Context, file *models.MediaFile, inputs []Input, pos float64) (float64, bool) {
	cacheKey := fmt.Sprintf("%d|%.3f", file.ID, pos)

	e.keyMu.Lock()
	if ent, ok := e.keyCache[cacheKey]; ok && time.Since(ent.at) < ent.ttl() {
		e.keyMu.Unlock()
		return ent.at1, ent.ok
	}
	e.keyMu.Unlock()

	video := inputs[0]
	key, err := e.prober.KeyframeAtOrBefore(ctx, video.Target, video.Headers, video.Proxy, pos, keyframeSearchWindow)
	if err != nil {
		log.Printf("keyframe alignment for file %d at %.1fs: %v", file.ID, pos, err)
		e.rememberAlignment(cacheKey, pos, false)
		return pos, false
	}
	if key > pos || pos-key > keyframeSearchWindow {
		// Outside what was read: a guess is worse than starting at the beginning.
		log.Printf("keyframe alignment for file %d: %.1fs is %.1fs from the nearest keyframe, too far",
			file.ID, pos, pos-key)
		e.rememberAlignment(cacheKey, pos, false)
		return pos, false
	}

	e.rememberAlignment(cacheKey, key, true)
	return key, true
}

// rememberAlignment caches one answer, success or failure. A failure is remembered
// for a much shorter time: it only has to hold for the other caller of the same
// playback, while a success describes the media and is worth keeping.
func (e *Engine) rememberAlignment(cacheKey string, at1 float64, ok bool) {
	e.keyMu.Lock()
	if len(e.keyCache) > 256 {
		e.keyCache = map[string]keyframeEntry{}
	}
	e.keyCache[cacheKey] = keyframeEntry{at1: at1, ok: ok, at: time.Now()}
	e.keyMu.Unlock()
}

// keyframeEntry is one resolved alignment. The field is named at1 rather than
// "value" to keep it distinct from the timestamp beside it.
type keyframeEntry struct {
	at1 float64
	ok  bool
	at  time.Time
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
// requested position rather than starting the session minutes early.
const keyframeSearchWindow = 30.0
