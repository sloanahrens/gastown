package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/done"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
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
	// otherRoleSeat names one seat in the agent record, in the escalation's
	// raiser and in the seat under test: keeping it one constant is what makes
	// the "raised by another role" row test the role check, not a missing tie.
	const otherRoleSeat = "operator"
	for _, name := range []string{"capable", "titled", "closer", "other", otherRoleSeat, "furiosa"} {
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
	bd.db.Seed(escalation("hq-wisp-other-role", "gastown/crew/"+otherRoleSeat, "gt-esc1", "gt-esc1: a decision", "open"))
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
		{"escalation raised by another role", overrunSeat{Rig: "gastown", Polecat: otherRoleSeat, Bead: "gt-esc1"}, false},
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
	norecord.db.Seed(escalation("hq-wisp-open", "gastown/polecats/capable", "gt-esc1", "gt-esc1: a decision", "open"))
	norecord.showErr = errors.New("bd show: connection refused")
	dNoRecord := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: norecord.open}
	got, err = newOverrunEscalationReader(dNoRecord).waiting(overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	if err == nil || got {
		t.Errorf("waiting after a failed record read = %v, %v; want false and an error", got, err)
	}

	// A seat no open escalation ties to is answered without its agent bead: the
	// record read that fails here is never made.
	untied := newWorkBD(t)
	untied.showErr = errors.New("bd show: connection refused")
	dUntied := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: untied.open}
	got, err = newOverrunEscalationReader(dUntied).waiting(overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"})
	if err != nil || got {
		t.Errorf("waiting with no escalation and an unreadable record = %v, %v; want false, nil", got, err)
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

// The .31 timeline of gt-bdlg6o, on the bead's own clock: the seat filed its
// escalation at 16:44, exited ESCALATED at 16:46:13, the overseer closed the
// escalation at 16:48:00, and a guard cycle at 17:16 stopped the seat anyway
// (logged at 17:16:37), 28 minutes after the answer, because its clock still
// ran from the exit.
var (
	overrunRaisedAt   = time.Date(2026, 11, 12, 16, 44, 0, 0, time.UTC)
	overrunExitedAt   = time.Date(2026, 11, 12, 16, 46, 13, 0, time.UTC)
	overrunAnsweredAt = time.Date(2026, 11, 12, 16, 48, 0, 0, time.UTC)
)

const overrunTimelineBead = "gt-3lvcjz.31"

// timelineEscalation is hq-wisp-hw1eu as the seat raised it: escalated_by is the
// seat's address and related_bead the bead. closed answers it at overrunAnsweredAt.
func timelineEscalation(closed bool) beads.Issue {
	title := overrunTimelineBead + ": a decision"
	is := beads.Issue{ID: "hq-wisp-hw1eu", Status: "open", Title: title, Labels: []string{"gt:escalation"},
		CreatedAt: overrunRaisedAt.Format(time.RFC3339),
		Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
			Severity: "high", EscalatedBy: "gastown/polecats/dag", RelatedBead: overrunTimelineBead}),
	}
	if closed {
		is.Status = "closed"
		is.ClosedAt = overrunAnsweredAt.Format(time.RFC3339)
	}
	return is
}

// newTimelineGuard returns a daemon and fake for the seat dag on the .31 bead,
// stopped at 30 minutes, whose run ended at overrunExitedAt. escalation, when
// not nil, is seeded beside the seat's ESCALATED agent record. tick runs one
// guard cycle at at, reading the escalations as that cycle would.
func newTimelineGuard(t *testing.T, escalation *beads.Issue) (tick func(at time.Time), f *overrunFake, bd *workBD) {
	t.Helper()
	bd = newWorkBD(t)
	townRoot := t.TempDir()
	seedOverrunAgent(bd, townRoot, "dag", "stuck", done.ExitEscalated)
	if escalation != nil {
		bd.db.Seed(*escalation)
	}
	d, _ := newOverrunDaemon(t, false)
	d.config.TownRoot = townRoot
	d.openWorkBeads = bd.open
	d.patrolConfig.Patrols.OverrunGuard.StopAfter = "30m"
	seat := overrunSeat{Rig: "gastown", Polecat: "dag", Bead: overrunTimelineBead, EscalatedEnd: overrunExitedAt}
	f = newOverrunFake(0, seat)
	f.slung[overrunTimelineBead] = overrunExitedAt
	tick = func(at time.Time) {
		f.now = at
		r := newOverrunEscalationReader(d)
		f.escalationWaiting = r.waiting
		f.escalationAnsweredAt = r.answeredAt
		d.runOverrunGuard(f.seams())
	}
	return tick, f, bd
}

