package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/slot"
)

// The parse, pane-listing and descendant-walk the pane scan reads are
// polecat.ParsePanes, polecat.ParseProcessTable and polecat.DescendantArgvs,
// tested where they live; what is local to this guard is which commands count,
// which is what these two cover.
func TestOverrunGateFromCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    polecat.Invocation
		wantOK  bool
	}{
		{"plain", "make test", polecat.Invocation{Program: "make", Target: "test"}, true},
		{"env prefix", "GOFLAGS=-p=8 make test", polecat.Invocation{Program: "make", Target: "test"}, true},
		{"flags before the target", "make -j4 test", polecat.Invocation{Program: "make", Target: "test"}, true},
		{"path and case", "/Library/Developer/CommandLineTools/usr/bin/MAKE BUILD", polecat.Invocation{Program: "make", Target: "build"}, true},
		{"program only", "make", polecat.Invocation{}, false},
		{"program and flags only", "make -j4", polecat.Invocation{}, false},
		{"nothing to run", "FOO=bar", polecat.Invocation{}, false},
		{"empty", "   ", polecat.Invocation{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := overrunGateFromCommand(tt.command)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("overrunGateFromCommand(%q) = %+v, %v; want %+v, %v", tt.command, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestOverrunGateInvocations(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A rig with no config still carries the bare invocations a polecat runs
	// as its own gate, and every rig carries them once its config is read.
	gates := overrunGateInvocations(town, "gastown")
	if !slices.Contains(gates, polecat.Invocation{Program: "go", Target: "test"}) {
		t.Errorf("overrunGateInvocations = %v, want the bare go test among them", gates)
	}
}

// holdOverrunSlot takes the pool's slot 0 under role and writes the owner file
// the status reader decorates its report with. The slot is really held (a
// second flock on the same file is refused), so this is the fake slot holder
// the gate tests stand in for a polecat's own `gt slot run`.
func holdOverrunSlot(t *testing.T, townRoot, role string) {
	t.Helper()
	if err := os.MkdirAll(slot.LockDir(townRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := lock.FlockTryAcquire(slot.SlotLockPath(townRoot, 0))
	if err != nil || !ok {
		t.Fatalf("taking slot 0's flock: %v (held %v)", err, ok)
	}
	t.Cleanup(unlock)
	owner, err := json.Marshal(slot.Owner{Role: role, PID: 1, AcquiredAt: overrunSlung})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(slot.SlotOwnerPath(townRoot, 0), owner, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeOverrunGateReader returns the gate reader with the pane scan answered by
// fakes: the pane seams list one pane (pid 100) and report ps output. Tests
// that hold the pool slot never reach them, and the ones that do must not
// start a real tmux or ps.
func fakeOverrunGateReader(t *testing.T, d *Daemon, ps string, psErr error) *overrunGateReader {
	t.Helper()
	r := newOverrunGateReader(d)
	r.panes = func(string) ([]int, error) { return []int{100}, nil }
	r.run = func(*exec.Cmd) ([]byte, []byte, error) { return []byte(ps), nil, psErr }
	return r
}

func TestOverrunGateReader(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"}
	gatePS := "  100     1 claude\n  101   100 make gate\n"
	quietPS := "  100     1 claude\n  101   100 git status\n"

	t.Run("a held slot exempts the seat", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		holdOverrunSlot(t, d.config.TownRoot, "gastown/capable")
		r := fakeOverrunGateReader(t, d, quietPS, nil)
		got, err := r.running(seat)
		if err != nil || !got {
			t.Errorf("running = %v, %v; want true, nil for a seat holding its own slot", got, err)
		}
	})

	t.Run("the seat's GT_ROLE spelling of the role exempts it too", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		holdOverrunSlot(t, d.config.TownRoot, "gastown/polecats/capable")
		r := fakeOverrunGateReader(t, d, quietPS, nil)
		if got, err := r.running(seat); err != nil || !got {
			t.Errorf("running = %v, %v; want true, nil", got, err)
		}
	})

	t.Run("another holder's slot does not exempt the seat", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		holdOverrunSlot(t, d.config.TownRoot, "gastown/other")
		r := fakeOverrunGateReader(t, d, quietPS, nil)
		if got, err := r.running(seat); err != nil || got {
			t.Errorf("running = %v, %v; want false, nil: the slot is another polecat's", got, err)
		}
	})

	t.Run("a gate suite in the pane exempts the seat", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		r := fakeOverrunGateReader(t, d, gatePS, nil)
		got, err := r.running(seat)
		if err != nil || !got {
			t.Errorf("running = %v, %v; want true, nil while make gate runs in the pane", got, err)
		}
	})

	t.Run("a suite a Makefile gate spawned exempts the seat", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		// The shape of the devops gate that cost three polecats their seats on
		// 2026-10-16: make gate runs go test itself, so the live child is the
		// Go tool, not make.
		r := fakeOverrunGateReader(t, d, "  100     1 claude\n  101   100 /usr/local/go/bin/go test ./...\n", nil)
		if got, err := r.running(seat); err != nil || !got {
			t.Errorf("running = %v, %v; want true, nil while go test runs in the pane", got, err)
		}
	})

	t.Run("an idle pane is not a gate", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		r := fakeOverrunGateReader(t, d, quietPS, nil)
		if got, err := r.running(seat); err != nil || got {
			t.Errorf("running = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("an unreadable pane list is an error", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		r := fakeOverrunGateReader(t, d, gatePS, nil)
		r.panes = func(string) ([]int, error) { return nil, errors.New("tmux is gone") }
		if got, err := r.running(seat); err == nil || got {
			t.Errorf("running = %v, %v; want an error so the caller counts all of the seat's time", got, err)
		}
	})

	t.Run("a session with no pane is an error", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		r := fakeOverrunGateReader(t, d, gatePS, nil)
		r.panes = func(string) ([]int, error) { return nil, nil }
		if got, err := r.running(seat); err == nil || got {
			t.Errorf("running = %v, %v; want an error", got, err)
		}
	})

	t.Run("an unreadable process table is an error", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		r := fakeOverrunGateReader(t, d, "", errors.New("ps is gone"))
		if got, err := r.running(seat); err == nil || got {
			t.Errorf("running = %v, %v; want an error", got, err)
		}
	})

	t.Run("an unreadable slot state is an error", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		// A file where the lock directory belongs: the flock probe cannot run,
		// so the seat's gate state cannot be read at all.
		lockDir := filepath.Dir(slot.SlotLockPath(d.config.TownRoot, 0))
		if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(lockDir, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := fakeOverrunGateReader(t, d, quietPS, nil)
		if got, err := r.running(seat); err == nil || got {
			t.Errorf("running = %v, %v; want an error so the caller counts all of the seat's time", got, err)
		}
	})

	t.Run("the process table is read once per cycle", func(t *testing.T) {
		d, _ := newOverrunDaemon(t, false)
		reads := 0
		r := fakeOverrunGateReader(t, d, gatePS, nil)
		r.run = func(*exec.Cmd) ([]byte, []byte, error) {
			reads++
			return []byte(gatePS), nil, nil
		}
		for range 3 {
			if _, err := r.running(seat); err != nil {
				t.Fatalf("running: %v", err)
			}
		}
		if reads != 1 {
			t.Errorf("process table read %d times, want 1 for the cycle", reads)
		}
	})
}

// The gate accounting is per sling: a bead re-slung to the same seat after a
// rework starts a new clock, and the gate time of the run before it is not
// charged to this one.
func TestOverrunGateSpentIsPerSling(t *testing.T) {
	t.Parallel()
	first := overrunSlung
	second := overrunSlung.Add(time.Hour)

	const cap, total = 2 * time.Minute, time.Hour
	st, _ := (overrunState{Bead: "gt-gate1"}).withGate(first, first.Add(time.Minute), true, cap, total)
	st, spent := st.withGate(first, first.Add(2*time.Minute), true, cap, total)
	if spent != time.Minute {
		t.Fatalf("gate time inside one gate = %v, want 1m (the interval between the two ticks)", spent)
	}

	st, spent = st.withGate(second, second.Add(10*time.Second), false, cap, total)
	if spent != 0 {
		t.Errorf("gate time of a new sling = %v, want 0: the earlier run's gate time is not this run's", spent)
	}
	if st.GateSpent != 0 || !st.GateSeenAt.IsZero() {
		t.Errorf("state after a re-sling = %+v, want no gate accounting", st)
	}
}

// tickOverrunGuard runs the guard once per interval from the sling, the way the
// patrol does: the gate accounting is saved per tick, so a test that jumped the
// clock to the end could not tell banking from a gap.
func tickOverrunGuard(d *Daemon, f *overrunFake, ticks int, stride time.Duration) {
	for i := 1; i <= ticks; i++ {
		f.now = overrunSlung.Add(time.Duration(i) * stride)
		d.runOverrunGuard(f.seams())
	}
}

// A seat inside its own gate is working, not overrunning: 25 minutes of wall
// time behind a held gate is neither the nudge nor the stop (gt-vfwuc).
func TestRunOverrunGuard_GateTimeIsNotCounted(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	f.gateRunning = func(overrunSeat) (bool, error) { return true, nil }

	tickOverrunGuard(d, f, 25, time.Minute)

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("a seat in its gate was nudged %v / stopped %v after 25 minutes of wall time, want neither",
			f.nudges, f.stops)
	}
}

// The same, through the production gate reader and a fake slot holder: the
// seat holds the pool's slot under its own role for the whole run.
func TestRunOverrunGuard_HeldGateSlotIsNotAnOverrun(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	holdOverrunSlot(t, d.config.TownRoot, "gastown/capable")
	r := fakeOverrunGateReader(t, d, "  100     1 claude\n", nil)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	f.gateRunning = r.running

	tickOverrunGuard(d, f, 25, time.Minute)

	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Errorf("a seat holding the gate slot was nudged %v / stopped %v after 25 minutes, want neither (log:\n%s)",
			f.nudges, f.stops, buf.String())
	}
}

