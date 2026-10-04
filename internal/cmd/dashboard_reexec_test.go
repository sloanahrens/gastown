package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func startWatch(t *testing.T, first stampResult) *watchHarness {
	t.Helper()
	h := &watchHarness{stamps: make(chan stampResult, 16), ticks: make(chan time.Time), done: make(chan struct{})}
	h.stamps <- first
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
	h := startWatch(t, stampResult{s: v1})
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
	h := startWatch(t, stampResult{s: v1})
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
	h := startWatch(t, stampResult{s: v1})
	h.tick(stampResult{s: v2})
	h.tick(stampResult{err: errors.New("no such file")}) // rename in flight: forget the sighting
	h.tick(stampResult{s: v2})
	if got := h.finish(); got != 0 {
		t.Fatalf("a missing file counted toward a change: fired %d times", got)
	}
}

// A baseline that cannot be read at the start must not switch the watcher off:
// it is read on a later tick and a replacement after that is still seen.
func TestWatchBinaryRecoversFromAFailedBaseline(t *testing.T) {
	t.Parallel()
	v1, v2 := binaryStamp{size: 1}, binaryStamp{size: 2}
	h := startWatch(t, stampResult{err: errors.New("no such file")})
	h.tick(stampResult{err: errors.New("still missing")})
	h.tick(stampResult{s: v1}) // baseline at last
	h.tick(stampResult{s: v2})
	h.tick(stampResult{s: v2})
	<-h.done
	if got := h.finish(); got != 1 {
		t.Fatalf("fired %d times after a late baseline, want 1", got)
	}
}

// stampBinary is the real reader the dashboard runs on: the same file reads
// the same, and a file renamed over it, which is how install replaces the
// binary, reads differently.
func TestStampBinaryTellsAReplacedFileApart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "gt")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := stampBinary(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := stampBinary(path); again != a {
		t.Fatalf("unchanged file stamped differently: %+v vs %+v", a, again)
	}
	next := filepath.Join(dir, "gt.new")
	if err := os.WriteFile(next, []byte("a longer second build"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
	b, err := stampBinary(path)
	if err != nil {
		t.Fatal(err)
	}
	if b == a {
		t.Fatalf("a replaced file kept its stamp: %+v", a)
	}
	if _, err := stampBinary(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing file stamped without error")
	}
}

func TestHandoffKeepsServingWhenTheNewBinaryDoesNotRun(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	execd := false
	err := handoff(
		func() error { return errors.New("signal: killed") },
		func() error { execd = true; return nil },
		&out,
	)
	if err == nil || !strings.Contains(err.Error(), "does not run") {
		t.Fatalf("err = %v, want the new binary does not run", err)
	}
	if execd {
		t.Fatal("exec'd over a binary that failed verification")
	}
	if strings.Contains(out.String(), "restarting") {
		t.Fatalf("announced a restart that did not happen: %q", out.String())
	}
}

func TestHandoffReportsAFailedExec(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := handoff(func() error { return nil }, func() error { return errors.New("exec format error") }, &out)
	if err == nil || !strings.Contains(err.Error(), "re-exec") {
		t.Fatalf("err = %v, want a re-exec failure", err)
	}
}

func TestExecEnvReplacesAStaleListenerFD(t *testing.T) {
	t.Parallel()
	in := []string{"A=1", dashboardListenFDEnv + "=7", "B=2"}
	got := execEnv(in, 9)
	want := []string{"A=1", "B=2", dashboardListenFDEnv + "=9"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("execEnv = %v, want %v", got, want)
	}
	if len(in) != 3 || in[1] != dashboardListenFDEnv+"=7" {
		t.Fatalf("execEnv modified its input: %v", in)
	}
}
