package media

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// Engine bundles probe/ffmpeg tooling with the store for probe caching.
type Engine struct {
	Store  *db.Store
	Cfg    *config.Config
	prober *Prober
	ffmpeg *FFMpeg

	subMu    sync.Mutex
	subCache map[string][]byte
	subOrder []string

	resolveMu    sync.Mutex
	resolveCache map[string]resolveEntry
	// resolveLocks serialises link resolution per file (see resolveTarget).
	resolveLocks map[string]*sync.Mutex

	// partsCache holds resolved multi-stream inputs (see parts.go). Same TTL and
	// the same per-file lock as the single-target cache above.
	partsMu    sync.Mutex
	partsCache map[string]partsEntry

	// keyCache holds resolved keyframe alignments for sources whose sessions may
	// start mid-file (see keyframe.go). Keyed by stream URL and position, so it
	// cannot outlive the link it was measured against.
	keyMu    sync.Mutex
	keyCache map[string]keyframeEntry

	// probe job registry: one job per file, shared by the async path
	// (/api/play → polling) and the synchronous fallback (HLS/subtitles).
	probeMu   sync.Mutex
	probeJobs map[int64]*probeJob

	// background pre-probe queue (cache warming after a scan)
	pre preProbeQueue

	// per-file segment generation counters (see progress.go)
	streamMu sync.Mutex
	stream   map[int64]*streamProgress

	// packaging sessions for HEVC copy to fMP4 (see session.go)
	sessMu   sync.Mutex
	sessions map[sessionKey]*Session
	// startMu serialises session creation (see EnsureSession). One mutex, not
	// one per key: a packager is started once per playback, so serialising the
	// spawn costs nothing, and a per-key map would have to be grown and pruned
	// alongside the sessions it guards.
	startMu sync.Mutex

	// execProbe runs one ffprobe pass with a given window. Indirection so tests
	// can drive the probe lifecycle without the ffprobe binary.
	execProbe func(ctx context.Context, p *Prober, target string, headers map[string]string, proxy string) (*Probe, error)

	// runStream runs one ffmpeg command. Indirection so tests can drive segment
	// generation (including source failures) without the ffmpeg binary.
	runStream func(ctx context.Context, args []string, out io.Writer) error
}

type resolveEntry struct {
	target  string
	headers map[string]string
	at      time.Time
}

func NewEngine(store *db.Store, cfg *config.Config) *Engine {
	prober := NewProber(cfg.FFProbe)
	if cfg.Playback.ProbeSizeMB > 0 {
		prober.ProbeSizeMB = cfg.Playback.ProbeSizeMB
	}
	if cfg.Playback.ProbeAnalyzeSec > 0 {
		prober.AnalyzeSec = cfg.Playback.ProbeAnalyzeSec
	}
	e := &Engine{
		Store:        store,
		Cfg:          cfg,
		prober:       prober,
		ffmpeg:       &FFMpeg{Path: cfg.FFmpeg},
		subCache:     map[string][]byte{},
		resolveCache: map[string]resolveEntry{},
		resolveLocks: map[string]*sync.Mutex{},
		partsCache:   map[string]partsEntry{},
		keyCache:     map[string]keyframeEntry{},
		probeJobs:    map[int64]*probeJob{},
		pre:          preProbeQueue{queued: map[int64]bool{}},
		stream:       map[int64]*streamProgress{},
		sessions:     map[sessionKey]*Session{},
		execProbe: func(ctx context.Context, p *Prober, target string, headers map[string]string, proxy string) (*Probe, error) {
			return p.ProbeFileVia(ctx, target, headers, proxy)
		},
	}
	// Resolved through e so a hot-reloaded ffmpeg path (SetBins) is picked up.
	e.runStream = func(ctx context.Context, args []string, out io.Writer) error {
		return e.ffmpeg.RunStream(ctx, args, out)
	}
	return e
}

// SetBins updates tool paths after config changes (hot reload).
func (e *Engine) SetBins(ffmpeg, ffprobe string) {
	prober := NewProber(ffprobe)
	if e.Cfg != nil {
		if e.Cfg.Playback.ProbeSizeMB > 0 {
			prober.ProbeSizeMB = e.Cfg.Playback.ProbeSizeMB
		}
		if e.Cfg.Playback.ProbeAnalyzeSec > 0 {
			prober.AnalyzeSec = e.Cfg.Playback.ProbeAnalyzeSec
		}
	}
	e.prober = prober
	e.ffmpeg = &FFMpeg{Path: ffmpeg}
}

