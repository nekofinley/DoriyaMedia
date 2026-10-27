package media

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// qualityPartsDriver is a multi-stream source with a quality ladder: it answers
// a different pair of streams per rung, and counts how often it had to resolve.
type qualityPartsDriver struct {
	fakeDriver
	quality int
	parts   map[int][]source.Part
	calls   int
}

type bypassPartsDriver struct {
	fakeDriver
	parts []source.Part
	calls int
}

func (d *qualityPartsDriver) Parts(context.Context, string) ([]source.Part, error) {
	d.calls++
	q := d.quality
	if q == 0 {
		q = 112 // automatic picks the best rung
	}
	return d.parts[q], nil
}

func (d *qualityPartsDriver) SetQuality(qn int)    { d.quality = qn }
func (d *qualityPartsDriver) Quality() int         { return d.quality }
func (d *qualityPartsDriver) SelectedQuality() int { return d.quality }
func (d *qualityPartsDriver) Qualities(context.Context, string) ([]source.Quality, error) {
	return nil, nil
}

func (d *bypassPartsDriver) Parts(context.Context, string) ([]source.Part, error) {
	d.calls++
	return d.parts, nil
}

func (d *bypassPartsDriver) BypassPartsCache() bool {
	return true
}

func TestPartsForBypassesCacheOnlyForOptedInDrivers(t *testing.T) {
	e := &Engine{
		partsCache:   map[string]partsEntry{},
		resolveLocks: map[string]*keyedMutex{},
	}
	file := &models.MediaFile{ID: 1, SourceID: 2, Path: "/fixture.mp4"}
	d := &bypassPartsDriver{parts: []source.Part{
		{URL: "https://cdn.invalid/video.mp4", Kind: "video", Codec: "h264"},
		{URL: "https://cdn.invalid/audio.mp4", Kind: "audio", Codec: "aac"},
	}}

	for i := 0; i < 2; i++ {
		parts, err := e.PartsFor(context.Background(), file, d, "")
		if err != nil || len(parts) != 2 {
			t.Fatalf("PartsFor call %d = %v/%v", i+1, parts, err)
		}
	}
	if d.calls != 2 {
		t.Fatalf("source resolved %d times, want two uncached calls", d.calls)
	}
	if _, ok := e.partsCache[partsForKey(file, d)]; ok {
		t.Fatal("bypass driver left a parts cache entry")
	}
}

// A resolved ladder belongs to one rung. Handing the cached 1080P parts to a
// request that asked for 360P is exactly how the old library-wide quality
// setting came to look like a no-op: the menu changed, the picture did not.
func TestPartsCacheSeparatesQualities(t *testing.T) {
	e := &Engine{
		partsCache:   map[string]partsEntry{},
		resolveLocks: map[string]*keyedMutex{},
	}
	file := &models.MediaFile{ID: 1, SourceID: 2, Path: "/某番 [series 1]/第1话 [ep1].mp4"}
	d := &qualityPartsDriver{parts: map[int][]source.Part{
		112: {
			{URL: "https://cdn.invalid/1080-v.mp4", Kind: "video", Codec: "h264", Height: 1080},
			{URL: "https://cdn.invalid/a.m4a", Kind: "audio", Codec: "aac"},
		},
		32: {
			{URL: "https://cdn.invalid/480-v.mp4", Kind: "video", Codec: "h264", Height: 480},
			{URL: "https://cdn.invalid/a.m4a", Kind: "audio", Codec: "aac"},
		},
	}}
	ctx := context.Background()

	high, err := e.PartsFor(ctx, file, d, "")
	if err != nil || len(high) != 2 {
		t.Fatalf("automatic parts = %v/%v", high, err)
	}
	if leadURL(high) != "https://cdn.invalid/1080-v.mp4" {
		t.Fatalf("automatic parts lead with %q", leadURL(high))
	}

	d.SetQuality(32)
	low, err := e.PartsFor(ctx, file, d, "")
	if err != nil || len(low) != 2 {
		t.Fatalf("480P parts = %v/%v", low, err)
	}
	if leadURL(low) != "https://cdn.invalid/480-v.mp4" {
		t.Fatalf("the 480P request was answered with %q (the cached rung) — the menu would be a no-op",
			leadURL(low))
	}
	if d.calls != 2 {
		t.Fatalf("the source resolved %d times, want one per rung", d.calls)
	}

	// An explicit rung has its own cache entry (automatic is a preference, not a
	// rung), and once resolved it is cached like any other.
	d.SetQuality(112)
	again, err := e.PartsFor(ctx, file, d, "")
	if err != nil || leadURL(again) != "https://cdn.invalid/1080-v.mp4" {
		t.Fatalf("switching to 1080P resolved the wrong rung: %v, err=%v", again, err)
	}
	if d.calls != 3 {
		t.Fatalf("the first explicit 1080P resolution should have asked the source once (calls=%d)", d.calls)
	}
	if _, err := e.PartsFor(ctx, file, d, ""); err != nil || d.calls != 3 {
		t.Fatalf("the explicit 1080P entry was not cached (calls=%d, err=%v)", d.calls, err)
	}
	d.SetQuality(32)
	again, err = e.PartsFor(ctx, file, d, "")
	if err != nil || leadURL(again) != "https://cdn.invalid/480-v.mp4" || d.calls != 3 {
		t.Fatalf("the 480P entry was not cached: %v, calls=%d, err=%v", again, d.calls, err)
	}

	// A refused link drops only the rung it belongs to.
	d.SetQuality(112)
	if _, err := e.PartsFor(ctx, file, d, "https://cdn.invalid/1080-v.mp4"); err != nil {
		t.Fatalf("re-resolving a refused link: %v", err)
	}
	if d.calls != 4 {
		t.Fatalf("a refused 1080P link resolved %d times, want a fresh resolution", d.calls)
	}
	d.SetQuality(32)
	if _, err := e.PartsFor(ctx, file, d, ""); err != nil {
		t.Fatalf("the untouched rung: %v", err)
	}
	if d.calls != 4 {
		t.Fatalf("refreshing 1080P threw away the 480P entry (calls=%d)", d.calls)
	}
}

