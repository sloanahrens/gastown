package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// workBeadStore is the work-bead store a seat read goes to, by bead id. A bead
// it does not hold is an open bead with no labels and no notes.
type workBeadStore map[string]*beads.Issue

func (b workBeadStore) open([]string, time.Duration) workBeadReader { return b }

func (b workBeadStore) Show(id string) (*beads.Issue, error) {
	if issue, ok := b[id]; ok {
		return issue, nil
	}
	return &beads.Issue{ID: id}, nil
}

func (b workBeadStore) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

// unreadableBeads is a work-bead store that cannot be read.
type unreadableBeads struct{}

func (unreadableBeads) open([]string, time.Duration) workBeadReader { return unreadableBeads{} }

func (unreadableBeads) Show(string) (*beads.Issue, error) {
	return nil, errors.New("bd show: store unavailable")
}

func (unreadableBeads) List(beads.ListOptions) ([]*beads.Issue, error) { return nil, nil }

// readyNote is the READY TO LAND block gt done writes when it submits a bead
// for landing at submitted.
func readyNote(submitted string) string {
	return "READY TO LAND\n" +
		"Branch: polecat/capable/gt-aaa11+abc123\n" +
		"Head: b4336e1b89ea1d7fca33a726377c463282601686\n" +
		"Target: main\n" +
		"Worker: capable\n" +
		"Submitted: " + submitted + "\n"
}

func TestOverrunGuardEnabled(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "overrun_guard") {
		t.Error("expected overrun_guard disabled for nil config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{}, "overrun_guard") {
		t.Error("expected overrun_guard disabled with no patrols")
	}
	absent := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(absent, "overrun_guard") {
		t.Error("expected overrun_guard disabled when the key is absent")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{}}}
	if IsPatrolEnabled(off, "overrun_guard") {
		t.Error("expected overrun_guard disabled when enabled is false")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "overrun_guard") {
		t.Error("expected overrun_guard enabled")
	}
}

func TestOverrunGuardInterval(t *testing.T) {
	t.Parallel()
	with := func(c OverrunGuardConfig) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &c}}
	}
	tests := []struct {
		name     string
		cfg      *DaemonPatrolConfig
		interval time.Duration
		nudge    time.Duration
		stop     time.Duration
	}{
		{"nil config", nil, time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"nil patrols", &DaemonPatrolConfig{}, time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"empty strings", with(OverrunGuardConfig{}), time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"invalid strings", with(OverrunGuardConfig{IntervalStr: "soon", NudgeAfter: "x", StopAfter: "y"}), time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"zero", with(OverrunGuardConfig{IntervalStr: "0s", NudgeAfter: "0s", StopAfter: "0s"}), time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"negative", with(OverrunGuardConfig{IntervalStr: "-1m", NudgeAfter: "-1m", StopAfter: "-1m"}), time.Minute, 12 * time.Minute, 20 * time.Minute},
		{"configured", with(OverrunGuardConfig{IntervalStr: "30s", NudgeAfter: "5m", StopAfter: "9m"}), 30 * time.Second, 5 * time.Minute, 9 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := overrunGuardInterval(tt.cfg); got != tt.interval {
				t.Errorf("interval = %v, want %v", got, tt.interval)
			}
			nudge, stop := overrunGuardThresholds(tt.cfg)
			if nudge != tt.nudge || stop != tt.stop {
				t.Errorf("thresholds = %v, %v; want %v, %v", nudge, stop, tt.nudge, tt.stop)
			}
		})
	}
}

// overrunFake holds the fake seams and the calls they received.
type overrunFake struct {
	now      time.Time
	seats    []overrunSeat
	slung    map[string]time.Time // by bead
	nudges   []string             // "<rig>/<polecat>"
	stops    []string             // seats whose session was stopped
	nudgeErr error

	// calls is the ordered log of the stop's side effects, "<step> <rig>/<polecat>";
	// failOn makes the named step return its error each time it runs.
	calls  []string
	failOn map[string]error

	// resetWorktree, when set, is the stop's worktree reset, logged as
	// "reset-worktree". nil leaves the seam unset, so the call lists of the
	// stops that do not test the reset stay as they were.
	resetWorktree func(seat overrunSeat) error
	// removeSandbox, when set, is the stop's sandbox removal, logged as
	// "sandbox". nil leaves the seam unset, so the call lists of the stops that
	// do not test it stay as they were.
	removeSandbox func(seat overrunSeat) error
	// handedBack lists the beads handBack received, with the branch it was given.
	handedBack []string
	// pushed lists the bead and branch each push received; agentBeadsReset the
	// bead each agent-bead reset was for; endRuns the bead whose run the stop
	// ended (with the time each end was stamped), endRunErr what that record
	// answers when it is set.
	pushed          []string
	agentBeadsReset []string
	endRuns         []string
	endRunAts       []time.Time
	endRunErr       error
	// stillOursErr, when set, is what the re-check before the run's end answers
	// an error; notOurs says the bead is no longer the stopped seat's.
	notOurs      bool
	stillOursErr error
	// handBackNotes lists the note each handBack call carried, in the same order.
	handBackNotes []string
	// spanEnd, when set, is the stop's span close; spanEnds records the
	// "<bead> <at>" each successful close was given, in call order.
	spanEnd  func(seat overrunSeat, at time.Time) error
	spanEnds []string
	// comments lists "<bead>: <text>" for each comment a scheduled stop added;
	// closed lists "<bead>: <reason>" for each run bead it closed.
	comments []string
	closed   []string

	// submitted is what the submittedNow re-check answers from the start;
	// readyAfterStop makes it answer true once the session has been stopped,
	// the bead gaining gt:ready-to-land between the listing and the hand-back.
	submitted, readyAfterStop bool

	// gateRunning answers whether the seat is inside its own gate. nil leaves
	// the gate seam unset: no exemption, every seat's time counts.
	gateRunning func(seat overrunSeat) (bool, error)
	// escalationWaiting answers whether the seat waits on an escalation it
	// raised. nil leaves the seam unset: no exemption.
	escalationWaiting func(seat overrunSeat) (bool, error)

	// lapsed and lapsedErr are what the resumed-stop re-check answers.
	lapsed    string
	lapsedErr error
}

