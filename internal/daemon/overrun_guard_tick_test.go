package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// gateTickRead is one gate read: ok is whether the read answered, inGate its
// answer.
type gateTickRead struct{ ok, inGate bool }

// scriptedGateReads serves the reads in order, then fails every read after them.
func scriptedGateReads(reads ...gateTickRead) func(overrunSeat) (bool, error) {
	i := 0
	return func(overrunSeat) (bool, error) {
		i++
		if i > len(reads) || !reads[i-1].ok {
			return false, errors.New("container-gate slot status: boom")
		}
		return reads[i-1].inGate, nil
	}
}

// tickUntilStop ticks once a minute and returns the wall time of the first stop,
// zero when there is none within maxTicks.
func tickUntilStop(d *Daemon, f *overrunFake, maxTicks int) time.Time {
	for i := 1; i <= maxTicks; i++ {
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.stops) > 0 {
			return f.now
		}
	}
	return time.Time{}
}

// A failed gate read carries the seat's last in-gate answer for two failures in
// a row and counts all of its time from the third; a successful read starts the
// run over. The log says which the guard did (gt-fsgn30).
func TestTickEffGateReadFailureCarriesState(t *testing.T) {
	t.Parallel()

	t.Run("two failures carry, the third counts all of the time", func(t *testing.T) {
		t.Parallel()
		d, buf := newOverrunDaemon(t, false)
		f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-carry1"})
		f.gateRunning = scriptedGateReads(gateTickRead{true, true})

		// Tick 1 opens the in-gate stretch; ticks 2 and 3 carry it and bank two
		// minutes. Tick 4, the third failure, counts the seat's own time, so the
		// 20 minute stop lands at 22 minutes of wall time. With the carry unbounded
		// the stop would wait for the 40 minute gate ceiling: 60 minutes.
		stoppedAt := tickUntilStop(d, f, 60)
		if want := overrunSlung.Add(22 * time.Minute); !stoppedAt.Equal(want) {
			t.Errorf("stopped at %v, want %v: two carried failures, then the third counts all its time",
				stoppedAt.Sub(overrunSlung), want.Sub(overrunSlung))
		}
		if got := strings.Count(buf.String(), "carrying its last gate answer"); got != 2 {
			t.Errorf("carry lines = %d, want 2 (one per carried failure):\n%s", got, buf.String())
		}
		if !strings.Contains(buf.String(), "(failure 3 in a row), counting all of its time") {
			t.Errorf("log does not say the third failure counts all of its time:\n%s", buf.String())
		}
	})

	t.Run("a successful read starts the run over", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		f := newOverrunFake(0, overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-carry2"})
		// Two failures, an in-gate read that resets the run, then two more failures
		// that carry: the third failure after the reset is the one that stops.
		f.gateRunning = scriptedGateReads(
			gateTickRead{true, true}, gateTickRead{false, false}, gateTickRead{false, false},
			gateTickRead{true, true}, gateTickRead{false, false}, gateTickRead{false, false},
		)

		// Ticks 2, 3, 4 bank the first minutes; tick 5 and 6 carry and bank two more:
		// five minutes of gate time, so the stop lands at 25 minutes of wall time.
		stoppedAt := tickUntilStop(d, f, 60)
		if want := overrunSlung.Add(25 * time.Minute); !stoppedAt.Equal(want) {
			t.Errorf("stopped at %v, want %v: the successful read resets the failure run",
				stoppedAt.Sub(overrunSlung), want.Sub(overrunSlung))
		}
	})
}

// The state carries the last in-gate answer across failed gate reads for at
// most two failures: a broken read does not exempt the seat for good, and one
// failure does not stop counting its time (gt-1ve5qh).
func TestTickEffGateStateCarry(t *testing.T) {
	t.Parallel()
	in, out, failed := gateTickRead{true, true}, gateTickRead{true, false}, gateTickRead{false, false}

	tests := []struct {
		name        string
		reads       []gateTickRead
		wantIn      bool
		wantCarried bool
	}{
		{"no read yet", nil, false, false},
		{"an in-gate answer stands", []gateTickRead{in}, true, true},
		{"one failure carries", []gateTickRead{in, failed}, true, true},
		{"two failures carry", []gateTickRead{in, failed, failed}, true, true},
		{"a third failure stops the carry", []gateTickRead{in, failed, failed, failed}, false, false},
		{"a success resets the run", []gateTickRead{in, failed, failed, in, failed, failed}, true, true},
		{"an outside answer is not carried", []gateTickRead{out, failed, failed}, false, false},
		{"a later outside answer clears the in-gate one", []gateTickRead{in, failed, out, failed}, false, false},
		{"failures with no successful read carry nothing", []gateTickRead{failed, failed}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var st overrunState
			for _, r := range tt.reads {
				st.recordGateRead(r.ok, r.inGate)
			}
			if gotIn, carried := st.carriedGate(); gotIn != tt.wantIn || carried != tt.wantCarried {
				t.Errorf("carriedGate() = (%v, %v), want (%v, %v)", gotIn, carried, tt.wantIn, tt.wantCarried)
			}
		})
	}
}

