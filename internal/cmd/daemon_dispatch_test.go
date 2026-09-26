package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

func TestDispatchDecision_NudgesWhenSeatsAreFreeAndWorkExists(t *testing.T) {
	// The acceptance case: seats free, and a single actionable ready bead.
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 1}}

	nudge, msg := dispatchDecision(seats, rigs)

	if !nudge {
		t.Fatalf("expected a nudge with free seats and 1 ready bead, got silence")
	}
	// The nudge's contract: free seats and per-rig counts, both named.
	for _, want := range []string{"4 of 4", "gastown=1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("nudge text does not name %q: %s", want, msg)
		}
	}
}

func TestDispatchDecision_SilentWhenNoSeatIsFree(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 4, Free: 0}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 5}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with every seat taken, got nudge: %s", msg)
	}
}

func TestDispatchDecision_SilentWhenNoWorkIsReady(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 3, Free: 1}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 0}, {Rig: "beads", Ready: 0}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with no ready work, got nudge: %s", msg)
	}
}

func TestDispatchDecision_SilentWhenNoSeatModelIsConfigured(t *testing.T) {
	// A town with neither a pool nor scheduler.max_polecats has no answer to
	// "is a seat free?", so the patrol has nothing to report.
	seats := dispatchSeats{Source: "none"}
	rigs := []dispatchRig{{Rig: "gastown", Ready: 5}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence with no seat model, got nudge: %s", msg)
	}
}

func TestDispatchDecision_BackpressuredRigIsNamedButNotCounted(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 2, Free: 2}
	rigs := []dispatchRig{
		{Rig: "om", Ready: 9, ReadyMRs: 15, MRCeiling: 12, Backpressure: true},
	}

	// Only the held rig has work: silence, because the work could not land.
	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence when every rig with work is over its ceiling, got: %s", msg)
	}

	// A second rig with work keeps the nudge, and the held rig is named with
	// its MR depth so the mayor can tell "empty board" from "board held".
	rigs = append(rigs, dispatchRig{Rig: "gastown", Ready: 4})
	nudge, msg := dispatchDecision(seats, rigs)
	if !nudge {
		t.Fatal("expected a nudge when a rig outside its ceiling has work")
	}
	if !strings.Contains(msg, "Held by merge-queue depth: om=15 ready MRs (ceiling 12)") {
		t.Errorf("nudge does not say which rig is held and why: %s", msg)
	}
	if strings.Contains(msg, "om=9") {
		t.Errorf("held rig's ready beads must not read as dispatchable work: %s", msg)
	}
}

func TestDispatchDecision_NamesUrgentSubsetOnlyWhenNonZero(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}

	_, msg := dispatchDecision(seats, []dispatchRig{{Rig: "gastown", Ready: 200, Urgent: 0}})
	if strings.Contains(msg, "P0/P1") {
		t.Errorf("a rig with no P0/P1 should not carry an empty urgent clause: %s", msg)
	}

	_, msg = dispatchDecision(seats, []dispatchRig{{Rig: "gastown", Ready: 200, Urgent: 2}})
	if !strings.Contains(msg, "gastown=200 (P0/P1 2)") {
		t.Errorf("nudge should name the urgent subset when there is one: %s", msg)
	}
}

func TestDispatchDecision_SkipsParkedRigs(t *testing.T) {
	seats := dispatchSeats{Source: "polecat_pool", Capacity: 4, Occupied: 0, Free: 4}
	rigs := []dispatchRig{{Rig: "mango", Ready: 7, Parked: true}}

	if nudge, msg := dispatchDecision(seats, rigs); nudge {
		t.Errorf("expected silence: the only rig with work is parked, got: %s", msg)
	}
}