// step logs one stop step and returns the error injected for it.
func (f *overrunFake) step(name string, seat overrunSeat) error {
	f.calls = append(f.calls, name+" "+seat.Rig+"/"+seat.Polecat)
	return f.failOn[name]
}

func (f *overrunFake) seams() overrunSeams {
	s := f.baseSeams()
	if f.resetWorktree != nil {
		s.resetWorktree = func(seat overrunSeat) error {
			f.calls = append(f.calls, "reset-worktree "+seat.Rig+"/"+seat.Polecat)
			return f.resetWorktree(seat)
		}
	}
	if f.removeSandbox != nil {
		s.removeSandbox = func(seat overrunSeat) error {
			f.calls = append(f.calls, "sandbox "+seat.Rig+"/"+seat.Polecat)
			return f.removeSandbox(seat)
		}
	}
	if f.spanEnd != nil {
		s.spanEnd = func(seat overrunSeat, at time.Time) error {
			f.calls = append(f.calls, "span-end "+seat.Rig+"/"+seat.Polecat)
			if err := f.spanEnd(seat, at); err != nil {
				return err
			}
			f.spanEnds = append(f.spanEnds, seat.Bead+" "+at.Format(time.RFC3339))
			return nil
		}
	}
	return s
}

func (f *overrunFake) baseSeams() overrunSeams {
	return overrunSeams{
		now:          func() time.Time { return f.now },
		workingSeats: func() ([]overrunSeat, error) { return f.seats, nil },
		slungAt: func(seat overrunSeat) (time.Time, bool) {
			t, ok := f.slung[seat.Bead]
			return t, ok
		},
		nudge: func(seat overrunSeat, msg string) error {
			if f.nudgeErr != nil {
				return f.nudgeErr
			}
			f.nudges = append(f.nudges, seat.Rig+"/"+seat.Polecat)
			return nil
		},
		submittedNow: func(overrunSeat) bool {
			return f.submitted || (f.readyAfterStop && len(f.stops) > 0)
		},
		gateRunning:       f.gateRunning,
		escalationWaiting: f.escalationWaiting,
		pause:             func(seat overrunSeat) error { return f.step("pause", seat) },
		clearPause:        func(seat overrunSeat) error { return f.step("clear-pause", seat) },
		stopSession: func(seat overrunSeat) error {
			if err := f.step("stop", seat); err != nil {
				return err
			}
			f.stops = append(f.stops, seat.Rig+"/"+seat.Polecat)
			return nil
		},
		checkpoint: func(seat overrunSeat) error { return f.step("checkpoint", seat) },
		comment: func(seat overrunSeat, text string) error {
			if err := f.step("comment", seat); err != nil {
				return err
			}
			f.comments = append(f.comments, seat.Bead+": "+text)
			return nil
		},
		closeBead: func(seat overrunSeat, reason string) error {
			if err := f.step("close", seat); err != nil {
				return err
			}
			f.closed = append(f.closed, seat.Bead+": "+reason)
			return nil
		},
		push: func(seat overrunSeat) error {
			if err := f.step("push", seat); err != nil {
				return err
			}
			f.pushed = append(f.pushed, seat.Bead+" "+seat.Branch)
			return nil
		},
		stillOurs: func(overrunSeat) (bool, error) {
			if f.stillOursErr != nil {
				return false, f.stillOursErr
			}
			return !f.notOurs, nil
		},
		endRun: func(seat overrunSeat, at time.Time) error {
			if f.endRunErr != nil {
				return f.endRunErr
			}
			f.endRuns = append(f.endRuns, seat.Bead)
			f.endRunAts = append(f.endRunAts, at)
			return nil
		},
		resetAgentBead: func(seat overrunSeat) error {
			if err := f.step("reset-agent-bead", seat); err != nil {
				return err
			}
			f.agentBeadsReset = append(f.agentBeadsReset, seat.Bead)
			return nil
		},
		recordCleanup: func(seat overrunSeat) error { return f.step("record-cleanup", seat) },
		retire:        func(seat overrunSeat) error { return f.step("retire", seat) },
		stopLapsed: func(seat overrunSeat, _ bool) (string, error) {
			f.calls = append(f.calls, "recheck "+seat.Rig+"/"+seat.Polecat)
			return f.lapsed, f.lapsedErr
		},
		handBack: func(seat overrunSeat, elapsed time.Duration, note string) error {
			if err := f.step("hand-back", seat); err != nil {
				return err
			}
			f.handedBack = append(f.handedBack, seat.Bead+" "+seat.Branch+" "+elapsed.String())
			f.handBackNotes = append(f.handBackNotes, note)
			return nil
		},
	}
}

