package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"doriyamedia/internal/config"
	"doriyamedia/internal/db"
	"doriyamedia/internal/models"
	"doriyamedia/internal/source"
)

// ---------- helpers ----------

// fakeDriver satisfies source.Driver without touching a real source.
type fakeDriver struct{}

func (fakeDriver) List(context.Context, string) ([]source.Entry, error) { return nil, nil }
func (fakeDriver) DirectURL(context.Context, string) (string, map[string]string, error) {
	return "http://example.invalid/x.mkv", nil, nil
}
func (fakeDriver) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}
func (fakeDriver) ProbeURL(context.Context, string) (string, map[string]string, error) {
	return "http://example.invalid/x.mkv", nil, nil
}
func (fakeDriver) HasDirect() bool { return true }

type failingMultiDriver struct {
	fakeDriver
	calls *atomic.Int32
	err   error
}

func (d failingMultiDriver) Parts(context.Context, string) ([]source.Part, error) {
	d.calls.Add(1)
	return nil, d.err
}

func newTestEngine(t *testing.T, probe func(ctx context.Context, p *Prober, target string, headers map[string]string, proxy string) (*Probe, error)) *Engine {
	t.Helper()
	store, err := db.Open(filepath.Join(t.TempDir(), "probe-test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.DB.Close() })
	e := NewEngine(store, config.Defaults())
	e.execProbe = probe
	return e
}

// doneObservedContext signals when code has reached a select that can wait on
// Done. Tests use it to synchronize a blocked caller without timing guesses.
type doneObservedContext struct {
	context.Context
	once     sync.Once
	observed chan struct{}
}

func newDoneObservedContext(parent context.Context) *doneObservedContext {
	return &doneObservedContext{
		Context:  parent,
		observed: make(chan struct{}),
	}
}

func (c *doneObservedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
}

func waitForSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// addLocalSource registers a real (local) source row so paths that resolve the
// driver from the store — the background pre-probe — work end to end.
func addLocalSource(t *testing.T, e *Engine, root string) int64 {
	t.Helper()
	src := &models.Source{Name: "test", Type: "local", Config: fmt.Sprintf(`{"root":%q}`, root)}
	if err := e.Store.CreateSource(src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return src.ID
}

func videoProbe(codec string) *Probe {
	p := &Probe{}
	p.Format.FormatName = "matroska,webm"
	p.Streams = append(p.Streams, ProbeStream{Index: 0, CodecType: "video", CodecName: codec},
		ProbeStream{Index: 1, CodecType: "audio", CodecName: "eac3"})
	return p
}

func testFile() *models.MediaFile {
	return &models.MediaFile{ID: 7, SourceID: 1, Path: "/movies/x.mkv"}
}

func addProbeTestFile(t *testing.T, e *Engine) *models.MediaFile {
	t.Helper()
	sourceID := addLocalSource(t, e, t.TempDir())
	library := &models.Library{
		Name: "probe", Type: "movie", SourceID: sourceID,
		Path: "/", Providers: "[]",
	}
	if err := e.Store.CreateLibrary(library); err != nil {
		t.Fatalf("create library: %v", err)
	}
	item := &models.Item{LibraryID: library.ID, Type: "movie", Name: "Probe"}
	if err := e.Store.InsertItem(item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	fileID, err := e.Store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: sourceID, Path: "/movies/probe.mkv",
		Size: 10, MTime: 20,
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	file, err := e.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	return file
}

// waitState polls until the probe job for a file reaches want, or fails.
func waitState(t *testing.T, e *Engine, fileID int64, want string) ProbeStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := e.ProbeStatus(fileID)
		if st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	return e.ProbeStatus(fileID)
}

// ---------- ProbeFromCache ----------

func TestProbeFromCache(t *testing.T) {
	withVideo := `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"video","codec_name":"hevc"}]}`
	noVideo := `{"format":{"format_name":"matroska,webm"},"streams":[{"index":0,"codec_type":"audio","codec_name":"aac"}]}`
	noStreams := `{"format":{"format_name":"image2"},"streams":[]}`
	malformed := `{not json`

	cases := []struct {
		name  string
		probe string
		want  bool
	}{
		{"empty", "", false},
		{"video", withVideo, true},
		{"audio-only is not playable", noVideo, false},
		{"no streams at all counts as usable", noStreams, true},
		{"malformed", malformed, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProbeFromCache(&models.MediaFile{Probe: c.probe}) != nil
			if got != c.want {
				t.Fatalf("ProbeFromCache = %v, want %v", got, c.want)
			}
		})
	}
	if ProbeFromCache(nil) != nil {
		t.Fatal("ProbeFromCache(nil) must be nil")
	}
}

// ---------- async lifecycle ----------

func TestProbeStatusIdleBeforeStart(t *testing.T) {
	e := newTestEngine(t, nil)
	if st := e.ProbeStatus(999); st.State != "idle" {
		t.Fatalf("state = %q, want idle", st.State)
	}
}

func TestStartProbeReachesReadyAndPersists(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		calls.Add(1)
		return videoProbe("hevc"), nil
	})
	file := testFile()
	e.StartProbe(file, fakeDriver{})

	st := waitState(t, e, file.ID, "ready")
	if st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}
	if st.Elapsed < 0 {
		t.Fatalf("elapsed must not be negative, got %v", st.Elapsed)
	}
	if calls.Load() != 1 {
		t.Fatalf("ffprobe passes = %d, want 1", calls.Load())
	}
	if file.Probe == "" {
		t.Fatal("probe must be written back onto the file row")
	}
	if ProbeFromCache(file) == nil {
		t.Fatal("stored probe must be usable by ProbeFromCache")
	}
}

