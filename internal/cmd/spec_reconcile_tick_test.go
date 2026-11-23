package cmd

import (
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// Pool reconcile 2/2 (gt-nsbeg9): the tick line names the running sandboxes the
// count does not hold, and the counted seats the gateway runs no sandbox for,
// each with the disposition word its polecat's own state gives.

// TestSpecTickNamesUncountedSandboxesAndSeatsWithNone is the tick line's half of
// the bead: a reading whose reconciliation found a stray sandbox and a counted
// seat with none appends both notes to the roster picture, through the seat
// formatter, each seat named with its disposition.
func TestSpecTickNamesUncountedSandboxesAndSeatsWithNone(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown()
	f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
	f.roster = specRosterFrom([]poolSession{
		{name: "gt-live", agent: "deepseek-flash", sandbox: "gt-live", rig: "gastown", polecat: "live"},
		{name: "dead/gastown/ember", agent: "deepseek-flash", sandbox: "gt-ember", rig: "gastown", polecat: "ember", deadHooked: true},
	}, nil)
	f.roster.Reconcile = poolReconcileNotes{
		Uncounted: []specdispatch.HeldSeat{{Name: "gt-stray", State: polecat.WorkstateVerdictWorking}},
		NoSandbox: []specdispatch.HeldSeat{{Name: "gt-ember", State: polecat.WorkstateVerdictNeedsRecovery}},
	}

	r := runSpecDispatchCycle(backgroundEnv(f))

	const want = "deepseek-flash 2/2 (1 live + 1 with no session); " +
		"uncounted sandboxes: gt-stray:" + polecat.WorkstateVerdictWorking + "; " +
		"counted without a sandbox: gt-ember:" + polecat.WorkstateVerdictNeedsRecovery
	if r.Roster != want {
		t.Fatalf("tick roster = %q, want %q", r.Roster, want)
	}
}

// TestSpecTickNotesAnUnreadableSandboxListing is the fail-closed half: a gateway
// listing that cannot be read prints the unavailable note, never "none", and the
// tick dispatches as it did without the reconciliation.
func TestSpecTickNotesAnUnreadableSandboxListing(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	env := f.env()
	failing := poolReconcileReads{Running: func() ([]string, error) { return nil, errors.New("gateway down") }}
	env.Roster = func() (specRoster, error) {
		return specRoster{Reconcile: failing.notes(nil)}, nil
	}

	r := runSpecDispatchCycle(env)

	if want := "deepseek-flash 0/2, claude-sonnet 0/2; sandbox listing unavailable"; r.Roster != want {
		t.Fatalf("tick roster = %q, want %q", r.Roster, want)
	}
	if len(f.slingRecords) == 0 {
		t.Fatal("an unreadable sandbox listing stopped the tick's dispatch")
	}
}

// specRosterTown is a temp town with two polecat seats, capable and ember, that
// the fake lister reports as live on claude-sonnet, so the production roster
// step counts both. It is the fixture both roster tests drive.
func specRosterTown(t *testing.T) (string, *config.TownSettings, *fakeLister) {
	t.Helper()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	for _, name := range []string{"capable", "ember"} {
		writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: name}, town,
			intent.Record{Agent: "claude-sonnet"})
	}
	ts := &config.TownSettings{PolecatPool: seatPool("claude-sonnet", 5)}
	lister := &fakeLister{
		sessions: map[string]map[string]string{
			"ga-capable": {"GT_ROLE": "gastown/polecats/capable", "GT_AGENT": "claude-sonnet"},
			"ga-ember":   {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "claude-sonnet"},
		},
		created: map[string]time.Time{
			"ga-capable": time.Now().Add(-time.Hour),
			"ga-ember":   time.Now().Add(-time.Hour),
		},
	}
	return town, ts, lister
}

