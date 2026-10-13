package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
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
	stops    []string
	nudgeErr error
}

func (f *overrunFake) seams() overrunSeams {
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
		stop: func(seat overrunSeat, elapsed time.Duration) error {
			f.stops = append(f.stops, seat.Rig+"/"+seat.Polecat)
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

func TestRunOverrunGuard_FailedNudgeRetriesNextTick(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(13*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"})
	f.nudgeErr = errors.New("no session")

	d.runOverrunGuard(f.seams())
	if !strings.Contains(buf.String(), "failed: no session") {
		t.Errorf("log lacks the nudge failure:\n%s", buf.String())
	}
	if st, _ := readOverrunState(d.config.TownRoot, "gt-aaa11"); !st.NudgedAt.IsZero() {
		t.Fatalf("a failed nudge was recorded as sent: %v", st.NudgedAt)
	}

	f.nudgeErr = nil
	f.now = f.now.Add(time.Minute)
	d.runOverrunGuard(f.seams())
	if len(f.nudges) != 1 {
		t.Errorf("nudges = %v, want the retry to deliver one", f.nudges)
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
