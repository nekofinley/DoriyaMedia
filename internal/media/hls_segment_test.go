package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"doriyamedia/internal/source"
)

// stubDriver mints a new "link" on every resolution, so a test can tell how
// many times the source was asked for a URL.
type stubDriver struct {
	mu    sync.Mutex
	mints int
}

func (d *stubDriver) mint() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mints++
	return fmt.Sprintf("http://cdn.test/link-%d", d.mints)
}

func (d *stubDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mints
}

func (d *stubDriver) List(context.Context, string) ([]source.Entry, error) { return nil, nil }
func (d *stubDriver) DirectURL(context.Context, string) (string, map[string]string, error) {
	return d.mint(), nil, nil
}
func (d *stubDriver) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (d *stubDriver) ProbeURL(context.Context, string) (string, map[string]string, error) {
	return d.mint(), nil, nil
}
func (d *stubDriver) HasDirect() bool { return true }

// inputOf returns the -i argument of an ffmpeg command line.
func inputOf(args []string) string {
	for i, a := range args {
		if a == "-i" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// A signed cloud link can be refused while still nominally valid. Every segment
// opens its own connection, so without a retry one dead link breaks the rest of
// the stream — the bug this guards.
func TestSegmentRetriesWithFreshLinkWhenSourceRefuses(t *testing.T) {
	drv := &stubDriver{}
	var mu sync.Mutex
	var tried []string

	e := newTestEngine(t, nil)
	e.runStream = func(_ context.Context, args []string, out io.Writer) error {
		target := inputOf(args)
		mu.Lock()
		tried = append(tried, target)
		mu.Unlock()
		if strings.HasSuffix(target, "link-1") {
			return errors.New("Error opening input: Server returned 403 Forbidden (access denied)")
		}
		if _, err := out.Write([]byte("segment")); err != nil {
			return err
		}
		return nil
	}

	file := testFile()
	n, err := e.Segment(context.Background(), file, drv, 320, 4, "transcode", "aac", 0, 0, 1080, io.Discard)
	if err != nil {
		t.Fatalf("Segment must recover from a refused link, got: %v", err)
	}
	if n != int64(len("segment")) {
		t.Fatalf("bytes = %d, want %d", n, len("segment"))
	}

	mu.Lock()
	got := append([]string(nil), tried...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "http://cdn.test/link-1" || got[1] != "http://cdn.test/link-2" {
		t.Fatalf("attempted %v, want the original link then a freshly minted one", got)
	}

	// The refreshed link must be cached, or every later segment would mint again.
	if _, _, err := e.ProbeTarget(context.Background(), file, drv); err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if c := drv.count(); c != 2 {
		t.Fatalf("source resolutions = %d, want 2 (refreshed link must be reused)", c)
	}
}

// A segment that already sent bytes cannot be retried into the same response.
func TestSegmentDoesNotRetryAfterPartialWrite(t *testing.T) {
	drv := &stubDriver{}
	calls := 0
	e := newTestEngine(t, nil)
	e.runStream = func(_ context.Context, _ []string, out io.Writer) error {
		calls++
		if _, err := out.Write([]byte("partial")); err != nil {
			return err
		}
		return errors.New("boom after writing")
	}

	file := testFile()
	n, err := e.Segment(context.Background(), file, drv, 4, 4, "remux", "copy", 0, 0, 1080, io.Discard)
	if err == nil {
		t.Fatal("want the write error to surface")
	}
	if n != int64(len("partial")) {
		t.Fatalf("bytes = %d, want %d", n, len("partial"))
	}
	if calls != 1 {
		t.Fatalf("ffmpeg runs = %d, want 1 (no retry once bytes were sent)", calls)
	}
	if c := drv.count(); c != 1 {
		t.Fatalf("source resolutions = %d, want 1 (no needless re-mint)", c)
	}
}

func TestSegmentReportsBothAttemptsWhenRetryAlsoFails(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	e.runStream = func(context.Context, []string, io.Writer) error {
		return errors.New("403 Forbidden")
	}

	file := testFile()
	n, err := e.Segment(context.Background(), file, drv, 8, 4, "transcode", "aac", 0, 0, 1080, io.Discard)
	if err == nil {
		t.Fatal("want an error when both attempts fail")
	}
	if n != 0 {
		t.Fatalf("bytes = %d, want 0", n)
	}
	if !strings.Contains(err.Error(), "attempt 2") {
		t.Fatalf("error should name the retry, got: %v", err)
	}
	if c := drv.count(); c != 2 {
		t.Fatalf("source resolutions = %d, want 2", c)
	}
}

// A burst of failing segments must mint ONE fresh link between them: the drive
// may invalidate the previous link each time a new one is issued.
func TestResolveTargetMintsOneFreshLinkUnderConcurrency(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	file := testFile()

	rejected, _, err := e.ProbeTarget(context.Background(), file, drv)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if drv.count() != 1 {
		t.Fatalf("resolutions = %d, want 1", drv.count())
	}

	const callers = 8
	var wg sync.WaitGroup
	got := make([]string, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tgt, _, err := e.resolveTarget(context.Background(), file, drv, rejected)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
				return
			}
			got[i] = tgt
		}(i)
	}
	wg.Wait()

	if c := drv.count(); c != 2 {
		t.Fatalf("resolutions = %d, want 2 (one shared refresh, not one per caller)", c)
	}
	for i, g := range got {
		if g != "http://cdn.test/link-2" {
			t.Fatalf("caller %d got %q, want the refreshed link", i, g)
		}
	}
}

func TestProbeTargetReusesCachedLink(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	file := testFile()

	a, _, err := e.ProbeTarget(context.Background(), file, drv)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	b, _, err := e.ProbeTarget(context.Background(), file, drv)
	if err != nil {
		t.Fatalf("ProbeTarget: %v", err)
	}
	if a != b {
		t.Fatalf("cached link not reused: %q then %q", a, b)
	}
	if c := drv.count(); c != 1 {
		t.Fatalf("resolutions = %d, want 1", c)
	}
}

// The ffmpeg command line must still map the real video stream and the chosen
// audio ordinal, and must never leave the output unconstrained.
func TestSegmentArgsMapping(t *testing.T) {
	args := segmentArgs([]Input{{Target: "http://cdn.test/x.mkv"}}, 12.5, 4, "transcode", "aac", 2, 7, 720)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-ss 12.500", "-i http://cdn.test/x.mkv", "-map 0:7", "-map 0:a:2",
		"-c:v libx264", "-pix_fmt yuv420p", "scale=-2:min(ih\\,720)",
		"-c:a aac", "-t 4.000", "-f mpegts", "pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command line missing %q:\n%s", want, joined)
		}
	}
}

