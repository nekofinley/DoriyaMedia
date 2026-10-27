package media

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"doriyamedia/internal/config"
	"doriyamedia/internal/models"
)

func TestSetToolsPublishesWindowSnapshotAndSetBinsPreservesIt(t *testing.T) {
	e := NewEngine(nil, config.Defaults())
	e.SetTools("ffmpeg-tools", "ffprobe-tools", 31, 17)

	prober, ffmpeg := e.currentTools()
	if prober == nil || ffmpeg == nil {
		t.Fatal("SetTools published an incomplete snapshot")
	}
	if prober.FFProbe != "ffprobe-tools" || ffmpeg.Path != "ffmpeg-tools" {
		t.Fatalf("tool snapshot = prober %q, ffmpeg %q",
			prober.FFProbe, ffmpeg.Path)
	}
	if prober.ProbeSizeMB != 31 || prober.AnalyzeSec != 17 {
		t.Fatalf("probe window = %dMB/%ds, want 31MB/17s",
			prober.ProbeSizeMB, prober.AnalyzeSec)
	}

	e.SetBins("ffmpeg-bins", "ffprobe-bins")
	prober, ffmpeg = e.currentTools()
	if prober == nil || ffmpeg == nil {
		t.Fatal("SetBins published an incomplete snapshot")
	}
	if prober.FFProbe != "ffprobe-bins" || ffmpeg.Path != "ffmpeg-bins" {
		t.Fatalf("SetBins snapshot = prober %q, ffmpeg %q",
			prober.FFProbe, ffmpeg.Path)
	}
	if prober.ProbeSizeMB != 31 || prober.AnalyzeSec != 17 {
		t.Fatalf("SetBins changed the probe window to %dMB/%ds",
			prober.ProbeSizeMB, prober.AnalyzeSec)
	}
}

func TestSetBinsAndSetToolsPublishAtomicToolSnapshots(t *testing.T) {
	e := NewEngine(nil, config.Defaults())
	const iterations = 400

	start := make(chan struct{})
	var writers sync.WaitGroup
	var reader sync.WaitGroup
	var writersDone atomic.Bool
	var reportOnce sync.Once
	var firstErr string

	report := func(format string, args ...any) {
		reportOnce.Do(func() {
			firstErr = fmt.Sprintf(format, args...)
		})
	}

	writers.Add(2)
	go func() {
		defer writers.Done()
		<-start
		for i := 0; i < iterations; i++ {
			e.SetBins(
				fmt.Sprintf("ffmpeg-bins-%d", i),
				fmt.Sprintf("ffprobe-bins-%d", i),
			)
		}
	}()
	go func() {
		defer writers.Done()
		<-start
		for i := 0; i < iterations; i++ {
			e.SetTools(
				fmt.Sprintf("ffmpeg-tools-%d", i),
				fmt.Sprintf("ffprobe-tools-%d", i),
				16+i,
				8+i,
			)
		}
	}()

	reader.Add(1)
	go func() {
		defer reader.Done()
		<-start
		for !writersDone.Load() {
			prober, ffmpeg := e.currentTools()
			switch {
			case prober == nil || ffmpeg == nil:
				report("reader observed an incomplete tool snapshot")
			default:
				if toolIndex, ok := toolNumber(ffmpeg.Path, "ffmpeg-tools-"); ok {
					probeIndex, probeOK := toolNumber(prober.FFProbe, "ffprobe-tools-")
					if !probeOK || toolIndex != probeIndex {
						report("mixed tools snapshot: ffmpeg %q, ffprobe %q",
							ffmpeg.Path, prober.FFProbe)
					} else if prober.ProbeSizeMB != 16+toolIndex ||
						prober.AnalyzeSec != 8+toolIndex {
						report("snapshot %d window = %dMB/%ds",
							toolIndex, prober.ProbeSizeMB, prober.AnalyzeSec)
					}
				} else if toolIndex, ok := toolNumber(ffmpeg.Path, "ffmpeg-bins-"); ok {
					probeIndex, probeOK := toolNumber(prober.FFProbe, "ffprobe-bins-")
					if !probeOK || toolIndex != probeIndex {
						report("mixed bins snapshot: ffmpeg %q, ffprobe %q",
							ffmpeg.Path, prober.FFProbe)
					}
					if prober.ProbeSizeMB <= 0 || prober.AnalyzeSec <= 0 {
						report("bins snapshot has incomplete probe window")
					}
				}
			}
			runtime.Gosched()
		}
	}()

	close(start)
	writers.Wait()
	writersDone.Store(true)
	reader.Wait()
	if firstErr != "" {
		t.Fatal(firstErr)
	}

	e.SetTools("ffmpeg-final", "ffprobe-final", 47, 23)
	prober, ffmpeg := e.currentTools()
	if prober.FFProbe != "ffprobe-final" || ffmpeg.Path != "ffmpeg-final" ||
		prober.ProbeSizeMB != 47 || prober.AnalyzeSec != 23 {
		t.Fatalf("final snapshot = prober %q/%dMB/%ds, ffmpeg %q",
			prober.FFProbe, prober.ProbeSizeMB, prober.AnalyzeSec, ffmpeg.Path)
	}
}