// Gate time is subtracted, not forgiven: the seat is stopped once its *non-gate*
// time passes the 20 minute threshold, even though that is 27 minutes of wall
// time behind an 8 minute gate (gt-vfwuc).
func TestRunOverrunGuard_NonGateTimeStillStops(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	inGate := true
	f.gateRunning = func(overrunSeat) (bool, error) { return inGate, nil }

	// Eight minutes inside the gate. The guard has seen the gate open at tick
	// one, so seven of those minutes are banked — the interval before the tick
	// that found the gate is never charged to it, which keeps the clock
	// counting (the fail-safe direction).
	tickOverrunGuard(d, f, 8, time.Minute)
	if len(f.nudges) != 0 || len(f.stops) != 0 {
		t.Fatalf("inside the gate: nudged %v / stopped %v, want neither", f.nudges, f.stops)
	}

	// Out of it, the clock resumes: 12 minutes of non-gate time fall due at 19
	// minutes of wall time and the stop at 20, which is minute 27.
	inGate = false
	var stoppedAt time.Time
	for i := 9; i <= 30 && stoppedAt.IsZero(); i++ {
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.stops) > 0 {
			stoppedAt = f.now
		}
	}
	if want := overrunSlung.Add(27 * time.Minute); !stoppedAt.Equal(want) {
		t.Errorf("stopped after %v of wall time, want %v: 20 minutes of non-gate time behind an 8 minute gate",
			stoppedAt.Sub(overrunSlung), want.Sub(overrunSlung))
	}
	if len(f.nudges) != 1 {
		t.Errorf("nudges = %v, want one at 12 minutes of non-gate time", f.nudges)
	}
}

