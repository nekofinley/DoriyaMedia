package media

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The player must never see ffmpeg's own relative URIs: it fetches everything
// through /api/hls, with the same signature as the playlist.
func TestRewriteSessionPlaylist(t *testing.T) {
	in := "#EXTM3U\n" +
		"#EXT-X-VERSION:7\n" +
		"#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:4.000000,\n" +
		"seg00000.m4s\n" +
		"#EXTINF:4.000000,\n" +
		"seg00001.m4s\n" +
		"#EXT-X-ENDLIST\n"
	got := RewriteSessionPlaylist(in, "/api/hls/7", "?exp=1&sig=abc")

	for _, want := range []string{
		`#EXT-X-MAP:URI="/api/hls/7/init.mp4?exp=1&sig=abc"`,
		"/api/hls/7/seg00000.m4s?exp=1&sig=abc",
		"/api/hls/7/seg00001.m4s?exp=1&sig=abc",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rewritten playlist missing %q:\n%s", want, got)
		}
	}
	// ffmpeg's own relative names must be gone.
	for _, unwanted := range []string{"\nseg00000.m4s", `URI="init.mp4"`} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("rewritten playlist still contains %q:\n%s", unwanted, got)
		}
	}
	// Tags other than EXT-X-MAP must be left alone.
	if !strings.Contains(got, "#EXTINF:4.000000,") {
		t.Fatalf("EXTINF was altered:\n%s", got)
	}
}