func TestIsActionableReadyBead(t *testing.T) {
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActionableReadyBead(tc.issue); got != tc.want {
				t.Errorf("isActionableReadyBead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPoolSeatPicture(t *testing.T) {
	now := time.Now()
	pool := &config.PolecatPool{
		LocalAgent:    "deepseek-flash",
		MaxLocal:      2,
		OverflowAgent: "flash-overflow",
		MaxOverflow:   2,
	}
	sessions := []poolSession{
		{name: "gastown/flint", agent: "deepseek-flash", created: now.Add(-time.Hour)},
		{name: "gastown/jade", agent: "deepseek-flash", created: now.Add(-time.Minute)},
		{name: "beads/fury", agent: "flash-overflow", created: now.Add(-time.Hour)},
	}

	seats := poolSeatPicture(pool, sessions)
	if seats.Source != "polecat_pool" {
		t.Errorf("source = %q, want polecat_pool", seats.Source)
	}
	if seats.Capacity != 4 || seats.Occupied != 3 || seats.Free != 1 {
		t.Errorf("seats = %+v, want capacity 4, occupied 3, free 1", seats)
	}

	// No pool at all: the model has nothing to say, and says so with an empty
	// source rather than a zero-seat town.
	if got := poolSeatPicture(nil, sessions); got.Source != "" {
		t.Errorf("nil pool should leave Source empty, got %+v", got)
	}

	// A pool with no local_agent is a pool that was never configured.
	if got := poolSeatPicture(&config.PolecatPool{MaxLocal: 2}, sessions); got.Source != "" {
		t.Errorf("pool without local_agent should leave Source empty, got %+v", got)
	}
}

func TestPoolSeatPicture_UncappedOverflowIsAtLeastOneFreeSeat(t *testing.T) {
	// An overflow seat with no cap is unbounded room: the pool never refuses a
	// spawn, so a full local pool must not read as a town with no seat free.
	pool := &config.PolecatPool{
		LocalAgent:    "local",
		MaxLocal:      1,
		OverflowAgent: "overflow", // MaxOverflow 0 = uncapped
	}
	sessions := []poolSession{{name: "gastown/flint", agent: "local"}}

	seats := poolSeatPicture(pool, sessions)
	if !seats.Uncapped {
		t.Fatalf("expected Uncapped for an unbounded overflow seat, got %+v", seats)
	}
	if seats.Free < 1 {
		t.Errorf("free = %d, want at least 1 while the overflow seat is uncapped", seats.Free)
	}
}

// TestRigMergeQueueDepthReadsRigRootMergeQueue reproduces gt-xwt9:
// rigMergeQueueDepth read max_ready_for_dispatch from rig-local settings/
// config.json only via config.LoadRigSettings, so a rig-root-only ceiling
// (gt-me9t's floor) was silently ignored and the dispatch patrol fell back
// to the operator default. Routing through rig.ResolveMergeQueueConfig makes
// the rig-root value visible with no rig-local settings/config.json present.
func TestRigMergeQueueDepthReadsRigRootMergeQueue(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	rigConfig := `{
  "type": "rig",
  "version": 1,
  "name": "testrig",
  "merge_queue": {"max_ready_for_dispatch": 3}
}`
	if err := os.WriteFile(filepath.Join(rigPath, "config.json"), []byte(rigConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	lister := &fakeDispatchMRLister{mrs: readyMRs(5)}
	origLister := newDispatchMRLister
	newDispatchMRLister = func(string) dispatchMRLister { return lister }
	t.Cleanup(func() { newDispatchMRLister = origLister })

	ready, ceiling := rigMergeQueueDepth(rigPath, rigName)
	if ceiling != 3 {
		t.Errorf("ceiling = %d, want 3 (rig-root merge_queue floor invisible to dispatch patrol)", ceiling)
	}
	if ready != 5 {
		t.Errorf("ready = %d, want 5", ready)
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

// TestReadyIssuesUnlimited_UninitializedRigHasNoReadyWork reproduces gt-ka00.
//
// The guard read beads.ResolveBeadsDir against "", which that function never
// returns, so it never fired: an uninitialized rig reached the store open,
// failed it, and dispatchRigPictures turned that into an error for the whole
// dispatch picture, every rig included.
//
// The fixture is what an uninitialized rig actually looks like on disk. The
// repo checkout supplies .beads/ (.beads/config.yaml and friends are tracked),
// so the directory exists; what is missing is the database under it.
func TestReadyIssuesUnlimited_UninitializedRigHasNoReadyWork(t *testing.T) {
	rigPath := t.TempDir()
	beadsDir := filepath.Join(rigPath, ".beads")
	mkdirTestDir(t, beadsDir)
	writeTestFile(t, filepath.Join(beadsDir, "config.yaml"), "status.custom: []\n")

	issues, err := readyIssuesUnlimited(rigPath)
	if err != nil {
		t.Fatalf("uninitialized rig is no ready work, not a read failure: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %d, want 0 from a rig with no database", len(issues))
	}
}

// boardStorage is a minimal beadsdk.Storage that serves a fixed ready board,
// applying the SDK's own limit semantics: a LIMIT is applied only when
// Limit > 0, so a caller that sends no Limit gets the whole board. The
// embedded interface covers the methods this test never reaches.
type boardStorage struct {
	beadsdk.Storage
	board []*beadsdk.Issue
}

func (s *boardStorage) GetReadyWork(_ context.Context, filter beadsdk.WorkFilter) ([]*beadsdk.Issue, error) {
	result := s.board
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}
	return result, nil
}

// TestReadyIssuesUnlimited_ReturnsBoardPastBdDefaultLimit is the gt-59o9
// regression test at the patrol's own seam. The board this check counts held
// 373 ready beads on 2026-09-21; a count capped at bd's default of 100 is
// exactly the under-report that left the mayor asleep with work to dispatch,
// so the assertion is on the whole 373 rather than on anything smaller.
//
// It drives readyIssuesUnlimited itself, through the store opener the daemon
// uses, so the guarantee is asserted on the function the patrol calls rather
// than on a helper beneath it.
func TestReadyIssuesUnlimited_ReturnsBoardPastBdDefaultLimit(t *testing.T) {
	rigPath := t.TempDir()
	beadsDir := filepath.Join(rigPath, ".beads")
	mkdirTestDir(t, beadsDir)
	writeTestFile(t, filepath.Join(beadsDir, "config.yaml"), "status.custom: []\n")
	mkdirTestDir(t, filepath.Join(beadsDir, "dolt"))

	board := make([]*beadsdk.Issue, 373)
	for i := range board {
		board[i] = &beadsdk.Issue{
			ID:     fmt.Sprintf("gt-board-%d", i),
			Title:  fmt.Sprintf("ready bead %d", i),
			Status: beadsdk.StatusOpen,
		}
	}

	prev := openDispatchReadyStore
	openDispatchReadyStore = func(*beads.Beads, context.Context) (beadsdk.Storage, func(), error) {
		return &boardStorage{board: board}, func() {}, nil
	}
	t.Cleanup(func() { openDispatchReadyStore = prev })

	issues, err := readyIssuesUnlimited(rigPath)
	if err != nil {
		t.Fatalf("readyIssuesUnlimited: %v", err)
	}
	if len(issues) != 373 {
		t.Fatalf("readyIssuesUnlimited returned %d issues, want the whole 373-bead board", len(issues))
	}
	if issues[0].ID != "gt-board-0" {
		t.Errorf("first issue = %q, want gt-board-0 (the board read whole, in order)", issues[0].ID)
	}
}

// A redirect whose target was never created is the same empty rig by another
// route: ResolveBeadsDir follows it out of the rig and lands somewhere with no
// database. It must be absorbed for the same reason, not fail the check.
func TestReadyIssuesUnlimited_DanglingRedirectHasNoReadyWork(t *testing.T) {
	rigPath := t.TempDir()
	beadsDir := filepath.Join(rigPath, ".beads")
	mkdirTestDir(t, beadsDir)
	writeTestFile(t, filepath.Join(beadsDir, "redirect"), "mayor/rig/.beads\n")

	issues, err := readyIssuesUnlimited(rigPath)
	if err != nil {
		t.Fatalf("a redirect to a missing database is no ready work, not a read failure: %v", err)
	}
	if len(issues) != 0 {
		t.Errorf("issues = %d, want 0 from a dangling redirect", len(issues))
	}
}

// TestHasBeadsDatabase pins the guard's contract: a rig has a database when one
// is reachable under the resolved beads directory, not when a tracked
// metadata.json merely names one.
func TestHasBeadsDatabase(t *testing.T) {
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