// A gate state that cannot be read counts all of the seat's time: the guard
// fails toward stopping, never toward letting a seat run unbounded (gt-vfwuc).
func TestRunOverrunGuard_UnreadableGateStateCountsAllTime(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(21*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	f.gateRunning = func(overrunSeat) (bool, error) {
		return false, errors.New("container-gate slot status: boom")
	}

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the seat stopped: an unreadable gate state counts all of its time", f.stops)
	}
	if !strings.Contains(buf.String(), "cannot read the gate state") {
		t.Errorf("log lacks the read failure:\n%s", buf.String())
	}
}

// A dry run writes no state, so it has no banked gate time to subtract: it says
// what it sees rather than logging a stop a real run would not make.
func TestRunOverrunGuard_DryRunInAGateLogsNoStop(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, true)
	f := newOverrunFake(25*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	f.gateRunning = func(overrunSeat) (bool, error) { return true, nil }

	d.runOverrunGuard(f.seams())

	if strings.Contains(buf.String(), "would stop") || strings.Contains(buf.String(), "would nudge") {
		t.Errorf("dry run reported an action for a seat inside its gate:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "inside its gate") {
		t.Errorf("dry run did not report the gate:\n%s", buf.String())
	}
}

// The same holds for an escalation wait, read from the live escalation state: a
// dry run banks no escalation time either, so a seat the live guard exempts
// must not read as past the stop threshold on every dry tick (gt-4nqej).
func TestRunOverrunGuard_DryRunWaitingOnAnEscalationLogsNoStop(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, true)
	f := newOverrunFake(35*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-esc01"})
	f.escalationWaiting = func(overrunSeat) (bool, error) { return true, nil }

	d.runOverrunGuard(f.seams())

	if strings.Contains(buf.String(), "would stop") || strings.Contains(buf.String(), "would nudge") {
		t.Errorf("dry run reported an action for a seat waiting on its escalation:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "waiting on an escalation") {
		t.Errorf("dry run did not report the escalation wait:\n%s", buf.String())
	}
	if st, err := readOverrunState(d.config.TownRoot, "gt-esc01"); err != nil || !st.sameWait(overrunState{}) {
		t.Errorf("dry run wrote wait state %+v (err %v), want none", st, err)
	}
}

// The nudge names the clock it is on, because a seat waiting inside its own
// gate is the one case where a long-running bead is not an overrun (gt-vfwuc).
func TestOverrunNudgeMessageNamesGateTime(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"gate", "does not count"} {
		if !strings.Contains(overrunNudgeMessage, want) {
			t.Errorf("overrunNudgeMessage does not mention %q: %s", want, overrunNudgeMessage)
		}
	}
}

// The ceiling: however long a gate runs, one sling banks at most maxTotal of
// it (gt-vfwuc).
func TestOverrunStateWithGate_TotalIsCapped(t *testing.T) {
	t.Parallel()
	slung := overrunSlung
	const maxCharge, maxTotal = 2 * time.Minute, 10 * time.Minute

	var st overrunState
	var spent time.Duration
	for i := 1; i <= 90; i++ {
		st, spent = st.withGate(slung, slung.Add(time.Duration(i)*time.Minute), true, maxCharge, maxTotal)
	}
	if spent != maxTotal || st.GateSpent != maxTotal {
		t.Errorf("gate time after 90 minutes inside one gate = %v (state %v), want the ceiling %v",
			spent, st.GateSpent, maxTotal)
	}
}

// The ceiling binds a record that already holds more than it, such as a state
// file written for a sling under a longer threshold, whether or not the seat is
// still inside the gate (gt-vfwuc).
func TestOverrunStateWithGate_TotalCapsALoadedRecord(t *testing.T) {
	t.Parallel()
	slung := overrunSlung
	const maxCharge, maxTotal = 2 * time.Minute, 40 * time.Minute

	tests := []struct {
		name   string
		inGate bool
	}{
		{"still inside the gate", true},
		{"out of the gate", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := overrunState{Bead: "gt-gate1", GateSlingAt: slung, GateSpent: 50 * time.Minute, GateSeenAt: slung}
			st, spent := st.withGate(slung, slung.Add(time.Minute), tt.inGate, maxCharge, maxTotal)
			if spent != maxTotal || st.GateSpent != maxTotal {
				t.Errorf("banked gate time from a record holding 50m = %v (state %v), want %v", spent, st.GateSpent, maxTotal)
			}
		})
	}
}

