package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// gateTickRead is one gate read: ok is whether the read answered, inGate its
// answer.
type gateTickRead struct{ ok, inGate bool }

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