// A source that publishes the picture and the sound separately has to be read as
// two inputs: each one carries its own headers, both are seeked to the same
// point, and the mapping is by input rather than by absolute stream index.
func TestSegmentArgsMultiInput(t *testing.T) {
	inputs := []Input{
		{Target: "http://cdn.test/video.m4s", Headers: map[string]string{"Referer": "https://www.bilibili.com/"}},
		{Target: "http://cdn.test/audio.m4s", Headers: map[string]string{"Referer": "https://www.bilibili.com/"}},
	}
	args := segmentArgs(inputs, 60, 4, "remux", "copy", 0, -1, 720)
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"-ss 60.000", "-i http://cdn.test/video.m4s",
		"-ss 60.000", "-i http://cdn.test/audio.m4s",
		"-map 0:v:0", "-map 1:a:0",
		"-c:v copy", "-c:a copy", "-t 4.000", "-f mpegts", "pipe:1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("command line missing %q:\n%s", want, joined)
		}
	}
	// Each input needs the headers of its own request, so there are two of them.
	if n := strings.Count(joined, "-headers"); n != 2 {
		t.Fatalf("headers applied to %d inputs, want 2:\n%s", n, joined)
	}
	if strings.Contains(joined, "-an") {
		t.Fatalf("a two-input segment must carry its audio:\n%s", joined)
	}
}
