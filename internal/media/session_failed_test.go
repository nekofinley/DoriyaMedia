package media

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A session whose packager died is not handed out again: a request that finds
// only a dead session has to start a fresh one (see EnsureSession).
func TestSessionLookupSkipsFailedPackager(t *testing.T) {
	e := &Engine{sessions: map[sessionKey]*Session{}}
	const variant = "h264640x360q16"
	key := SessionKeyVariant(7, "remux", 0, 0, 1080, 0, variant)

	dead := &Session{key: key, pack: &Packager{done: make(chan struct{}), err: errors.New("source stalled")}}
	close(dead.pack.done)
	e.sessions[key] = dead

	if got := e.SessionForStream(7, "remux", 0, 0, 1080, variant); got != nil {
		t.Fatalf("SessionForStream handed out a dead session")
	}
	if got := e.SessionForSegment(7, "remux", 0, 0, 1080, variant, 0, SessionSeekAhead); got != nil {
		t.Fatalf("SessionForSegment handed out a dead session")
	}

	// A healthy session for the same stream is still found.
	live := &Session{key: key, pack: &Packager{done: make(chan struct{})}}
	e.sessions[key] = live
	if got := e.SessionForStream(7, "remux", 0, 0, 1080, variant); got != live {
		t.Fatalf("SessionForStream did not return the live session")
	}
}

// A stalled packager leaves output whose segments all exist but whose playlist has
// no end marker. Adopting that at startup leaves a session nobody will ever finish,
// so it has to be dropped like any other partial output.
func TestLoadCacheDropsOutputWithoutEndMarker(t *testing.T) {
	e := newTestEngine(t, nil)
	e.Cfg.DataDir = t.TempDir()
	root := filepath.Join(e.Cfg.Abs(), "cache", "hls")

	stalled := filepath.Join(root, SessionKeyVariant(7, "remux", 0, 0, 1080, 0, "h264640x360q16").dirName())
	if err := os.MkdirAll(stalled, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFileT(t, filepath.Join(stalled, packagerPlaylistName),
		"#EXTM3U\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXTINF:5,\nseg00000.m4s\n")
	writeFileT(t, filepath.Join(stalled, packagerInitName), "init")
	writeFileT(t, filepath.Join(stalled, fmt.Sprintf(packagerSegmentPattern, 0)), "segment")

	e.LoadCache()

	if _, err := os.Stat(stalled); !os.IsNotExist(err) {
		t.Error("output without an end marker must not be adopted; it should be removed")
	}
	if s := e.SessionForStream(7, "remux", 0, 0, 1080, "h264640x360q16"); s != nil {
		t.Error("a stalled session directory must not answer for the stream")
	}
}

// A stalled HTTP source has to fail the packaging job rather than hang it, so
// the read timeout is passed for HTTP inputs, and only for them.
func TestPackagerArgsReadTimeoutOnlyForHTTP(t *testing.T) {
	remote, err := packagerArgs(PackagerOptions{Target: "https://cdn.test/v.m4s", VideoMode: "copy", Dir: "/tmp/out"})
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	if !containsPair(remote, "-rw_timeout", "60000000") {
		t.Fatalf("an HTTP input must carry a read timeout, got %v", remote)
	}

	local, err := packagerArgs(PackagerOptions{Target: "/tmp/x.mkv", VideoMode: "copy", Dir: "/tmp/out"})
	if err != nil {
		t.Fatalf("packagerArgs: %v", err)
	}
	for _, a := range local {
		if a == "-rw_timeout" {
			t.Fatalf("a local file must not carry -rw_timeout, got %v", local)
		}
	}
}