var overrunSlung = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

func newOverrunFake(elapsed time.Duration, seats ...overrunSeat) *overrunFake {
	f := &overrunFake{now: overrunSlung.Add(elapsed), slung: map[string]time.Time{}}
	for _, s := range seats {
		f.slung[s.Bead] = overrunSlung
	}
	f.seats = seats
	return f
}

func newOverrunDaemon(t *testing.T, dryRun bool) (*Daemon, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{OverrunGuard: &OverrunGuardConfig{Enabled: true, DryRun: dryRun}}}
	return &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		patrolConfig: cfg,
		logger:       log.New(&buf, "", 0),
	}, &buf
}

func TestRunOverrunGuard_DryRunLogsOnly(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, true)
	f := newOverrunFake(21*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("dry run called nudge %v / stop %v, want neither", f.nudges, f.stops)
	}
	if n := strings.Count(buf.String(), "would stop gastown/capable on gt-aaa11"); n != 1 {
		t.Errorf("log has %d 'would stop' lines, want 1:\n%s", n, buf.String())
	}
	if st, err := readOverrunState(d.config.TownRoot, "gt-aaa11"); err != nil || !st.NudgedAt.IsZero() {
		t.Errorf("dry run wrote state %+v (err %v), want none", st, err)
	}
}

func TestRunOverrunGuard_DryRunNudgeWritesNothing(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, true)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 {
		t.Errorf("dry run nudged %v", f.nudges)
	}
	if !strings.Contains(buf.String(), "would nudge gastown/capable on gt-aaa11") {
		t.Errorf("log lacks the would-nudge line:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.NudgedAt.IsZero() {
		t.Errorf("dry run persisted NudgedAt %v", st.NudgedAt)
	}
}

func TestRunOverrunGuard_SkipsSubmittedAndParked(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(30*time.Minute,
		overrunSeat{Rig: "gastown", Polecat: "a", Bead: "gt-sub11", Submitted: true},
		overrunSeat{Rig: "gastown", Polecat: "b", Bead: "gt-par11", Parked: true},
		overrunSeat{Rig: "gastown", Polecat: "c", Bead: ""},
	)

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("skipped seats were acted on: nudges %v, stops %v", f.nudges, f.stops)
	}
}

// The scheduled deep-review runs past 20 minutes by design, so a seat on a
// scheduled: bead gets no nudge and no 20 minute stop (its own limit is
// scheduled_stop_after, see overrun_guard_scheduled_test.go), while an
// identical seat without the label is still judged.
func TestRunOverrunGuard_SkipsScheduledSlings(t *testing.T) {
	t.Parallel()
	for _, elapsed := range []time.Duration{13 * time.Minute, 21 * time.Minute} {
		d, buf := newOverrunDaemon(t, false)
		f := newOverrunFake(elapsed,
			overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-r71dd", Scheduled: true},
			overrunSeat{Rig: "gastown", Polecat: "nux", Bead: "gt-plain"},
		)

		d.runOverrunGuard(f.seams())

		wantNudges, wantStops := []string{"gastown/nux"}, 0
		if elapsed >= 20*time.Minute {
			wantNudges, wantStops = nil, 1
		}
		if !slices.Equal(f.nudges, wantNudges) || len(f.stops) != wantStops {
			t.Errorf("after %v: nudges %v, stops %v; want nudges %v and %d stop of the unlabelled seat only", elapsed, f.nudges, f.stops, wantNudges, wantStops)
		}
		if wantStops == 1 && f.stops[0] != "gastown/nux" {
			t.Errorf("after %v: stopped %v, want gastown/nux", elapsed, f.stops)
		}
		if strings.Contains(buf.String(), "gt-r71dd") {
			t.Errorf("skipped scheduled seat logged at normal level:\n%s", buf.String())
		}
		if st, _ := readOverrunState(d.config.TownRoot, "gt-r71dd"); !st.NudgedAt.IsZero() {
			t.Errorf("scheduled seat has nudge state %+v", st)
		}
	}
}

