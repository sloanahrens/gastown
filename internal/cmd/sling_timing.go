package cmd

import (
	"fmt"
	"io"
	"time"
)

// slingTimer prints one line per dispatch step so a slow sling can be
// attributed to worktree creation, admission, convoy creation, formula
// instantiation, the hook write, or session start (gt-llg8). Measured
// 2026-09-19: a convoy-fed sling took 11m15s from feed to tmux session with
// nothing on the path timed.
//
// A nil *slingTimer is a no-op so callers on the spawn path never guard it.
// slingSteps is the timer for the sling command in this process; runSling
// sets it and every seam on the dispatch path calls slingSteps.step(...).
// Other entry points leave it nil, which is a no-op.
var slingSteps *slingTimer

type slingTimer struct {
	w     io.Writer
	now   func() time.Time
	start time.Time
	last  time.Time
}

func newSlingTimer(w io.Writer) *slingTimer {
	return newSlingTimerWithClock(w, time.Now)
}

func newSlingTimerWithClock(w io.Writer, now func() time.Time) *slingTimer {
	t := now()
	return &slingTimer{w: w, now: now, start: t, last: t}
}

// step records the end of the named step: its own duration since the previous
// step (or construction) and the running total since construction.
func (t *slingTimer) step(name string) {
	if t == nil {
		return
	}
	n := t.now()
	fmt.Fprintf(t.w, "[sling] step %s took %s (total %s)\n",
		name, n.Sub(t.last).Round(time.Millisecond), n.Sub(t.start).Round(time.Millisecond))
	t.last = n
}