// tickEveryMinute runs a guard cycle every minute from one time through to
// another, inclusive.
func tickEveryMinute(tick func(time.Time), from, to time.Time) {
	for at := from; !at.After(to); at = at.Add(time.Minute) {
		tick(at)
	}
}

// The clock of an ESCALATED seat that was answered starts at the answer. Built
// from the .31 timeline: the escalation is open at the first cycle after the
// exit, so the seat waits; it is answered at 16:48:00; the cycle at 17:16:30
// is 28.5 minutes after the answer and must not stop the seat or add
// needs-split; the cycle at 17:18:00 is the 30 minutes after the answer and does.
func TestRunOverrunGuard_AnsweredEscalationStartsTheClockAtTheAnswer(t *testing.T) {
	t.Parallel()
	open := timelineEscalation(false)
	tick, f, bd := newTimelineGuard(t, &open)

	tick(overrunExitedAt.Add(time.Minute))
	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Fatalf("seat stopped while its escalation was open: stops %v, handed back %v", f.stops, f.handedBack)
	}

	closed := timelineEscalation(true)
	bd.db.Seed(closed)
	tickEveryMinute(tick, overrunExitedAt.Add(2*time.Minute), overrunAnsweredAt.Add(28*time.Minute+30*time.Second))
	tick(time.Date(2026, 11, 12, 17, 16, 30, 0, time.UTC))
	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Fatalf("seat stopped 28.5 minutes after its answer: stops %v, handed back %v (needs-split)", f.stops, f.handedBack)
	}

	tick(time.Date(2026, 11, 12, 17, 17, 0, 0, time.UTC))
	if len(f.stops) != 0 {
		t.Fatalf("seat stopped 29 minutes after its answer: stops %v", f.stops)
	}
	tick(time.Date(2026, 11, 12, 17, 18, 0, 0, time.UTC))
	if len(f.stops) != 1 {
		t.Fatalf("seat not stopped 30 minutes after its answer: stops %v", f.stops)
	}
}

// Re-slung after the answer, the clock starts at the re-sling, not at the answer:
// the seat's run is the new dispatch, so the answer does not date it.
func TestRunOverrunGuard_ReslungAfterTheAnswerStartsAtTheReSling(t *testing.T) {
	t.Parallel()
	closed := timelineEscalation(true)
	tick, f, _ := newTimelineGuard(t, &closed)
	resling := overrunAnsweredAt.Add(10 * time.Minute)
	f.slung[overrunTimelineBead] = resling

	tickEveryMinute(tick, overrunAnsweredAt.Add(time.Minute), resling.Add(29*time.Minute))
	if len(f.stops) != 0 {
		t.Fatalf("seat stopped 29 minutes after its re-sling: stops %v", f.stops)
	}
	tick(resling.Add(30 * time.Minute))
	if len(f.stops) != 1 {
		t.Fatalf("seat not stopped 30 minutes after its re-sling: stops %v", f.stops)
	}
}

// A seat with no escalation answered after its ESCALATED exit keeps the clock
// from the exit: 30 minutes after the exit it is stopped at the threshold.
func TestRunOverrunGuard_UnansweredExitStillStopsAtTheThreshold(t *testing.T) {
	t.Parallel()
	tick, f, _ := newTimelineGuard(t, nil)

	tickEveryMinute(tick, overrunExitedAt.Add(time.Minute), overrunExitedAt.Add(29*time.Minute))
	if len(f.stops) != 0 {
		t.Fatalf("seat stopped 29 minutes after its ESCALATED exit: stops %v", f.stops)
	}
	tick(overrunExitedAt.Add(30 * time.Minute))
	if len(f.stops) != 1 {
		t.Fatalf("seat not stopped 30 minutes after its ESCALATED exit: stops %v", f.stops)
	}
}

