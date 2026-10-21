package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/done"
)

// seedOverrunAgent stores gastown/polecats/<name>'s agent bead in the state and
// exit type the test names; stopped-on-escalation is stuck beside ESCALATED.
func seedOverrunAgent(bd *workBD, townRoot, name, state, exit string) {
	bd.db.Seed(beads.Issue{
		ID: beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, "gastown"), "gastown", name),
		Description: beads.FormatAgentDescription(name, &beads.AgentFields{
			RoleType: constants.RolePolecat, Rig: "gastown", AgentState: state, ExitType: exit,
		}),
	})
}

// The tie between a seat and the escalations it raised is the one the stuck-seat
// tick reads (patrolscan.SeatEscalationHold). This pins what the guard reads
// from it: an open escalation the seat raised that names its bead, while the
// seat's record says it stopped on it, holds the clock, and nothing else does.
func TestOverrunGuardEscalationWaiting(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	townRoot := t.TempDir()
	for _, name := range []string{"capable", "titled", "closer", "other", "sloan", "furiosa"} {
		seedOverrunAgent(bd, townRoot, name, "stuck", done.ExitEscalated)
	}
	escalation := func(id, raiser, related, title, status string) beads.Issue {
		return beads.Issue{ID: id, Status: status, Title: title, Labels: []string{"gt:escalation"},
			Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
				Severity: "high", EscalatedBy: raiser, RelatedBead: related}),
		}
	}
	bd.db.Seed(escalation("hq-wisp-open", "gastown/polecats/capable", "gt-esc1", "gt-esc1: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-titled", "gastown/polecats/titled", "", "gt-esc1: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-closed", "gastown/polecats/closer", "gt-esc1", "gt-esc1: answered", "closed"))
	bd.db.Seed(escalation("hq-wisp-other-bead", "gastown/polecats/other", "gt-esc2", "gt-esc2: a decision", "open"))
	bd.db.Seed(escalation("hq-wisp-other-role", "gastown/crew/sloan", "gt-esc1", "gt-esc1: a decision", "open"))
	d := &Daemon{config: &Config{TownRoot: townRoot}, openWorkBeads: bd.open}
	r := newOverrunEscalationReader(d)

	tests := []struct {
		name string
		seat overrunSeat
		want bool
	}{
		{"open escalation naming the bead", overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"}, true},
		{"open escalation named by its title", overrunSeat{Rig: "gastown", Polecat: "titled", Bead: "gt-esc1"}, true},
		{"escalation closed", overrunSeat{Rig: "gastown", Polecat: "closer", Bead: "gt-esc1"}, false},
		{"escalation for another bead", overrunSeat{Rig: "gastown", Polecat: "other", Bead: "gt-esc1"}, false},
		{"escalation raised by another role", overrunSeat{Rig: "gastown", Polecat: "sloan", Bead: "gt-esc1"}, false},
		{"seat with no escalation", overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-esc1"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.waiting(tt.seat)
			if err != nil || got != tt.want {
				t.Errorf("waiting(%+v) = %v, %v; want %v, nil", tt.seat, got, err, tt.want)
			}
		})
	}

	// A failed read is an error, which the guard turns into counting all of the
	// seat's time.
	failing := newWorkBD(t)
	failingRoot := t.TempDir()
	seedOverrunAgent(failing, failingRoot, "capable", "stuck", done.ExitEscalated)
	failing.listErr = errors.New("bd list: connection refused")
	dFailing := &Daemon{config: &Config{TownRoot: failingRoot}, openWorkBeads: failing.open}
	got, err := newOverrunEscalationReader(dFailing).waiting(overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	if err == nil || got {
		t.Errorf("waiting after a failed read = %v, %v; want false and an error", got, err)
	}

	// An unreadable agent record is the same error: the guard counts all of the
	// seat's time rather than guess it is stopped.
	norecord := newWorkBD(t)
	norecord.showErr = errors.New("bd show: connection refused")
	dNoRecord := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: norecord.open}
	got, err = newOverrunEscalationReader(dNoRecord).waiting(overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	if err == nil || got {
		t.Errorf("waiting after a failed record read = %v, %v; want false and an error", got, err)
	}
}

