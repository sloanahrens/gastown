package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
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
// sandbox across ticks (gt-tj446).
var sandboxPhases = newSandboxPhaseWatch()

// SandboxPhaseTerminal reports whether the gateway reports session's sandbox in
// a terminal failure phase: Error or Failed. A session with no registry record
// is a host session, and a gateway that cannot answer is an error rather than a
// "no", so a caller keeps the seat on one.
func SandboxPhaseTerminal(townRoot, session string) (bool, error) {
	rec, ok, err := ReadSandboxRecord(townRoot, session)
	if err != nil || !ok {
		return false, err
	}
	return sandboxPhaseTerminalWith(rec.Gateway, session, askSandboxPhase)
}

// sandboxPhaseTerminalWith is SandboxPhaseTerminal over the gateway question.
func sandboxPhaseTerminalWith(gateway, session string, phaseOf func(gateway, session string) (string, error)) (bool, error) {
	phase, err := phaseOf(gateway, session)
	if err != nil {
		return false, fmt.Errorf("asking the gateway for the phase of sandbox %s: %w", session, err)
	}
	return sandboxPhaseTerminal(phase), nil
}

// askSandboxPhase is the gateway question the roster asks, bounded well under
// the two minutes an openshell call may otherwise take, so an unreachable
// gateway cannot hold a pool read for the whole of it.
func askSandboxPhase(gateway, session string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxPhaseTimeout)
	defer cancel()
	st, err := (&sandbox.CLI{Gateway: gateway}).Get(ctx, session)
	if err != nil {
		return "", err
	}
	return st.Phase, nil
}
