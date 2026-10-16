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
	"github.com/steveyegge/gastown/internal/slot"
)

func TestParseOverrunPanes(t *testing.T) {
	t.Parallel()
	out := "gt-slit\t100\nother\t200\ngt-slit\t101\n\ngt-slit\tnotapid\ngt-slit\t0\n"
	if got, want := parseOverrunPanes(out, "gt-slit"), []int{100, 101}; !slices.Equal(got, want) {
		t.Errorf("parseOverrunPanes = %v, want %v", got, want)
	}
	if got := parseOverrunPanes(out, "absent"); len(got) != 0 {
		t.Errorf("parseOverrunPanes for an absent session = %v, want none", got)
	}
}

func TestParseOverrunProcessTable(t *testing.T) {
	t.Parallel()
	out := "  PID  PPID ARGS\n" +
		"  1     0 /sbin/launchd\n" +
		"100     1 claude --dangerously-skip-permissions\n" +
		"101   100 make -j4 test\n" +
		"junk line\n" +
		"102   bad ps itself\n"
	got := parseOverrunProcessTable(out)
	want := []overrunGateProc{
		{PID: 1, PPID: 0, Args: "/sbin/launchd"},
		{PID: 100, PPID: 1, Args: "claude --dangerously-skip-permissions"},
		{PID: 101, PPID: 100, Args: "make -j4 test"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseOverrunProcessTable =\n%v\nwant\n%v", got, want)
	}
}

func TestOverrunDescendantArgvs(t *testing.T) {
	t.Parallel()
	procs := []overrunGateProc{
		{PID: 100, PPID: 1, Args: "pane agent"},
		{PID: 101, PPID: 100, Args: "bash -c make test"},
		{PID: 102, PPID: 101, Args: "go test ./..."},
		{PID: 103, PPID: 1, Args: "unrelated"},
	}
	got := overrunDescendantArgvs([]int{100}, procs)
	want := []string{"bash -c make test", "go test ./..."}
	if !slices.Equal(got, want) {
		t.Errorf("overrunDescendantArgvs = %v, want %v (the root itself and other trees excluded)", got, want)
	}
	if got := overrunDescendantArgvs([]int{999}, procs); len(got) != 0 {
		t.Errorf("overrunDescendantArgvs of an absent root = %v, want none", got)
	}
}

func TestOverrunGateFromCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    overrunGate
		wantOK  bool
	}{
		{"plain", "make test", overrunGate{Program: "make", Target: "test"}, true},
		{"env prefix", "GOFLAGS=-p=8 make test", overrunGate{Program: "make", Target: "test"}, true},
		{"flags before the target", "make -j4 test", overrunGate{Program: "make", Target: "test"}, true},
		{"path and case", "/Library/Developer/CommandLineTools/usr/bin/MAKE BUILD", overrunGate{Program: "make", Target: "build"}, true},
		{"program only", "make", overrunGate{}, false},
		{"program and flags only", "make -j4", overrunGate{}, false},
		{"nothing to run", "FOO=bar", overrunGate{}, false},
		{"empty", "   ", overrunGate{}, false},
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

func TestOverrunGateArgv(t *testing.T) {
	t.Parallel()
	gates := slices.Concat(overrunBareGateInvocations,
		[]overrunGate{{Program: "make", Target: "gate"}, {Program: "make", Target: "test"}})

	tests := []struct {
		name string
		argv string
		want string
	}{
		{"a bare gate", "make gate", "make gate"},
		{"a make in the middle of a shell body", "/bin/sh -c make test", "make test"},
		{"a background wrapper", "bash -c eval 'gt slot run --role gastown/slit -- make gate' < /dev/null", "gt slot run"},
		{"a wrapper with no slot", "sh -c \"make -j4 test\"", "make test"},
		{"the Go quality gate", "go test ./internal/daemon -run TestOverrun", "go test"},
		{"go vet", "go vet ./...", "go vet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := overrunGateArgv(tt.argv, gates)
			if !ok || got != tt.want {
				t.Errorf("overrunGateArgv(%q) = %q, %v; want %q, true", tt.argv, got, ok, tt.want)
			}
		})
	}

	notGates := []string{
		"claude --dangerously-skip-permissions",
		"git status",
		"go build ./...",
		"make",
		"",
	}
	for _, argv := range notGates {
		if got, ok := overrunGateArgv(argv, gates); ok {
			t.Errorf("overrunGateArgv(%q) = %q, want no gate", argv, got)
		}
	}
}

func TestOverrunGateInvocations(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A rig with no config still carries the bare invocations a polecat runs
	// as its own gate, and every rig carries them once its config is read.
	gates := overrunGateInvocations(town, "gastown")
	if !slices.Contains(gates, overrunGate{Program: "go", Target: "test"}) {
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

	const cap = 2 * time.Minute
	st, _ := (overrunState{Bead: "gt-gate1"}).withGate(first, first.Add(time.Minute), true, cap)
	st, spent := st.withGate(first, first.Add(2*time.Minute), true, cap)
	if spent != time.Minute {
		t.Fatalf("gate time inside one gate = %v, want 1m (the interval between the two ticks)", spent)
	}

	st, spent = st.withGate(second, second.Add(10*time.Second), false, cap)
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
