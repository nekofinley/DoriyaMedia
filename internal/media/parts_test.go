package media

import (
	"context"
	"sync"
	"testing"

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

// A resolved ladder belongs to one rung. Handing the cached 1080P parts to a
// request that asked for 360P is exactly how the old library-wide quality
// setting came to look like a no-op: the menu changed, the picture did not.
func TestPartsCacheSeparatesQualities(t *testing.T) {
	e := &Engine{
		partsCache:   map[string]partsEntry{},
		resolveLocks: map[string]*sync.Mutex{},
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
