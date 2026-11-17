package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/session"
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
			// The exemption's ceiling, past what a 15 minute quiet can reach.
			f.src.gateExemptFor = 40 * time.Minute
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
	f.src.gateExemptFor = 40 * time.Minute
	f.src.seatWork = func(string, string) (bool, error) {
		t.Error("a seat inside its gate was asked for work")
		return false, nil
	}

	if items := f.collect(t, f.src.collectPolecatStall); len(items) != 0 {
		t.Fatalf("items = %+v, want none", items)
	}
}

// TestStallGateExemptionIsBounded: a gate explains a stretch of quiet, not an
// unbounded one. Past the ceiling on the gate time one sling may bank — the
// overrun guard's own ceiling — a gate that has not returned is a stuck seat,
// and the item is raised even while the gate still answers true (gt-mcvyw0).
func TestStallGateExemptionIsBounded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	const ceiling = 40 * time.Minute
	tests := []struct {
		name     string
		quiet    time.Duration
		inGate   bool
		wantItem bool
	}{
		{name: "inside its gate under the ceiling", quiet: 30 * time.Minute, inGate: true},
		{name: "inside its gate at the ceiling", quiet: ceiling, inGate: true},
		{name: "inside its gate past the ceiling", quiet: ceiling + time.Minute, inGate: true, wantItem: true},
		{name: "outside its gate past the ceiling", quiet: ceiling + time.Minute, wantItem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			f.src.gateExemptFor = ceiling
			f.src.seats = func() ([]townhealth.Seat, error) {
				return []townhealth.Seat{{
					Rig: attentionRig, Name: "polecat/opal", Run: true,
					Sampled: now.Add(-time.Minute), Changed: now.Add(-tt.quiet),
				}}, nil
			}
			f.src.seatWork = func(string, string) (bool, error) { return true, nil }
			f.src.gateRunning = func(string, string) (bool, error) { return tt.inGate, nil }

			items := f.collect(t, f.src.collectPolecatStall)

			if tt.wantItem != (len(items) == 1) {
				t.Fatalf("items = %+v, want item %v for a %s quiet past a gate answering %v",
					items, tt.wantItem, tt.quiet, tt.inGate)
			}
			if tt.wantItem && items[0].Key != "stall:gastown/opal" {
				t.Errorf("item = %+v, want stall:gastown/opal", items[0])
			}
		})
	}
}

// TestStallGateExemptForFollowsTheOverrunGuard: the ceiling is the guard's
// own, configured or default, so the stall item and the guard cannot disagree
// about how long a gate explains a seat's silence (gt-mcvyw0).
func TestStallGateExemptForFollowsTheOverrunGuard(t *testing.T) {
	t.Parallel()
	guard := func(allowance string) *DaemonPatrolConfig {
		return &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			OverrunGuard: &OverrunGuardConfig{Enabled: true, GateAllowance: allowance},
		}}
	}
	tests := []struct {
		name string
		cfg  *DaemonPatrolConfig
		want time.Duration
	}{
		{name: "unset is twice the default stop threshold", want: 40 * time.Minute},
		{name: "configured", cfg: guard("15m"), want: 15 * time.Minute},
		{name: "zero exempts nothing", cfg: guard("0")},
		{name: "unusable falls back", cfg: guard("five minutes"), want: 40 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{config: &Config{TownRoot: t.TempDir()}, patrolConfig: tt.cfg}
			if got := d.attentionGateExemptFor(); got != tt.want {
				t.Errorf("attentionGateExemptFor = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestStallGateDrivesTheRealGateReader: the default seam's reader — the
// overrun guard's own constructor, its ps and pane reads replaced by their
// test seams — answers through the collector. A seat with make gate live in
// its pane raises nothing; the same seat with an idle pane raises the item
// (gt-mcvyw0).
func TestStallGateDrivesTheRealGateReader(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	const gatePS = "  100     1 claude\n  101   100 make gate\n"
	const quietPS = "  100     1 claude\n  101   100 git status\n"
	tests := []struct {
		name     string
		ps       string
		wantItem bool
	}{
		{name: "a gate suite in the pane", ps: gatePS},
		{name: "an idle pane", ps: quietPS, wantItem: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			f.src.gateExemptFor = 40 * time.Minute
			f.src.seats = func() ([]townhealth.Seat, error) {
				return []townhealth.Seat{{
					Rig: attentionRig, Name: "polecat/opal", Run: true,
					Sampled: now.Add(-time.Minute), Changed: now.Add(-15 * time.Minute),
				}}, nil
			}
			f.src.seatWork = func(string, string) (bool, error) { return true, nil }
			r := fakeOverrunGateReader(t, f.d, tt.ps, nil)
			// The reader must ask about this seat's own session: a mapping that
			// named another seat would read another pane and answer for it.
			wantSession := session.PolecatSessionName(session.DefaultRegistry().PrefixForRig(attentionRig), "opal")
			r.panes = func(sessionName string) ([]int, error) {
				if sessionName != wantSession {
					return nil, fmt.Errorf("panes asked for %q, want the seat's own session %q", sessionName, wantSession)
				}
				return []int{100}, nil
			}
			f.src.gateRunning = attentionGateRunning(r.running)

			items := f.collect(t, f.src.collectPolecatStall)

			if tt.wantItem != (len(items) == 1) {
				t.Fatalf("items = %+v, want item %v from the guard's own gate read over %q", items, tt.wantItem, tt.ps)
			}
		})
	}
}

// TestStallGateFailedReadIsLogged: a gate state that cannot be read raises the
// item, and the log says so once with the seat and the error, so a broken read
// is visible rather than looking like a quiet seat (gt-mcvyw0).
func TestStallGateFailedReadIsLogged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	f.src.seats = func() ([]townhealth.Seat, error) {
		return []townhealth.Seat{{
			Rig: attentionRig, Name: "polecat/opal", Run: true,
			Sampled: now.Add(-time.Minute), Changed: now.Add(-15 * time.Minute),
		}}, nil
	}
	f.src.seatWork = func(string, string) (bool, error) { return true, nil }
	f.src.gateRunning = func(string, string) (bool, error) { return false, errors.New("tmux is gone") }

	items := f.collect(t, f.src.collectPolecatStall)

	if len(items) != 1 {
		t.Fatalf("items = %+v, want the stall item raised on an unreadable gate state", items)
	}
	want := "attention: cannot read the gate state for gastown/opal, raising its stall: tmux is gone"
	if !strings.Contains(f.logs.String(), want) {
		t.Errorf("log = %q, want %q", f.logs.String(), want)
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
// guard's gate read and the guard's ceiling on it, so the exemption exists
// outside tests and ends where the guard's does (gt-mcvyw0).
func TestStallGateWiredIntoTheProductionSources(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		config:         &Config{TownRoot: t.TempDir()},
		logger:         log.New(io.Discard, "", 0),
		forgejoBaseURL: noForgejo,
	}
	now := time.Date(2026, 11, 16, 9, 10, 0, 0, time.UTC)
	src := d.attentionSources(now)
	if src.gateRunning == nil {
		t.Fatal("the production sources carry no gate read: a seat inside its gate would be called stalled")
	}
	if want := overrunMaxGateTotal(defaultOverrunGuardStopAfter); src.gateExemptFor != want {
		t.Errorf("the production sources bound the gate exemption at %v, want the guard's default ceiling %v", src.gateExemptFor, want)
	}
}