func toolNumber(path, prefix string) (int, bool) {
	if !strings.HasPrefix(path, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(path, prefix))
	return n, err == nil
}

func TestInvalidateSourceCachesPreservesLocksAndOtherSources(t *testing.T) {
	e := NewEngine(nil, config.Defaults())
	sourceOne := &models.MediaFile{
		ID: 1, SourceID: 1, Path: "/one.mkv", Size: 10, MTime: 20,
	}
	sourceOneNewer := &models.MediaFile{
		ID: 1, SourceID: 1, Path: "/one.mkv", Size: 11, MTime: 21,
	}
	sourceTwo := &models.MediaFile{
		ID: 2, SourceID: 2, Path: "/two.mkv", Size: 30, MTime: 40,
	}
	sourceTen := &models.MediaFile{
		ID: 10, SourceID: 10, Path: "/ten.mkv", Size: 50, MTime: 60,
	}

	oneKey := resolveKey(sourceOne)
	oneNewerKey := resolveKey(sourceOneNewer)
	twoKey := resolveKey(sourceTwo)
	tenKey := resolveKey(sourceTen)
	onePartsKey := partsForKey(sourceOne, nil)
	oneNewerPartsKey := partsForKey(sourceOneNewer, nil)
	twoPartsKey := partsForKey(sourceTwo, nil)
	tenPartsKey := partsForKey(sourceTen, nil)

	e.resolveCache[oneKey] = resolveEntry{target: "old-one"}
	e.resolveCache[oneNewerKey] = resolveEntry{target: "old-one-newer"}
	e.resolveCache[twoKey] = resolveEntry{target: "keep-two"}
	e.resolveCache[tenKey] = resolveEntry{target: "keep-ten"}
	e.partsCache[onePartsKey] = partsEntry{}
	e.partsCache[oneNewerPartsKey] = partsEntry{}
	e.partsCache[twoPartsKey] = partsEntry{}
	e.partsCache[tenPartsKey] = partsEntry{}

	oneLock := newKeyedMutex()
	oneNewerLock := newKeyedMutex()
	twoLock := newKeyedMutex()
	tenLock := newKeyedMutex()
	e.resolveLocks[oneKey] = oneLock
	e.resolveLocks[oneNewerKey] = oneNewerLock
	e.resolveLocks[twoKey] = twoLock
	e.resolveLocks[tenKey] = tenLock

	e.InvalidateSourceCaches(1)

	for _, key := range []string{oneKey, oneNewerKey} {
		if _, ok := e.resolveCache[key]; ok {
			t.Fatalf("resolve cache %q survived source invalidation", key)
		}
	}
	for _, key := range []string{onePartsKey, oneNewerPartsKey} {
		if _, ok := e.partsCache[key]; ok {
			t.Fatalf("parts cache %q survived source invalidation", key)
		}
	}
	for _, key := range []string{twoKey, tenKey} {
		if _, ok := e.resolveCache[key]; !ok {
			t.Fatalf("unrelated resolve cache %q was removed", key)
		}
	}
	for _, key := range []string{twoPartsKey, tenPartsKey} {
		if _, ok := e.partsCache[key]; !ok {
			t.Fatalf("unrelated parts cache %q was removed", key)
		}
	}
	if e.resolveLocks[oneKey] != oneLock ||
		e.resolveLocks[oneNewerKey] != oneNewerLock {
		t.Fatal("source invalidation replaced a per-file resolution lock")
	}
	if e.resolveLocks[twoKey] != twoLock || e.resolveLocks[tenKey] != tenLock {
		t.Fatal("source invalidation changed an unrelated per-file lock")
	}
}