func TestTickEffGateStatePersisted(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	t.Run("an old record without the fields reads as zero", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(town, ".runtime", "overrun-guard")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		legacy := `{"bead":"gt-old01","nudged_at":"2026-11-19T12:00:00Z"}`
		if err := os.WriteFile(filepath.Join(dir, "gt-old01.json"), []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		st, err := readOverrunState(town, "gt-old01")
		if err != nil {
			t.Fatalf("readOverrunState: %v", err)
		}
		if st.GateIn || st.GateReadFailures != 0 {
			t.Errorf("gate fields = (%v, %d), want (false, 0)", st.GateIn, st.GateReadFailures)
		}
		if in, carried := st.carriedGate(); in || carried {
			t.Errorf("carriedGate() = (%v, %v), want (false, false)", in, carried)
		}
	})

	t.Run("the fields round-trip", func(t *testing.T) {
		t.Parallel()
		want := overrunState{Bead: "gt-carry1", GateIn: true, GateReadFailures: 2}
		if err := writeOverrunState(town, want); err != nil {
			t.Fatalf("writeOverrunState: %v", err)
		}
		got, err := readOverrunState(town, want.Bead)
		if err != nil {
			t.Fatalf("readOverrunState: %v", err)
		}
		if !got.GateIn || got.GateReadFailures != 2 {
			t.Errorf("gate fields = (%v, %d), want (true, 2)", got.GateIn, got.GateReadFailures)
		}
	})

	t.Run("a failed read moves the write", func(t *testing.T) {
		t.Parallel()
		before := overrunState{GateIn: true}
		after := before
		after.recordGateRead(false, false)
		if after.sameWait(before) {
			t.Error("sameWait says a lengthened failure run did not move, so the tick would not save it")
		}
	})
}

// The gate answer and the run of failed reads belong to one sling: a bead slung
// again starts with neither, so the last sling's in-gate answer cannot exempt
// the new sling's first minutes (gt-uojbm7). Three in-gate reads of the first
// sling, then the bead is slung again and every read fails: nothing is carried,
// so the 20 minute stop lands 20 minutes after the new sling. A carried answer
// would exempt the first failed reads and push the stop later.
func TestTickEffGateStateResetsOnANewSling(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-resling1"}
	f := newOverrunFake(0, seat)
	in := gateTickRead{true, true}
	f.gateRunning = scriptedGateReads(in, in, in)

	for i := 1; i <= 3; i++ {
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
	}
	if len(f.stops) != 0 {
		t.Fatalf("setup: the first sling was stopped %v, want it inside its gate", f.stops)
	}

	newSling := overrunSlung.Add(3 * time.Minute)
	f.slung[seat.Bead] = newSling
	var stoppedAt time.Time
	for i := 1; i <= 60 && stoppedAt.IsZero(); i++ {
		f.now = newSling.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(f.seams())
		if len(f.stops) > 0 {
			stoppedAt = f.now
		}
	}
	if want := newSling.Add(20 * time.Minute); !stoppedAt.Equal(want) {
		t.Errorf("the second sling stopped at %v after its sling, want %v: the first sling's gate answer must not carry",
			stoppedAt.Sub(newSling), want.Sub(newSling))
	}
}

// The gate answer belongs to the sling, which only a new sling event changes:
// a seat restarted inside the sling keeps the answer however the restart shows
// up. The run here is floored to its session start (slungFloor: the session was
// created long after the sling), and the restart either carries a stamp or only
// moves the session start, which moves the floored value the guard measures
// from. The first failed read after it must still carry the answer
// (gt-uojbm7, om 6f8521c653db and 3a34b4b0fa6e).
func TestTickEffGateAnswerSurvivesARestartInTheSameSling(t *testing.T) {
	t.Parallel()
	for name, restart := range map[string]func(*overrunSeat){
		"a supervised restart stamp": func(s *overrunSeat) { s.RestartedAt = overrunSlung.Add(40 * time.Minute) },
		"a later session start":      func(s *overrunSeat) { s.SessionStart = overrunSlung.Add(50 * time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, buf := newOverrunDaemon(t, false)
			seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-restart1", SessionStart: overrunSlung.Add(30 * time.Minute)}
			f := newOverrunFake(0, seat)
			in := gateTickRead{true, true}
			f.gateRunning = scriptedGateReads(in, in, in)
			for i := 1; i <= 3; i++ {
				f.now = overrunSlung.Add(31*time.Minute + time.Duration(i)*time.Minute)
				d.runOverrunGuard(f.seams())
			}
			restart(&f.seats[0])
			f.now = overrunSlung.Add(55 * time.Minute)
			d.runOverrunGuard(f.seams())
			if got := strings.Count(buf.String(), "(failure 1 in a row), carrying its last gate answer"); got != 1 {
				t.Errorf("the first failed read after %s carried the answer %d time(s), want once:\n%s", name, got, buf.String())
			}
		})
	}
}

