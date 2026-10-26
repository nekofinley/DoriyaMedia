package media

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

const mediaLogCanary = "https://source.invalid/private/movie.mkv?token=TOP_SECRET"

func captureMediaLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return &buf
}

func assertNoMediaLogCanary(t *testing.T, got string) {
	t.Helper()
	for _, secret := range []string{
		mediaLogCanary,
		"source.invalid",
		"TOP_SECRET",
		"Authorization:",
		"/private/movie.mkv",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q:\n%s", secret, got)
		}
	}
}

func TestSegmentFailureLogOmitsSourceDetails(t *testing.T) {
	logs := captureMediaLog(t)
	e := newTestEngine(t, nil)
	e.runStream = func(_ context.Context, args []string, out io.Writer) error {
		if strings.HasSuffix(inputOf(args), "link-1") {
			return errors.New("ffmpeg: " + mediaLogCanary + " headers=Authorization: Basic SECRET")
		}
		_, err := out.Write([]byte("segment"))
		return err
	}

	n, err := e.Segment(
		context.Background(), testFile(), &stubDriver{},
		4, 4, "remux", "copy", 0, 0, 1080, io.Discard,
	)
	if err != nil {
		t.Fatalf("Segment: %v", err)
	}
	if n != int64(len("segment")) {
		t.Fatalf("bytes = %d, want %d", n, len("segment"))
	}
	assertNoMediaLogCanary(t, logs.String())
	if !strings.Contains(logs.String(), "operation failed") {
		t.Fatalf("log should retain a safe failure class:\n%s", logs.String())
	}
}

func TestProbeFailureStatusAndLogOmitSourceDetails(t *testing.T) {
	logs := captureMediaLog(t)
	canaryErr := errors.New("ffprobe: " + mediaLogCanary + " response=Set-Cookie: session=SECRET")
	e := newTestEngine(t, func(context.Context, *Prober, string, map[string]string, string) (*Probe, error) {
		return nil, canaryErr
	})
	file := testFile()
	job := &probeJob{file: file, done: make(chan struct{}), started: time.Now()}
	e.probeMu.Lock()
	e.probeJobs[file.ID] = job
	e.probeMu.Unlock()

	e.runProbeJob(context.Background(), file, &stubDriver{}, job)

	st := e.ProbeStatus(file.ID)
	if st.State != "failed" {
		t.Fatalf("state = %q, want failed", st.State)
	}
	if st.Error != "media probe failed" {
		t.Fatalf("status error = %q, want a safe generic message", st.Error)
	}
	assertNoMediaLogCanary(t, logs.String())
	if !strings.Contains(logs.String(), "operation failed") {
		t.Fatalf("log should retain a safe failure class:\n%s", logs.String())
	}
}
