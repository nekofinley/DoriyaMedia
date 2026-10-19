package media

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// session.go: per-playback packaging sessions (P2, phase 1).
//
// A session is one long-lived ffmpeg (see packager.go) packaging a file into
// <data>/cache/hls/<key>/. It is created on the first HLS request for a given
// file + parameters and torn down once idle, which also removes its directory —
// so disk use is bounded by the sessions actually in flight, not by the library.
//
// Only HEVC *copy* for Apple is routed here: that is the case that cannot work
// as MPEG-TS (Apple does not decode HEVC from TS) and needs fMP4 with a
// continuous timeline. Everything else keeps using the on-demand TS segmenter,
// which needs no temporary disk at all.

const (
	// sessionIdleTTL: a session with no requests for this long is closed and its
	// directory removed.
	sessionIdleTTL = 90 * time.Second
	// sessionFileWait bounds how long one HTTP request waits for a segment that
	// ffmpeg has not produced yet.
	sessionFileWait = 45 * time.Second
	// SessionSeekAhead is how far past what has already been packaged a request
	// may be before it is treated as a seek rather than as buffering. Below it,
	// waiting is right: a player asking for the next segment of a slow transcode
	// is buffering, and restarting the packager for that would thrash. Above it,
	// the player has jumped somewhere the running packager will not reach soon.
	SessionSeekAhead = 12
	// SessionActiveFor is how recently a session must have served a segment to
	// count as a playback in progress. A playlist request while one is active has
	// to follow it (a seek moved it); a playlist request when nothing has been
	// consumed for this long starts a new playback, and its position wins.
	SessionActiveFor = 15 * time.Second
)

var reSessionSegment = regexp.MustCompile(`^seg\d+\.m4s$`)

// ParseSessionSegment extracts the index from a "segNNNNN.m4s" resource name
// (the naming ffmpeg's HLS muxer is given in packager.go).
func ParseSessionSegment(rest string) (int, bool) {
	if !reSessionSegment.MatchString(rest) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "seg"), ".m4s"))
	if err != nil {
		return 0, false
	}
	return n, true
}

type sessionKey struct {
	fileID int64
	mode   string
	audio  int
	vindex int
	height int
	// start is the session's first segment number, and part of its identity: a
	// different start is a different packaging job, so seeking backwards starts a
	// fresh session instead of reusing one that is already past that point.
	start int
	// variant names the source stream the output was packaged from, for a source
	// whose ladder can change between two plays (bilibili re-resolves its streams
	// and the rung it hands back varies). Empty for every ordinary file, whose
	// bytes cannot become something else between two plays.
	variant string
}

// SessionKey identifies one packaging session. The type is deliberately
// unexported: callers obtain a value here and hand it straight back to
// EnsureSession.
func SessionKey(fileID int64, mode string, audio, vindex, height, start int) sessionKey {
	return SessionKeyVariant(fileID, mode, audio, vindex, height, start, "")
}

// SessionKeyVariant is SessionKey for a source whose stream is not fixed by the
// file alone: variant separates the packaging jobs of two different streams of
// the same file, so a session cached for one of them is never served for the
// other.
func SessionKeyVariant(fileID int64, mode string, audio, vindex, height, start int, variant string) sessionKey {
	return sessionKey{fileID: fileID, mode: mode, audio: audio, vindex: vindex,
		height: height, start: start, variant: sanitizeVariant(variant)}
}