func TestParseSessionSegment(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"seg00000.m4s", 0, true},
		{"seg00042.m4s", 42, true},
		{"seg9.m4s", 9, true},
		{"init.mp4", 0, false},
		{"seg.m4s", 0, false},
		{"segabc.m4s", 0, false},
		{"index.m3u8", 0, false},
		{"seg00001", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSessionSegment(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("ParseSessionSegment(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// End to end through the session layer: a real packager is started, the
// playlist is waited for, the init segment is fetched and must be hvc1, and
// closing the session must remove its directory (that is what bounds disk use).
func TestSessionLifecycle(t *testing.T) {
	requireFFmpeg(t)
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir() // never write into the package directory

	key := SessionKey(42, "remux", 0, 0, 1080, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sess, err := e.EnsureSession(ctx, key, PackagerOptions{
		Target:       testMKV,
		VideoIndex:   0,
		AudioOrdinal: 0,
		VideoMode:    "copy",
		AudioMode:    "aac",
		VideoCodec:   "hevc",
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if sess.Dir() == "" || !strings.Contains(sess.Dir(), "cache") {
		t.Fatalf("unexpected session dir %q", sess.Dir())
	}

	// A second call must reuse the same session, not spawn another packager.
	again, err := e.EnsureSession(ctx, key, PackagerOptions{Target: testMKV})
	if err != nil {
		t.Fatalf("EnsureSession (again): %v", err)
	}
	if again != sess {
		t.Fatal("EnsureSession started a second session for the same key")
	}

	if err := sess.WaitForFile(ctx, sess.PlaylistPath(), 30*time.Second); err != nil {
		t.Fatalf("waiting for playlist: %v", err)
	}
	pl, err := os.ReadFile(sess.PlaylistPath())
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	if !strings.Contains(string(pl), "#EXT-X-MAP") {
		t.Fatalf("playlist has no EXT-X-MAP:\n%s", pl)
	}
	if !strings.Contains(RewriteSessionPlaylist(string(pl), "/api/hls/42", "?s=1"), "/api/hls/42/seg") {
		t.Fatalf("rewritten playlist has no signed segment URIs:\n%s", pl)
	}

	if err := sess.WaitForFile(ctx, sess.InitPath(), 30*time.Second); err != nil {
		t.Fatalf("waiting for init segment: %v", err)
	}
	initBytes, err := os.ReadFile(sess.InitPath())
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	if !strings.Contains(string(initBytes), "hvc1") {
		t.Error("session init segment is not hvc1")
	}

	if err := sess.WaitForFile(ctx, sess.SegmentPath(0), 30*time.Second); err != nil {
		t.Fatalf("waiting for first segment: %v", err)
	}

	// Let it finish (an event playlist gains ENDLIST at the end), because what
	// happens to the directory now depends on whether the file was completed.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if pl, err := os.ReadFile(sess.PlaylistPath()); err == nil && strings.Contains(string(pl), "#EXT-X-ENDLIST") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("packaging did not finish")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// ENDLIST lands when the last segment is written, which is not the same moment
	// as the packager exiting — and CloseSessions only keeps output whose packager
	// finished cleanly. Waiting for that (which is also what marks the session
	// complete) is what makes this a test of the cache rather than a race with the
	// process: the original flake failed whenever the poll won by a few
	// milliseconds, and killed the output it was about to assert on.
	deadline = time.Now().Add(30 * time.Second)
	for !sess.finished() {
		if time.Now().After(deadline) {
			t.Fatal("the packager never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}

	dir := sess.Dir()
	e.CloseSessions()
	// Completed output is cache: replaying the file must not package it again.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a completed session must be kept as cache: %v", err)
	}
	if !sess.Complete() {
		t.Error("a finished session must be marked complete")
	}
	if sess.Size() <= 0 {
		t.Error("a completed session must have a measured size")
	}

	// And a fresh engine adopts it from disk, so a restart does not redo the work.
	e2 := newTestEngine(t, nil)
	e2.Cfg.DataDir = e.Cfg.DataDir
	e2.LoadCache()
	adopted := e2.SessionForStream(42, "remux", 0, 0, 1080, "")
	if adopted == nil {
		t.Fatal("LoadCache did not adopt the completed session from disk")
	}
	if !adopted.Complete() || adopted.Size() != sess.Size() {
		t.Errorf("adopted session looks wrong: complete=%v size=%d want %d", adopted.Complete(), adopted.Size(), sess.Size())
	}
	// Reuse: asking again must return the adopted session, not start a packager.
	reuse, err := e2.EnsureSession(ctx, SessionKey(42, "remux", 0, 0, 1080, 0), PackagerOptions{Target: testMKV})
	if err != nil {
		t.Fatalf("EnsureSession after LoadCache: %v", err)
	}
	if reuse != adopted {
		t.Error("a cached session must be reused instead of packaging the file again")
	}
	if reuse.pack != nil {
		t.Error("a reused session must not have started a packager")
	}
}

// Half-written output is worthless: it must not survive as cache.
func TestIncompleteSessionIsDiscarded(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := requireSeekableSource(t, dir, 120)

	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := e.EnsureSession(ctx, SessionKey(5, "remux", 0, 0, 1080, 0), PackagerOptions{
		Target: src, VideoIndex: 0, AudioOrdinal: -1, VideoMode: "copy", VideoCodec: "h264",
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	out := sess.Dir()
	if sess.Complete() {
		t.Fatal("the session cannot have completed this fast")
	}
	e.CloseSessions()
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("an unfinished session's directory must be removed (stat err=%v)", err)
	}
}

// fakeCacheSession registers a completed session holding a file of n bytes.
func fakeCacheSession(t *testing.T, e *Engine, fileID int64, start int, n int) *Session {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seg00000.m4s"), make([]byte, n), 0o644); err != nil {
		t.Fatalf("write segment: %v", err)
	}
	s := &Session{key: SessionKey(fileID, "remux", 0, 0, 1080, start), dir: dir, lastUsed: time.Now()}
	s.markComplete()
	e.sessMu.Lock()
	e.sessions[s.key] = s
	e.sessMu.Unlock()
	return s
}

// The cache has to stay inside its budget, and it must evict the session nobody
// has touched for longest — never the one serving a request right now.
func TestCacheEvictionDropsLeastRecentlyUsed(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	e.Cfg.Playback.HLSCacheMB = 1 // 1 MiB budget

	oldest := fakeCacheSession(t, e, 1, 0, 400<<10)
	middle := fakeCacheSession(t, e, 2, 0, 400<<10)
	inUse := fakeCacheSession(t, e, 3, 0, 400<<10)
	oldest.lastUsed = time.Now().Add(-time.Hour)
	middle.lastUsed = time.Now().Add(-time.Minute)
	inUse.lastUsed = time.Now() // active: must survive this pass

	e.evictCache(nil)

	if _, err := os.Stat(oldest.Dir()); !os.IsNotExist(err) {
		t.Error("the least recently used session must be evicted first")
	}
	if _, err := os.Stat(middle.Dir()); err != nil {
		t.Error("eviction should have stopped once the cache fitted its budget")
	}
	if _, err := os.Stat(inUse.Dir()); err != nil {
		t.Error("a session in active use must not be evicted")
	}

	// With the budget already satisfied, nothing else goes.
	e.evictCache(nil)
	if _, err := os.Stat(middle.Dir()); err != nil {
		t.Error("eviction must not run past the budget")
	}

	// A budget of zero means no cache at all.
	if (&Engine{Cfg: e.Cfg}).keepCompleted() != true {
		t.Error("a positive budget must keep completed output")
	}
	e.Cfg.Playback.HLSCacheMB = 0
	if e.keepCompleted() {
		t.Error("a zero budget must disable the cache")
	}
}

// Startup must adopt finished output and throw away anything partial.
func TestLoadCacheAdoptsCompleteAndDropsPartial(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	root := filepath.Join(e.Cfg.Abs(), "cache", "hls")

	good := filepath.Join(root, SessionKey(7, "remux", 0, 0, 1080, 12).dirName())
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(good, packagerPlaylistName), "#EXTM3U\n#EXTINF:4,\nseg00012.m4s\n#EXT-X-ENDLIST\n")
	writeFileT(t, filepath.Join(good, packagerInitName), "init")
	writeFileT(t, filepath.Join(good, fmt.Sprintf(packagerSegmentPattern, 12)), "segment")

	partial := filepath.Join(root, SessionKey(8, "remux", 0, 0, 1080, 0).dirName())
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(partial, packagerPlaylistName), "#EXTM3U\n") // no init, no segments

	junk := filepath.Join(root, "not-a-session")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	e.LoadCache()

	if s := e.SessionForSegment(7, "remux", 0, 0, 1080, "", 12, SessionSeekAhead); s == nil {
		t.Error("a completed session on disk must be adopted")
	} else if !s.Complete() || s.StartSegment() != 12 {
		t.Errorf("adopted session has wrong state: complete=%v start=%d", s.Complete(), s.StartSegment())
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Error("a partial session directory must be removed at startup")
	}
	if _, err := os.Stat(junk); err != nil {
		t.Error("a directory that is not a session should be left alone")
	}

	// With the cache turned off, even a finished directory is cleared out.
	e2 := newTestEngine(t, nil)
	e2.Cfg.DataDir = e.Cfg.DataDir
	e2.Cfg.Playback.HLSCacheMB = 0
	e2.LoadCache()
	if _, err := os.Stat(good); !os.IsNotExist(err) {
		t.Error("with caching off, leftover output must be cleared at startup")
	}
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Waiting for a resource that will never appear must not hang forever.
func TestSessionWaitForFileTimesOut(t *testing.T) {
	requireFFmpeg(t)
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	defer e.CloseSessions()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sess, err := e.EnsureSession(ctx, SessionKey(1, "remux", 0, 0, 1080, 0), PackagerOptions{
		Target: testMKV, VideoIndex: 0, AudioOrdinal: 0, VideoMode: "copy", AudioMode: "aac", VideoCodec: "hevc",
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	start := time.Now()
	err = sess.WaitForFile(ctx, sess.SegmentPath(999999), 700*time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout error for a segment that is never produced")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout was not honoured (took %v)", elapsed)
	}
}

// requireSeekableSource builds a source with real keyframes, so seeking has
// something to land on. The committed fixtures carry a single keyframe, which
// makes every -ss snap back to zero and would hide the bug this guards.
func requireSeekableSource(t *testing.T, dir string, seconds int) string {
	t.Helper()
	src := filepath.Join(dir, "src.mp4")
	out, err := exec.Command("ffmpeg", "-y", "-v", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x240:rate=25:duration=%d", seconds),
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "50", "-keyint_min", "50",
		"-sc_threshold", "0", "-pix_fmt", "yuv420p", src).CombinedOutput()
	if err != nil {
		t.Fatalf("build test source: %v: %s", err, out)
	}
	return src
}

// A session that begins mid-file must still describe itself in absolute terms:
// segment N covers N*segLen, and the media it serves must be timestamped at that
// position — otherwise a player places a resumed stream at zero.
func TestSessionStartingMidFile(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := requireSeekableSource(t, dir, 40)

	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	defer e.CloseSessions()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	const startSeg = 2
	const segLen = 4.0
	opts := PackagerOptions{
		Target: src, VideoIndex: 0, AudioOrdinal: -1,
		VideoMode: "copy", VideoCodec: "h264",
		SegmentSeconds: segLen, StartSeconds: startSeg * segLen, StartSegment: startSeg,
	}
	sess, err := e.EnsureSession(ctx, SessionKey(9, "remux", 0, 0, 1080, startSeg), opts)
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := sess.WaitForFile(ctx, sess.PlaylistPath(), 30*time.Second); err != nil {
		t.Fatalf("waiting for playlist: %v", err)
	}
	pl, err := os.ReadFile(sess.PlaylistPath())
	if err != nil {
		t.Fatalf("read playlist: %v", err)
	}
	if !strings.Contains(string(pl), "#EXT-X-MEDIA-SEQUENCE:2") {
		t.Errorf("playlist must keep absolute numbering:\n%s", pl)
	}
	if !strings.Contains(string(pl), "seg00002.m4s") {
		t.Errorf("segments must keep absolute naming:\n%s", pl)
	}

	segPath := sess.SegmentPath(startSeg)
	if err := sess.WaitForFile(ctx, segPath, 30*time.Second); err != nil {
		t.Fatalf("waiting for the first segment: %v", err)
	}
	raw := readFileT(t, segPath)

	// The premise: ffmpeg rebased the timeline to zero.
	if v := tfdtValues(raw); len(v) == 0 || v[0] != 0 {
		t.Fatalf("expected the raw segment to start at 0, got %v", v)
	}

	initBytes := readFileT(t, sess.InitPath())
	comb := filepath.Join(dir, "shifted.mp4")
	if err := os.WriteFile(comb, append(append([]byte(nil), initBytes...), sess.ShiftSegment(raw)...), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := firstVideoPTS(t, comb); got < 7.8 || got > 8.2 {
		t.Errorf("served segment starts at %.3fs, want ~8s (its absolute position)", got)
	}
}

// A different start position is a different packaging job, not a cache hit.
func TestSessionKeyIncludesStart(t *testing.T) {
	if SessionKey(1, "remux", 0, 0, 1080, 0) == SessionKey(1, "remux", 0, 0, 1080, 2) {
		t.Fatal("sessions with different start positions must not share an identity")
	}
}

// fakeSession registers a session with a chosen start and production point, so
// the selection rules can be tested without running ffmpeg.
func fakeSession(t *testing.T, e *Engine, fileID int64, start, produced int) *Session {
	t.Helper()
	dir := t.TempDir()
	for i := start; i <= produced; i++ {
		name := filepath.Join(dir, fmt.Sprintf("seg%05d.m4s", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatalf("seed segment: %v", err)
		}
	}
	s := &Session{key: SessionKey(fileID, "remux", 0, 0, 1080, start), dir: dir, lastUsed: time.Now()}
	e.sessMu.Lock()
	e.sessions[s.key] = s
	e.sessMu.Unlock()
	return s
}

// Which session serves a request is what decides whether a seek works. Inside
// the packaged window an existing session answers; far outside it the caller has
// to start a new one, because waiting would be slower than packaging afresh.
func TestSessionSelectionForSegment(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	early := fakeSession(t, e, 7, 0, 20) // started at the top, 20 segments in hand
	late := fakeSession(t, e, 7, 10, 30) // started at segment 10
	other := fakeSession(t, e, 8, 0, 30) // a different file
	_ = other

	// Inside a session's window: the latest session that had already begun wins.
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, "", 15, SessionSeekAhead); got != late {
		t.Errorf("segment 15 should be served by the session that started at 10")
	}
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, "", 5, SessionSeekAhead); got != early {
		t.Errorf("segment 5 predates the late session, so the early one must serve it")
	}
	// A seek far past what is packaged cannot be served by waiting.
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, "", 500, SessionSeekAhead); got != nil {
		t.Errorf("a request far ahead must not attach to a session that cannot reach it")
	}
	// Nor can a position before every session's start.
	e2 := newTestEngine(t, nil)
	e2.Cfg.DataDir = t.TempDir()
	fakeSession(t, e2, 7, 10, 20)
	if got := e2.SessionForSegment(7, "remux", 0, 0, 1080, "", 3, SessionSeekAhead); got != nil {
		t.Errorf("a request before a session's start must not attach to it")
	}
	// Buffering (just past production) must NOT look like a seek, or a slow
	// transcode would restart the packager on every request.
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, "", 25, SessionSeekAhead); got != late {
		t.Errorf("a request just past production must keep waiting on the live session")
	}
	// Sessions of another file or mode are never borrowed.
	if got := e.SessionForSegment(7, "transcode", 0, 0, 1080, "", 15, SessionSeekAhead); got != nil {
		t.Errorf("another mode's session must not be reused")
	}
	if got := e.SessionForSegment(9, "remux", 0, 0, 1080, "", 15, SessionSeekAhead); got != nil {
		t.Errorf("another file's session must not be reused")
	}
}

// The playlist follows whatever session is actually packaging, so a refreshed
// playlist after a seek does not resurrect the abandoned position.
func TestSessionForStreamFollowsLatestUse(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	first := fakeSession(t, e, 7, 0, 5)
	second := fakeSession(t, e, 7, 40, 45)
	first.lastUsed = time.Now().Add(-time.Minute)
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, ""); got != second {
		t.Fatal("the most recently used session must answer the playlist")
	}
	if got := e.SessionForStream(7, "transcode", 0, 0, 1080, ""); got != nil {
		t.Fatal("another mode has no session")
	}

	// Closing the abandoned ones leaves exactly the live session.
	e.CloseStreamSessions(7, "remux", 0, 0, 1080, "", second)
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, ""); got != second {
		t.Fatal("the kept session must survive")
	}
	e.sessMu.Lock()
	n := len(e.sessions)
	e.sessMu.Unlock()
	if n != 1 {
		t.Fatalf("expected only the kept session to remain, got %d", n)
	}
}

// A two-stream source is addressed by where its packaging begins. /api/play told
// the client a stream_start, and the element's clock is relative to it, so the
// playlist request carrying that start has to be answered by the session that
// begins there — even when one at another position was used more recently.
//
// Following the latest use regardless (as a remembered seek used to) hands a
// reloaded playback the previous position's timeline: every time, seek and
// subtitle the player computes is then shifted by the distance between the two,
// and the first picture lands early.
func TestSessionForStreamAtHonoursTheRequestedStart(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	old := fakeSession(t, e, 7, 0, 5)          // packaging from the top
	atResume := fakeSession(t, e, 7, 110, 130) // the position a resume is starting at
	old.lastUsed = time.Now()
	atResume.lastUsed = time.Now().Add(-2 * time.Second)

	if got := e.SessionForStreamAt(7, "remux", 0, 0, 1080, "", 110); got != atResume {
		t.Fatal("the playlist for a session starting at 110s must get that session")
	}
	if got := e.SessionForStreamAt(7, "remux", 0, 0, 1080, "", 0); got != old {
		t.Fatal("a different start is a different session")
	}
	if got := e.SessionForStreamAt(7, "remux", 0, 0, 1080, "", 12); got != nil {
		t.Fatal("a start nothing is packaging for has no session")
	}
}

// Two rungs of one bilibili episode are two pictures, not two encodings of the
// same one. Their sessions must never answer for each other: a segment request
// pinned to 360P that is served by the 1080P packaging job would show the wrong
// picture and its timestamps would belong to a different stream.
func TestSessionsAreKeptApartByQualityVariant(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	const (
		v1080 = "h2641920x1080-q112"
		v360  = "h264640x360-q16"
	)
	high := fakeSessionVariant(t, e, 7, v1080)
	low := fakeSessionVariant(t, e, 7, v360)

	if got := e.SessionForStream(7, "remux", 0, 0, 1080, v1080); got != high {
		t.Fatal("the playlist for 1080P must get the 1080P session")
	}
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, v360); got != low {
		t.Fatal("the playlist for 360P must get the 360P session")
	}
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, v360, 0, SessionSeekAhead); got != low {
		t.Fatal("a 360P segment must be served by the 360P packaging job")
	}
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, "h264852x480-q32", 0, SessionSeekAhead); got != nil {
		t.Fatal("a rung with no session must not borrow another rung's")
	}

	// A seek inside one rung cleans up only that rung's abandoned sessions.
	e.CloseStreamSessions(7, "remux", 0, 0, 1080, v360, nil)
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, v360); got != nil {
		t.Fatal("the closed 360P session survived")
	}
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, v1080); got != high {
		t.Fatal("closing one rung must not touch the other one's playback")
	}
}