// A live seat that raised an escalation and kept working is not waiting on it:
// gt escalate names the seat and its hooked bead, so the escalation alone ties
// to the seat, but only a record stopped on ESCALATED (stuck beside exit
// ESCALATED) makes it a waiter, as in seatHeldOnEscalation and the tick. Read as
// waiting, its time was banked and it was neither nudged at 12 minutes nor
// stopped at 20.
func TestOverrunGuardEscalationLiveSeatIsNotWaiting(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"}
	tests := []struct {
		name        string
		state, exit string
		want        bool
	}{
		{"working, no exit", "working", "", false},
		{"working with a stale ESCALATED exit", "working", done.ExitEscalated, false},
		{"stuck on another exit", "stuck", done.ExitDeferred, false},
		{"stuck on ESCALATED", "stuck", done.ExitEscalated, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			townRoot := t.TempDir()
			seedOverrunAgent(bd, townRoot, "capable", tt.state, tt.exit)
			title := "gt-esc1: a decision"
			bd.db.Seed(beads.Issue{ID: "hq-wisp-open", Status: "open", Title: title, Labels: []string{"gt:escalation"},
				Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
					Severity: "high", EscalatedBy: "gastown/polecats/capable", RelatedBead: "gt-esc1"}),
			})
			d, _ := newOverrunDaemon(t, false)
			d.config.TownRoot = townRoot
			d.openWorkBeads = bd.open
			f := newOverrunFake(0, seat)
			f.escalationWaiting = newOverrunEscalationReader(d).waiting

			got, err := newOverrunEscalationReader(d).waiting(seat)
			if err != nil || got != tt.want {
				t.Fatalf("waiting = %v, %v; want %v, nil", got, err, tt.want)
			}

			// Twelve minutes of wall time: a live seat is nudged as it would be
			// with no escalation; a stopped one is waiting on a person.
			tickOverrunGuard(d, f, 12, time.Minute)
			if nudged := len(f.nudges) == 1; nudged == tt.want {
				t.Errorf("nudges after 12 minutes = %v, want a nudge only when the seat is not waiting", f.nudges)
			}
		})
	}
}

// An open escalation the seat raised exempts the seat's wait, and the clock
// does not run through it: 25 minutes of wall time behind an unanswered
// question is neither the nudge nor the stop.
func TestRunOverrunGuard_EscalationTimeIsNotCounted(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	f.escalationWaiting = func(overrunSeat) (bool, error) { return true, nil }

	tickOverrunGuard(d, f, 25, time.Minute)

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("a seat waiting on its escalation was nudged %v / stopped %v after 25 minutes, want neither",
			f.nudges, f.stops)
	}
}

// The exemption is capped: an escalation nobody answers holds the clock for
// overrunMaxEscalationTotal only. With the default thresholds that is 40
// minutes, so the nudge falls due at minute 52 and the stop at minute 60.
func TestRunOverrunGuard_UnansweredEscalationResumesTheClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	f.escalationWaiting = func(overrunSeat) (bool, error) { return true, nil }

	var nudgedAt, stoppedAt time.Time
	for i := 1; i <= 75 && stoppedAt.IsZero(); i++ {
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.nudges) > 0 && nudgedAt.IsZero() {
			nudgedAt = f.now
		}
		if len(f.stops) > 0 {
			stoppedAt = f.now
		}
	}
	if want := overrunSlung.Add(52 * time.Minute); !nudgedAt.Equal(want) {
		t.Errorf("nudged at %v of wall time, want %v: 12 minutes past a 40 minute exemption",
			nudgedAt.Sub(overrunSlung), want.Sub(overrunSlung))
	}
	if want := overrunSlung.Add(60 * time.Minute); !stoppedAt.Equal(want) {
		t.Errorf("stopped at %v of wall time, want %v: the escalation exemption is capped at 40 minutes",
			stoppedAt.Sub(overrunSlung), want.Sub(overrunSlung))
	}
}