// sanitizeVariant keeps a variant filesystem-safe: it becomes part of a
// directory name, and "-" separates the key's fields.
func sanitizeVariant(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (k sessionKey) dirName() string {
	name := fmt.Sprintf("%d-%s-a%d-v%d-h%d-s%d", k.fileID, k.mode, k.audio, k.vindex, k.height, k.start)
	if k.variant != "" {
		name += "-" + k.variant
	}
	return name
}

// Session is one packaging session and its output directory.
type Session struct {
	key    sessionKey
	dir    string
	pack   *Packager
	cancel context.CancelFunc

	mu       sync.Mutex
	lastUsed time.Time
	closed   bool
	// startSeconds is where this session began (absolute file time), used to put
	// packaged segments back onto the absolute timeline.
	startSeconds float64
	// tracks caches the init segment's track timescales for that shift.
	tracks []mp4Track
	// complete is set once the packager finished the whole file successfully:
	// such a session's output is kept on disk so replaying the file is instant,
	// and it survives a restart (see LoadCache).
	complete bool
	// size is the directory's size in bytes, measured when it completes.
	size int64
	// startedAt is when this session's packager was launched. Progress is reported
	// against it, so a viewer can see whether production is faster than realtime.
	startedAt time.Time
}

// Output reports what this session's packager has written so far: how many media
// segments the playlist lists, how much timeline they cover, when the output last
// changed, and whether the packager is still alive.
//
// The playlist is the right thing to read. ffmpeg rewrites it every time a segment
// is finished, so it carries both the count and the liveness signal, and it is the
// same file the player is being served from.
func (s *Session) Output() (segments int, seconds float64, lastAt time.Time, running bool) {
	if raw, err := os.ReadFile(s.PlaylistPath()); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			d, ok := strings.CutPrefix(strings.TrimSpace(line), "#EXTINF:")
			if !ok {
				continue
			}
			segments++
			if v, err := strconv.ParseFloat(strings.TrimSuffix(d, ","), 64); err == nil {
				seconds += v
			}
		}
	}
	if fi, err := os.Stat(s.PlaylistPath()); err == nil {
		lastAt = fi.ModTime()
	} else if fi, err := os.Stat(s.dir); err == nil {
		lastAt = fi.ModTime()
	}
	if s.pack != nil {
		select {
		case <-s.pack.Done():
			running = false // the packager has exited
		default:
			running = true
		}
	}
	return segments, seconds, lastAt, running
}

// markComplete records that the packager finished and measures what it produced.
func (s *Session) markComplete() {
	s.mu.Lock()
	s.complete = true
	s.mu.Unlock()
	s.size = dirSize(s.dir)
}

// Complete reports whether this session packaged the whole file.
func (s *Session) Complete() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.complete
}

// Size is the bytes this session holds, once measured.
func (s *Session) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// dirSize totals the regular files in a directory, ignoring errors.
func dirSize(dir string) int64 {
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// StartSeconds is the absolute file position this session began at.
func (s *Session) StartSeconds() float64 { return s.startSeconds }

// StartSegment is this session's first segment number.
func (s *Session) StartSegment() int { return s.key.start }

// ShiftSegment moves a packaged segment onto the absolute timeline. ffmpeg
// rebases timestamps to zero whenever it starts a new process (even with -ss),
// but players derive media time from tfdt for fMP4 — so a session that began
// mid-file would otherwise look like it began at zero, and resume or seek would
// land in the wrong place. Failures are non-fatal: the segment is served as-is.
func (s *Session) ShiftSegment(data []byte) []byte {
	if s.startSeconds <= 0 {
		return data
	}
	s.mu.Lock()
	tracks := s.tracks
	s.mu.Unlock()
	if tracks == nil {
		initBytes, err := os.ReadFile(s.pack.InitPath())
		if err != nil {
			return data
		}
		parsed, err := ParseInitTracks(initBytes)
		if err != nil {
			log.Printf("hls session %d: cannot read track timescales (%v); serving segments unshifted", s.key.fileID, err)
			return data
		}
		s.mu.Lock()
		s.tracks = parsed
		tracks = parsed
		s.mu.Unlock()
	}
	out, _, err := ShiftTfdt(data, tracks, s.startSeconds)
	if err != nil {
		log.Printf("hls session %d: tfdt shift failed (%v); serving segment unshifted", s.key.fileID, err)
		return data
	}
	return out
}

// Touch records that the session was just used.
func (s *Session) Touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

func (s *Session) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastUsed)
}

// IdleFor reports how long this session has gone without serving a request.
func (s *Session) IdleFor() time.Duration { return s.idleFor() }

// ActiveFor reporting whether a playback is in progress here.
func (s *Session) activeWithin(d time.Duration) bool { return s.idleFor() <= d }

// Dir is the session's output directory.
func (s *Session) Dir() string { return s.dir }

