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

	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

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
	// handedBack lists the beads handBack received, with the branch it was given.
	handedBack []string
	// pushed lists the bead and branch each push received; agentBeadsReset the
	// bead each agent-bead reset was for.
	pushed          []string
	agentBeadsReset []string
	// handBackNotes lists the note each handBack call carried, in the same order.
	handBackNotes []string
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
	return s
}

func (f *overrunFake) baseSeams() overrunSeams {
	return overrunSeams{
		now:          func() time.Time { return f.now },
		workingSeats: func() ([]overrunSeat, error) { return f.seats, nil },
		slungAt: func(bead, rig, polecat string) (time.Time, bool) {
			t, ok := f.slung[bead]
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
		gateRunning: f.gateRunning,
		pause:       func(seat overrunSeat) error { return f.step("pause", seat) },
		clearPause:  func(seat overrunSeat) error { return f.step("clear-pause", seat) },
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

func TestOverrunSlungReader_LatestSlingToSeat(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	line := func(ts, bead, target string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":%q,"target":%q},"visibility":"both"}`,
			ts, events.TypeSling, bead, target)
	}
	body := strings.Join([]string{
		line("2026-10-13T11:00:00Z", "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T12:00:00Z", "gt-aaa11", "gastown/polecats/capable"),
		line("2026-10-13T12:30:00Z", "gt-aaa11", "gastown/polecats/rictus"),  // another seat
		line("2026-10-13T12:40:00Z", "gt-bbb22", "gastown/polecats/capable"), // another bead
		`{"ts":"2026-10-13T11:30:00Z","source":"gt","type":"done","actor":"x","payload":{"bead":"gt-aaa11","target":"gastown/polecats/capable"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(town, events.EventsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
	r := &overrunSlungReader{d: d}

	got, ok := r.slungAt("gt-aaa11", "gastown", "capable")
	if !ok {
		t.Fatal("slungAt: not found")
	}
	if want := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("slungAt = %v, want %v", got, want)
	}
}