// resolveTTL is how long one minted source link is reused before a new one is
// requested. Cloud-drive links (OpenList / 天翼云盘) are signed and long-lived
// (~hours), but they can still be refused early — see resolveTarget.
const resolveTTL = 10 * time.Minute

func resolveKey(file *models.MediaFile) string {
	return fmt.Sprintf("%d|%s", file.SourceID, file.Path)
}

// ProbeTarget resolves a file into an ffprobe-able target (cached, TTL resolveTTL).
func (e *Engine) ProbeTarget(ctx context.Context, file *models.MediaFile, drv source.Driver) (string, map[string]string, error) {
	return e.resolveTarget(ctx, file, drv, "")
}

// resolveTarget returns a usable source URL for a file.
//
// rejected is the target the caller just saw the source refuse: the cache entry
// is then dropped and a new link minted. Signed cloud links can be refused while
// still nominally valid (rotated session, per-link use limit, account
// concurrency), and reusing a dead link forever is what kills playback
// mid-stream.
//
// Resolution for one file is serialised and re-checked after taking the lock, so
// a burst of failing segment requests mints one fresh link between them instead
// of one each — the drive may invalidate the previous link whenever a new one is
// issued, so a stampede would make things worse.
func (e *Engine) resolveTarget(ctx context.Context, file *models.MediaFile, drv source.Driver, rejected string) (string, map[string]string, error) {
	key := resolveKey(file)
	mu := e.resolveLock(key)
	mu.Lock()
	defer mu.Unlock()

	e.resolveMu.Lock()
	if ent, ok := e.resolveCache[key]; ok {
		switch {
		case rejected == "" && time.Since(ent.at) < resolveTTL:
			e.resolveMu.Unlock()
			return ent.target, ent.headers, nil
		case rejected != "" && ent.target != rejected:
			// Somebody already refreshed this link while we waited for the lock.
			e.resolveMu.Unlock()
			return ent.target, ent.headers, nil
		}
		delete(e.resolveCache, key)
	}
	e.resolveMu.Unlock()

	if drv == nil {
		var err error
		drv, err = e.DriverFor(file)
		if err != nil {
			return "", nil, err
		}
	}
	target, headers, err := drv.ProbeURL(ctx, file.Path)
	if err != nil {
		return "", nil, err
	}
	e.resolveMu.Lock()
	if len(e.resolveCache) > 512 {
		e.resolveCache = map[string]resolveEntry{}
	}
	e.resolveCache[key] = resolveEntry{target: target, headers: headers, at: time.Now()}
	e.resolveMu.Unlock()
	return target, headers, nil
}

// resolveLock returns the mutex guarding one file's link resolution.
func (e *Engine) resolveLock(key string) *sync.Mutex {
	e.resolveMu.Lock()
	defer e.resolveMu.Unlock()
	if mu, ok := e.resolveLocks[key]; ok {
		return mu
	}
	if len(e.resolveLocks) > 1024 {
		e.resolveLocks = map[string]*sync.Mutex{}
	}
	mu := &sync.Mutex{}
	e.resolveLocks[key] = mu
	return mu
}

// DriverFor resolves the right driver for a file (URL files use URLDriver).
func (e *Engine) DriverFor(file *models.MediaFile) (source.Driver, error) {
	if isHTTPPath(file.Path) {
		return &source.URLDriver{}, nil
	}
	src, err := e.Store.SourceByID(file.SourceID)
	if err != nil {
		return nil, err
	}
	return source.NewDriver(src)
}

func isHTTPPath(p string) bool {
	return strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://")
}

// EnsureProbe lives in probe_tasks.go: it joins an in-flight background probe
// when one exists, so the async (/api/play) and synchronous (HLS/subtitles)
// paths never probe the same file twice.

func (e *Engine) RegisterEmbeddedSubs(file *models.MediaFile, probe *Probe) {
	for i, s := range probe.SubtitleStreams() {
		conv := false
		switch s.CodecName {
		case "subrip", "ass", "ssa", "mov_text", "webvtt", "text":
			conv = true
		}
		sub := &models.Subtitle{
			FileID:      file.ID,
			Lang:        s.Language(),
			Title:       s.Title(),
			Format:      s.CodecName,
			Embedded:    true,
			StreamIdx:   s.Index,
			Convertible: conv,
		}
		_ = e.Store.UpsertSubtitle(sub)
		_ = i
	}
}

// ---------- HLS segment generation ----------

