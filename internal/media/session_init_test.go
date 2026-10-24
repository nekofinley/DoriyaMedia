package media

import (
	"context"
	"testing"
	"time"
)

// A media segment must not go out before the init segment has been answered: a
// browser rejects a segment appended ahead of its init (see WaitInitServed).
func TestWaitInitServedHoldsUntilInitIsAnswered(t *testing.T) {
	s := &Session{initServed: make(chan struct{})}
	released := make(chan struct{})
	go func() {
		s.WaitInitServed(context.Background(), 5*time.Second)
		close(released)
	}()
	select {
	case <-released:
		t.Fatal("segment released before the init was answered")
	case <-time.After(60 * time.Millisecond):
	}
	s.MarkInitServed()
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("segment still held after the init was answered")
	}
	// The fence is sticky: a later segment does not wait again.
	start := time.Now()
	s.WaitInitServed(context.Background(), 5*time.Second)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a segment waited %v after the init had already been answered", d)
	}
}

// A session whose init nobody asks for must not stall its segments forever.
func TestWaitInitServedFallsThroughOnTimeout(t *testing.T) {
	s := &Session{initServed: make(chan struct{})}
	start := time.Now()
	s.WaitInitServed(context.Background(), 60*time.Millisecond)
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Fatalf("timeout wait took %v, want about 60ms", d)
	}
}

// A cancelled request stops waiting at once.
func TestWaitInitServedStopsWhenRequestCancelled(t *testing.T) {
	s := &Session{initServed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	s.WaitInitServed(ctx, 5*time.Second)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("cancelled wait took %v", d)
	}
}

// A zero-value session (no fence) must neither block nor panic.
func TestWaitInitServedWithoutFenceDoesNotBlock(t *testing.T) {
	s := &Session{}
	start := time.Now()
	s.WaitInitServed(context.Background(), 5*time.Second)
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a session without the fence blocked for %v", d)
	}
	s.MarkInitServed()
}
