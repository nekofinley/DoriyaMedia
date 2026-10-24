package media

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A session is enough for these: the waits only read the directory they are given.
func testSession(t *testing.T) *Session {
	t.Helper()
	dir := t.TempDir()
	return &Session{dir: dir, key: SessionKey(1, "remux", 0, 0, 1080, 0)}
}

// A playlist that is still being rewritten must not be handed to a player: half a
// manifest is a parse error, and players treat that as fatal rather than retrying.
func TestWaitForPlaylistDoesNotServeAHalfWrittenManifest(t *testing.T) {
	s := testSession(t)
	path := s.PlaylistPath()

	// What ffmpeg's in-place rewrite looks like mid-flight: the next line is
	// there only up to wherever the write got to.
	partial := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQU"
	writeFileT(t, path, partial)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.WaitForPlaylist(ctx, 4*time.Second) }()

	select {
	case err := <-done:
		t.Fatalf("returned on a half-written playlist: %v", err)
	case <-time.After(250 * time.Millisecond):
	}

	// ffmpeg finishes the version it was writing.
	writeFileT(t, path, partial+"G\n#EXTINF:4.000,\nseg00000.m4s\n")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("completed playlist refused: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a finished playlist was still not served")
	}
}

// The init segment is written in place (temp_file does not cover it), so the wait
// has to hold out until ffmpeg has stopped appending to it.
func TestWaitForInitWaitsUntilItStopsGrowing(t *testing.T) {
	s := testSession(t)
	path := s.InitPath()
	writeFileT(t, path, "0123456789")

	stop := make(chan struct{})
	go func() {
		for i := 0; i < 4; i++ {
			time.Sleep(70 * time.Millisecond)
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return
			}
			_, _ = f.WriteString("0123456789")
			_ = f.Close()
		}
		close(stop)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.WaitForInit(ctx, 4*time.Second); err != nil {
		t.Fatalf("WaitForInit: %v", err)
	}
	<-stop
	// It must have waited past the last append, not merely for the file to exist.
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("returned after %v: it accepted an init segment that was still being written", elapsed)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() != 50 {
		t.Fatalf("returned with %d bytes on disk, want the finished 50", fi.Size())
	}
}

// A finished session (or one adopted from cache) must not pay for any of this.
func TestWaitsReturnImmediatelyOnCompletedFiles(t *testing.T) {
	s := testSession(t)
	writeFileT(t, s.PlaylistPath(), "#EXTM3U\n#EXT-X-VERSION:7\n#EXTINF:4.000,\nseg00000.m4s\n#EXT-X-ENDLIST\n")
	writeFileT(t, s.InitPath(), "init-bytes")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := s.WaitForPlaylist(ctx, 4*time.Second); err != nil {
		t.Fatalf("WaitForPlaylist on a finished file: %v", err)
	}
	if err := s.WaitForInit(ctx, 4*time.Second); err != nil {
		t.Fatalf("WaitForInit on a finished file: %v", err)
	}
	// One stability sample for the init segment; the playlist needs none.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("waits took %v on finished files, want them immediate", elapsed)
	}
}

// A file that never arrives must still give up rather than hang the request.
func TestWaitForPlaylistTimesOut(t *testing.T) {
	s := testSession(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if err := s.WaitForPlaylist(ctx, 300*time.Millisecond); err == nil {
		t.Fatal("want a timeout for a playlist that is never written")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout not honoured (took %v)", elapsed)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "index.m3u8")); !os.IsNotExist(err) {
		t.Fatal("the wait must not create the playlist itself")
	}
}

// A packager killed mid-run leaves a playlist that already lists a segment it
// never finished. Adopting that as a finished cache sends a player to a segment
// nothing will produce, so the directory has to be dropped instead.
//
// The playlist here carries its end marker throughout, so this is about the
// segments alone: the end marker is a separate condition, covered by
// TestLoadCacheDropsOutputWithoutEndMarker.
func TestLooksCompleteRejectsAPlaylistAheadOfItsSegments(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "init.mp4"), "init")
	writeFileT(t, filepath.Join(dir, "seg00000.m4s"), "first")
	playlist := "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MAP:URI=\"init.mp4\"\n" +
		"#EXTINF:4.000,\nseg00000.m4s\n#EXTINF:4.000,\nseg00001.m4s\n#EXT-X-ENDLIST\n"
	writeFileT(t, filepath.Join(dir, "index.m3u8"), playlist)
	if looksComplete(dir) {
		t.Fatal("adopted a session whose playlist names a segment that does not exist")
	}

	// What temp_file leaves behind when it is killed: the half-written segment
	// under its temporary name. The finished name is still missing.
	writeFileT(t, filepath.Join(dir, "seg00001.m4s.tmp"), "half")
	if looksComplete(dir) {
		t.Fatal("a leftover .tmp was mistaken for a finished segment")
	}

	// Once the segment is really there, the directory is usable again.
	writeFileT(t, filepath.Join(dir, "seg00001.m4s"), "second")
	if !looksComplete(dir) {
		t.Fatal("a complete session was rejected")
	}
}
