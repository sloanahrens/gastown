package cmd

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// TestPoolReconcile compares fake running sandboxes with fake counted seats.
// Each row names one state the comparison must answer.
func TestPoolReconcile(t *testing.T) {
	t.Parallel()
	live := poolSession{name: "gt-live", agent: "a", sandbox: "gt-live"}
	deadHooked := poolSession{name: "dead/gt/ember", agent: "a", sandbox: "gt-ember", deadHooked: true}
	claim := poolSession{name: "pool-claim/x", agent: "a", claim: true}
	landing := poolSession{name: "landing/gt/pike", agent: "a", sandbox: "gt-pike", landing: true}
	parked := poolSession{name: "gt-parked", agent: "a", sandbox: "gt-parked", parkedStale: true}
	tests := []struct {
		name          string
		counted       []poolSession
		running       []string
		wantUncounted []string
		wantNoSandbox []string
	}{
		{
			name:    "counted seat with its sandbox is in neither list",
			counted: []poolSession{live},
			running: []string{"gt-live"},
		},
		{
			name:          "running sandbox with no counted seat is uncounted",
			counted:       []poolSession{live},
			running:       []string{"gt-live", "gt-stray"},
			wantUncounted: []string{"gt-stray"},
		},
		{
			name:          "counted dead-hooked seat with no sandbox is in NoSandbox",
			counted:       []poolSession{deadHooked},
			wantNoSandbox: []string{"gt-ember"},
		},
		{
			// A dead-hooked seat's name is dead/<rig>/<polecat>; its sandbox is
			// named for the session the seat no longer has, so a running one is
			// the same seat and not an uncounted sandbox beside it (gt-nsbeg9).
			name:    "dead-hooked seat whose sandbox still runs is in neither list",
			counted: []poolSession{deadHooked},
			running: []string{"gt-ember"},
		},
		{
			name:    "claim and landing seats with no sandbox are not listed",
			counted: []poolSession{claim, landing},
		},
		{
			name:          "landing seat's running sandbox is uncounted",
			counted:       []poolSession{landing},
			running:       []string{"gt-pike"},
			wantUncounted: []string{"gt-pike"},
		},
		{
			name:    "released parked seat with no sandbox is not listed",
			counted: []poolSession{parked},
		},
		{
			name:          "sandbox of a released parked seat is uncounted",
			counted:       []poolSession{parked},
			running:       []string{"gt-parked"},
			wantUncounted: []string{"gt-parked"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := poolReconcile(tc.counted, tc.running)
			if !reflect.DeepEqual(got.Uncounted, tc.wantUncounted) {
				t.Errorf("Uncounted = %v, want %v", got.Uncounted, tc.wantUncounted)
			}
			if !reflect.DeepEqual(got.NoSandbox, tc.wantNoSandbox) {
				t.Errorf("NoSandbox = %v, want %v", got.NoSandbox, tc.wantNoSandbox)
			}
		})
	}
}

// TestPoolReconcileNotesOfNamesEachSeatWithItsDisposition is the note formatter:
// every seat the comparison named prints with the word `gt polecat list` reports
// for it — its reuse status when it has one, else its verdict — and a seat whose
// own state cannot be read, or cannot be resolved to a rig, still prints, as
// unknown. A clean reading prints nothing.
func TestPoolReconcileNotesOfNamesEachSeatWithItsDisposition(t *testing.T) {
	t.Parallel()
	reusable := polecat.WorkstateDisposition{ReuseStatus: "idle-pr-open", Verdict: polecat.WorkstateVerdictPendingMR}
	working := polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictWorking}
	seats := map[string]polecat.WorkstateDisposition{"gastown/ember": reusable, "gastown/stray": working}
	dispose := func(rig, name string) (polecat.WorkstateDisposition, error) {
		if rig != "gastown" {
			return polecat.WorkstateDisposition{}, errors.New("no such rig")
		}
		if d, ok := seats[rig+"/"+name]; ok {
			return d, nil
		}
		return polecat.WorkstateDisposition{}, errors.New("unreadable seat")
	}
	seatOf := func(sandbox string) (string, string, bool) {
		if sandbox == "gt-nowhere" {
			return "", "", false
		}
		return "gastown", sandbox[len("gt-"):], true
	}
	tests := []struct {
		name string
		r    poolReconciled
		want []string
	}{
		{
			name: "a clean reading prints nothing",
			want: nil,
		},
		{
			name: "the uncounted sandbox prints its reuse status and the counted seat its verdict",
			r:    poolReconciled{Uncounted: []string{"gt-ember"}, NoSandbox: []string{"gt-stray"}},
			want: []string{"uncounted sandboxes: gt-ember:idle-pr-open", "counted without a sandbox: gt-stray:WORKING"},
		},
		{
			name: "a seat with no rig is named and unknown",
			r:    poolReconciled{Uncounted: []string{"gt-nowhere"}},
			want: []string{"uncounted sandboxes: gt-nowhere:unknown"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := poolReconcileNotesOf(tc.r, seatOf, dispose).Lines()
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("notes = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPoolReconcileReadsNotesFailsClosedOnTheListing is the reads: a gateway
// listing that cannot be read is the unavailable note, never an empty
// comparison, and a read set with no census prints nothing at all.
func TestPoolReconcileReadsNotesFailsClosedOnTheListing(t *testing.T) {
	t.Parallel()
	failing := poolReconcileReads{Running: func() ([]string, error) { return nil, errors.New("gateway down") }}
	if got := failing.notes(nil).Lines(); !reflect.DeepEqual(got, []string{"sandbox listing unavailable"}) {
		t.Fatalf("notes on a failed listing = %q, want the unavailable note", got)
	}
	if got := (poolReconcileReads{}).notes(nil).Lines(); len(got) != 0 {
		t.Fatalf("notes with no census read = %q, want none", got)
	}
	reads := 0
	once := poolReconcileReads{Running: func() ([]string, error) { reads++; return []string{"gt-live"}, nil }}
	live := poolSession{name: "gt-live", agent: "a", sandbox: "gt-live"}
	if got := once.notes([]poolSession{live}).Lines(); len(got) != 0 {
		t.Fatalf("notes on a clean reading = %q, want none", got)
	}
	if reads != 1 {
		t.Fatalf("the census was read %d times, want exactly 1", reads)
	}
}

// TestPoolSandboxSeatForSplitsAtTheRigPrefix resolves a sandbox name the way
// session.PolecatSessionName built it, and refuses a name no rig's prefix
// matches rather than inventing a seat.
func TestPoolSandboxSeatForSplitsAtTheRigPrefix(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	seatOf := poolSandboxSeatFor(town)
	if seatOf == nil {
		t.Fatal("a town with a rig registry resolved no seat")
	}
	if rig, name, ok := seatOf("ga-ember"); !ok || rig != "gastown" || name != "ember" {
		t.Errorf("seat of ga-ember = %q/%q, %v, want gastown/ember, true", rig, name, ok)
	}
	if rig, name, ok := seatOf("zz-ember"); ok || rig != "" || name != "" {
		t.Errorf("seat of zz-ember = %q/%q, %v, want no seat", rig, name, ok)
	}
}
