package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