func TestHasScheduledSlingLabel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		labels []string
		want   bool
	}{
		{nil, false},
		{[]string{"gt:ready-to-land"}, false},
		{[]string{"unscheduled:x", "scheduled"}, false},
		{[]string{"gt:task", "scheduled:deep-review"}, true},
		{[]string{scheduledSlingLabel(ScheduledSlingEntry{Name: "doc-audit"})}, true},
	}
	for _, tt := range tests {
		if got := hasScheduledSlingLabel(tt.labels); got != tt.want {
			t.Errorf("hasScheduledSlingLabel(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}

func TestRunOverrunGuard_NudgesOncePerBead(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 1 {
		t.Fatalf("nudges = %v, want exactly one across two ticks", f.nudges)
	}
	if len(f.stops) != 0 {
		t.Errorf("stopped at 14 minutes: %v", f.stops)
	}
	st, err := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if want := overrunSlung.Add(13 * time.Minute); !st.NudgedAt.Equal(want) {
		t.Errorf("NudgedAt = %v, want %v", st.NudgedAt, want)
	}

	// A restart is a new Daemon over the same town: the state file still
	// suppresses the nudge.
	d2, _ := newOverrunDaemon(t, false)
	d2.config.TownRoot = d.config.TownRoot
	d2.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 {
		t.Errorf("nudges after restart = %v, want still one", f.nudges)
	}
}

func TestRunOverrunGuard_NoActionBeforeNudgeThreshold(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(11*time.Minute+59*time.Second, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("acted before 12 minutes: nudges %v, stops %v", f.nudges, f.stops)
	}
}

func TestRunOverrunGuard_StopAtThreshold(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 || len(f.nudges) != 0 {
		t.Errorf("stops %v, nudges %v; want one stop and no nudge", f.stops, f.nudges)
	}
}

// The skip that keeps a stopped run from being stopped twice is dated by the
// run end the stop recorded on the bead — the time the dispatch clock reads —
// not by StoppedAt, which the stop writes after the hand-back. A re-sling that
// lands between the two is the new run's and is policed (gt-lfszo).
func TestRunOverrunGuard_StoppedRunSkipIsDatedByTheRecordedRunEnd(t *testing.T) {
	t.Parallel()
	runEnd := overrunSlung.Add(10 * time.Minute)
	stoppedAt := runEnd.Add(5 * time.Minute)
	for _, tt := range []struct {
		name      string
		slung     time.Time
		wantStops int
	}{
		{"a dispatch before the recorded end is the ended run's", runEnd.Add(-time.Minute), 0},
		{"a dispatch at the recorded end is the new run's", runEnd, 1},
		{"a dispatch after the end but before StoppedAt is the new run's", runEnd.Add(2 * time.Minute), 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			st := overrunState{Bead: "gt-aaa11", RunEndedAt: runEnd, StoppedAt: stoppedAt}
			if err := writeOverrunState(d.config.TownRoot, st); err != nil {
				t.Fatal(err)
			}
			f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
			f.now = runEnd.Add(25 * time.Minute)
			f.slung["gt-aaa11"] = tt.slung

			d.runOverrunGuard(f.seams())

			if len(f.stops) != tt.wantStops {
				t.Errorf("stops = %v, want %d", f.stops, tt.wantStops)
			}
		})
	}
}

// A stop that recorded no run end — abandoned when the seat was taken back, or
// a scheduled run whose close is its end — keeps its sling from a second stop:
// StoppedAt is then the only record of it (gt-lfszo).
func TestRunOverrunGuard_StoppedRunWithoutARecordedEndKeepsItsSling(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	st := overrunState{Bead: "gt-aaa11", StoppedAt: overrunSlung.Add(time.Minute)}
	if err := writeOverrunState(d.config.TownRoot, st); err != nil {
		t.Fatal(err)
	}
	f := newOverrunFake(22*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 0 || len(f.calls) != 0 {
		t.Errorf("stopped a run that recorded no end again: stops %v, calls %v", f.stops, f.calls)
	}
}

func TestRunOverrunGuard_FailedNudgeRetriesAfterBackoff(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
	f.nudgeErr = errors.New("no session")

	d.runOverrunGuard(f.seams())
	if !strings.Contains(buf.String(), "failed (attempt 1 of 3): no session") {
		t.Errorf("log lacks the nudge failure:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.NudgedAt.IsZero() {
		t.Fatalf("a failed nudge was recorded as sent: %v", st.NudgedAt)
	}

	// Within the backoff the guard does not try again, even once the cause is gone.
	f.nudgeErr = nil
	f.now = f.now.Add(overrunNudgeRetryAfter - time.Second)
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 0 {
		t.Fatalf("nudges = %v, want none inside the 5-minute backoff", f.nudges)
	}

	f.now = f.now.Add(time.Second)
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 {
		t.Errorf("nudges = %v, want the retry to deliver one", f.nudges)
	}
}

func TestRunOverrunGuard_NudgeAbandonedAfterThreeAttempts(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	// With the default 12m/20m thresholds only two attempts fit before the
	// stop; a later stop leaves room for the cap to bind.
	d.patrolConfig.Patrols.OverrunGuard.StopAfter = "1h"
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
	attempts := 0
	s := f.seams()
	s.nudge = func(overrunSeat, string) error {
		attempts++
		return errors.New("busy")
	}

	// A tick every minute: the attempts land at 13, 18 and 23 minutes, never
	// closer than the backoff, and there is no fourth.
	for range 7 {
		d.runOverrunGuard(s)
		f.now = f.now.Add(time.Minute)
	}
	if attempts != 2 {
		t.Fatalf("attempts after 7 minutes = %d, want 2 (13m and 18m)", attempts)
	}
	f.now = overrunSlung.Add(23 * time.Minute)
	d.runOverrunGuard(s)
	if attempts != 3 {
		t.Fatalf("attempts at 23 minutes = %d, want the third", attempts)
	}
	if got := strings.Count(buf.String(), "abandoned"); got != 1 {
		t.Errorf("abandoned logged %d times, want once:\n%s", got, buf.String())
	}

	// Nothing more is tried, and the log stays quiet, however long the seat runs
	// short of the stop.
	f.now = overrunSlung.Add(40 * time.Minute)
	d.runOverrunGuard(s)
	if attempts != 3 {
		t.Errorf("attempts = %d, want the cap at 3", attempts)
	}
	if got := strings.Count(buf.String(), "abandoned"); got != 1 {
		t.Errorf("abandoned logged %d times after the cap, want still once", got)
	}
}

func TestRunOverrunGuard_StopDoesNotDependOnAbandonedNudge(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"}
	f := newOverrunFake(13*time.Minute, seat)
	f.nudgeErr = errors.New("busy")
	d.runOverrunGuard(f.seams())
	f.now = overrunSlung.Add(18 * time.Minute)
	d.runOverrunGuard(f.seams())
	f.now = overrunSlung.Add(20 * time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the 20-minute stop despite the failed nudges", f.stops)
	}
}

func TestRunOverrunGuard_NudgeFailuresResetOnResling(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"}
	f := newOverrunFake(13*time.Minute, seat)
	f.nudgeErr = errors.New("busy")
	d.runOverrunGuard(f.seams())

	// The bead is slung again; its new run is nudged afresh, backoff or not.
	f.nudgeErr = nil
	f.slung[seat.Bead] = f.now.Add(time.Second)
	f.now = f.slung[seat.Bead].Add(13 * time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 {
		t.Errorf("nudges = %v, want one for the new sling", f.nudges)
	}
}

// TestOverrunProdNudgeQueuesWithoutWaiting pins the production nudge seam to
// queue mode (gt-t61ft): the default wait-idle delivery watches a busy seat
// for longer than the seam's context lasts.
func TestOverrunProdNudgeQueuesWithoutWaiting(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	rec := notifyfake.New()
	d.notifier = rec

	seat := overrunSeat{Rig: "gastown", Polecat: "furiosa", Bead: "gt-jhf24"}
	if err := d.overrunProdSeams().nudge(seat, overrunNudgeMessage); err != nil {
		t.Fatal(err)
	}
	calls := rec.Nudges()
	if len(calls) != 1 || calls[0].Kind != notifyfake.KindNudge || calls[0].Target != "gastown/furiosa" {
		t.Fatalf("calls = %+v, want one nudge to gastown/furiosa", calls)
	}
	if !calls[0].Nudge.Queue {
		t.Errorf("the nudge was not sent in queue mode")
	}
}

func TestRunOverrunGuard_ReslingStartsNewClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 {
		t.Fatalf("first nudge: %v", f.nudges)
	}

	// Rework: the bead is slung again at +30m; 13 minutes later it earns a
	// second nudge because the first belonged to the earlier sling.
	resling := overrunSlung.Add(30 * time.Minute)
	f.slung["gt-aaa11"] = resling
	f.now = resling.Add(13 * time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 2 {
		t.Errorf("nudges = %v, want a second one for the new sling", f.nudges)
	}
	if len(f.stops) != 0 {
		t.Errorf("stopped on the old clock: %v", f.stops)
	}
}

// A seat restarted hours after it died gets a fresh clock: the dead stretch was
// not time a session was alive on the bead, so the new session is not stopped
// seconds after it starts. The adopted restart is saved with the bead's state,
// because the supervisor's restart stamps last only its budget window
// (gt-y9lkx).
func TestRunOverrunGuard_RestartStartsAFreshClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	// Slung at 12:00, dead until the supervised restart at 14:00.
	restart := overrunSlung.Add(2 * time.Hour)
	seat := overrunSeat{Rig: "gastown", Polecat: "goose", Bead: "gt-aaa11", RestartedAt: restart}
	f := newOverrunFake(0, seat)
	f.now = restart.Add(6 * time.Second)

	d.runOverrunGuard(f.seams())
	if len(f.stops) != 0 || len(f.nudges) != 0 {
		t.Fatalf("a restarted seat was acted on 6s in: stops %v, nudges %v", f.stops, f.nudges)
	}
	st, err := readOverrunState(d.config.TownRoot, "gt-aaa11")
	if err != nil {
		t.Fatal(err)
	}
	if !st.RestartedAt.Equal(restart) {
		t.Fatalf("RestartedAt = %v, want the restart %v saved", st.RestartedAt, restart)
	}

	// A later tick reads no restart stamp: the adopted restart carries the clock.
	seat.RestartedAt = time.Time{}
	f.seats = []overrunSeat{seat}
	f.now = restart.Add(12 * time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 || len(f.stops) != 0 {
		t.Fatalf("12m after the restart: nudges %v, stops %v; want the nudge on the new clock", f.nudges, f.stops)
	}

	f.now = restart.Add(20 * time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the stop 20m after the restart", f.stops)
	}
}

// A seat that never died is stopped on its sling clock, as before: the restart
// field it does not have moves nothing (gt-y9lkx).
func TestRunOverrunGuard_SeatThatNeverDiedKeepsItsSlingClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(20*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the stop 20m after the sling", f.stops)
	}
}

// A restart stamped before the sling restarted the seat for whatever it held
// then; it is not a restart of this dispatch, so the clock stays the sling's
// (gt-y9lkx).
func TestRunOverrunGuard_RestartBeforeTheSlingDoesNotMoveTheClock(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{
		Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11",
		RestartedAt: overrunSlung.Add(-30 * time.Minute),
	}
	f := newOverrunFake(20*time.Minute, seat)

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the stop 20m after the sling", f.stops)
	}
}

// A rework slung after a restart starts its own clock: the restart adopted for
// the earlier sling is not this run's, or every rework on a seat that was ever
// restarted would be judged only minutes old (gt-y9lkx).
func TestRunOverrunGuard_ReworkAfterARestartStartsOver(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	restart := overrunSlung.Add(2 * time.Hour)
	seat := overrunSeat{Rig: "gastown", Polecat: "goose", Bead: "gt-aaa11", RestartedAt: restart}
	f := newOverrunFake(0, seat)
	f.now = restart.Add(time.Minute)
	d.runOverrunGuard(f.seams())

	resling := restart.Add(30 * time.Minute)
	f.slung["gt-aaa11"] = resling
	f.now = resling.Add(13 * time.Minute)
	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 1 || len(f.stops) != 0 {
		t.Errorf("nudges %v, stops %v; want one nudge 13m into the rework", f.nudges, f.stops)
	}
}

// A seat that keeps dying and coming back does not get a fresh clock on every
// restart: the first overrunMaxRestartClocks restarts each start the clock, and
// after that it stays where the last of them left it, so the seat is stopped on
// its next tick. The dashboard reads the same clock (gt-y9lkx).
func TestRunOverrunGuard_RestartsStartTheClockOnlyUpToTheCap(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "goose", Bead: "gt-aaa11"}
	f := newOverrunFake(0, seat)
	// Each restart comes two hours after the one before, so the supervisor's
	// hourly restart budget never freezes the seat.
	restarts := make([]time.Time, overrunMaxRestartClocks+1)
	for i := range restarts {
		restarts[i] = overrunSlung.Add(time.Duration(i+1) * 2 * time.Hour)
	}
	for i, restart := range restarts[:overrunMaxRestartClocks] {
		seat.RestartedAt = restart
		f.seats = []overrunSeat{seat}
		f.now = restart.Add(time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.stops) != 0 {
			t.Fatalf("restart %d: stopped 1m after it: %v", i+1, f.stops)
		}
	}

	last := restarts[overrunMaxRestartClocks-1]
	if got := OverrunRestartClock(d.config.TownRoot, seat.Bead, overrunSlung); !got.Equal(last) {
		t.Fatalf("clock = %v, want the last allowed restart %v", got, last)
	}

	seat.RestartedAt = restarts[overrunMaxRestartClocks]
	f.seats = []overrunSeat{seat}
	f.now = seat.RestartedAt.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the seat stopped on the clock the cap left it", f.stops)
	}
	if got := OverrunRestartClock(d.config.TownRoot, seat.Bead, overrunSlung); !got.Equal(last) {
		t.Errorf("clock after the capped restart = %v, want it still %v", got, last)
	}
}

// Gate time a seat spent before its restart is not charged to the clock the
// restart starts. The clock counts the restarted session's own time, and gate
// time after the restart is excluded as it is for any run (gt-y9lkx).
func TestRunOverrunGuard_GateTimeBeforeARestartIsNotCarried(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "goose", Bead: "gt-aaa11"}
	f := newOverrunFake(0, seat)
	inGate := false
	f.gateRunning = func(overrunSeat) (bool, error) { return inGate, nil }
	tick := func(at time.Time) {
		f.now = at
		d.runOverrunGuard(f.seams())
	}

	// Ten minutes inside its gate before the session dies.
	inGate = true
	for m := 5; m <= 15; m++ {
		tick(overrunSlung.Add(time.Duration(m) * time.Minute))
	}
	inGate = false
	tick(overrunSlung.Add(16 * time.Minute))

	// Restarted two hours later, then nine minutes inside its gate: 21 minutes
	// of its own time by the 30th minute, past the 20 minute stop.
	restart := overrunSlung.Add(2 * time.Hour)
	seat.RestartedAt = restart
	f.seats = []overrunSeat{seat}
	tick(restart.Add(time.Minute))
	inGate = true
	for m := 2; m <= 11; m++ {
		tick(restart.Add(time.Duration(m) * time.Minute))
	}
	if len(f.stops) != 0 {
		t.Fatalf("stopped inside its gate, 2m of its own time: %v", f.stops)
	}

	inGate = false
	tick(restart.Add(30 * time.Minute))
	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the stop at 21m of the restarted session's own time", f.stops)
	}
}

