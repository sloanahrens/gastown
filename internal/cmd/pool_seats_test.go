package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The dispatchable-ready-bead predicate and the pool seat picture outlive the
// idle-seat dispatch check that introduced them (gt-rwp7z.3): the spec
// dispatcher's roster and the pool's admission read the seat helpers, and the
// ready board reads the predicate.

func TestIsActionableReadyBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		issue *beads.Issue
		want  bool
	}{
		{"plain P2 task", &beads.Issue{ID: "gt-1", Title: "fix the thing", Priority: 2}, true},
		{"plain P0 bug", &beads.Issue{ID: "gt-2", Title: "[bug] broken", Priority: 0}, true},
		{"nil issue", nil, false},
		{"P3 backlog", &beads.Issue{ID: "gt-3", Title: "someday", Priority: 3}, false},
		{"unset priority", &beads.Issue{ID: "gt-4", Title: "unscored", Priority: -1}, false},
		{
			"agent bead",
			&beads.Issue{ID: "gt-gastown-polecat-flint", Title: "gt-gastown-polecat-flint", Priority: 2, Labels: []string{"gt:agent"}},
			false,
		},
		{
			"merge slot",
			&beads.Issue{ID: "gt-zdcv", Title: "merge-slot", Priority: 0, Labels: []string{"gt:merge-slot"}},
			false,
		},
		{
			"escalation",
			&beads.Issue{ID: "gt-5", Title: "[HIGH] disk full", Priority: 1, Labels: []string{"gt:escalation"}},
			false,
		},
		{
			"escalation titled without its label",
			&beads.Issue{ID: "gt-6", Title: "[CRITICAL] dolt is down", Priority: 0},
			false,
		},
		{
			"state collapse",
			&beads.Issue{ID: "gt-7", Title: "STATE_COLLAPSE gt-abc closed, MR still open", Priority: 1},
			false,
		},
		{
			"epic container",
			&beads.Issue{ID: "gt-8", Title: "[epic] de-flake the suite", Priority: 1, Type: "epic"},
			false,
		},
		{
			"label case and padding",
			&beads.Issue{ID: "gt-9", Title: "agent-ish", Priority: 2, Labels: []string{" GT:AGENT "}},
			false,
		},
		{
			// The operator's own work is not dispatchable: a sling of it
			// refuses (gt-21pl0).
			"operator-reserved bead",
			&beads.Issue{ID: "gt-10", Title: "Hand-run audit", Priority: 1, Labels: []string{"operator"}},
			false,
		},
		{
			"another label on the same bead shape",
			&beads.Issue{ID: "gt-11", Title: "Ordinary work", Priority: 1, Labels: []string{"run-blocker"}},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActionableReadyBead(tc.issue, 2, nil); got != tc.want {
				t.Errorf("isActionableReadyBead = %v, want %v", got, tc.want)
			}
		})
	}
}

// The ceiling is the operator's, not a constant (gt-h2kyc): the same bead is
// backlog at the default and dispatchable work at a raised one, and an
// unscored bead is never dispatchable.
func TestIsActionableReadyBeadHonorsTheCeiling(t *testing.T) {
	t.Parallel()
	p3 := &beads.Issue{ID: "gt-3", Title: "someday", Priority: 3}
	if isActionableReadyBead(p3, 2, nil) {
		t.Error("a P3 bead is dispatchable at the default P2 ceiling")
	}
	if !isActionableReadyBead(p3, 4, nil) {
		t.Error("a P3 bead is backlog at a P4 ceiling")
	}
	if isActionableReadyBead(&beads.Issue{ID: "gt-u", Title: "unscored", Priority: -1}, 4, nil) {
		t.Error("an unscored bead is dispatchable")
	}
}

// TestIsActionableReadyBead_HoldsRedMainBeadWhileItsRevertIsInFlight is
// gt-1fiv4: a bead the rig's red-main owner filed is not work to dispatch
// while that owner is undoing the same breakage. The predicate is
// specdispatch.RedMainHold's, so the dispatcher and the fold hold the same
// beads (gt-zkdwt).
func TestIsActionableReadyBead_HoldsRedMainBeadWhileItsRevertIsInFlight(t *testing.T) {
	t.Parallel()
	redMain := &beads.Issue{
		ID: "gt-3u7zu", Title: "gastown post-land test red", Priority: 1,
		Labels: []string{specdispatch.LabelRedMain},
	}
	revert := &specdispatch.Revert{Culprit: "gt-cq5gb"}
	plain := &beads.Issue{ID: "gt-plain", Title: "ordinary work", Priority: 1}

	if isActionableReadyBead(redMain, 2, revert) {
		t.Error("a red-main bead is dispatchable while the rig's revert of the same breakage is in flight")
	}
	// The hold is the label's: a revert in flight does not hold the rig's
	// other work, and a red-main bead is work again once the revert is gone.
	if !isActionableReadyBead(plain, 2, revert) {
		t.Error("a bead without the red-main label is held by another bead's revert")
	}
	if !isActionableReadyBead(redMain, 2, nil) {
		t.Error("a red-main bead is held with no revert in flight")
	}
	if !isActionableReadyBead(redMain, 2, &specdispatch.Revert{}) {
		t.Error("a revert with no culprit is no hold")
	}
}