// A clock that steps back does not give banked gate time back: the stretch since
// the stamp is dropped, and what is not charged counts toward the stop
// (gt-vfwuc).
func TestOverrunStateWithGate_BackwardStepKeepsBankedTime(t *testing.T) {
	t.Parallel()
	slung := overrunSlung
	const maxCharge, maxTotal = 2 * time.Minute, time.Hour

	var st overrunState
	st, _ = st.withGate(slung, slung.Add(time.Minute), true, maxCharge, maxTotal)
	st, spent := st.withGate(slung, slung.Add(2*time.Minute), true, maxCharge, maxTotal)
	if spent != time.Minute {
		t.Fatalf("banked after two ticks = %v, want 1m", spent)
	}

	st, spent = st.withGate(slung, slung.Add(30*time.Second), true, maxCharge, maxTotal)
	if spent != time.Minute {
		t.Errorf("banked after a stamp 90s in the future = %v, want 1m: a backward step does not give gate time back", spent)
	}
	if !st.GateSeenAt.IsZero() {
		t.Errorf("GateSeenAt = %v after a backward step, want no stamp so the next tick starts a fresh stretch", st.GateSeenAt)
	}
}

// A gate that never returns is a stuck seat: past the ceiling the clock runs
// again even inside the gate, so the seat is nudged and then stopped a stop
// threshold after the ceiling — an hour of wall time at the defaults (gt-vfwuc).
func TestRunOverrunGuard_HungGateIsStopped(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})
	f.gateRunning = func(overrunSeat) (bool, error) { return true, nil }

	nudgeAfter, stopAfter := overrunGuardThresholds(d.patrolConfig)
	ceiling := overrunMaxGateTotal(stopAfter)

	var nudgedAt, stoppedAt time.Time
	for i := 1; i <= 120 && stoppedAt.IsZero(); i++ {
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if nudgedAt.IsZero() && len(f.nudges) > 0 {
			nudgedAt = f.now
		}
		if len(f.stops) > 0 {
			stoppedAt = f.now
		}
	}

	if stoppedAt.IsZero() {
		t.Fatalf("a seat inside a gate that never returns was never stopped:\n%s", buf.String())
	}
	if want := overrunSlung.Add(ceiling + stopAfter); !stoppedAt.Equal(want) {
		t.Errorf("stopped after %v of wall time, want %v (%v inside the gate, then %v of the seat's own time)",
			stoppedAt.Sub(overrunSlung), ceiling+stopAfter, ceiling, stopAfter)
	}
	if want := overrunSlung.Add(ceiling + nudgeAfter); !nudgedAt.Equal(want) {
		t.Errorf("nudged after %v of wall time, want %v: the ceiling, then the nudge threshold",
			nudgedAt.Sub(overrunSlung), ceiling+nudgeAfter)
	}
}