// fakeSessionVariant registers a session for one stream variant, with the first
// segment already produced so segment selection has something to see.
func fakeSessionVariant(t *testing.T, e *Engine, fileID int64, variant string) *Session {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seg00000.m4s"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed segment: %v", err)
	}
	s := &Session{key: SessionKeyVariant(fileID, "remux", 0, 0, 1080, 0, variant), dir: dir, lastUsed: time.Now()}
	e.sessMu.Lock()
	e.sessions[s.key] = s
	e.sessMu.Unlock()
	return s
}

// The name in the play decision and the name of the packaging session's
// directory must be the same string: the player matches its status poll against
// the former, and a stray dash (sanitized out of a directory name) would make
// every poll look like another stream's.
func TestStreamVariantMatchesSessionKey(t *testing.T) {
	v := StreamVariant("h264", 1920, 1080, 112)
	if v != "h2641920x1080q112" {
		t.Fatalf("StreamVariant = %q", v)
	}
	k := SessionKeyVariant(7, "remux", 0, 0, 1080, 0, v)
	if k.variant != v {
		t.Fatalf("the session key stored %q for variant %q", k.variant, v)
	}
	if got := StreamVariant("", 0, 0, 0); got != "" {
		t.Fatalf("a source without a picture named a variant %q", got)
	}
}