// When the escalation is closed, the clock runs again: the interval after the
// close is charged to the seat. Waiting through minute 30 banks 29 minutes, so
// the nudge falls due at minute 41 and the stop at minute 49.
func TestRunOverrunGuard_ClosedEscalationReleasesTheClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	waiting := true
	f.escalationWaiting = func(overrunSeat) (bool, error) { return waiting, nil }

	var nudgedAt, stoppedAt time.Time
	for i := 1; i <= 60 && stoppedAt.IsZero(); i++ {
		waiting = i <= 30
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.nudges) > 0 && nudgedAt.IsZero() {
			nudgedAt = f.now
		}
		if len(f.stops) > 0 {
			stoppedAt = f.now
		}
	}
	if want := overrunSlung.Add(41 * time.Minute); !nudgedAt.Equal(want) {
		t.Errorf("nudged at %v of wall time, want %v", nudgedAt.Sub(overrunSlung), want.Sub(overrunSlung))
	}
	if want := overrunSlung.Add(49 * time.Minute); !stoppedAt.Equal(want) {
		t.Errorf("stopped at %v of wall time, want %v: 20 minutes of non-waiting time behind a 29 minute wait",
			stoppedAt.Sub(overrunSlung), want.Sub(overrunSlung))
	}
}

// A seat that cannot have its escalations read counts all of its time, and the
// log names the failure once for the tick, not once per seat.
func TestRunOverrunGuard_UnreadableEscalationCountsAllTime(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute,
		overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"},
		overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-esc2"})
	f.escalationWaiting = func(overrunSeat) (bool, error) {
		return false, errors.New("bd list: connection refused")
	}

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 2 {
		t.Errorf("stops = %v, want both seats stopped: an unreadable escalation state counts all of their time", f.stops)
	}
	if n := strings.Count(buf.String(), "cannot read the escalation state"); n != 1 {
		t.Errorf("the escalation read failure was logged %d times, want once:\n%s", n, buf.String())
	}
}

// A seat inside its gate is not read for escalations: the gate already exempts
// the time, and one stretch is never exempt twice.
func TestRunOverrunGuard_EscalationIsNotReadInsideAGate(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	f.gateRunning = func(overrunSeat) (bool, error) { return true, nil }
	reads := 0
	f.escalationWaiting = func(overrunSeat) (bool, error) {
		reads++
		return true, nil
	}

	tickOverrunGuard(d, f, 10, time.Minute)

	if reads != 0 {
		t.Errorf("escalations read %d times for a seat inside its gate, want none", reads)
	}
}

// The escalation time is subtracted from the same elapsed the gate time is,
// and the stop still fires on what is left.
func TestOverrunGuardEscalationSubtractsFromElapsed(t *testing.T) {
	t.Parallel()
	now := overrunSlung.Add(25 * time.Minute)
	tests := []struct {
		name       string
		escalation time.Duration
		want       overrunAction
	}{
		{"no escalation time: 25 minutes is a stop", 0, overrunStop},
		{"22 minutes left of 25 is a stop", 3 * time.Minute, overrunStop},
		{"19 minutes left of 25 is a nudge", 6 * time.Minute, overrunNudge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideOverrun(overrunInput{
				Now: now, SlungAt: overrunSlung,
				NudgeAt: 12 * time.Minute, StopAt: 20 * time.Minute,
				EscalationSpent: tt.escalation,
			})
			if got != tt.want {
				t.Errorf("decideOverrun = %v, want %v", got, tt.want)
			}
		})
	}
}

// The escalation accounting is per sling and capped, like the gate's: a rework
// re-sling starts a new clock, and the earlier run's escalation time is not this
// run's.
func TestOverrunGuardEscalationIsPerSlingAndCapped(t *testing.T) {
	t.Parallel()
	first := overrunSlung
	second := overrunSlung.Add(time.Hour)
	const maxCharge, maxTotal = 2 * time.Minute, 3 * time.Minute

	st, _ := (overrunState{Bead: "gt-esc1"}).withEscalation(first, first.Add(time.Minute), true, maxCharge, maxTotal)
	var spent time.Duration
	for i := 2; i <= 8; i++ {
		st, spent = st.withEscalation(first, first.Add(time.Duration(i)*time.Minute), true, maxCharge, maxTotal)
	}
	if spent != maxTotal {
		t.Fatalf("escalation time after 7 minutes waiting = %v, want the ceiling %v", spent, maxTotal)
	}

	st, spent = st.withEscalation(second, second.Add(10*time.Second), false, maxCharge, maxTotal)
	if spent != 0 || st.EscalationSpent != 0 || !st.EscalationSeenAt.IsZero() {
		t.Errorf("after a re-sling: spent %v, state %+v; want no escalation accounting", spent, st)
	}
	if st.GateSpent != 0 {
		t.Errorf("escalation accounting moved the gate's: GateSpent = %v", st.GateSpent)
	}
}