// specRosterReads is the reconciliation read set the production roster step is
// handed: the running sandboxes from the test's fake gateway, and the seat and
// disposition lookups the town's registry and polecat state answer.
func specRosterReads(running func() ([]string, error)) poolReconcileReads {
	return poolReconcileReads{
		Running: running,
		SeatOf: func(sandbox string) (string, string, bool) {
			return "gastown", strings.TrimPrefix(sandbox, "ga-"), true
		},
		Dispose: func(rig, name string) (polecat.WorkstateDisposition, error) {
			return polecat.WorkstateDisposition{ReuseStatus: "idle-pr-open"}, nil
		},
	}
}

// specRosterStepFor is the tick's production roster step over the fixture,
// specDispatchRoster, wired the way a tick wires it.
func specRosterStepFor(town string, ts *config.TownSettings, lister *fakeLister, reads poolReconcileReads) func() (specRoster, error) {
	var snap atomic.Pointer[poolSeatSnapshot]
	return specDispatchRoster(newSpecTickReads(), town, ts,
		func() sessionLister { return lister },
		func(string) beads.Client { return beadsfake.New() },
		&snap, reads)
}

// TestSpecRosterCleanReconcileKeepsTheLine is the byte-identical half: a reading
// whose seats are all the sandboxes the gateway runs prints exactly today's
// line. It runs the tick's production roster step, specDispatchRoster, so the
// census read and the reconcile wiring are the ones the tick uses.
func TestSpecRosterCleanReconcileKeepsTheLine(t *testing.T) {
	t.Parallel()
	town, ts, lister := specRosterTown(t)
	calls := 0
	reads := specRosterReads(func() ([]string, error) {
		calls++
		// Both counted seats have a running sandbox, so nothing disagrees and
		// the picture is today's line.
		return []string{"ga-capable", "ga-ember"}, nil
	})
	f := newFakeSpecTown()
	f.seats = []specdispatch.Seat{{Agent: "claude-sonnet", Cap: 5}}
	env := f.env()
	env.Roster = specRosterStepFor(town, ts, lister, reads)

	if r := runSpecDispatchCycle(env); r.Roster != "claude-sonnet 2/5" {
		t.Fatalf("tick roster = %q, want today's line", r.Roster)
	}
	if calls != 1 {
		t.Fatalf("the census was read %d times, want 1", calls)
	}
}

// TestSpecRosterReadsTheCensusOnceForTheReconcile is the wiring: the roster step
// asks the census for the running sandboxes once per tick, and names the stray
// against the seats it counted. A listing that cannot be read is the unavailable
// note, and the roster still comes back.
func TestSpecRosterReadsTheCensusOnceForTheReconcile(t *testing.T) {
	t.Parallel()
	town, ts, lister := specRosterTown(t)
	tests := []struct {
		name    string
		running []string // the sandboxes the fake gateway lists; the error row lists none
		want    poolReconcileNotes
	}{
		{
			// ga-capable and ga-ember are the fake lister's sessions, so the
			// roster counts both seats. The gateway lists ga-capable and a stray
			// ga-stray, so ga-ember is a counted seat with no running sandbox and
			// ga-stray is a running sandbox no counted seat holds.
			name:    "a stray sandbox and a seat with none are named",
			running: []string{"ga-capable", "ga-stray"},
			want: poolReconcileNotes{
				Uncounted: []specdispatch.HeldSeat{{Name: "ga-stray", State: "idle-pr-open"}},
				NoSandbox: []specdispatch.HeldSeat{{Name: "ga-ember", State: "idle-pr-open"}},
			},
		},
		{
			name: "a listing that cannot be read is the unavailable note",
			want: poolReconcileNotes{Unavailable: true},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			reads := specRosterReads(func() ([]string, error) {
				calls++
				if tc.want.Unavailable {
					return nil, errors.New("gateway down")
				}
				return tc.running, nil
			})
			roster, err := specRosterStepFor(town, ts, lister, reads)()
			if err != nil {
				t.Fatalf("roster: %v", err)
			}
			if calls != 1 {
				t.Fatalf("the census was read %d times, want 1: one read per tick", calls)
			}
			if !reflect.DeepEqual(roster.Reconcile, tc.want) {
				t.Fatalf("reconcile = %+v, want %+v", roster.Reconcile, tc.want)
			}
		})
	}
}
