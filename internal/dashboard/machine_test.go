package dashboard

import (
	"context"
	"testing"

	"github.com/steveyegge/gastown/internal/hoststat"
)

// A host that answered none of its probes carries nothing to paint, so the read
// fails and the hub ages the tile. Reading it as a calm machine would be the
// mistake the tile exists to prevent (gt-q6h8e, gt-auiji).
func TestSampleMachineReportsAFailedHost(t *testing.T) {
	t.Parallel()

	m, err := sampleMachine(func(context.Context) hoststat.Reading {
		return hoststat.Reading{Cores: 24}
	})
	if err == nil {
		t.Fatalf("a host that read nothing came back as %+v", m)
	}
}

// A host that answered all of them carries every reading, the ones macOS
// inflates included: the tile shows them beside the CPU idle share rather than
// in place of it (gt-auiji).
func TestSampleMachineCarriesEveryReadingTheHostReported(t *testing.T) {
	t.Parallel()

	l1, l5, l15, idle, dolt := 30.5, 21.0, 19.0, 47.2, 3.8
	m, err := sampleMachine(func(context.Context) hoststat.Reading {
		return hoststat.Reading{
			Load1: &l1, Load5: &l5, Load15: &l15,
			CPUIdle: &idle, DoltCores: &dolt, Cores: 24,
			Top: []hoststat.Proc{{Name: "go", CPU: 384.8}},
		}
	})
	if err != nil {
		t.Fatalf("sampleMachine: %v", err)
	}
	if m.Load1 != l1 || m.Load5 != l5 || m.Load15 != l15 {
		t.Errorf("loads = %v/%v/%v, want %v/%v/%v", m.Load1, m.Load5, m.Load15, l1, l5, l15)
	}
	if m.CPUIdle == nil || *m.CPUIdle != idle {
		t.Errorf("CPUIdle = %v, want %v", m.CPUIdle, idle)
	}
	if m.DoltCores == nil || *m.DoltCores != dolt {
		t.Errorf("DoltCores = %v, want %v", m.DoltCores, dolt)
	}
	if m.Cores != 24 {
		t.Errorf("Cores = %d, want 24", m.Cores)
	}
	if len(m.Top) != 1 || m.Top[0].Name != "go" {
		t.Errorf("Top = %+v, want the one process the host reported", m.Top)
	}
	if m.HostState != "" {
		t.Errorf("HostState = %q, want no colour on a host with %v%% idle", m.HostState, idle)
	}
}

// The tile's colour is decided here, from the host's own idle share, so that
// the page draws a verdict instead of comparing thresholds itself: the tile
// is amber below hoststat.CPUIdleScarce idle and red below
// hoststat.CPUIdleCritical, and every reading here is run against the rule,
// the two failing ones included (gt-auiji).
func TestHostStateColoursTheHostByItsIdleShare(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		idle *float64
		want string
	}{
		{"a host that reported no idle reading", nil, ""},
		{"an idle host", f64(94), ""},
		{"a host with CPU to spare", f64(hoststat.CPUIdleScarce), ""},
		{"a host just below the share it needs", f64(hoststat.CPUIdleScarce - 0.1), "warn"},
		{"a host at the share it is nearly out of", f64(hoststat.CPUIdleCritical), "warn"},
		{"a host just below it", f64(hoststat.CPUIdleCritical - 0.1), "bad"},
		{"a host with no CPU idle at all", f64(0), "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hostState(tc.idle); got != tc.want {
				t.Errorf("hostState(%v) = %q, want %q", tc.idle, got, tc.want)
			}
		})
	}
}

// The sample carries the verdict the page paints: a host read at half idle is
// an uncoloured tile, and one read at 3% idle is a red tile — the reading a
// load-led tile would have called calm while the machine was out of CPU
// (gt-auiji).
func TestSampleMachineCarriesTheHostTilesVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		idle float64
		want string
	}{
		{"half the host idle", 47.2, ""},
		{"3% of the host idle", 3, "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l1, idle := 30.5, tc.idle
			m, err := sampleMachine(func(context.Context) hoststat.Reading {
				return hoststat.Reading{Load1: &l1, CPUIdle: &idle, Cores: 24}
			})
			if err != nil {
				t.Fatalf("sampleMachine: %v", err)
			}
			if m.HostState != tc.want {
				t.Errorf("HostState = %q, want %q", m.HostState, tc.want)
			}
		})
	}
}

// The load average is the one reading that cannot be absent beside a live
// host: it is an instant counter read, so a sample without it is a sample
// that read nothing. A host that reported the load and neither of the
// readings macOS inflates makes a tile that shows the load alone.
func TestSampleMachineHoldsAHostThatReportedOnlyTheLoad(t *testing.T) {
	t.Parallel()

	l1 := 4.0
	m, err := sampleMachine(func(context.Context) hoststat.Reading {
		return hoststat.Reading{Load1: &l1, Load5: &l1, Load15: &l1, Cores: 8}
	})
	if err != nil {
		t.Fatalf("sampleMachine: %v", err)
	}
	if m.Load1 != l1 || m.CPUIdle != nil || m.DoltCores != nil {
		t.Errorf("machine = %+v, want the load with no idle or Dolt reading", m)
	}
}

func f64(v float64) *float64 { return &v }