// The guard judges the bead's clock, not the seat's: a bead re-slung to a
// second seat is nudged or stopped on the dispatch it first went out on, even
// though that seat has held it only a few minutes. A done event ends that run,
// so a bead dispatched again after it is judged from the new dispatch
// (gt-tzbtf).
func TestRunOverrunGuard_JudgesTheBeadsDispatchClock(t *testing.T) {
	t.Parallel()
	line := func(ts, typ, bead, target string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":%q,"target":%q}}`, ts, typ, bead, target)
	}
	for _, tt := range []struct {
		name string
		// submittedAt is what the bead's last READY TO LAND block says: the end
		// of the run that submission was made in. The seat read hands it over
		// with the bead, so the tick reads no store (overrunSeatFor).
		submittedAt time.Time
		events      []string
		now         time.Time
		nudges      []string
		stops       []string
	}{
		{
			// The bead was slung to capable at 12:00 and re-slung to rictus at
			// 12:10; at 12:13 rictus is 3 minutes into its own sling but the
			// bead has run 13 minutes, so the nudge is due.
			name: "a second seat keeps the bead's clock",
			events: []string{
				line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:10:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
			},
			now:    time.Date(2026, 10, 13, 12, 13, 0, 0, time.UTC),
			nudges: []string{"gastown/rictus"},
		},
		{
			// Same bounce at the stop threshold: the guard stops the seat
			// holding the bead, on the bead's clock.
			name: "the stop runs on the bead's clock",
			events: []string{
				line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:10:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
			},
			now:   time.Date(2026, 10, 13, 12, 21, 0, 0, time.UTC),
			stops: []string{"gastown/rictus"},
		},
		{
			// The bead was submitted at 12:05, so the run that began at 12:00 is
			// over: the dispatch at 12:50 is three minutes old at 12:53 and the
			// guard acts on nothing.
			name: "a dispatch after a done event starts over",
			events: []string{
				line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:05:00Z", events.TypeDone, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:50:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
			},
			now: time.Date(2026, 10, 13, 12, 53, 0, 0, time.UTC),
		},
		{
			// The bead was submitted at 12:05 and rejected; the rework went out at
			// 12:50 and is three minutes old at 12:53. The first dispatch at 12:00
			// is not the clock: the guard acts on nothing.
			name:        "a re-sling after a rejected submission starts over",
			submittedAt: time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC),
			events: []string{
				line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:50:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
			},
			now: time.Date(2026, 10, 13, 12, 53, 0, 0, time.UTC),
		},
		{
			// The same rework runs on its own clock to the stop threshold: 20
			// minutes after the 12:50 re-sling, at 13:12, the guard stops the seat
			// holding it.
			name:        "the rework stops on its own clock",
			submittedAt: time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC),
			events: []string{
				line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
				line("2026-10-13T12:50:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
			},
			now:   time.Date(2026, 10, 13, 13, 12, 0, 0, time.UTC),
			stops: []string{"gastown/rictus"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, _ := newOverrunDaemon(t, false)
			body := strings.Join(tt.events, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(d.config.TownRoot, events.EventsFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "rictus", Bead: "gt-aaa11", SubmittedAt: tt.submittedAt})
			f.now = tt.now
			seams := f.seams()
			seams.slungAt = (&overrunSlungReader{d: d}).slungAt

			d.runOverrunGuard(seams)

			if !slices.Equal(f.nudges, tt.nudges) {
				t.Errorf("nudges = %v, want %v", f.nudges, tt.nudges)
			}
			if !slices.Equal(f.stops, tt.stops) {
				t.Errorf("stops = %v, want %v", f.stops, tt.stops)
			}
		})
	}
}

