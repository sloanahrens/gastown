package sling

import (
	"fmt"
	"io"
	"time"
)

// StepPrefix starts every line a Timer writes. A caller that reads the timing
// back out of the dispatch's output — the daemon's scheduled-sling runner,
// which strips them from its own buffer — matches on it, so it lives here
// beside the writer rather than in each reader.
const StepPrefix = "[sling] step "

// Timer prints one line per dispatch step so a slow dispatch can be attributed
// to admission, allocation, worktree creation, the hook write, or session
// start (gt-llg8). Measured 2026-09-19: a sling took 11m15s from sling to tmux
// session with nothing on the path timed.
//
// A nil *Timer is a no-op, so callers on the spawn path never guard it.
type Timer struct {
	w     io.Writer
	now   func() time.Time
	start time.Time
	last  time.Time
}

// NewTimer starts a timer writing its lines to w.
func NewTimer(w io.Writer) *Timer {
	return NewTimerWithClock(w, time.Now)
}

// NewTimerWithClock is NewTimer over a caller-supplied clock, so a test can
// pin the durations it asserts on.
func NewTimerWithClock(w io.Writer, now func() time.Time) *Timer {
	t := now()
	return &Timer{w: w, now: now, start: t, last: t}
}

// Step records the end of the named step: its own duration since the previous
// step (or construction) and the running total since construction.
func (t *Timer) Step(name string) {
	if t == nil {
		return
	}
	n := t.now()
	fmt.Fprintf(t.w, StepPrefix+"%s took %s (total %s)\n",
		name, n.Sub(t.last).Round(time.Millisecond), n.Sub(t.start).Round(time.Millisecond))
	t.last = n
}

// Steps is Step as a callback, for passing down a call chain that must not
// know where its timing goes. A nil Timer yields a nil Steps, which every
// caller already treats as "no timer".
func (t *Timer) Steps() func(name string) {
	if t == nil {
		return nil
	}
	return t.Step
}
