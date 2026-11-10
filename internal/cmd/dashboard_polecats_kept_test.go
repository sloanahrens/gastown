package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/polecat"
)

// A finished seat the daemon keeps for the jev_rules battery reads kept (jev),
// not the recovery its stale host worktree would read as. The daemon's kept fact
// decides only for a seat holding no bead, and a person label or a queued bead
// still applies first (gt-ddg96x).
func TestDashPolecatStateKeepsAFinishedSeatTheDaemonKeeps(t *testing.T) {
	t.Parallel()
	recovery := polecat.WorkstateDisposition{NeedsRecovery: true, Reason: "git-check-failed"}
	for name, c := range map[string]struct {
		item   polecatInventoryItem
		work   bool
		ready  bool
		kept   bool
		labels []string
		want   string
	}{
		"kept reads kept":          {dashItem(polecat.StateIdle, recovery), false, false, true, nil, dashboard.StateKept},
		"not kept keeps recovery":  {dashItem(polecat.StateIdle, recovery), false, false, false, nil, dashboard.StateRecovery},
		"kept and slung again":     {dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), true, false, true, nil, dashboard.StateWorking},
		"kept, bead queued":        {dashItem(polecat.StateIdle, recovery), true, true, true, nil, dashboard.StateQueued},
		"kept, person label first": {dashItem(polecat.StateIdle, recovery), true, false, true, []string{"gt:needs-human"}, dashboard.StateNeedsHuman},
	} {
		got, _ := dashPolecatState(c.item, c.work, c.ready, c.kept, c.labels)
		if got != c.want {
			t.Errorf("%s: state = %q, want %q", name, got, c.want)
		}
	}
}

// A kept seat is dated by its submit, the stamp its record holds, so the Age
// column shows how long the battery has not asked. A seat the daemon does not
// keep stays a recovery with no state clock (gt-ddg96x).
func TestBuildDashPolecatsDatesAKeptSeatBySubmit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	submitted := now.Add(-20 * time.Minute)
	recovery := polecat.WorkstateDisposition{NeedsRecovery: true, Reason: "git-check-failed"}
	got := buildDashPolecats(dashPolecatInputs{
		Now: now, SessionsKnown: true,
		Seats: []dashSeat{
			{Rig: "gastown", Name: "beaver", Kept: &submitted, Item: dashItem(polecat.StateIdle, recovery)},
			{Rig: "gastown", Name: "ozark", Item: dashItem(polecat.StateIdle, recovery)},
		},
	})
	byName := map[string]dashboard.Polecat{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if p := byName["beaver"]; p.State != dashboard.StateKept || p.StateSince == nil || !p.StateSince.Equal(submitted) {
		t.Errorf("beaver = %+v, want kept since its submit %s", p, submitted)
	}
	if p := byName["ozark"]; p.State != dashboard.StateRecovery || p.StateSince != nil {
		t.Errorf("ozark = %+v, want recovery with no state clock", p)
	}
}

// writeKeptRecord puts a daemon seat-end record where the daemon keeps it, the
// file the dashboard reads (gt-ddg96x).
func writeKeptRecord(t *testing.T, town, rig, name, body string) {
	t.Helper()
	path := filepath.Join(town, constants.DirRuntime, "sandbox-seat-ends", rig, name+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The dashboard reads the seats the daemon keeps from its records, keyed by
// rig and polecat, each at its submitted time (gt-ddg96x).
func TestDashKeptSeatsReadsTheDaemonsRecords(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeKeptRecord(t, town, "gastown", "beaver", `{"landing":{"BeadID":"gt-1","Submitted":"2026-11-10T09:08:00Z"}}`)

	kept, err := dashKeptSeats(town)
	if err != nil {
		t.Fatalf("reading the kept seats: %v", err)
	}
	want := time.Date(2026, 11, 10, 9, 8, 0, 0, time.UTC)
	if at, ok := kept["gastown/beaver"]; !ok || !at.Equal(want) || len(kept) != 1 {
		t.Errorf("kept seats = %v, want gastown/beaver at %s", kept, want)
	}
}

// A record the dashboard cannot read is an error, not a town with no kept
// seats: the seat it keeps would read as the recovery it is kept from.
func TestDashKeptSeatsRefusesAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeKeptRecord(t, town, "gastown", "beaver", `{not json`)

	if kept, err := dashKeptSeats(town); err == nil {
		t.Errorf("kept seats = %v with no error, want the unreadable record reported", kept)
	}
}

// A store the dashboard cannot list is an error too, so no seat is read as not
// kept on a store it could not open (gt-ddg96x).
func TestDashKeptSeatsRefusesAStoreThatCannotBeListed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := filepath.Join(town, constants.DirRuntime, "sandbox-seat-ends")
	if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if kept, err := dashKeptSeats(town); err == nil {
		t.Errorf("kept seats = %v with no error, want the unlistable store reported", kept)
	}
}

// A town that has never kept a seat has no store: no kept seats and no error.
func TestDashKeptSeatsIsEmptyWithoutAStore(t *testing.T) {
	t.Parallel()
	kept, err := dashKeptSeats(t.TempDir())
	if err != nil {
		t.Fatalf("a town with no store read as an error: %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("kept seats = %v, want none", kept)
	}
}
