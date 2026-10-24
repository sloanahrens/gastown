package session

import (
	"strings"
	"sync"
	"time"
)

const (
	// SandboxDeadGrace is how long a sandbox's gateway phase may read as a
	// terminal failure before its seat counts as dead (gt-tj446).
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

// sandboxPhaseWatch remembers when each sandbox's gateway phase first read as a
// terminal failure, so the seat is dead only once the failure has lasted
// SandboxDeadGrace. It is memory, not state on disk: a daemon restart starts
// every clock again.
type sandboxPhaseWatch struct {
	mu    sync.Mutex
	since map[string]time.Time
}

func newSandboxPhaseWatch() *sandboxPhaseWatch {
	return &sandboxPhaseWatch{since: map[string]time.Time{}}
}

// failing records that name's phase read as a terminal failure at now and
// returns how long it has read that way, this reading included.
func (w *sandboxPhaseWatch) failing(name string, now time.Time) time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	first, ok := w.since[name]
	if !ok {
		w.since[name] = now
		return 0
	}
	if failing := now.Sub(first); failing > 0 {
		return failing
	}
	return 0
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