// countWriter counts how many bytes reached the client, so a failed ffmpeg run
// can tell "produced nothing" (safe to retry) from "already partly sent".
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Segment writes one MPEG-TS HLS segment for the file and reports how many bytes
// it delivered.
//
// When ffmpeg fails without emitting anything, the source link itself is the
// likely culprit: signed cloud links get refused while still nominally valid
// (rotated session, per-link use limit, account concurrency). Every segment
// opens its own connection with the cached link, so one dead link would
// otherwise break the rest of the stream — a fresh link is minted and the
// segment retried once.
//
// start: seconds into the media; length: nominal segment length.
// videoMode: remux (copy) | transcode (h264). audioMode: copy | aac.
// audioOrdinal: which audio stream (ordinal among audio streams, -1 = none).
// videoIndex: absolute ffmpeg stream index of the real video stream
// (cover-art attachments excluded by the prober); -1 falls back to 0:v:0.
//
// A source that publishes the picture and the sound as separate streams
// (bilibili DASH) is read as one input per stream instead; the audio/video
// stream arguments above then do not apply, because each input carries exactly
// the stream it was resolved for.
func (e *Engine) Segment(ctx context.Context, file *models.MediaFile, drv source.Driver,
	start, length float64, videoMode, audioMode string, audioOrdinal, videoIndex, transcodeHeight int, out io.Writer) (n int64, err error) {

	e.noteSegmentStart(file.ID)
	began := time.Now()
	defer func() { e.noteSegmentDone(file.ID, length, time.Since(began), n, err) }()

	inputs, err := e.SegmentInputs(ctx, file, drv, "")
	if err != nil {
		return 0, err
	}

	// The segment is collected before it is sent, so a failed attempt leaves the
	// response untouched (see segBuf). That is what makes the failure retryable:
	// a truncated fMP4 reaching the player looks exactly like a decoding error, and
	// players do not retry those.
	buf := &segBuf{}
	defer buf.Close()

	cw := &countWriter{w: buf}
	runErr := e.runStream(ctx, segmentArgs(inputs, start, length, videoMode, audioMode, audioOrdinal, videoIndex, transcodeHeight), cw)
	if runErr == nil {
		return buf.WriteTo(out)
	}
	// Nothing has reached the client, so a cancelled request must not start more
	// work against the source — but a failed one can still be retried.
	if ctx.Err() != nil {
		return 0, runErr
	}

	retryInputs, rerr := e.SegmentInputs(ctx, file, drv, inputs[0].Target)
	if rerr != nil || sameInputs(retryInputs, inputs) {
		return 0, runErr
	}
	log.Printf("hls segment %d @%.0fs: source refused the link, retrying with a fresh one: %v",
		file.ID, start, runErr)

	buf.Reset()
	cw2 := &countWriter{w: buf}
	if err2 := e.runStream(ctx, segmentArgs(retryInputs, start, length, videoMode, audioMode, audioOrdinal, videoIndex, transcodeHeight), cw2); err2 != nil {
		return 0, fmt.Errorf("attempt 1: %w; attempt 2 with a fresh source link: %v", runErr, err2)
	}
	log.Printf("hls segment %d @%.0fs: recovered with a fresh source link", file.ID, start)
	return buf.WriteTo(out)
}

// segmentArgs builds the ffmpeg command line for one segment.
//
// inputs is what the segment reads: one entry for an ordinary file, two for a
// source that publishes the picture and the sound separately (bilibili DASH).
// The single-input form maps the video by absolute stream index, because 0:v:0
// would pick an attached cover image when one is present; with one input per
// stream each one carries exactly the stream it was resolved for, so the mapping
// is by input instead.
func segmentArgs(inputs []Input, start, length float64,
	videoMode, audioMode string, audioOrdinal, videoIndex, transcodeHeight int) []string {

	multi := len(inputs) > 1
	args := []string{"-hide_banner", "-loglevel", "error"}
	for _, in := range inputs {
		args = HeaderArgs(args, in.Headers)
		args = ProxyArgs(args, in.Proxy)
		if isHTTPPath(in.Target) {
			// reconnect helps mid-segment CDN hiccups for http sources
			args = append(args, "-reconnect", "1", "-reconnect_streamed", "1")
		}
		// Every input is seeked: the parts of a DASH source are two independent
		// files, and seeking only one of them would put the sound out of step.
		args = append(args, "-ss", fmt.Sprintf("%.3f", start), "-i", in.Target)
	}

	switch {
	case multi:
		args = append(args, "-map", "0:v:0", "-map", "1:a:0")
	case videoIndex >= 0:
		args = append(args, "-map", fmt.Sprintf("0:%d", videoIndex))
	default:
		args = append(args, "-map", "0:v:0")
	}
	if !multi {
		if audioOrdinal >= 0 {
			args = append(args, "-map", fmt.Sprintf("0:a:%d", audioOrdinal))
		} else {
			args = append(args, "-an")
		}
	}

	if videoMode == "transcode" {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "21")
		// 10-bit sources (HEVC Main10 / Hi10P) must be converted to 8-bit
		// yuv420p — MSE hardware paths cannot handle anything else.
		args = append(args, "-pix_fmt", "yuv420p")
		args = append(args, "-vf", fmt.Sprintf("scale=-2:min(ih\\,%d)", transcodeHeight))
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	} else {
		args = append(args, "-c:v", "copy")
		if audioMode == "aac" {
			args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
		} else {
			args = append(args, "-c:a", "copy")
		}
	}
	args = append(args,
		"-avoid_negative_ts", "make_zero",
		"-f", "mpegts",
		"-t", fmt.Sprintf("%.3f", length),
		"pipe:1",
	)
	return args
}