// TestIsActionableReadyBead_AbsentRedMainStateCountsAsToday is the compat half
// of gt-1fiv4: a rig with no red-main state file reads as no revert, so the
// predicate counts its beads exactly as it did before the hold existed.
func TestIsActionableReadyBead_AbsentRedMainStateCountsAsToday(t *testing.T) {
	t.Parallel()
	bead := &beads.Issue{
		ID: "gt-3u7zu", Title: "gastown post-land test red", Priority: 1,
		Labels: []string{specdispatch.LabelRedMain},
	}

	rv := rigRevertInFlight(t.TempDir(), "gastown", func(string) bool { return false })
	if rv != nil {
		t.Fatalf("absent red-main state = %+v, want no revert", rv)
	}
	if !isActionableReadyBead(bead, 2, rv) {
		t.Error("a red-main bead is held by a rig whose red-main state file does not exist")
	}
}

// ── The seat picture's sources (gt-thy6r) ───────────────────────────────────

// writeLandingSeat stages a seat mid-landing: the polecat's directory, so the
// seat exists, and the intent record `gt done` leaves behind — desired
// submitted, naming the work bead it handed to the landing worker.
func writeLandingSeat(t *testing.T, townRoot, rigName, polecatName, workBead string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, "polecats", polecatName), 0o755); err != nil {
		t.Fatal(err)
	}
	writeIntentRecord(t, intent.Seat{Rig: rigName, Role: constants.RolePolecat, Name: polecatName}, townRoot,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: workBead})
}

