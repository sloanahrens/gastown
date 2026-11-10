package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dashboard"
	gtevents "github.com/steveyegge/gastown/internal/events"
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

// keptRecord is a daemon seat-end record in the shape the daemon writes (its
// pendingSandboxSeatEnd: a landing with the bead and its submit stamp), encoded
// rather than typed by hand; TestDashKeptSeatsReadsTheDaemonsRecords reads it
// back through the daemon's own reader, which pins the shape (gt-ddg96x).
func keptRecord(t *testing.T, beadID string, submitted time.Time) string {
	t.Helper()
	rec := struct {
		Landing struct {
			BeadID    string
			Submitted time.Time
		} `json:"landing"`
	}{}
	rec.Landing.BeadID, rec.Landing.Submitted = beadID, submitted
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
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
	writeKeptRecord(t, town, "gastown", "beaver", keptRecord(t, "gt-1", time.Date(2026, 11, 10, 9, 8, 0, 0, time.UTC)))

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

// A kept seat whose record holds no submit stamp has no clock. The row keeps the
// kept state and its reason names the read that failed, instead of showing no
// clock with no explanation (gt-ddg96x).
func TestBuildDashPolecatsNamesAKeptSeatWhoseSubmitIsUnread(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	unread := time.Time{}
	recovery := polecat.WorkstateDisposition{NeedsRecovery: true, Reason: "git-check-failed"}
	got := buildDashPolecats(dashPolecatInputs{
		Now: now, SessionsKnown: true,
		Seats: []dashSeat{{Rig: "gastown", Name: "beaver", Kept: &unread, Item: dashItem(polecat.StateIdle, recovery)}},
	})
	if len(got) != 1 {
		t.Fatalf("buildDashPolecats = %d rows, want the kept seat", len(got))
	}
	p := got[0]
	if p.State != dashboard.StateKept || p.StateSince != nil {
		t.Errorf("kept seat = state %q since %v, want kept with no clock", p.State, p.StateSince)
	}
	if !strings.Contains(p.Reason, "submit time could not be read") {
		t.Errorf("reason = %q, want it to name the failed submit read", p.Reason)
	}
}

// A kept record the dashboard cannot read does not blank the polecat table: the
// seats are still read and drawn, the seats reader is handed no kept set (so a
// kept seat shows as the recovery it is kept from), SeatsError stays unset, and
// the summary names the failed kept read and its error for the page. A town
// whose records read clean shows no note and hands the set on (gt-ddg96x).
func TestDashboardPolecatsNamesAFailedKeptReadAndKeepsTheSeats(t *testing.T) {
	t.Parallel()
	submitted := time.Date(2026, 11, 10, 9, 8, 0, 0, time.UTC)
	recovery := polecat.WorkstateDisposition{NeedsRecovery: true, Reason: "git-check-failed"}
	for name, c := range map[string]struct {
		kept     map[string]time.Time
		keptErr  error
		wantNote bool
		wantSet  int // the kept entries the seats reader must be handed
		want     string
	}{
		"unreadable records": {nil, errors.New("1 sandbox seat-end record(s) cannot be read"), true, 0, dashboard.StateRecovery},
		"records read":       {map[string]time.Time{"gastown/beaver": submitted}, nil, false, 1, dashboard.StateKept},
	} {
		c := c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			var handed map[string]time.Time
			readers := dashPolecatReaders{
				kept: func(string) (map[string]time.Time, error) { return c.kept, c.keptErr },
				seats: func(_ string, _ []string, _ bool, _ *dashSeatCache, _ *gtevents.DispatchReader, _ sessionLister, kept map[string]time.Time) ([]dashSeat, error) {
					handed = kept
					seat := dashSeat{Rig: "gastown", Name: "beaver", Item: dashItem(polecat.StateIdle, recovery)}
					if at, ok := kept["gastown/beaver"]; ok {
						seat.Kept = &at
					}
					return []dashSeat{seat}, nil
				},
			}
			var s dashboard.Summary
			dashboardPolecatsWith(readers, &s, town, nil, nil, true, nil, newDashLandings(town), newDashSeatCache(), gtevents.NewDispatchReader(town), fakeSessionLister{})
			if s.SeatsError {
				t.Fatalf("a kept read hid the seats table")
			}
			if len(handed) != c.wantSet {
				t.Errorf("seats reader was handed %d kept seat(s), want %d", len(handed), c.wantSet)
			}
			if len(s.Polecats) != 1 || s.Polecats[0].Name != "beaver" {
				t.Fatalf("polecats = %+v, want the one seat still drawn", s.Polecats)
			}
			if got := s.Polecats[0].State; got != c.want {
				t.Errorf("beaver = %q, want %q", got, c.want)
			}
			if got := s.KeptError != ""; got != c.wantNote {
				t.Errorf("kept note = %q, want a note %v", s.KeptError, c.wantNote)
			}
			if c.wantNote && !strings.Contains(s.KeptError, "cannot be read") {
				t.Errorf("kept note = %q, want it to name the unread record", s.KeptError)
			}
		})
	}
}

// A seats read that fails hides the table and names its reason on the summary:
// the first line of the error, with no path beyond the file name. The next read
// that succeeds carries no reason, so the page's note clears on that read
// (gt-6qc3nw.2).
func TestDashboardSeatsErrorNamesWhy(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	breaker := errors.New("dolt circuit breaker is open: server appears down, failing fast\n" +
		"  socket /sandbox/gt/.beads/dolt.sock")
	readers := func(err error) dashPolecatReaders {
		return dashPolecatReaders{
			kept: func(string) (map[string]time.Time, error) { return nil, nil },
			seats: func(_ string, _ []string, _ bool, _ *dashSeatCache, _ *gtevents.DispatchReader, _ sessionLister, _ map[string]time.Time) ([]dashSeat, error) {
				return nil, err
			},
		}
	}
	read := func(r dashPolecatReaders) dashboard.Summary {
		var s dashboard.Summary
		dashboardPolecatsWith(r, &s, town, nil, nil, true, nil, newDashLandings(town), newDashSeatCache(), gtevents.NewDispatchReader(town), fakeSessionLister{})
		return s
	}

	failed := read(readers(breaker))
	if !failed.SeatsError {
		t.Fatalf("a failed seats read did not set SeatsError")
	}
	const want = "dolt circuit breaker is open: server appears down, failing fast"
	if failed.SeatsWhy != want {
		t.Errorf("SeatsWhy = %q, want the error's first line %q", failed.SeatsWhy, want)
	}

	blank := read(readers(errors.New("\n  stack line")))
	if !blank.SeatsError || blank.SeatsWhy != dashWhyNoText {
		t.Errorf("a failed seats read with no error text: SeatsError=%v SeatsWhy=%q, want the error flagged and %q", blank.SeatsError, blank.SeatsWhy, dashWhyNoText)
	}

	ok := read(readers(nil))
	if ok.SeatsError || ok.SeatsWhy != "" {
		t.Errorf("a successful seats read kept SeatsError=%v SeatsWhy=%q, want both cleared", ok.SeatsError, ok.SeatsWhy)
	}
}