// These derive from the directory rather than from a running packager, so a
// session adopted from a previous run's cache behaves like a live one.

// InitPath is the fMP4 initialization segment path.
func (s *Session) InitPath() string { return filepath.Join(s.dir, packagerInitName) }

// PlaylistPath is ffmpeg's own media playlist path.
func (s *Session) PlaylistPath() string { return filepath.Join(s.dir, packagerPlaylistName) }

// SegmentPath is the file path for segment index i.
func (s *Session) SegmentPath(i int) string {
	return filepath.Join(s.dir, fmt.Sprintf(packagerSegmentPattern, i))
}

// Failed reports a packaging failure that has already happened.
func (s *Session) Failed() error {
	if s.pack == nil {
		return nil // not started yet
	}
	select {
	case <-s.pack.Done():
		return s.pack.Err()
	default:
		return nil
	}
}

// WaitForFile blocks until path exists with non-zero size, the packaging process
// exits without producing it, or ctx/max elapses. Waiting (instead of answering
// 404) keeps the player from treating a not-yet-packaged segment as fatal.
func (s *Session) WaitForFile(ctx context.Context, path string, max time.Duration) error {
	return s.waitFor(ctx, path, max, func(os.FileInfo) bool { return true })
}

// WaitForPlaylist blocks until the media playlist is there in one complete piece.
//
// ffmpeg rewrites index.m3u8 in place every time a segment is finished — the file
// is truncated and rewritten, so at any instant it may hold a prefix of the next
// version. Handing that to a player is a manifest it cannot parse, which it treats
// as fatal: the picture never starts, and the same stream plays on a retry.
//
// A playlist is only ever rewritten by appending whole lines, so one that ends in a
// newline and carries the header is a version ffmpeg finished writing.
func (s *Session) WaitForPlaylist(ctx context.Context, max time.Duration) error {
	path := s.PlaylistPath()
	return s.waitFor(ctx, path, max, func(fi os.FileInfo) bool {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			return false
		}
		text := string(raw)
		return strings.HasSuffix(text, "\n") && strings.Contains(text, "#EXTM3U")
	})
}

// WaitForInit blocks until the fMP4 initialization segment is complete.
//
// temp_file makes ffmpeg rename finished media segments into place, but it writes
// this one directly — measured: init.mp4 exists at 0 bytes and grows to its full
// size. A player that reads it in that window has an unusable initialization
// segment for the whole stream, so it is worth the two samples it takes to tell a
// finished write from one in progress.
func (s *Session) WaitForInit(ctx context.Context, max time.Duration) error {
	path := s.InitPath()
	var lastSize int64 = -1
	return s.waitFor(ctx, path, max, func(fi os.FileInfo) bool {
		stable := fi.Size() > 0 && fi.Size() == lastSize
		lastSize = fi.Size()
		return stable
	})
}

// waitFor polls until ready accepts the file's state, packaging ends, or the
// context or the deadline runs out.
func (s *Session) waitFor(ctx context.Context, path string, max time.Duration, ready func(os.FileInfo) bool) error {
	deadline := time.Now().Add(max)
	var done <-chan struct{}
	if s.pack != nil {
		done = s.pack.Done()
	}
	check := func() bool {
		fi, err := os.Stat(path)
		if err != nil || fi.Size() == 0 || !ready(fi) {
			return false
		}
		s.Touch()
		return true
	}
	for {
		if check() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			if check() {
				return nil
			}
			if err := s.pack.Err(); err != nil {
				return fmt.Errorf("packaging %s failed: %w (%s)", filepath.Base(path), err, s.pack.Stderr())
			}
			return fmt.Errorf("packaging ended before %s was written", filepath.Base(path))
		case <-time.After(60 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", filepath.Base(path))
		}
	}
}

// close stops the packager. The output is removed unless the session completed
// and is being kept as cache (see keepCompleted).
func (s *Session) close(remove bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	// Give the child a moment to die before removing its output, otherwise it
	// may recreate a file inside a directory we are deleting.
	if s.pack != nil {
		select {
		case <-s.pack.Done():
		case <-time.After(3 * time.Second):
		}
	}
	if remove {
		_ = os.RemoveAll(s.dir)
	}
}

