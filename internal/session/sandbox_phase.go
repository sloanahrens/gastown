package session

import (
	"strings"
	"sync"
	"time"
)

const (
	// SandboxDeadGrace is how long after a sandbox's gateway phase turns a
	// terminal failure its seat counts as dead, however late a patrol first
	// reads that phase (gt-tj446, gt-j18af).
	SandboxDeadGrace = 5 * time.Minute
	// sandboxPhaseTimeout bounds one gateway phase question.
	sandboxPhaseTimeout = 15 * time.Second
)

// sandboxPhaseTerminal reports whether phase is one a sandbox does not leave on
// its own: Error and Failed are failures. Provisioning, Pending, Starting,
// Ready and Stopped are not, so a sandbox coming up or stopped on purpose keeps
// its existing grace.
func sandboxPhaseTerminal(phase string) bool {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "error", "failed":
		return true
	}
	return false
}

// sandboxPhaseWatch remembers when each sandbox's terminal failure began, so
// the seat is dead only once the failure has lasted SandboxDeadGrace. The
// gateway's own transition time seeds it when it is readable; otherwise the
// first reading that saw the failure does. It is memory, not state on disk: a
// daemon restart starts every clock again.
type sandboxPhaseWatch struct {
	mu    sync.Mutex
	since map[string]time.Time
}

func newSandboxPhaseWatch() *sandboxPhaseWatch {
	return &sandboxPhaseWatch{since: map[string]time.Time{}}
}

// failing records that name's phase read as a terminal failure at now and
// returns how long it has read that way, this reading included.
//
// since is when the gateway says the sandbox stopped being Ready, and the
// gateway's word is taken as the start of the failure. The earlier of since
// and the start already recorded wins, on any reading, so a clock that began
// at a late first reading moves back to the gateway's time (gt-j18af). A
// stale since is therefore trusted too: on the first reading it can already
// have spent the grace, and the seat is dead at once. A zero or future since
// is ignored; with no start recorded the clock starts at now.
func (w *sandboxPhaseWatch) failing(name string, since, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	start, ok := w.since[name]
	if !since.IsZero() && !since.After(now) && (!ok || since.Before(start)) {
		start, ok = since, true
	}
	if !ok {
		start = now
	}
	if start.After(now) {
		// A clock that reads backwards is never a failure duration, and the
		// start the watch recorded stays where it was.
		return 0
	}
	w.since[name] = start
	return now.Sub(start)
}

// forget drops name's clock: the sandbox answered, or its phase is no longer a
// terminal failure.
func (w *sandboxPhaseWatch) forget(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.since, name)
}

// sandboxPhases is the process's watch. A backend built from a registry record
// (recordBackend) carries none of its own, so the daemon's patrol judges one
// sandbox across ticks (gt-tj446). Anything outside that process — the pool
// roster runs in its own — reads the verdict the patrol recorded for the seat,
// not the clock, so the two can never disagree about one sandbox.
var sandboxPhases = newSandboxPhaseWatch()