func TestRunOverrunGuard_UndatedSeatIsLeftAlone(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(45*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
	delete(f.slung, "gt-aaa11")

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("acted on an undated seat: nudges %v, stops %v", f.nudges, f.stops)
	}
}

func TestRunOverrunGuard_DisabledDoesNothing(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	d.patrolConfig = nil
	f := newOverrunFake(45*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})

	d.runOverrunGuard(f.seams())

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("disabled guard acted: nudges %v, stops %v", f.nudges, f.stops)
	}
}

// The guard's clock is the bead's, not the seat's: a bead re-slung to a second
// seat is still dated from its first dispatch, and a done event ends that run,
// so a bead dispatched again after it starts over (gt-tzbtf).
func TestOverrunSlungReader_DatesTheBeadsFirstDispatch(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	line := func(ts, typ, bead, target string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":%q,"target":%q},"visibility":"both"}`,
			ts, typ, bead, target)
	}
	body := strings.Join([]string{
		`{"ts":"2026-10-13T09:00:00Z","source":"gt","type":"done","actor":"x","payload":{"bead":"gt-aaa11"}}`,
		line("2026-10-13T10:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T12:30:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),  // another seat
		line("2026-10-13T12:40:00Z", events.TypeSling, "gt-bbb22", "gastown/polecats/capable"), // another bead
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(town, events.EventsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}

	// The other seat asks for the same bead's clock as the seat that was slung.
	for _, seat := range []string{"capable", "rictus"} {
		r := &overrunSlungReader{d: d}
		got, ok := r.slungAt(overrunSeat{Rig: "gastown", Polecat: seat, Bead: "gt-aaa11"})
		if !ok {
			t.Fatalf("%s: slungAt: not found", seat)
		}
		if want := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("%s: slungAt = %v, want the bead's first dispatch %v", seat, got, want)
		}
	}

	// A dispatch after the run's done event starts the clock over.
	body = strings.Join([]string{
		line("2026-10-13T10:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T11:00:00Z", events.TypeDone, "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T12:00:00Z", events.TypeSling, "gt-aaa11", "gastown/polecats/rictus"),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(town, events.EventsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &overrunSlungReader{d: d}
	got, ok := r.slungAt(overrunSeat{Rig: "gastown", Polecat: "rictus", Bead: "gt-aaa11"})
	if !ok {
		t.Fatal("slungAt: not found")
	}
	if want := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("slungAt = %v, want the dispatch after the done event, %v", got, want)
	}
}

// A submission for landing ends the run it was made in, so the bead's clock is
// the first sling since the submission, not the dispatch the run began with
// (gt-chd44). A bead that was never submitted keeps its first dispatch.
func TestOverrunSlungReader_ASubmissionEndsTheRun(t *testing.T) {
	t.Parallel()
	line := func(ts, typ string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":"gt-aaa11","target":"gastown/polecats/capable"},"visibility":"both"}`,
			ts, typ)
	}
	t10 := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	t12 := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name        string
		events      []string
		submittedAt time.Time
		want        time.Time
		ok          bool
	}{
		{
			name: "a re-sling after the submission starts the clock over",
			events: []string{
				line("2026-10-13T10:00:00Z", events.TypeSling),
				line("2026-10-13T12:00:00Z", events.TypeSling),
			},
			submittedAt: time.Date(2026, 10, 13, 10, 30, 0, 0, time.UTC),
			want:        t12,
			ok:          true,
		},
		{
			name:        "a submitted bead with no re-sling yet is dated from its submission",
			events:      []string{line("2026-10-13T10:00:00Z", events.TypeSling)},
			submittedAt: time.Date(2026, 10, 13, 10, 30, 0, 0, time.UTC),
			want:        time.Date(2026, 10, 13, 10, 30, 0, 0, time.UTC),
			ok:          true,
		},
		{
			name: "a bead never submitted keeps its first dispatch",
			events: []string{
				line("2026-10-13T10:00:00Z", events.TypeSling),
				line("2026-10-13T10:20:00Z", events.TypeSling),
			},
			want: t10,
			ok:   true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			body := strings.Join(tt.events, "\n") + "\n"
			if err := os.WriteFile(filepath.Join(town, events.EventsFile), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}

			r := &overrunSlungReader{d: d}
			got, ok := r.slungAt(overrunSeat{Rig: "gastown", Polecat: "rictus", Bead: "gt-aaa11", SubmittedAt: tt.submittedAt})
			if ok != tt.ok {
				t.Fatalf("slungAt found = %t, want %t (got %v)", ok, tt.ok, got)
			}
			if ok && !got.Equal(tt.want) {
				t.Errorf("slungAt = %v, want %v", got, tt.want)
			}
		})
	}
}