func TestProbePersistenceFailureKeepsCurrentPlaybackReady(t *testing.T) {
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		return videoProbe("h264"), nil
	})
	file := testFile()
	if err := e.Store.DB.Close(); err != nil {
		t.Fatalf("close probe store: %v", err)
	}

	e.StartProbe(file, fakeDriver{})
	st := waitState(t, e, file.ID, "ready")
	if st.Error != "" {
		t.Fatalf("cache write failure failed playback probe: %s", st.Error)
	}
	if ProbeFromCache(file) == nil {
		t.Fatal("current playback did not retain the successful probe")
	}
}

func TestProbePersistsEmbeddedSubtitlesAtomically(t *testing.T) {
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		probe := videoProbe("h264")
		probe.Streams = append(probe.Streams, ProbeStream{
			Index: 4, CodecType: "subtitle", CodecName: "subrip",
			Tags: map[string]string{"language": "ja", "title": "Japanese"},
		})
		return probe, nil
	})
	file := addProbeTestFile(t, e)
	if err := e.Store.UpsertSubtitle(&models.Subtitle{
		FileID: file.ID, Lang: "zh", Format: "subrip",
		Embedded: true, StreamIdx: 2, Convertible: true,
	}); err != nil {
		t.Fatalf("seed stale embedded subtitle: %v", err)
	}
	if err := e.Store.UpsertSubtitle(&models.Subtitle{
		FileID: file.ID, Path: "/subtitles/external.srt",
		Lang: "en", Format: "subrip", Convertible: true,
	}); err != nil {
		t.Fatalf("seed external subtitle: %v", err)
	}

	e.StartProbe(file, fakeDriver{})
	if st := waitState(t, e, file.ID, "ready"); st.Error != "" {
		t.Fatalf("probe failed: %s", st.Error)
	}
	subs, err := e.Store.SubtitlesByFile(file.ID)
	if err != nil {
		t.Fatalf("read subtitles: %v", err)
	}
	if len(subs) != 2 {
		t.Fatalf("subtitle rows = %+v, want one external and one embedded", subs)
	}
	var embedded, external bool
	for _, sub := range subs {
		switch {
		case sub.Embedded && sub.StreamIdx == 4:
			embedded = sub.Lang == "ja" && sub.Title == "Japanese"
		case !sub.Embedded && sub.Path == "/subtitles/external.srt":
			external = true
		case sub.Embedded && sub.StreamIdx == 2:
			t.Fatalf("stale embedded subtitle survived: %+v", sub)
		}
	}
	if !embedded || !external {
		t.Fatalf("atomic probe subtitles = %+v", subs)
	}
}

func TestProbeGenerationChangeRetriesWithoutCooldown(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return videoProbe("h264"), nil
	})
	file := addProbeTestFile(t, e)

	e.StartProbe(file, fakeDriver{})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first probe did not start")
	}
	fresh := *file
	fresh.Size++
	fresh.MTime++
	if _, err := e.Store.UpsertFile(&fresh); err != nil {
		t.Fatalf("replace file generation: %v", err)
	}
	close(release)

	if st := waitState(t, e, file.ID, "failed"); st.Error == "" {
		t.Fatal("stale generation probe must fail")
	}
	e.probeMu.Lock()
	first := e.probeJobs[file.ID]
	firstErr := first.err
	e.probeMu.Unlock()
	if !errors.Is(firstErr, db.ErrMediaFileChanged) {
		t.Fatalf("stale probe error = %v, want ErrMediaFileChanged", firstErr)
	}
	if file.Probe != "" {
		t.Fatalf("stale generation wrote in-memory probe: %q", file.Probe)
	}

	e.StartProbe(&fresh, fakeDriver{})
	e.probeMu.Lock()
	second := e.probeJobs[file.ID]
	e.probeMu.Unlock()
	if second == first {
		t.Fatal("generation change incorrectly entered the failed-probe cooldown")
	}
	if st := waitState(t, e, file.ID, "ready"); st.Error != "" {
		t.Fatalf("retry after generation change failed: %s", st.Error)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe passes after generation change = %d, want 2", calls.Load())
	}
}

func TestStartProbeIsSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return videoProbe("h264"), nil
	})
	file := testFile()

	e.StartProbe(file, fakeDriver{})
	<-entered
	// While the first probe runs, further requests must not start another one.
	for i := 0; i < 5; i++ {
		e.StartProbe(testFile(), fakeDriver{})
	}
	if st := e.ProbeStatus(file.ID); st.State != "probing" {
		t.Fatalf("state = %q, want probing", st.State)
	}
	close(release)

	waitState(t, e, file.ID, "ready")
	if got := calls.Load(); got != 1 {
		t.Fatalf("ffprobe passes = %d, want 1 (concurrent starts must dedupe)", got)
	}
}

func TestStartProbeSkippedWhenProbeCached(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	file := testFile()
	file.Probe = `{"format":{"format_name":"mov,mp4"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`

	e.StartProbe(file, fakeDriver{})
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("ffprobe must not run for an already-probed file, got %d calls", calls.Load())
	}
	if st := e.ProbeStatus(file.ID); st.State != "idle" {
		t.Fatalf("state = %q, want idle (no job created)", st.State)
	}
}

func TestProbeTransientDoesNotPersistMetadata(t *testing.T) {
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		probe := videoProbe("h264")
		probe.Streams = append(probe.Streams, ProbeStream{
			Index: 2, CodecType: "subtitle", CodecName: "subrip",
		})
		return probe, nil
	})
	sourceID := addLocalSource(t, e, t.TempDir())
	library := &models.Library{
		Name: "transient", Type: "movie", SourceID: sourceID,
		Path: "/", Providers: "[]",
	}
	if err := e.Store.CreateLibrary(library); err != nil {
		t.Fatalf("create library: %v", err)
	}
	item := &models.Item{LibraryID: library.ID, Type: "movie", Name: "Transient"}
	if err := e.Store.InsertItem(item); err != nil {
		t.Fatalf("create item: %v", err)
	}
	fileID, err := e.Store.UpsertFile(&models.MediaFile{
		ItemID: item.ID, SourceID: sourceID, Path: "/movies/x.mkv",
		Size: 10, MTime: 20,
	})
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	file, err := e.Store.FileByID(fileID)
	if err != nil {
		t.Fatalf("load file: %v", err)
	}
	if err := e.Store.UpsertSubtitle(&models.Subtitle{
		FileID: fileID, Path: "existing.srt", Format: "subrip",
		Convertible: true,
	}); err != nil {
		t.Fatalf("seed subtitle: %v", err)
	}
	probe, err := e.ProbeTransient(context.Background(), file, fakeDriver{})
	if err != nil {
		t.Fatalf("transient probe: %v", err)
	}
	if probe == nil || probe.Video() == nil {
		t.Fatal("transient probe did not return the video stream")
	}
	if file.Probe != "" {
		t.Fatalf("transient probe wrote file cache: %q", file.Probe)
	}
	subs, err := e.Store.SubtitlesByFile(file.ID)
	if err != nil {
		t.Fatalf("read transient subtitles: %v", err)
	}
	if len(subs) != 1 || subs[0].Path != "existing.srt" {
		t.Fatalf("transient probe changed subtitles: %+v", subs)
	}
}

