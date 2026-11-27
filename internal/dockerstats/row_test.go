package dockerstats

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/workspaceid"
)

// outageStats is the reading from the 2026-11-24 outage line, with the limit
// the parser gives it.
var outageStats = Stats{CPUPct: 349.74, MemBytes: 8578123432, MemLimitBytes: 8589934592}

func TestRowFixedFields(t *testing.T) {
	t.Parallel()

	row := Row(outageStats, time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC))

	if row.V != 1 {
		t.Errorf("V = %d, want 1", row.V)
	}
	if row.TS != "2026-11-24T21:37:00Z" {
		t.Errorf("TS = %q, want 2026-11-24T21:37:00Z", row.TS)
	}
	if row.IntervalS != 60 {
		t.Errorf("IntervalS = %v, want 60", row.IntervalS)
	}
	if row.Source != "docker" {
		t.Errorf("Source = %q, want docker", row.Source)
	}
	if row.Class != "control-plane" {
		t.Errorf("Class = %q, want control-plane", row.Class)
	}
	if row.Component != "dolt-server" {
		t.Errorf("Component = %q, want dolt-server", row.Component)
	}
	if row.Name != "gt-dolt" {
		t.Errorf("Name = %q, want gt-dolt", row.Name)
	}
	if row.Workspace != workspaceid.CustomerZero {
		t.Errorf("Workspace = %q, want %q", row.Workspace, workspaceid.CustomerZero)
	}
}

func TestRowReadingsFromStats(t *testing.T) {
	t.Parallel()

	row := Row(outageStats, time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC))

	if row.CPUPct == nil {
		t.Fatal("CPUPct is nil, want the parsed percentage")
	}
	if *row.CPUPct != 349.74 {
		t.Errorf("CPUPct = %v, want 349.74", *row.CPUPct)
	}
	if row.MemBytes != 8578123432 {
		t.Errorf("MemBytes = %d, want 8578123432", row.MemBytes)
	}
	if row.MemLimitBytes == nil {
		t.Fatal("MemLimitBytes is nil, want the parsed limit")
	}
	if *row.MemLimitBytes != 8589934592 {
		t.Errorf("MemLimitBytes = %d, want 8589934592", *row.MemLimitBytes)
	}
}

func TestRowLeavesScopePointersNil(t *testing.T) {
	t.Parallel()

	row := Row(outageStats, time.Date(2026, 11, 24, 21, 37, 0, 0, time.UTC))

	if row.Rig != nil {
		t.Errorf("Rig = %q, want nil", *row.Rig)
	}
	if row.Polecat != nil {
		t.Errorf("Polecat = %q, want nil", *row.Polecat)
	}
	if row.Bead != nil {
		t.Errorf("Bead = %q, want nil", *row.Bead)
	}
	if row.Cluster != nil {
		t.Errorf("Cluster = %q, want nil", *row.Cluster)
	}
}

func TestRowTimeIsUTC(t *testing.T) {
	t.Parallel()

	// 23:37 at UTC+2 is 21:37 UTC; the row must carry the UTC instant.
	zone := time.FixedZone("UTC+2", 2*60*60)
	row := Row(outageStats, time.Date(2026, 11, 24, 23, 37, 0, 0, zone))

	if row.TS != "2026-11-24T21:37:00Z" {
		t.Errorf("TS = %q, want the UTC instant 2026-11-24T21:37:00Z", row.TS)
	}
}