// The gate accounting outlives the tick that saw it: it is saved when it moves,
// so a later tick — and the stop — measures the same clock (gt-vfwuc).
func TestRunOverrunGuard_GateAccountingIsSaved(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"}
	f := newOverrunFake(0, seat)
	f.gateRunning = func(overrunSeat) (bool, error) { return true, nil }

	tickOverrunGuard(d, f, 3, time.Minute)

	st, err := readOverrunState(d.config.TownRoot, seat.Bead)
	if err != nil {
		t.Fatalf("readOverrunState: %v", err)
	}
	if want := 2 * time.Minute; st.GateSpent != want {
		t.Errorf("banked gate time on disk = %v, want %v (the first tick only opens the stretch)", st.GateSpent, want)
	}
	if want := overrunSlung.Add(3 * time.Minute); !st.GateSeenAt.Equal(want) {
		t.Errorf("GateSeenAt = %v, want the last tick that saw the gate, %v", st.GateSeenAt, want)
	}
	if !st.GateSlingAt.Equal(overrunSlung) {
		t.Errorf("GateSlingAt = %v, want the sling, %v", st.GateSlingAt, overrunSlung)
	}
}

// An unset gate seam is no exemption: with no gate reader the guard counts every
// seat's time, so a daemon that cannot answer the question still stops an
// overrun (gt-vfwuc).
func TestRunOverrunGuard_UnsetGateSeamCountsAllTime(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	// gateRunning is left nil: the guard substitutes "not in a gate".
	f := newOverrunFake(20*time.Minute, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"})

	d.runOverrunGuard(f.seams())

	if len(f.stops) != 1 {
		t.Errorf("stops = %v, want the seat stopped at 20 minutes with no gate seam", f.stops)
	}
}
