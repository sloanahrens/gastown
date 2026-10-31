package supervisor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/ring/ringtest"
)

// ringKey is the key the supervisor checks the Ring under in these tests.
var ringKey = []byte("supervisor-ring-key")

// ringSup is the harness's supervisor, checking the Ring under ringKey rather
// than the process environment.
func ringSup(h *harness) *Supervisor {
	s := h.sup()
	s.o.RingKey = func(string) ([]byte, error) { return ringKey, nil }
	return s
}

// A Waned Ring refuses Restart and KillStray with ErrEstop, names the wane, and
// lets nothing run (gt-6mlik.5).
func TestRingWanedRefusesRestartAndKillStray(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ringtest.Waned(t, h.town, ringKey, h.now)
	h.tmux.sessions["gt-witness"] = true
	s := ringSup(h)

	err := s.Restart(flint, "dead", "daemon")
	if !errors.Is(err, ErrEstop) {
		t.Fatalf("Restart under a waned Ring = %v, want ErrEstop", err)
	}
	if !strings.Contains(err.Error(), "the Ring waned at") {
		t.Errorf("Restart refusal %q does not name the wane", err)
	}
	if err := s.KillStray("gt-witness", "ghost", "daemon"); !errors.Is(err, ErrEstop) {
		t.Fatalf("KillStray under a waned Ring = %v, want ErrEstop", err)
	}
	if len(h.restarts) != 0 || len(h.tmux.kills()) != 0 {
		t.Fatalf("restarts=%v kills=%v, want none", h.restarts, h.tmux.kills())
	}
}

// A Valid Ring refuses nothing: Restart runs and KillStray kills.
func TestRingValidRunsRestartAndKillStray(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ringtest.Valid(t, h.town, ringKey, h.now)
	h.tmux.sessions["gt-witness"] = true
	s := ringSup(h)

	if err := s.Restart(flint, "dead", "daemon"); err != nil {
		t.Fatalf("Restart under a valid Ring = %v, want nil", err)
	}
	if err := s.KillStray("gt-witness", "ghost", "daemon"); err != nil {
		t.Fatalf("KillStray under a valid Ring = %v, want nil", err)
	}
	if len(h.restarts) != 1 || len(h.tmux.kills()) != 1 {
		t.Fatalf("restarts=%v kills=%v, want one of each", h.restarts, h.tmux.kills())
	}
}

// A town with no ring section changes neither verb: an Absent Ring refuses
// nothing until Absent = off lands with slice 9, after the proving step
// (slice 8).
func TestRingAbsentChangesNeitherVerb(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.tmux.sessions["gt-witness"] = true
	s := ringSup(h)

	if err := s.Restart(flint, "dead", "daemon"); err != nil {
		t.Fatalf("Restart with no ring section = %v, want nil", err)
	}
	if err := s.KillStray("gt-witness", "ghost", "daemon"); err != nil {
		t.Fatalf("KillStray with no ring section = %v, want nil", err)
	}
}

// A ring section that exists but cannot be verified refuses both verbs with the
// reason (gt-ifijm): here the signature does not verify.
func TestRingUnverifiableRefusesRestart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ringtest.Tampered(t, h.town, ringKey, h.now)
	s := ringSup(h)

	err := s.Restart(flint, "dead", "daemon")
	if !errors.Is(err, ErrEstop) {
		t.Fatalf("Restart under a tampered Ring = %v, want ErrEstop", err)
	}
	if !strings.Contains(err.Error(), ring.ReasonBadSignature) {
		t.Errorf("Restart refusal %q does not carry the reason", err)
	}
	if len(h.restarts) != 0 {
		t.Fatalf("restarts = %v, want none", h.restarts)
	}
}

// The Ring is read at the supervisor's clock, so a lease that expires between
// two calls refuses the second.
func TestRingWanesAtTheSupervisorsClock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ringtest.Valid(t, h.town, ringKey, h.now)
	s := ringSup(h)
	if err := s.Restart(flint, "dead", "daemon"); err != nil {
		t.Fatalf("Restart before the lease ends = %v, want nil", err)
	}
	h.now = h.now.Add(ring.Lease + time.Minute)
	if err := s.KillStray("gt-witness", "ghost", "daemon"); !errors.Is(err, ErrEstop) {
		t.Fatalf("KillStray after the lease ends = %v, want ErrEstop", err)
	}
}
