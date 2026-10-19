package media

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	e.Cfg.Playback.ProbeSizeMB = 8
	e.Cfg.Playback.ProbeAnalyzeSec = 4

	file := testFile()
	e.StartProbe(file, fakeDriver{})
	if st := waitState(t, e, file.ID, "ready"); st.Error != "" {
		t.Fatalf("unexpected error: %s", st.Error)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []call{{8, 4}, {deepProbeSizeMB, deepProbeAnalyzeSec}}
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
	srcID := addLocalSource(t, e, t.TempDir())

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