// Burning a subtitle is the only way to show a bitmap track (PGS/VobSub) at all,
// so it runs through the ordinary session machinery: a long-lived packager that
// re-encodes the video with the subtitle overlaid, cacheable like any other.
func TestBurnOverlaysSubtitleAndReencodes(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src := requireSeekableSource(t, dir, 8)

	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prepared := ""
	// The mode carries a dot rather than a colon: it becomes part of the session's
	// directory name, and Windows forbids a colon there.
	sess, err := e.EnsureSession(ctx, SessionKey(77, "burn.5", 0, 0, 1080, 0), PackagerOptions{
		Target: src, VideoIndex: 0, AudioOrdinal: -1,
		VideoMode: "transcode", AudioMode: "aac",
		Burn: &BurnRequest{StreamIndex: -1, File: "burn.srt"},
		PrepareDir: func(d string) error {
			prepared = d
			// The packager runs with the directory as its working directory, so a
			// bare name is what the filter gets.
			return os.WriteFile(filepath.Join(d, "burn.srt"),
				[]byte("1\n00:00:00,200 --> 00:00:02,000\nhello\n"), 0o644)
		},
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if prepared != sess.Dir() {
		t.Fatalf("PrepareDir ran in %q, want the session directory %q", prepared, sess.Dir())
	}
	// A filter that does not work means ffmpeg exits at once, so the playlist
	// never appears and this waits out its timeout — the failure is visible.
	if err := sess.WaitForFile(ctx, sess.PlaylistPath(), 90*time.Second); err != nil {
		t.Fatalf("burning produced no playlist: %v", err)
	}
	if err := sess.WaitForFile(ctx, sess.SegmentPath(0), 90*time.Second); err != nil {
		t.Fatalf("burning produced no segment: %v", err)
	}

	// The video must have been re-encoded (a copy cannot carry an overlay), and
	// the output must still be the fMP4 that Apple needs.
	init, err := os.ReadFile(sess.InitPath())
	if err != nil {
		t.Fatalf("read init: %v", err)
	}
	if !bytes.Contains(init, []byte("avc1")) {
		t.Error("the burned stream should carry a re-encoded avc1 track")
	}
	if bytes.Contains(init, []byte("hvc1")) || bytes.Contains(init, []byte("hev1")) {
		t.Error("a burned stream must not copy HEVC")
	}

	// And it decodes. A bare .m4s segment cannot be read on its own — the fMP4 is
	// split across the init and the media segments — so they are concatenated
	// first, exactly as a player's buffer would hold them.
	seg, err := os.ReadFile(sess.SegmentPath(0))
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	combined := filepath.Join(dir, "burned.mp4")
	if err := os.WriteFile(combined, append(append([]byte{}, init...), seg...), 0o644); err != nil {
		t.Fatalf("write combined: %v", err)
	}
	decOut, err := exec.Command("ffmpeg", "-v", "error", "-i", combined,
		"-frames:v", "1", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("burned segment does not decode: %v: %s", err, decOut)
	}
}

// The PGS case cannot be synthesised (ffmpeg has no PGS encoder), so its
// arguments are pinned directly: the subtitle stream is overlaid by index, and
// the video cannot be copied.
func TestPackagerBurnArgsForBitmapSubtitle(t *testing.T) {
	args, err := packagerArgs(PackagerOptions{
		Target: "/x/movie.mkv", VideoIndex: 0, AudioOrdinal: 0,
		VideoMode: "transcode", AudioMode: "aac", TranscodeHeight: 720,
		Burn: &BurnRequest{StreamIndex: 3}, Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	want := `[0:v]scale=-2:min(ih\,720)[vin];[vin][0:3]overlay[vout]`
	if !strings.Contains(joined, want) {
		t.Errorf("bitmap burn graph missing.\n got: %s\nwant: %s", joined, want)
	}
	if !strings.Contains(joined, "-map [vout]") {
		t.Errorf("the overlay output must be mapped: %s", joined)
	}
	if strings.Contains(joined, "-c:v copy") {
		t.Errorf("a burned video cannot be copied: %s", joined)
	}
	if !strings.Contains(joined, "-c:v libx264") {
		t.Errorf("a burned video must be re-encoded: %s", joined)
	}
	if strings.Contains(joined, "-vf ") {
		t.Errorf("with a filter graph the scale must be inside it, not a -vf: %s", joined)
	}

	// A plain session must not grow a filter graph.
	plain, err := packagerArgs(PackagerOptions{
		Target: "/x/movie.mkv", VideoIndex: 0, AudioOrdinal: 0,
		VideoMode: "remux", AudioMode: "copy", Dir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	if strings.Contains(strings.Join(plain, " "), "-filter_complex") {
		t.Error("an ordinary session must not use a filter graph")
	}
}