// Playlist renders the VOD HLS playlist for a file.
func (e *Engine) Playlist(basePath string, sigQuery string, duration, segLen float64) string {
	if segLen <= 0 {
		segLen = 4
	}
	if duration <= 0 {
		duration = segLen
	}
	n := int(duration/segLen + 0.999999)
	if n < 1 {
		n = 1
	}
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int(segLen+0.999))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString("#EXT-X-INDEPENDENT-SEGMENTS\n")
	for i := 0; i < n; i++ {
		d := segLen
		if i == n-1 {
			d = duration - float64(i)*segLen
			if d <= 0 {
				d = segLen
			}
		}
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n", d)
		fmt.Fprintf(&b, "%s/%d%s\n", basePath, i, sigQuery)
	}
	b.WriteString("#EXT-X-ENDLIST\n")
	return b.String()
}

// ---------- subtitles ----------

// ExtractEmbeddedSubtitle pulls one embedded text subtitle as WebVTT.
func (e *Engine) ExtractEmbeddedSubtitle(ctx context.Context, file *models.MediaFile, drv source.Driver, streamIndex int) ([]byte, error) {
	// The key carries a converter version: the output changed with the converter,
	// and a cached entry would keep serving the flattened document.
	key := fmt.Sprintf("%d|%d|v2", file.ID, streamIndex)
	e.subMu.Lock()
	if b, ok := e.subCache[key]; ok {
		e.subMu.Unlock()
		return b, nil
	}
	e.subMu.Unlock()

	target, headers, err := e.ProbeTarget(ctx, file, drv)
	if err != nil {
		return nil, err
	}
	// ordinal of the stream among subtitle streams
	probe, err := e.EnsureProbe(ctx, file, drv)
	if err != nil {
		return nil, err
	}
	ordinal := -1
	for i, s := range probe.SubtitleStreams() {
		if s.Index == streamIndex {
			ordinal = i
			break
		}
	}
	if ordinal < 0 {
		return nil, fmt.Errorf("subtitle stream not found")
	}

	// Ask ffmpeg for ASS rather than WebVTT: its ASS writer keeps the container's
	// styles, alignment and positioning, and ASSToVTT carries those into the
	// WebVTT the player wants. ffmpeg's own WebVTT muxer flattens both away.
	var buf strings.Builder
	args := []string{"-hide_banner", "-loglevel", "error"}
	args = HeaderArgs(args, headers)
	args = append(args, "-i", target, "-map", fmt.Sprintf("0:s:%d", ordinal), "-f", "ass", "pipe:1")
	if err := e.ffmpeg.RunStream(ctx, args, writerFunc(buf.WriteString)); err != nil {
		return nil, err
	}
	data := ASSToVTT([]byte(buf.String()))
	e.subMu.Lock()
	if len(e.subOrder) >= 64 {
		delete(e.subCache, e.subOrder[0])
		e.subOrder = e.subOrder[1:]
	}
	e.subCache[key] = data
	e.subOrder = append(e.subOrder, key)
	e.subMu.Unlock()
	return data, nil
}

func writerFunc(fn func(string) (int, error)) io.Writer {
	return writerFn{fn}
}

type writerFn struct {
	fn func(string) (int, error)
}

func (w writerFn) Write(p []byte) (int, error) {
	return w.fn(string(p))
}