// finished reports whether the packager exited successfully, i.e. the whole file
// was packaged and the output is worth keeping.
func (s *Session) finished() bool {
	if s.pack == nil {
		return s.Complete()
	}
	select {
	case <-s.pack.Done():
		if err := s.pack.Err(); err == nil {
			if !s.Complete() {
				s.markComplete()
			}
			return true
		}
		return false
	default:
		return false
	}
}

// keepCompleted is the cache policy: completed output stays on disk so a replay
// does not package the file again, while anything partial is worthless and goes.
// A zero budget disables the cache entirely.
func (e *Engine) keepCompleted() bool {
	return e.Cfg == nil || e.Cfg.Playback.HLSCacheMB > 0
}

// cacheBudget is the cache's size limit in bytes (0 = unlimited).
func (e *Engine) cacheBudget() int64 {
	if e.Cfg == nil || e.Cfg.Playback.HLSCacheMB <= 0 {
		return 0
	}
	return int64(e.Cfg.Playback.HLSCacheMB) << 20
}

// evictCache deletes least-recently-used completed sessions until the cache fits
// its budget, never touching one that is still in use or the caller's protectee.
func (e *Engine) evictCache(protect *Session) {
	budget := e.cacheBudget()
	if budget <= 0 {
		return
	}
	e.sessMu.Lock()
	total := int64(0)
	var done []*Session
	for _, s := range e.sessions {
		if s.Complete() {
			total += s.Size()
			done = append(done, s)
		}
	}
	e.sessMu.Unlock()
	if total <= budget {
		return
	}
	// Oldest first, but a session serving a request right now is not a candidate,
	// and neither is the one the caller is about to use.
	sort.Slice(done, func(i, j int) bool { return done[i].lastUsedTime().Before(done[j].lastUsedTime()) })
	for _, s := range done {
		if total <= budget {
			return
		}
		if s == protect || s.activeWithin(SessionActiveFor) {
			continue
		}
		e.sessMu.Lock()
		delete(e.sessions, s.key)
		e.sessMu.Unlock()
		size := s.Size()
		s.close(true)
		total -= size
		log.Printf("hls cache: evicted session %s (%d MB)", s.key.dirName(), size>>20)
	}
}

// LoadCache adopts packaging output left by a previous run, so replaying a file
// does not package it again, and removes directories that never completed.
// It is safe to call before serving anything.
func (e *Engine) LoadCache() {
	root := filepath.Join(e.Cfg.Abs(), "cache", "hls")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	adopted, dropped := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		key, ok := parseSessionDirName(entry.Name())
		if !ok {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if !e.keepCompleted() || !looksComplete(dir) {
			// Nothing that can be resumed: a partial directory is dead weight.
			_ = os.RemoveAll(dir)
			dropped++
			continue
		}
		s := &Session{key: key, dir: dir, lastUsed: time.Now(), startedAt: time.Now()}
		s.markComplete()
		e.sessMu.Lock()
		e.sessions[key] = s
		e.sessMu.Unlock()
		adopted++
	}
	if adopted > 0 || dropped > 0 {
		log.Printf("hls cache: adopted %d completed session(s), removed %d incomplete", adopted, dropped)
	}
	e.evictCache(nil)
}

// looksComplete reports whether a directory holds a usable packaged session: the
// playlist, the init segment, and every segment the playlist names.
//
// That last part matters more than it looks. A packager killed mid-run leaves a
// playlist that already lists a segment it never finished writing — with
// -hls_flags temp_file the half-written file stays behind as <name>.tmp — and
// adopting such a directory as a finished cache sends the player to a segment
// that nothing is going to produce.
func looksComplete(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, packagerPlaylistName))
	if err != nil || len(raw) == 0 {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, packagerInitName)); err != nil || fi.Size() == 0 {
		return false
	}
	segments := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !reSessionSegment.MatchString(line) {
			continue
		}
		if fi, err := os.Stat(filepath.Join(dir, line)); err != nil || fi.Size() == 0 {
			return false
		}
		segments++
	}
	return segments > 0
}