// The reader's answer is read through the same tie the exemption uses: the
// escalation the seat raised for its bead, answered after the exit. A seat
// that never exited ESCALATED has no answer, whatever escalations it raised.
func TestOverrunGuardEscalationAnsweredAt(t *testing.T) {
	t.Parallel()
	closed := timelineEscalation(true)
	open := timelineEscalation(false)
	seat := overrunSeat{Rig: "gastown", Polecat: "dag", Bead: overrunTimelineBead, EscalatedEnd: overrunExitedAt}
	tests := []struct {
		name      string
		escalated beads.Issue
		seat      overrunSeat
		want      time.Time
	}{
		{"answered after the exit", closed, seat, overrunAnsweredAt},
		{"still open", open, seat, time.Time{}},
		{"no ESCALATED exit on the seat", closed, overrunSeat{Rig: "gastown", Polecat: "dag", Bead: overrunTimelineBead}, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			bd.db.Seed(tt.escalated)
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: bd.open}
			got, err := newOverrunEscalationReader(d).answeredAt(tt.seat)
			if err != nil || !got.Equal(tt.want) {
				t.Errorf("answeredAt = %v, %v; want %v, nil", got, err, tt.want)
			}
		})
	}

	// A failed read is an error, which the guard turns into keeping the clock
	// where the stop left it.
	failing := newWorkBD(t)
	failing.listErr = errors.New("bd list: connection refused")
	dFailing := &Daemon{config: &Config{TownRoot: t.TempDir()}, openWorkBeads: failing.open}
	if got, err := newOverrunEscalationReader(dFailing).answeredAt(seat); err == nil || !got.IsZero() {
		t.Errorf("answeredAt after a failed read = %v, %v; want zero and an error", got, err)
	}
}

// The clock is the answer only when the answer is later than the run's clock.
func TestOverrunAnsweredFloor(t *testing.T) {
	t.Parallel()
	slung := overrunExitedAt
	tests := []struct {
		name     string
		answered time.Time
		want     time.Time
	}{
		{"no answer read", time.Time{}, slung},
		{"answered after the clock", overrunAnsweredAt, overrunAnsweredAt},
		{"answered before the clock", overrunExitedAt.Add(-time.Minute), slung},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := overrunAnsweredFloor(slung, tt.answered); !got.Equal(tt.want) {
				t.Errorf("overrunAnsweredFloor = %v, want %v", got, tt.want)
			}
		})
	}
}

// The seat waits on its open escalation for the whole of the 30 minutes after
// its ESCALATED exit: the cycle at 30 minutes finds it unanswered and does not
// stop it or hand the bead back as needs-split (gt-bdlg6o, .31 timeline).
func TestRunOverrunGuard_OpenEscalationAtThirtyMinutesDoesNotStop(t *testing.T) {
	t.Parallel()
	open := timelineEscalation(false)
	tick, f, _ := newTimelineGuard(t, &open)

	tickEveryMinute(tick, overrunExitedAt.Add(time.Minute), overrunExitedAt.Add(30*time.Minute))
	if len(f.stops) != 0 || len(f.handedBack) != 0 {
		t.Fatalf("seat stopped 30 minutes after its ESCALATED exit with its escalation open: stops %v, handed back %v",
			f.stops, f.handedBack)
	}
}

// An answer that cannot be read is not used: the guard keeps the clock where
// the stop left it and stops the seat rather than trust a time it could not
// read, and the alarm names the failure once for the tick, not once per seat
// (gt-bdlg6o).
func TestRunOverrunGuard_UnreadableEscalationAnswerKeepsTheStopClock(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute,
		overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc1"},
		overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-esc2"})
	// The seam answers a time two hours out together with its error: the branch
	// must drop that time, or the clock would restart in the seat's favour.
	f.escalationAnsweredAt = func(overrunSeat) (time.Time, error) {
		return overrunSlung.Add(2 * time.Hour), errors.New("bd list: connection refused")
	}

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 2 {
		t.Errorf("stops = %v, want both seats stopped: an unreadable answer restarts no clock", f.stops)
	}
	if n := strings.Count(buf.String(), "cannot read the escalation answer"); n != 1 {
		t.Errorf("the answer read failure was logged %d times, want once:\n%s", n, buf.String())
	}
}