func TestProbeTransientRejectsCanceledContextBeforeCachedProbe(t *testing.T) {
	file := testFile()
	file.Probe = `{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e := &Engine{}
	probe, err := e.ProbeTransient(ctx, file, fakeDriver{})
	if probe != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cached probe = %v, %v", probe, err)
	}
}

func TestProbeTransientDeduplicatesConcurrentCallers(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return videoProbe("h264"), nil
	})
	file := testFile()
	type result struct {
		probe *Probe
		err   error
	}
	results := make(chan result, 2)
	run := func(ctx context.Context) {
		probe, err := e.ProbeTransient(ctx, file, fakeDriver{})
		results <- result{probe: probe, err: err}
	}
	go run(context.Background())
	<-entered
	followerCtx, cancelFollower := context.WithCancel(context.Background())
	defer cancelFollower()
	observedFollower := newDoneObservedContext(followerCtx)
	go run(observedFollower)
	waitForSignal(t, observedFollower.observed, "follower to wait on the in-flight probe")
	releaseOnce.Do(func() { close(release) })

	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil || got.probe == nil {
			t.Fatalf("transient caller %d = probe %v err %v", i, got.probe, got.err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transient ffprobe passes = %d, want 1", got)
	}
}

func TestProbeTransientDoesNotReuseLeaderContextCancellation(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	file := testFile()
	key := ticketProbeJobKey(file)
	done := make(chan struct{})
	close(done)
	e.ticketProbeJobs[key] = &ticketProbeJob{
		done:           done,
		finished:       time.Now(),
		err:            context.Canceled,
		leaderCanceled: true,
	}

	probe, err := e.ProbeTransient(context.Background(), file, fakeDriver{})
	if err != nil || probe == nil {
		t.Fatalf("probe after leader cancellation = %v, %v", probe, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe passes after leader cancellation = %d, want 1", got)
	}

	probe, err = e.ProbeTransient(context.Background(), file, fakeDriver{})
	if err != nil || probe == nil {
		t.Fatalf("cached probe after retry = %v, %v", probe, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("successful retry was not cached: passes = %d, want 1", got)
	}
}

func TestProbeTransientCachesDriverContextFailureWithLiveLeaderContext(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	file := testFile()
	key := ticketProbeJobKey(file)
	leader := &ticketProbeJob{done: make(chan struct{})}
	e.ticketProbeJobs[key] = leader
	e.finishTicketProbeJob(
		key, leader, nil, context.DeadlineExceeded, false)

	probe, err := e.ProbeTransient(
		context.Background(), file, fakeDriver{})
	if probe != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cached driver context failure = %v, %v", probe, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("probe passes after fabricated result = %d, want 0", got)
	}
}

func TestProbeTransientLeaderCachesLiveDriverContextFailure(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return nil, context.DeadlineExceeded
	})
	file := testFile()

	probe, err := e.ProbeTransient(
		context.Background(), file, fakeDriver{})
	if probe != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("live driver context failure = %v, %v", probe, err)
	}
	firstPasses := calls.Load()
	if firstPasses == 0 {
		t.Fatal("live driver context failure did not run a probe")
	}
	probe, err = e.ProbeTransient(
		context.Background(), file, fakeDriver{})
	if probe != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cached live driver context failure = %v, %v", probe, err)
	}
	if got := calls.Load(); got != firstPasses {
		t.Fatalf("live driver context failure was retried: passes %d -> %d",
			firstPasses, got)
	}

	key := ticketProbeJobKey(file)
	e.ticketProbeMu.Lock()
	job := e.ticketProbeJobs[key]
	leaderCanceled := job != nil && job.leaderCanceled
	e.ticketProbeMu.Unlock()
	if job == nil {
		t.Fatal("live driver context failure was not cached")
	}
	if leaderCanceled {
		t.Fatal("live driver context failure was treated as caller cancellation")
	}
}

func TestProbeTransientCachesMultiPartResolutionFailure(t *testing.T) {
	for _, sourceErr := range []error{
		context.DeadlineExceeded,
		errors.New("source resolution failed"),
	} {
		t.Run(sourceErr.Error(), func(t *testing.T) {
			var calls atomic.Int32
			e := newTestEngine(t, nil)
			file := testFile()
			drv := failingMultiDriver{calls: &calls, err: sourceErr}

			for attempt := 0; attempt < 2; attempt++ {
				probe, err := e.ProbeTransient(
					context.Background(), file, drv)
				if probe != nil || !errors.Is(err, sourceErr) {
					t.Fatalf("attempt %d = %v, %v; want source failure %v",
						attempt, probe, err, sourceErr)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("MultiPart source resolutions = %d, want one cached failure", got)
			}

			e.ticketProbeMu.Lock()
			job := e.ticketProbeJobs[ticketProbeJobKey(file)]
			leaderCanceled := job != nil && job.leaderCanceled
			e.ticketProbeMu.Unlock()
			if job == nil || job.finished.IsZero() {
				t.Fatal("MultiPart source failure was not cached")
			}
			if leaderCanceled {
				t.Fatal("live MultiPart source failure was treated as caller cancellation")
			}
		})
	}
}

func TestProbeTransientCacheTracksFileGeneration(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		if calls.Add(1) == 1 {
			return videoProbe("h264"), nil
		}
		return videoProbe("hevc"), nil
	})
	file := testFile()
	file.Size, file.MTime = 100, 10
	if _, err := e.ProbeTransient(context.Background(), file, fakeDriver{}); err != nil {
		t.Fatalf("first probe: %v", err)
	}

	updated := testFile()
	updated.Size, updated.MTime = 200, 20
	probe, err := e.ProbeTransient(context.Background(), updated, fakeDriver{})
	if err != nil {
		t.Fatalf("updated probe: %v", err)
	}
	if probe.Video() == nil || probe.Video().CodecName != "hevc" {
		t.Fatalf("updated probe = %+v, want the new file generation", probe)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe passes across file generations = %d, want 2", got)
	}
}

func TestTicketProbeRegistryWaitsForInFlightCapacity(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	e.ticketProbeJobs = make(map[ticketProbeKey]*ticketProbeJob, ticketProbeMaxJobs)
	var releaseKey ticketProbeKey
	var releaseJob *ticketProbeJob
	for i := 0; i < ticketProbeMaxJobs; i++ {
		key := ticketProbeKey{fileID: int64(i + 1)}
		job := &ticketProbeJob{
			done: make(chan struct{}),
		}
		e.ticketProbeJobs[key] = job
		if i == 0 {
			releaseKey, releaseJob = key, job
		}
	}
	file := testFile()
	file.ID = ticketProbeMaxJobs + 1
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	observedWait := newDoneObservedContext(waitCtx)
	type result struct {
		probe *Probe
		err   error
	}
	done := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(
			observedWait, file, fakeDriver{})
		done <- result{probe: probe, err: err}
	}()

	waitForSignal(t, observedWait.observed, "probe to wait for registry capacity")
	if got := calls.Load(); got != 0 {
		t.Errorf("probe bypassed full in-flight registry: %d pass(es)", got)
	}
	e.finishTicketProbeJob(releaseKey, releaseJob, videoProbe("h264"), nil, false)

	select {
	case got := <-done:
		if got.err != nil || got.probe == nil {
			t.Fatalf("probe after capacity release = %v, %v",
				got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not resume after registry capacity was released")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe passes after capacity release = %d, want 1", got)
	}
	e.ticketProbeMu.Lock()
	size := len(e.ticketProbeJobs)
	e.ticketProbeMu.Unlock()
	if size != ticketProbeMaxJobs {
		t.Fatalf("ticket probe registry size = %d, want %d",
			size, ticketProbeMaxJobs)
	}
}

func TestTicketProbeRegistryWaitHonoursContext(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	e.ticketProbeJobs = make(map[ticketProbeKey]*ticketProbeJob, ticketProbeMaxJobs)
	for i := 0; i < ticketProbeMaxJobs; i++ {
		e.ticketProbeJobs[ticketProbeKey{fileID: int64(i + 1)}] =
			&ticketProbeJob{done: make(chan struct{})}
	}
	file := testFile()
	file.ID = ticketProbeMaxJobs + 1
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := newDoneObservedContext(parent)
	type result struct {
		probe *Probe
		err   error
	}
	done := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(ctx, file, fakeDriver{})
		done <- result{probe: probe, err: err}
	}()
	waitForSignal(t, ctx.observed, "probe to wait for registry capacity")
	cancel()

	select {
	case got := <-done:
		if got.probe != nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("full-registry wait = %v, %v", got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled registry waiter did not return")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("full-registry context cancellation ran %d probe(s)", got)
	}
	e.ticketProbeMu.Lock()
	defer e.ticketProbeMu.Unlock()
	if got := len(e.ticketProbeJobs); got != ticketProbeMaxJobs {
		t.Fatalf("ticket probe registry size = %d, want %d",
			got, ticketProbeMaxJobs)
	}
	if _, ok := e.ticketProbeJobs[ticketProbeJobKey(file)]; ok {
		t.Fatal("canceled registry waiter left a probe-job entry")
	}
}

func TestProbeTransientFollowerReelectsAfterLeaderCancellation(t *testing.T) {
	leaderEntered := make(chan struct{})
	var calls atomic.Int32
	e := newTestEngine(t, func(
		ctx context.Context, _ *Prober, _ string, _ map[string]string, _ string,
	) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return videoProbe("h264"), nil
	})
	file := testFile()
	type result struct {
		probe *Probe
		err   error
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderDone := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(leaderCtx, file, fakeDriver{})
		leaderDone <- result{probe: probe, err: err}
	}()
	waitForSignal(t, leaderEntered, "leader probe to start")

	followerCtx, cancelFollower := context.WithCancel(context.Background())
	defer cancelFollower()
	observedFollower := newDoneObservedContext(followerCtx)
	followerDone := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(observedFollower, file, fakeDriver{})
		followerDone <- result{probe: probe, err: err}
	}()
	waitForSignal(t, observedFollower.observed, "follower to wait on the leader")

	cancelLeader()
	select {
	case got := <-leaderDone:
		if got.probe != nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled leader = %v, %v", got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled leader did not return")
	}
	select {
	case got := <-followerDone:
		if got.err != nil || got.probe == nil || got.probe.Video() == nil {
			t.Fatalf("follower after leader cancellation = %v, %v",
				got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not re-elect a leader and finish")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe passes after leader cancellation = %d, want 2", got)
	}
}

func TestProbeTransientDiscardsSuccessAfterLeaderCancellation(t *testing.T) {
	leaderEntered := make(chan struct{})
	finishLeaderProbe := make(chan struct{})
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(leaderEntered)
			<-finishLeaderProbe
		}
		return videoProbe("h264"), nil
	})
	file := testFile()
	type result struct {
		probe *Probe
		err   error
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()
	leaderDone := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(leaderCtx, file, fakeDriver{})
		leaderDone <- result{probe: probe, err: err}
	}()
	waitForSignal(t, leaderEntered, "leader probe to start")

	followerCtx, cancelFollower := context.WithCancel(context.Background())
	defer cancelFollower()
	observedFollower := newDoneObservedContext(followerCtx)
	followerDone := make(chan result, 1)
	go func() {
		probe, err := e.ProbeTransient(observedFollower, file, fakeDriver{})
		followerDone <- result{probe: probe, err: err}
	}()
	waitForSignal(t, observedFollower.observed, "follower to wait on the leader")

	cancelLeader()
	close(finishLeaderProbe)
	select {
	case got := <-leaderDone:
		if got.probe != nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled successful leader = %v, %v", got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled successful leader did not return")
	}
	select {
	case got := <-followerDone:
		if got.err != nil || got.probe == nil || got.probe.Video() == nil {
			t.Fatalf("follower after successful canceled leader = %v, %v",
				got.probe, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower did not re-elect after successful canceled leader")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("probe passes after successful cancellation = %d, want 2", got)
	}
}

func TestTicketProbeRegistryEvictsOnlyOldestFinishedResult(t *testing.T) {
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		return videoProbe("h264"), nil
	})
	now := time.Now()
	e.ticketProbeJobs = make(map[ticketProbeKey]*ticketProbeJob, ticketProbeMaxJobs)
	oldKey := ticketProbeKey{fileID: 1}
	recentKey := ticketProbeKey{fileID: 2}
	for i := 0; i < ticketProbeMaxJobs; i++ {
		key := ticketProbeKey{fileID: int64(i + 1)}
		job := &ticketProbeJob{done: make(chan struct{})}
		switch key {
		case oldKey:
			job.finished = now.Add(-20 * time.Second)
			job.probe = videoProbe("h264")
			close(job.done)
		case recentKey:
			job.finished = now.Add(-time.Second)
			job.probe = videoProbe("h264")
			close(job.done)
		}
		e.ticketProbeJobs[key] = job
	}
	file := testFile()
	file.ID = ticketProbeMaxJobs + 1
	probe, err := e.ProbeTransient(context.Background(), file, fakeDriver{})
	if err != nil || probe == nil {
		t.Fatalf("probe with finished registry entries = %v, %v", probe, err)
	}

	e.ticketProbeMu.Lock()
	defer e.ticketProbeMu.Unlock()
	if _, ok := e.ticketProbeJobs[oldKey]; ok {
		t.Fatal("oldest finished result was not evicted")
	}
	if _, ok := e.ticketProbeJobs[recentKey]; !ok {
		t.Fatal("newer unexpired result was evicted with the oldest one")
	}
	if _, ok := e.ticketProbeJobs[ticketProbeJobKey(file)]; !ok {
		t.Fatal("new probe result was not registered")
	}
	if got := len(e.ticketProbeJobs); got != ticketProbeMaxJobs {
		t.Fatalf("ticket probe registry size = %d, want %d",
			got, ticketProbeMaxJobs)
	}
}

func TestTicketProbeRegistryReprobesExpiredResult(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		calls.Add(1)
		return videoProbe("hevc"), nil
	})
	file := testFile()
	key := ticketProbeJobKey(file)
	expired := &ticketProbeJob{
		done:     make(chan struct{}),
		finished: time.Now().Add(-ticketProbeResultTTL),
		probe:    videoProbe("h264"),
	}
	close(expired.done)
	e.ticketProbeJobs[key] = expired

	probe, err := e.ProbeTransient(context.Background(), file, fakeDriver{})
	if err != nil || probe == nil || probe.Video() == nil {
		t.Fatalf("probe after TTL expiry = %v, %v", probe, err)
	}
	if probe.Video().CodecName != "hevc" {
		t.Fatalf("expired result reused codec %q, want a fresh probe",
			probe.Video().CodecName)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("probe passes after TTL expiry = %d, want 1", got)
	}
}

func TestTicketProbeExecutionConcurrencyIsBounded(t *testing.T) {
	const total = ticketProbeMaxConcurrent * 3
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	started := make(chan struct{}, total)
	results := make(chan error, total)
	var active atomic.Int32
	var maxActive atomic.Int32
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		nowActive := active.Add(1)
		for {
			current := maxActive.Load()
			if nowActive <= current ||
				maxActive.CompareAndSwap(current, nowActive) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		return videoProbe("h264"), nil
	})

	for i := 0; i < total; i++ {
		file := testFile()
		file.ID = int64(i + 100)
		go func() {
			_, err := e.ProbeTransient(
				context.Background(), file, fakeDriver{})
			results <- err
		}()
	}
	for i := 0; i < ticketProbeMaxConcurrent; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("ticket probes did not reach the configured concurrency")
		}
	}
	tooMany := false
	select {
	case <-started:
		tooMany = true
	case <-time.After(75 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < total; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("bounded ticket probe %d failed: %v", i, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("bounded ticket probes did not finish")
		}
	}
	if tooMany {
		t.Fatalf("ticket probe concurrency exceeded %d", ticketProbeMaxConcurrent)
	}
	if got := maxActive.Load(); got > ticketProbeMaxConcurrent {
		t.Fatalf("observed ticket probe concurrency = %d, limit %d",
			got, ticketProbeMaxConcurrent)
	}
}

func TestSetBinsPublishesCompleteToolSnapshots(t *testing.T) {
	e := newTestEngine(t, func(
		context.Context, *Prober, string, map[string]string, string,
	) (*Probe, error) {
		return videoProbe("h264"), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			e.SetBins(
				fmt.Sprintf("ffmpeg-%d", i),
				fmt.Sprintf("ffprobe-%d", i))
		}(i)
		go func() {
			defer wg.Done()
			if p := e.currentProber(); p == nil || p.FFProbe == "" {
				t.Error("published prober is incomplete")
			}
			if f := e.currentFFMpeg(); f == nil || f.Path == "" {
				t.Error("published ffmpeg is incomplete")
			}
		}()
	}
	wg.Wait()
	e.SetBins("ffmpeg-final", "ffprobe-final")
	if got := e.currentProber().FFProbe; got != "ffprobe-final" {
		t.Fatalf("final ffprobe = %q", got)
	}
	if got := e.currentFFMpeg().Path; got != "ffmpeg-final" {
		t.Fatalf("final ffmpeg = %q", got)
	}
}

func TestStartProbeCooldownThenRetry(t *testing.T) {
	var passes atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		passes.Add(1)
		return nil, errors.New("源请求失败")
	})
	file := testFile()

	jobOf := func() *probeJob {
		e.probeMu.Lock()
		defer e.probeMu.Unlock()
		return e.probeJobs[file.ID]
	}

	e.StartProbe(file, fakeDriver{})
	st := waitState(t, e, file.ID, "failed")
	if st.Error == "" {
		t.Fatal("failed job must carry the reason")
	}
	// A failing probe costs two passes (shallow, then the deep fallback).
	if got := passes.Load(); got != 2 {
		t.Fatalf("ffprobe passes for one failed job = %d, want 2", got)
	}
	first := jobOf()

	// A retry inside the cooldown must not start a new job.
	e.StartProbe(testFile(), fakeDriver{})
	time.Sleep(50 * time.Millisecond)
	if jobOf() != first {
		t.Fatal("a retry inside the cooldown must reuse the failed job, not start a new one")
	}
	if got := passes.Load(); got != 2 {
		t.Fatalf("ffprobe passes = %d, want 2 inside the cooldown", got)
	}

	// Age the finished job past the cooldown: the retry must be allowed now.
	e.probeMu.Lock()
	e.probeJobs[file.ID].finished = time.Now().Add(-2 * failedProbeCooldown)
	e.probeMu.Unlock()
	e.StartProbe(testFile(), fakeDriver{})
	waitState(t, e, file.ID, "failed")
	if jobOf() == first {
		t.Fatal("after the cooldown a retry must start a fresh job")
	}
	if got := passes.Load(); got != 4 {
		t.Fatalf("ffprobe passes = %d, want 4 (two failed jobs)", got)
	}
}

func TestProbeWithoutVideoStreamFailsInsteadOfLooping(t *testing.T) {
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		return &Probe{Streams: []ProbeStream{{Index: 0, CodecType: "audio", CodecName: "aac"}}}, nil
	})
	file := testFile()
	e.StartProbe(file, fakeDriver{})

	st := waitState(t, e, file.ID, "failed")
	if st.Error != errNoVideoStream.Error() {
		t.Fatalf("error = %q, want %q", st.Error, errNoVideoStream)
	}
	// The probe row is still stored (it is real information), but it must not be
	// accepted as playable — otherwise /api/play would re-probe forever.
	if file.Probe == "" {
		t.Fatal("the probe should still be persisted for diagnosis")
	}
	if ProbeFromCache(file) != nil {
		t.Fatal("an audio-only probe must not count as playable")
	}
}

// ---------- shallow window + deep fallback ----------

func TestProbeUsesConfiguredWindowAndFallsBackToDeep(t *testing.T) {
	type call struct{ sizeMB, analyzeSec int }
	var mu sync.Mutex
	var got []call

	e := newTestEngine(t, func(_ context.Context, p *Prober, _ string, _ map[string]string, _ string) (*Probe, error) {
		mu.Lock()
		got = append(got, call{p.ProbeSizeMB, p.AnalyzeSec})
		mu.Unlock()
		if p.ProbeSizeMB == deepProbeSizeMB {
			return videoProbe("hevc"), nil // only the deep window finds the stream
		}
		return &Probe{}, nil
	})
	e.Cfg.Playback.ProbeSizeMB = 13
	e.Cfg.Playback.ProbeAnalyzeSec = 7
	e.SetTools(e.Cfg.FFmpeg, e.Cfg.FFProbe, 13, 7)
	// probeWithFallback must use the immutable tool snapshot, not mutable Cfg.
	e.Cfg.Playback.ProbeSizeMB = 8
	e.Cfg.Playback.ProbeAnalyzeSec = 4

	file := testFile()
	e.StartProbe(file, fakeDriver{})
	if st := waitState(t, e, file.ID, "ready"); st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []call{{13, 7}, {deepProbeSizeMB, deepProbeAnalyzeSec}}
	if len(got) != len(want) {
		t.Fatalf("ffprobe passes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pass %d window = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestProbeStopsAtShallowWindowWhenItSuffices(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	e.Cfg.Playback.ProbeSizeMB = 8
	e.Cfg.Playback.ProbeAnalyzeSec = 4
	e.SetTools(e.Cfg.FFmpeg, e.Cfg.FFProbe, 8, 4)

	file := testFile()
	e.StartProbe(file, fakeDriver{})
	waitState(t, e, file.ID, "ready")
	if calls.Load() != 1 {
		t.Fatalf("ffprobe passes = %d, want 1 (no unnecessary deep retry)", calls.Load())
	}
}

// ---------- synchronous path joins the async job ----------

func TestEnsureProbeJoinsInflightBackgroundProbe(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return videoProbe("hevc"), nil
	})
	file := testFile()
	e.StartProbe(file, fakeDriver{})
	<-entered

	// The HLS/subtitle handlers call EnsureProbe; it must wait for the job that
	// is already running rather than probing the source a second time.
	type res struct {
		probe *Probe
		err   error
	}
	done := make(chan res, 1)
	go func() {
		p, err := e.EnsureProbe(context.Background(), testFile(), fakeDriver{})
		done <- res{p, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("EnsureProbe returned before the probe finished: %+v", r)
	case <-time.After(80 * time.Millisecond):
	}
	close(release)

	r := <-done
	if r.err != nil {
		t.Fatalf("EnsureProbe error: %v", r.err)
	}
	if r.probe == nil || r.probe.Video() == nil {
		t.Fatal("EnsureProbe must return the shared probe")
	}
	if calls.Load() != 1 {
		t.Fatalf("ffprobe passes = %d, want 1", calls.Load())
	}
}

func TestEnsureProbeHonoursContextCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	e := newTestEngine(t, func(ctx context.Context, _ *Prober, _ string, _ map[string]string, _ string) (*Probe, error) {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
		return videoProbe("h264"), nil
	})
	file := testFile()
	e.StartProbe(file, fakeDriver{})
	<-entered

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.EnsureProbe(ctx, testFile(), fakeDriver{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	close(release)
}

// ---------- background pre-probe queue ----------

func TestPreProbeSkipsProbedFilesAndCountsProgress(t *testing.T) {
	var calls atomic.Int32
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		calls.Add(1)
		return videoProbe("h264"), nil
	})
	e.Cfg.Playback.PreProbeConcurrency = 2
	root := t.TempDir()
	srcID := addLocalSource(t, e, root)
	if err := os.WriteFile(filepath.Join(root, "b.mkv"), []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}

	cached := testFile()
	cached.ID, cached.SourceID, cached.Path = 1, srcID, "/a.mp4"
	cached.Probe = `{"format":{"format_name":"mov"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}`
	fresh := testFile()
	fresh.ID, fresh.SourceID, fresh.Path = 2, srcID, "/b.mkv"

	e.PreProbeFiles([]*models.MediaFile{cached, fresh})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.PreProbeStatus(); st.Done == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := e.PreProbeStatus()
	if st.Done != 1 {
		t.Fatalf("pre-probe done = %d, want 1 (cached file must be skipped): %+v", st.Done, st)
	}
	if st.Failed != 0 {
		t.Fatalf("pre-probe failed = %d, want 0 (last error: %s)", st.Failed, st.LastError)
	}
	if st.Queued != 0 {
		t.Fatalf("pre-probe queued = %d, want 0 once the queue drained", st.Queued)
	}
	if st.Running {
		t.Fatal("pre-probe must report not running once the queue drained")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ffprobe passes = %d, want 1 (only the unprobed file)", got)
	}
}

func TestPreProbeCountsFailures(t *testing.T) {
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		return nil, fmt.Errorf("网盘取直链失败")
	})
	f := testFile()
	f.SourceID = addLocalSource(t, e, t.TempDir())
	e.PreProbeFiles([]*models.MediaFile{f})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := e.PreProbeStatus(); st.Failed == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st := e.PreProbeStatus()
	if st.Failed != 1 {
		t.Fatalf("pre-probe failed = %d, want 1: %+v", st.Failed, st)
	}
	if st.LastError == "" {
		t.Fatal("a failed pre-probe must report why")
	}
}
