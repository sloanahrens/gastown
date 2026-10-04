package cmd

import (
	"context"
	"errors"
	"testing"
	"time"
)

// watchHarness drives watchBinary one tick at a time. ticks is unbuffered, so a
// send returns only after the watcher has finished the previous tick.
type watchHarness struct {
	stamps  chan stampResult
	ticks   chan time.Time
	done    chan struct{}
	cancel  context.CancelFunc
	changed int
}

type stampResult struct {
	s   binaryStamp
	err error
}

func startWatch(t *testing.T, first binaryStamp) *watchHarness {
	t.Helper()
	h := &watchHarness{stamps: make(chan stampResult, 16), ticks: make(chan time.Time), done: make(chan struct{})}
	h.stamps <- stampResult{s: first}
	var ctx context.Context
	ctx, h.cancel = context.WithCancel(context.Background())
	t.Cleanup(h.cancel)
	stamp := func() (binaryStamp, error) { r := <-h.stamps; return r.s, r.err }
	go func() {
		defer close(h.done)
		watchBinary(ctx, stamp, h.ticks, func() { h.changed++ })
	}()
	return h
}

// tick queues one stamp reading and delivers one tick.
func (h *watchHarness) tick(r stampResult) {
	h.stamps <- r
	h.ticks <- time.Time{}
}

// finish stops the watcher and reports how many times it fired.
func (h *watchHarness) finish() int {
	h.cancel()
	<-h.done
	return h.changed
}

func TestWatchBinaryFiresOnceAfterReplacement(t *testing.T) {
	t.Parallel()
	v1, v2 := binaryStamp{size: 1, ino: 1}, binaryStamp{size: 2, ino: 2}
	h := startWatch(t, v1)
	h.tick(stampResult{s: v1})
	h.tick(stampResult{s: v2}) // first sight of the new build: not yet
	h.tick(stampResult{s: v2}) // held for a second tick: fire
	<-h.done
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times, want 1", got)
	}
}

func TestWatchBinaryWaitsOutAFileStillBeingWritten(t *testing.T) {
	t.Parallel()
	v1, growing, v2 := binaryStamp{size: 1}, binaryStamp{size: 5}, binaryStamp{size: 9}
	h := startWatch(t, v1)
	h.tick(stampResult{s: growing})
	h.tick(stampResult{s: v2}) // size moved again: restart the count
	h.tick(stampResult{s: v1}) // back to the original: forget it
	h.tick(stampResult{s: v1})
	if got := h.finish(); got != 0 {
		t.Fatalf("fired %d times on an unsettled file, want 0", got)
	}
}

func TestWatchBinaryIgnoresMissingFile(t *testing.T) {
	t.Parallel()
	v1, v2 := binaryStamp{size: 1}, binaryStamp{size: 2}
	h := startWatch(t, v1)
	h.tick(stampResult{s: v2})
	h.tick(stampResult{err: errors.New("no such file")}) // rename in flight: forget the sighting
	h.tick(stampResult{s: v2})
	if got := h.finish(); got != 0 {
		t.Fatalf("a missing file counted toward a change: fired %d times", got)
	}
}
