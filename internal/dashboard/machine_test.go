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
