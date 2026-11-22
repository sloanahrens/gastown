package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/liveness"
)

// TestReviveCutCooldownHoldsASecondCut: a seat revived because its model stream was cut and cut
// again 10 minutes later is skipped with the cooldown's detail and restarted no second time, so
// only the overrun stop (which measures from the revival, 20 minutes on) ends a seat that keeps
// getting cut; once the 30-minute cooldown has passed the seat is revived again (gt-08vkyk).
func TestReviveCutCooldownHoldsASecondCut(t *testing.T) {
	t.Parallel()
	base := newFake()
	liveQuiet(base, "ruby", 6*time.Minute)
	env := &cutFake{fakeEnv: base, cut: map[string]bool{"ruby": true}, cutErr: map[string]error{}}
	at := now
	s := New(env, memLedger{}, Options{
		Now:       func() time.Time { return at },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		SeatCap:   base.seatCapFor,
	})

	r := s.Tick("gastown")
	if len(env.restarts) != 1 {
		t.Fatalf("restarts = %v, want the one revival; report %v", env.restarts, r.Lines())
	}

	at = now.Add(10 * time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 10 * time.Minute}
	r = s.Tick("gastown")
	f := seatFinding(t, r, "ruby")
	if len(env.restarts) != 1 || f.Outcome != OutcomeSkipped || !strings.Contains(f.Detail, "cooldown holds") {
		t.Fatalf("second cut after 10 minutes: restarts %v, finding %q %q; want skipped by the cooldown and no second restart", env.restarts, f.Outcome, f.Detail)
	}

	at = now.Add(reviveCooldown + time.Minute)
	env.verdicts["ruby"] = liveness.Result{Verdict: liveness.Alive, Reason: "quiet", QuietFor: 10 * time.Minute}
	r = s.Tick("gastown")
	if len(env.restarts) != 2 {
		t.Fatalf("cut again after the cooldown: restarts = %v, want the seat revived again; report %v", env.restarts, r.Lines())
	}
}