// A record written before the sling field existed names no sling, so its first
// tick starts the answer over: the failed read counts all of the seat's time
// instead of carrying an answer of unknown age, and the record then names the
// sling (gt-uojbm7, om 374a866c1151).
func TestTickEffGateAnswerOfARecordWithoutASlingStartsOver(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-legacy1"}
	if err := writeOverrunState(d.config.TownRoot, overrunState{Bead: seat.Bead, GateIn: true}); err != nil {
		t.Fatal(err)
	}
	f := newOverrunFake(0, seat)
	f.gateRunning = scriptedGateReads(gateTickRead{false, false})
	f.now = overrunSlung.Add(2 * time.Minute)
	d.runOverrunGuard(f.seams())
	if strings.Contains(buf.String(), "carrying its last gate answer") {
		t.Errorf("a record with no sling carried its answer:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "counting all of its time") {
		t.Errorf("the failed read did not count all of the seat's time:\n%s", buf.String())
	}
	if st, err := readOverrunState(d.config.TownRoot, seat.Bead); err != nil || !st.GateAnswerSlingAt.Equal(overrunSlung) {
		t.Errorf("record after the tick = %+v, %v; want the sling %v named", st, err, overrunSlung)
	}
}

// dispatchAt is the sling event's time, which a first spawn beyond the startup
// grace does not move: slungAt answers the spawn, dispatchAt the sling.
func TestOverrunSlungReaderDispatchAtIsTheSlingEvent(t *testing.T) {
	t.Parallel()
	d, _ := newOverrunDaemon(t, false)
	line := func(ts, typ, payload string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"gastown/polecats/capable","payload":%s}`, ts, typ, payload)
	}
	body := line("2026-10-13T12:00:00Z", events.TypeSling, `{"bead":"gt-aaa11","target":"gastown/polecats/capable"}`) + "\n" +
		line("2026-10-13T12:30:00Z", events.TypeSpawn, `{"rig":"gastown","polecat":"capable"}`) + "\n"
	if err := os.WriteFile(filepath.Join(d.config.TownRoot, events.EventsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &overrunSlungReader{d: d}
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-aaa11"}
	sling := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	if got, ok := r.dispatchAt(seat); !ok || !got.Equal(sling) {
		t.Errorf("dispatchAt = %v, %v; want the sling %v", got, ok, sling)
	}
	if got, ok := r.slungAt(seat); !ok || !got.Equal(sling.Add(30*time.Minute)) {
		t.Errorf("slungAt = %v, %v; want the first spawn %v", got, ok, sling.Add(30*time.Minute))
	}
}

// A run whose log holds no sling event is dated from the agent bead's update
// time, which moves whenever that bead is written. It names no sling, so the
// gate answer and the run of failed reads stay across ticks (the run of failures
// accumulates to a second one), and a sling event that then appears starts them
// over once (gt-uojbm7, overseer read of 2e7d28129).
func TestTickEffGateAnswerOfARunWithNoSlingEventIsKept(t *testing.T) {
	t.Parallel()
	d, buf := newOverrunDaemon(t, false)
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-nosling1"}
	f := newOverrunFake(0, seat)
	in := gateTickRead{true, true}
	f.gateRunning = scriptedGateReads(in, in, in)
	var sling time.Time
	seams := f.seams()
	seams.dispatchAt = func(overrunSeat) (time.Time, bool) { return sling, !sling.IsZero() }
	tick := func(i int) {
		f.slung[seat.Bead] = overrunSlung.Add(time.Duration(i) * time.Second)
		f.now = overrunSlung.Add(time.Duration(i) * time.Minute)
		d.runOverrunGuard(seams)
	}
	for i := 1; i <= 5; i++ {
		tick(i)
	}
	if !strings.Contains(buf.String(), "(failure 2 in a row), carrying its last gate answer") {
		t.Errorf("the failed reads did not accumulate across ticks of a fallback clock that moves:\n%s", buf.String())
	}
	buf.Reset()
	sling = overrunSlung
	tick(6)
	if !strings.Contains(buf.String(), "(failure 1 in a row), counting all of its time") {
		t.Errorf("a sling event that appeared did not start the run over once:\n%s", buf.String())
	}
}
