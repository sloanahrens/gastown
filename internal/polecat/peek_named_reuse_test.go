package polecat

import (
	"errors"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestPeekNamedReuse_AgreesWithTheGate: `gt sling --dry-run <bead>
// <rig>/<name>` prints the route a live sling would take, so the peek must
// stop exactly where ReuseIdlePolecat stops — no earlier (a refusal the sling
// would not raise), no later (a reuse line printed for a polecat the sling
// refuses). Each case peeks, then runs the gate for real, and asserts the two
// agree; the peek must also leave the polecat untouched (gt-yxc7m).
func TestPeekNamedReuse_AgreesWithTheGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		setup       func(t *testing.T, mgr *Manager, bd *polecatDB, name string)
		wantRefusal bool
		wantParked  bool
		wantHeld    string
	}{
		{name: "idle and clean"},
		{
			name: "parked",
			setup: func(t *testing.T, mgr *Manager, _ *polecatDB, name string) {
				parkPolecat(t, mgr, name)
			},
			wantRefusal: true,
			wantParked:  true,
		},
		{
			name: "dirty worktree",
			setup: func(t *testing.T, mgr *Manager, _ *polecatDB, name string) {
				dirtyWorktree(t, mgr.clonePath(name))
			},
			wantRefusal: true,
		},
		{
			name: "still holds work",
			setup: func(t *testing.T, mgr *Manager, bd *polecatDB, name string) {
				held := "gt-held"
				hookStatus := string(beads.StatusHooked)
				if _, err := bd.Fake.Create(beads.CreateOptions{
					ID: held, Title: "held work", Assignee: mgr.assigneeID(name),
				}); err != nil {
					t.Fatalf("creating %s: %v", held, err)
				}
				if err := bd.Fake.Update(held, beads.UpdateOptions{Status: &hookStatus}); err != nil {
					t.Fatalf("hooking %s: %v", held, err)
				}
				bd.setAgent(t, mgr.agentBeadID(name), func(f *beads.AgentFields) { f.HookBead = held })
			},
			wantRefusal: true,
			wantHeld:    "gt-held",
		},
		{
			name: "worktree gone",
			setup: func(t *testing.T, mgr *Manager, _ *polecatDB, name string) {
				if err := os.RemoveAll(mgr.clonePath(name)); err != nil {
					t.Fatalf("removing worktree: %v", err)
				}
			},
			wantRefusal: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, added, w := canonicalWithPolecats(t, false, "alpha")
			clonePath := added["alpha"].ClonePath
			if tc.setup != nil {
				tc.setup(t, mgr, bd, "alpha")
			}
			before := ""
			if _, err := os.Stat(clonePath); err == nil {
				before = w.branch(t, clonePath)
			}

			held, peekErr := mgr.PeekNamedReuse("alpha")

			if before != "" {
				if after := w.branch(t, clonePath); after != before {
					t.Fatalf("peek moved alpha's worktree from %s to %s; a preview must not mutate", before, after)
				}
			}
			if got := (peekErr != nil); got != tc.wantRefusal {
				t.Fatalf("peek refused = %v, want %v; err = %v", got, tc.wantRefusal, peekErr)
			}
			if tc.wantHeld != "" && held != tc.wantHeld {
				t.Errorf("held issue = %q, want %q", held, tc.wantHeld)
			}

			_, gateErr := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next"})
			if (peekErr == nil) != (gateErr == nil) {
				t.Fatalf("peek err = %v; gate err = %v — the preview and the live sling disagree", peekErr, gateErr)
			}
			if peekErr == nil {
				return
			}
			if !errors.Is(peekErr, ErrPolecatNeedsRecovery) {
				t.Errorf("refusal %v does not wrap ErrPolecatNeedsRecovery", peekErr)
			}
			if tc.wantParked {
				if !errors.Is(peekErr, ErrPolecatParked) {
					t.Errorf("parked refusal %v does not wrap ErrPolecatParked", peekErr)
				}
				// The peek never hands new work to a parked slot, and never
				// undoes the park to make one.
				assertStillParked(t, mgr, "alpha")
			}
		})
	}
}

// TestPeekNamedReuse_AbsentPolecat: a name with no polecat behind it is the
// lookup's refusal, told apart from a refusal of an existing one so the
// caller can offer --create (gt-2w4f9).
func TestPeekNamedReuse_AbsentPolecat(t *testing.T) {
	t.Parallel()
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, "alpha")

	held, err := mgr.PeekNamedReuse("flint")
	if !errors.Is(err, ErrPolecatNotFound) {
		t.Fatalf("err = %v; want ErrPolecatNotFound", err)
	}
	if held != "" {
		t.Errorf("held issue = %q; want empty for an absent polecat", held)
	}
}