// parseSessionDirName reverses sessionKey.dirName. The mode never contains a
// dash, so the fields split unambiguously; a seventh field is the variant, which
// older directories do not have.
func parseSessionDirName(name string) (sessionKey, bool) {
	var k sessionKey
	parts := strings.Split(name, "-")
	if len(parts) != 6 && len(parts) != 7 {
		return k, false
	}
	fileID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return k, false
	}
	num := func(s string) (int, bool) {
		v, err := strconv.Atoi(strings.TrimLeft(s, "avhs"))
		return v, err == nil
	}
	audio, ok1 := num(parts[2])
	vindex, ok2 := num(parts[3])
	height, ok3 := num(parts[4])
	start, ok4 := num(parts[5])
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return k, false
	}
	variant := ""
	if len(parts) == 7 {
		variant = sanitizeVariant(parts[6])
		if variant == "" {
			return k, false
		}
	}
	return sessionKey{fileID: fileID, mode: parts[1], audio: audio, vindex: vindex,
		height: height, start: start, variant: variant}, true
}

// EnsureSession returns the live session for key, reusing a completed one when
// the cache still holds it, and starting a packager otherwise.
//
// Creation is serialised. Two requests for the same stream arrive together on
// every fresh playback — the media playlist and the init segment are fetched at
// the same instant — and both used to miss the map, both start an ffmpeg, and
// both write init.mp4, index.m3u8 and seg00000.m4s into the *same* directory.
// The loser then closed itself with remove=true, deleting the output the winner
// was still writing. What a player sees at that point is a stream assembled from
// two packagers and then partly deleted: segments that decode to the wrong
// picture, and a retry (a refresh) that works because by then the directory has
// settled. The lock makes the second request wait and take the finished session
// instead.
func (e *Engine) EnsureSession(ctx context.Context, key sessionKey, opts PackagerOptions) (*Session, error) {
	e.gcSessions()

	e.startMu.Lock()
	defer e.startMu.Unlock()

	e.sessMu.Lock()
	if s, ok := e.sessions[key]; ok {
		e.sessMu.Unlock()
		s.Touch()
		return s, nil
	}
	e.sessMu.Unlock()

	// Make room before writing anything.
	e.evictCache(nil)

	// The packager needs an existing directory and runs with it as its working
	// directory (see packager.go for why). EnsureSession lets the caller fill it
	// first, which is how a subtitle to burn gets there.
	dir := filepath.Join(e.Cfg.Abs(), "cache", "hls", key.dirName())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create session dir: %w", err)
	}
	opts.Dir = dir

	// Detach from the request context: the session must outlive the HTTP request
	// that happened to start it.
	sessCtx, cancel := context.WithCancel(context.Background())
	p, err := StartPackager(sessCtx, opts)
	if err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return nil, err
	}

	s := &Session{key: key, dir: dir, pack: p, cancel: cancel, lastUsed: time.Now(), startedAt: time.Now(), startSeconds: opts.StartSeconds}

	e.sessMu.Lock()
	// Another request may have won the race; keep only one session per key. It
	// cannot happen under the creation lock, but the check stays as the invariant
	// it is — and if it ever does, the loser must not touch the directory it
	// shares with the winner.
	if existing, ok := e.sessions[key]; ok {
		e.sessMu.Unlock()
		s.close(false)
		existing.Touch()
		return existing, nil
	}
	e.sessions[key] = s
	e.sessMu.Unlock()
	return s, nil
}

// gcSessions ends sessions that have been idle for a while. Output that finished
// cleanly is kept as cache; anything partial is removed.
func (e *Engine) gcSessions() {
	e.sessMu.Lock()
	var dead []*Session
	for k, s := range e.sessions {
		if s.idleFor() > sessionIdleTTL || s.Failed() != nil {
			dead = append(dead, s)
			delete(e.sessions, k)
		}
	}
	e.sessMu.Unlock()
	keep := e.keepCompleted()
	for _, s := range dead {
		s.close(!(keep && s.finished()))
	}
}