// A seat's bead read answers the whole bead, submission end included, so the
// guard's clock costs no second read (gt-chd44). A bead it cannot read is not
// judged, and the drop is logged: the submission it may hold is the run end the
// clock has to start after, so acting on the log alone could date a rework from
// the dispatch its submission ended.
func TestOverrunSeatFor_ReadsTheSubmissionAndDropsAnUnreadableBead(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	tm := newFakeTmux(newFixedClock())
	d.tmux = tm
	d.openWorkBeads = workBeadStore{
		"gt-aaa11": {ID: "gt-aaa11", Notes: readyNote("2026-10-13T12:05:00Z")},
	}.open

	seatFor := supervisor.SeatFor("gastown", string(constants.RolePolecat), "capable")
	restarts := []time.Time{
		time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 13, 12, 10, 0, 0, time.UTC),
	}
	if _, err := intent.Update(d.config.TownRoot, supervisor.IntentSeat(seatFor), func(r *intent.Record) error {
		r.WorkBead = "gt-aaa11"
		r.Restarts = restarts
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	tm.addSession(session.PolecatSessionName(d.prefixRegistry().PrefixForRig("gastown"), "capable"), "node", time.Now())

	seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable")
	if !ok {
		t.Fatalf("a readable seat was dropped:\n%s", buf.String())
	}
	if want := time.Date(2026, 10, 13, 12, 5, 0, 0, time.UTC); !seat.SubmittedAt.Equal(want) {
		t.Errorf("SubmittedAt = %v, want the READY TO LAND block's time %v", seat.SubmittedAt, want)
	}
	if want := restarts[1]; !seat.RestartedAt.Equal(want) {
		t.Errorf("RestartedAt = %v, want the newest restart stamp %v", seat.RestartedAt, want)
	}

	d.openWorkBeads = unreadableBeads{}.open
	if seat, ok := d.overrunSeatFor(t.TempDir(), "gastown", "capable"); ok {
		t.Fatalf("judged a seat whose bead could not be read: %+v", seat)
	}
	if !strings.Contains(buf.String(), "cannot read bead gt-aaa11") {
		t.Errorf("no alarm for the unreadable bead:\n%s", buf.String())
	}
}
