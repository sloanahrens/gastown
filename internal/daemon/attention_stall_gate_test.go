package daemon

import (
	"errors"
	"io"
	"log"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// gt-nh2dgf: a seat running its own gate changes no progress evidence for
// minutes, so collectPolecatStall reads the gate before it calls that quiet a
// stall.

// TestStallGateGateStateDecidesTheItem drives collectPolecatStall over one seat
// quiet for 15 minutes under each answer the gate seam can give: inside its
// gate raises nothing, outside raises the item as before, and a read that fails
// raises it, the direction the overrun guard takes.
func TestStallGateGateStateDecidesTheItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	quiet := now.Add(-15 * time.Minute) // past attentionPolecatStall (10m)
	tests := []struct {
		name     string
		gate     func(rig, name string) (bool, error)
		wantItem bool
	}{
		{name: "inside its gate", gate: func(string, string) (bool, error) { return true, nil }},
		{name: "no gate running", gate: func(string, string) (bool, error) { return false, nil }, wantItem: true},
		{name: "gate state unreadable", gate: func(string, string) (bool, error) { return false, errors.New("tmux unreachable") }, wantItem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			f.src.seats = func() ([]townhealth.Seat, error) {
				return []townhealth.Seat{{
					Rig: attentionRig, Name: "polecat/opal", Run: true,
					Sampled: now.Add(-time.Minute), Changed: quiet,
				}}, nil
			}
			f.src.seatWork = func(string, string) (bool, error) { return true, nil }
			var asked []string
			f.src.gateRunning = func(rig, name string) (bool, error) {
				asked = append(asked, rig+"/"+name)
				return tt.gate(rig, name)
			}

			items := f.collect(t, f.src.collectPolecatStall)

			if len(asked) != 1 || asked[0] != attentionRig+"/opal" {
				t.Fatalf("gate asked about %v, want once about gastown/opal", asked)
			}
			if !tt.wantItem {
				if len(items) != 0 {
					t.Fatalf("items = %+v, want none for a seat inside its own gate", items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatalf("items = %+v, want the stall item", items)
			}
			got := items[0]
			if got.Key != "stall:gastown/opal" || got.Kind != attention.KindPolecatStall ||
				got.Rig != attentionRig || got.Severity != attention.SeverityLow {
				t.Errorf("item = %+v, want stall:gastown/opal at low severity", got)
			}
			if want := "opal silent 15m with work in hand"; got.Summary != want {
				t.Errorf("summary = %q, want %q", got.Summary, want)
			}
		})
	}
}

// TestStallGateGatedSeatIsNotAskedForWork: the gate is read before the work
// bead, so a seat inside its gate costs no bd query.
func TestStallGateGatedSeatIsNotAskedForWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.seats = func() ([]townhealth.Seat, error) {
		return []townhealth.Seat{{
			Rig: attentionRig, Name: "polecat/opal", Run: true,
			Sampled: now.Add(-time.Minute), Changed: now.Add(-15 * time.Minute),
		}}, nil
	}
	f.src.gateRunning = func(string, string) (bool, error) { return true, nil }
	f.src.seatWork = func(string, string) (bool, error) {
		t.Error("a seat inside its gate was asked for work")
		return false, nil
	}

	if items := f.collect(t, f.src.collectPolecatStall); len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
}

// TestStallGateDefaultSeamAsksTheOverrunGuard: the default seam maps the walk's
// rig and polecat to the guard's seat and passes its answer through, error
// included.
func TestStallGateDefaultSeamAsksTheOverrunGuard(t *testing.T) {
	t.Parallel()
	readErr := errors.New("ps is gone")
	tests := []struct {
		name   string
		inGate bool
		err    error
	}{
		{name: "inside the gate", inGate: true},
		{name: "a failed read", err: readErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asked overrunSeat
			gate := attentionGateRunning(func(seat overrunSeat) (bool, error) {
				asked = seat
				return tt.inGate, tt.err
			})

			inGate, err := gate("gastown", "capable")

			if inGate != tt.inGate || !errors.Is(err, tt.err) {
				t.Fatalf("gate = %v, %v; want %v, %v", inGate, err, tt.inGate, tt.err)
			}
			if asked.Rig != "gastown" || asked.Polecat != "capable" {
				t.Errorf("guard asked about %+v, want gastown/capable", asked)
			}
		})
	}
}

// TestStallGateWiredIntoTheProductionSources: every tick's sources carry the
// guard's gate read, so the exemption exists outside tests.
func TestStallGateWiredIntoTheProductionSources(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config:         &Config{TownRoot: t.TempDir()},
		logger:         log.New(io.Discard, "", 0),
		forgejoBaseURL: noForgejo,
	}
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	if d.attentionSources(now).gateRunning == nil {
		t.Fatal("the production sources carry no gate read: a seat inside its gate would be called stalled")
	}
}