// CloseSessions stops every session (used on shutdown). Completed output stays
// behind as cache; partial output goes.
func (e *Engine) CloseSessions() {
	e.sessMu.Lock()
	all := make([]*Session, 0, len(e.sessions))
	for k, s := range e.sessions {
		all = append(all, s)
		delete(e.sessions, k)
	}
	e.sessMu.Unlock()
	keep := e.keepCompleted()
	for _, s := range all {
		s.close(!(keep && s.finished()))
	}
}

// Produced reports the highest segment number this session has packaged so far,
// or one before its first segment when nothing has landed yet.
func (s *Session) Produced() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return s.key.start - 1
	}
	best := s.key.start - 1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n, ok := ParseSessionSegment(e.Name()); ok && n > best {
			best = n
		}
	}
	return best
}

func (s *Session) lastUsedTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

// streamSessions lists the live sessions serving one stream, whatever position
// each of them started at.
func (e *Engine) streamSessions(fileID int64, mode string, audio, vindex, height int) []*Session {
	e.sessMu.Lock()
	defer e.sessMu.Unlock()
	var out []*Session
	for k, s := range e.sessions {
		if k.fileID == fileID && k.mode == mode && k.audio == audio &&
			k.vindex == vindex && k.height == height {
			out = append(out, s)
		}
	}
	return out
}

// SessionForStream returns the stream's most recently used live session, or nil.
// The playlist uses this, so after a seek the refreshed playlist follows the
// session that is actually packaging, rather than racing back to the position
// the URL was originally built with.
func (e *Engine) SessionForStream(fileID int64, mode string, audio, vindex, height int) *Session {
	var best *Session
	for _, s := range e.streamSessions(fileID, mode, audio, vindex, height) {
		if best == nil || s.lastUsedTime().After(best.lastUsedTime()) {
			best = s
		}
	}
	return best
}

// SessionForSegment picks the live session that can serve segment idx: the one
// that started latest without starting after idx. A session that has fallen more
// than seekAhead segments behind is rejected — waiting for it would take longer
// than packaging from where the player actually is, which is what a seek means.
func (e *Engine) SessionForSegment(fileID int64, mode string, audio, vindex, height, idx, seekAhead int) *Session {
	var best *Session
	for _, s := range e.streamSessions(fileID, mode, audio, vindex, height) {
		if s.StartSegment() > idx {
			continue // began after this segment: it never packaged it
		}
		if idx > s.Produced()+seekAhead {
			continue // too far behind to wait for
		}
		if best == nil || s.StartSegment() > best.StartSegment() {
			best = s
		}
	}
	return best
}

// CloseStreamSessions closes every live session for these parameters except
// keep. A player can only be in one place, so the sessions a seek leaves behind
// would otherwise keep reading the same source and filling the disk.
func (e *Engine) CloseStreamSessions(fileID int64, mode string, audio, vindex, height int, keep *Session) {
	e.sessMu.Lock()
	var dead []*Session
	for k, s := range e.sessions {
		if s == keep {
			continue
		}
		if k.fileID == fileID && k.mode == mode && k.audio == audio &&
			k.vindex == vindex && k.height == height {
			dead = append(dead, s)
			delete(e.sessions, k)
		}
	}
	e.sessMu.Unlock()
	// A session that finished is still useful cache (seeking back to that
	// position becomes instant); a half-written one is discarded.
	cacheOn := e.keepCompleted()
	for _, s := range dead {
		s.close(!(cacheOn && s.finished()))
	}
}

// RewriteSessionPlaylist points ffmpeg's own playlist at our signed HTTP
// endpoints. ffmpeg writes `init.mp4` and `segNNNNN.m4s` because the child runs
// with the session directory as its working directory (see packager.go); the
// player must instead fetch them through /api/hls.
func RewriteSessionPlaylist(pl, base, sig string) string {
	lines := strings.Split(pl, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, "#EXT-X-MAP:") {
				lines[i] = `#EXT-X-MAP:URI="` + base + `/init.mp4` + sig + `"`
			}
			continue
		}
		if reSessionSegment.MatchString(trimmed) {
			lines[i] = base + "/" + trimmed + sig
		}
	}
	return strings.Join(lines, "\n")
}
