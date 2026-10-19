package media

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The first play of a cold file resolves the source twice — once to probe, once to
// package — and a cloud drive may invalidate the previous link when a new one is
// issued. The packager then fails holding a link that has just been refused, and
// the play fails for a reason the viewer's own retry fixes. This is that retry.
func TestRetryWithFreshLinkRecoversARefusedLink(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	file := testFile()

	inputs, err := e.SegmentInputs(context.Background(), file, drv, "")
	if err != nil {
		t.Fatalf("SegmentInputs: %v", err)
	}
	if len(inputs) == 0 || !strings.HasSuffix(inputs[0].Target, "link-1") {
		t.Fatalf("first link = %+v, want link-1", inputs)
	}

	var tried []string
	err = RetryWithFreshLink(context.Background(), e, file, drv, inputs, func(in []Input) error {
		tried = append(tried, in[0].Target)
		if strings.HasSuffix(in[0].Target, "link-1") {
			return errors.New("Error opening input: Server returned 403 Forbidden (access denied)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("must recover with a fresh link, got: %v", err)
	}
	if len(tried) != 2 || !strings.HasSuffix(tried[0], "link-1") || !strings.HasSuffix(tried[1], "link-2") {
		t.Fatalf("attempted %v, want the original link then a freshly minted one", tried)
	}
	if c := drv.count(); c != 2 {
		t.Fatalf("source resolutions = %d, want 2", c)
	}
}

// A second failure is the answer: retrying forever would turn a broken file into a
// request that never returns.
func TestRetryWithFreshLinkReportsTheSecondFailure(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	file := testFile()
	inputs, err := e.SegmentInputs(context.Background(), file, drv, "")
	if err != nil {
		t.Fatalf("SegmentInputs: %v", err)
	}

	calls := 0
	err = RetryWithFreshLink(context.Background(), e, file, drv, inputs, func([]Input) error {
		calls++
		return errors.New("second failure")
	})
	if err == nil {
		t.Fatal("want an error when the retry fails too")
	}
	if calls != 2 {
		t.Fatalf("attempts = %d, want 2", calls)
	}
	if !strings.Contains(err.Error(), "second failure") {
		t.Fatalf("the error should be the retry's: %v", err)
	}
}

// Work that succeeded must not be repeated, and a cancelled request must not start
// more of it against the source.
func TestRetryWithFreshLinkLeavesSuccessAndCancellationAlone(t *testing.T) {
	drv := &stubDriver{}
	e := newTestEngine(t, nil)
	file := testFile()
	inputs, err := e.SegmentInputs(context.Background(), file, drv, "")
	if err != nil {
		t.Fatalf("SegmentInputs: %v", err)
	}

	calls := 0
	if err := RetryWithFreshLink(context.Background(), e, file, drv, inputs, func([]Input) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1 for a successful attempt", calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	wantErr := errors.New("client went away")
	if err := RetryWithFreshLink(ctx, e, file, drv, inputs, func([]Input) error {
		calls++
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want the original failure", err)
	}
	if calls != 1 {
		t.Fatalf("attempts = %d, want 1: a cancelled request must not be retried", calls)
	}
	if c := drv.count(); c != 1 {
		t.Fatalf("source resolutions = %d, want 1: no needless re-mint after cancellation", c)
	}
}