func TestPartsProbeCopiesBitDepthAndHDR(t *testing.T) {
	probe := partsProbe([]source.Part{
		{
			URL: "https://cdn.invalid/video.m4s", Kind: "video", Codec: "hevc",
			Profile: "Main 10", BitDepth: 10, HDR: true,
			Width: 3840, Height: 2160, Duration: 1426,
		},
		{
			URL: "https://cdn.invalid/audio.m4s", Kind: "audio", Codec: "aac",
			Channels: 2, Duration: 1426,
		},
	})
	video := probe.Video()
	if video == nil {
		t.Fatal("parts probe has no video stream")
	}
	if video.BitDepth != 10 || !video.HDR || video.Profile != "Main 10" {
		t.Fatalf("parts video = %+v, want Main 10 10-bit HDR", video)
	}
}

func TestPartsForHonorsContextWhileWaitingForResolutionLock(t *testing.T) {
	e := &Engine{
		partsCache:   map[string]partsEntry{},
		resolveLocks: map[string]*keyedMutex{},
	}
	file := &models.MediaFile{ID: 9, SourceID: 2, Path: "/blocked.mp4"}
	d := &qualityPartsDriver{parts: map[int][]source.Part{
		112: {
			{URL: "https://cdn.invalid/v.mp4", Kind: "video"},
			{URL: "https://cdn.invalid/a.m4a", Kind: "audio"},
		},
	}}
	mu := e.resolveLock(partsForKey(file, d))
	defer e.releaseResolveLock(mu)
	mu.Lock()
	defer mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := e.PartsFor(ctx, file, d, "")
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PartsFor lock wait = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PartsFor ignored context while waiting for the resolution lock")
	}
	if d.calls != 0 {
		t.Fatalf("PartsFor called the source %d times while the lock was held", d.calls)
	}
}

type blockingPartsDriver struct {
	fakeDriver
	started chan struct{}
	release chan struct{}
	once    sync.Once
	parts   []source.Part
}

func (d *blockingPartsDriver) Parts(context.Context, string) ([]source.Part, error) {
	d.once.Do(func() { close(d.started) })
	<-d.release
	return d.parts, nil
}

func TestPartsForBoundsContextIgnoringDriverAndKeepsLock(t *testing.T) {
	e := &Engine{
		partsCache:   map[string]partsEntry{},
		resolveLocks: map[string]*keyedMutex{},
	}
	file := &models.MediaFile{ID: 10, SourceID: 2, Path: "/blocking-driver.mp4"}
	d := &blockingPartsDriver{
		started: make(chan struct{}),
		release: make(chan struct{}),
		parts: []source.Part{
			{URL: "https://cdn.invalid/v.mp4", Kind: "video"},
			{URL: "https://cdn.invalid/a.m4a", Kind: "audio"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := e.PartsFor(ctx, file, d, "")
		result <- err
	}()

	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("PartsFor did not enter the context-ignoring driver")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PartsFor blocking driver = %v, want deadline exceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PartsFor remained blocked by a context-ignoring driver")
	}

	mu := e.resolveLock(partsForKey(file, d))
	defer e.releaseResolveLock(mu)
	lockedCtx, cancelLocked := context.WithTimeout(context.Background(), 50*time.Millisecond)
	if err := mu.LockContext(lockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		cancelLocked()
		t.Fatalf("resolution lock after canceled driver = %v, want held", err)
	}
	cancelLocked()

	close(d.release)
	releasedCtx, cancelReleased := context.WithTimeout(context.Background(), time.Second)
	defer cancelReleased()
	if err := mu.LockContext(releasedCtx); err != nil {
		t.Fatalf("resolution lock was not released after the driver returned: %v", err)
	}
	mu.Unlock()
}

func TestResolveLockPruningKeepsActiveKeyEntry(t *testing.T) {
	e := &Engine{resolveLocks: map[string]*keyedMutex{}}
	const activeKey = "active-file"
	active := e.resolveLock(activeKey)
	defer e.releaseResolveLock(active)

	for i := 0; i < 1100; i++ {
		mu := e.resolveLock(fmt.Sprintf("inactive-%d", i))
		e.releaseResolveLock(mu)
	}
	again := e.resolveLock(activeKey)
	defer e.releaseResolveLock(again)
	if again != active {
		t.Fatal("idle-lock pruning replaced an active per-file resolution lock")
	}
}