// writeIntentRecord writes an intent record at its own seat path.
func writeIntentRecord(t *testing.T, seat intent.Seat, townRoot string, rec intent.Record) {
	t.Helper()
	path := seat.Path(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSeatClaim stages the claim a sling holds on a seat while its session is
// still starting, at the path and in the shape poolSeatClaimSessions reads.
func writeSeatClaim(t *testing.T, townRoot, id, agent string) {
	t.Helper()
	dir := poolSeatClaimDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(poolSeatClaim{ID: id, PID: os.Getpid(), Agent: agent, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// submittedWork answers the batched seat-work read with the named beads as
// they look while they are still waiting to land.
func submittedWork(ids ...string) poolSeatWorkFunc {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	return func(got []string) (map[string]*beads.Issue, error) {
		out := map[string]*beads.Issue{}
		for _, id := range got {
			if want[id] {
				out[id] = &beads.Issue{ID: id, Status: string(beads.StatusOpen), Labels: []string{land.LabelReadyToLand}}
			}
		}
		return out, nil
	}
}

// TestPoolSeatSessionsCountsEveryKindOfTakenSeat pins the three sources the
// picture is built from — the live session, the in-flight claim a sling writes
// before its session exists, and the seat mid-landing — against the fourth
// state, a seat that is simply free. The mid-landing case is the one the
// deleted seat-refill plugin and the first cut of the Go fold both counted as
// free (gt-thy6r, gt-59o9, gt-t8q5).
func TestPoolSeatSessionsCountsEveryKindOfTakenSeat(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 3}
	live := &fakeLister{sessions: map[string]map[string]string{
		"gt-jade": {"GT_ROLE": "gastown/polecats/jade", "GT_AGENT": "deepseek-flash"},
	}}

	cases := []struct {
		name    string
		lister  sessionLister
		claim   bool
		landing bool
		want    int
	}{
		{"a live session holds the seat", live, false, false, 1},
		{"an in-flight claim holds the seat", &fakeLister{}, true, false, 1},
		{"a seat mid-landing holds the seat", &fakeLister{}, false, true, 1},
		{"no session, no claim, no submission: free", &fakeLister{}, false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			work := submittedWork()
			if c.claim {
				writeSeatClaim(t, town, "claim-1", pool.OverflowAgent)
			}
			if c.landing {
				writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")
				work = submittedWork("gt-ruby")
			}

			got, err := poolSeatSessionsWith(c.lister, town, nil, work, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if n := poolSeatCount(pool, got); n != c.want {
				t.Fatalf("seat count = %d, want %d (%+v)", n, c.want, got)
			}
		})
	}
}

// TestPoolLandingSeatEndsWithTheLabel pins that the intent record is only the
// cheap half of the signal: the bead decides. A submission pulled back for
// rework, or already landed, leaves a record the patrol scan has not corrected
// yet (gt-xs1ni); its bead no longer carries gt:ready-to-land, so the seat is
// free rather than held for a wait that is over.
func TestPoolLandingSeatEndsWithTheLabel(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 3}
	town := t.TempDir()
	writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")

	// The record says submitted, the bead says otherwise.
	over := func(got []string) (map[string]*beads.Issue, error) {
		out := map[string]*beads.Issue{}
		for _, id := range got {
			out[id] = &beads.Issue{ID: id, Status: string(beads.StatusOpen)}
		}
		return out, nil
	}
	got, err := poolSeatSessionsWith(&fakeLister{}, town, nil, over, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat for a bead that is no longer submitted, got %+v", got)
	}

	// A record with no work bead names nothing to confirm, so it holds nothing.
	writeLandingSeat(t, town, "gastown", "opal", "")
	got, err = poolSeatSessionsWith(&fakeLister{}, town, nil, over, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat for a submission with no bead, got %+v", got)
	}
}

// TestPoolLandingSeatSkipsASeatThatIsGone: an intent record outlives the seat
// it belongs to, and a seat that is gone holds nothing. Records that are not a
// polecat's — a witness, refinery or crew seat — hold no pool seat either.
func TestPoolLandingSeatSkipsASeatThatIsGone(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 3}
	town := t.TempDir()

	// A submitted record with no polecat directory behind it.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "ghost"}, town,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-ghost"})
	// A submitted record for a seat that is not a polecat's.
	writeIntentRecord(t, intent.Seat{Rig: "gastown", Role: "witness"}, town,
		intent.Record{Desired: intent.DesiredSubmitted, WorkBead: "gt-witness"})

	got, err := poolSeatSessionsWith(&fakeLister{}, town, nil, submittedWork("gt-ghost", "gt-witness"), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("want no occupied seat, got %+v", got)
	}
}

// testServerMetadata names a database the way a tracked .beads/metadata.json
// does in gastown's server mode.
const testServerMetadata = `{"dolt_mode":"server","dolt_database":"beads_testrig"}`

func mkdirTestDir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestHasBeadsDatabase pins the guard's contract: a rig has a database when one
// is reachable under the resolved beads directory, not when a tracked
// metadata.json merely names one.
func TestHasBeadsDatabase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T, townRoot, rigPath string)
		want  bool
	}{
		{
			name:  "empty .beads directory",
			setup: func(*testing.T, string, string) {},
			want:  false,
		},
		{
			name: "tracked config.yaml without a database",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "config.yaml"), "status.custom: []\n")
			},
			want: false,
		},
		{
			name: "embedded dolt directory",
			setup: func(t *testing.T, _, rigPath string) {
				mkdirTestDir(t, filepath.Join(rigPath, ".beads", "dolt"))
			},
			want: true,
		},
		{
			// bd's embedded data root. A .beads directory carrying it needs no
			// metadata.json, so testing metadata.json alone would read the rig
			// as empty and report no ready work.
			name: "embeddeddolt directory",
			setup: func(t *testing.T, _, rigPath string) {
				mkdirTestDir(t, filepath.Join(rigPath, ".beads", "embeddeddolt", "beads", ".dolt"))
			},
			want: true,
		},
		{
			// A same-named regular file is not a database, and accepting it
			// would put the rig back on the store-open path.
			name: "dolt as a regular file",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "dolt"), "")
			},
			want: false,
		},
		{
			name: "unparseable metadata.json",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), "{not json")
			},
			want: true,
		},
		{
			name: "metadata.json with no server database reference",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), `{"dolt_mode":"embedded"}`)
			},
			want: true,
		},
		{
			name: "server mode naming a database the server has",
			setup: func(t *testing.T, townRoot, rigPath string) {
				mkdirTestDir(t, filepath.Join(townRoot, ".dolt-data", "beads_testrig"))
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: true,
		},
		{
			// The documented stale-metadata.json case: the file is tracked from
			// a workspace whose Dolt server had the database, this one does not.
			// It is an uninitialized rig, not a read failure.
			name: "server mode naming a database the server lacks",
			setup: func(t *testing.T, _, rigPath string) {
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: false,
		},
		{
			name: "server mode with no town to look in",
			setup: func(t *testing.T, townRoot, rigPath string) {
				if err := os.RemoveAll(filepath.Join(townRoot, "mayor")); err != nil {
					t.Fatal(err)
				}
				writeTestFile(t, filepath.Join(rigPath, ".beads", "metadata.json"), testServerMetadata)
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			townRoot := t.TempDir()
			mkdirTestDir(t, filepath.Join(townRoot, "mayor"))
			writeTestFile(t, filepath.Join(townRoot, "mayor", "town.json"), "{}\n")
			rigPath := filepath.Join(townRoot, "testrig")
			mkdirTestDir(t, filepath.Join(rigPath, ".beads"))
			tt.setup(t, townRoot, rigPath)

			if got := hasBeadsDatabase(filepath.Join(rigPath, ".beads")); got != tt.want {
				t.Errorf("hasBeadsDatabase() = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("missing directory", func(t *testing.T) {
		if hasBeadsDatabase(filepath.Join(t.TempDir(), ".beads")) {
			t.Error("hasBeadsDatabase() = true for a directory that does not exist")
		}
	})
}