// The answer-restart path in a sandbox seat's shape: the run's end is only on
// the bead's ESCALATED RUN END note, because a sandbox seat's done event dies
// with its town (gt-1m9gr). The seat is read from that note, the dispatch clock
// still holds the sling, the escalation was raised before the end and closed
// after it, and the guard dates the clock from the close: no stop through 29
// minutes after the answer, the stop at 30 (gt-bdlg6o).
func TestRunOverrunGuard_SandboxSeatClockStartsAtTheAnswer(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	town := d.config.TownRoot
	bd := newWorkBD(t)
	d.openWorkBeads = bd.open
	d.patrolConfig.Patrols.OverrunGuard.StopAfter = "30m"
	const bead, seatName = "gt-sbx1", "dag"
	seatAddr := "gastown/polecats/" + seatName

	slung := time.Date(2026, 11, 12, 16, 42, 49, 0, time.UTC)
	raised := time.Date(2026, 11, 12, 16, 44, 0, 0, time.UTC)
	runEnd := time.Date(2026, 11, 12, 16, 46, 13, 0, time.UTC)
	answered := time.Date(2026, 11, 12, 16, 48, 0, 0, time.UTC)

	// The dispatch log holds the sling and nothing else; the run's end reaches
	// the host only through the bead's note.
	line := fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"gt","payload":{"bead":%q,"target":%q},"visibility":"both"}`,
		slung.Format(time.RFC3339), events.TypeSling, bead, seatAddr)
	if err := os.WriteFile(filepath.Join(town, events.EventsFile), []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bd.db.Seed(beads.Issue{ID: bead, Status: "open", UpdatedAt: slung.Format(time.RFC3339),
		Notes: land.FormatEscalatedEndNote(seatAddr, runEnd)})
	title := bead + ": a decision"
	bd.db.Seed(beads.Issue{ID: "hq-wisp-sbx", Status: "closed", Title: title, Labels: []string{"gt:escalation"},
		CreatedAt: raised.Format(time.RFC3339), ClosedAt: answered.Format(time.RFC3339),
		Description: beads.FormatEscalationDescription(title, &beads.EscalationFields{
			Severity: "high", EscalatedBy: seatAddr, RelatedBead: bead})})

	// A live seat holding the bead, as the guard sees a working sandbox seat.
	tm := newFakeTmux(newFixedClock())
	d.tmux = tm
	seatFor := supervisor.SeatFor("gastown", string(constants.RolePolecat), seatName)
	if _, err := intent.Update(town, supervisor.IntentSeat(seatFor), func(r *intent.Record) error {
		r.WorkBead = bead
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tm.addSession(session.PolecatSessionName(d.prefixRegistry().PrefixForRig("gastown"), seatName), "node", slung)

	seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", seatName)
	if !ok {
		t.Fatal("the sandbox seat was dropped")
	}
	if !seat.EscalatedEnd.Equal(runEnd) {
		t.Fatalf("EscalatedEnd = %v, want the ESCALATED RUN END note's %v", seat.EscalatedEnd, runEnd)
	}

	f := newOverrunFake(0, seat)
	tick := func(at time.Time) {
		f.now = at
		// The escalation read is per cycle, so the reader is built per tick.
		r := newOverrunEscalationReader(d)
		s := f.seams()
		s.workingSeats = func() ([]overrunSeat, error) { return []overrunSeat{seat}, nil }
		s.slungAt = (&overrunSlungReader{d: d}).slungAt
		s.escalationAnsweredAt = r.answeredAt
		d.runOverrunGuard(s)
	}

	tickEveryMinute(tick, answered.Add(time.Minute), answered.Add(29*time.Minute))
	if len(f.stops) != 0 {
		t.Fatalf("seat stopped 29 minutes after its answer: stops %v; the clock started at the run end", f.stops)
	}
	tick(answered.Add(30 * time.Minute))
	if len(f.stops) != 1 {
		t.Fatalf("seat not stopped 30 minutes after its answer: stops %v", f.stops)
	}
}
