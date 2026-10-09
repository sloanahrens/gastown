package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/session"
)

// TestHookPolecatEnvCheck verifies that the polecat guard in runHook uses
// GT_ROLE as the authoritative check, so coordinators with a stale GT_POLECAT
// in their environment are not blocked from hooking (GH #1707).
func TestHookPolecatEnvCheck(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		role      string
		polecat   string
		wantBlock bool
	}{
		{
			name:      "bare polecat role is blocked",
			role:      "polecat",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "compound polecat role is blocked",
			role:      "gastown/polecats/Toast",
			polecat:   "Toast",
			wantBlock: true,
		},
		{
			name:      "deacon with stale GT_POLECAT is NOT blocked",
			role:      "deacon",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound witness with stale GT_POLECAT is NOT blocked",
			role:      "gastown/witness",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "crew with stale GT_POLECAT is NOT blocked",
			role:      "crew",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "compound crew with stale GT_POLECAT is NOT blocked",
			role:      "gastown/crew/den",
			polecat:   "alpha",
			wantBlock: false,
		},
		{
			name:      "no GT_ROLE with GT_POLECAT set is blocked",
			role:      "",
			polecat:   "alpha",
			wantBlock: true,
		},
		{
			name:      "no GT_ROLE and no GT_POLECAT is not blocked",
			role:      "",
			polecat:   "",
			wantBlock: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := hookPolecatRefusal(envMap(map[string]string{"GT_ROLE": tt.role, "GT_POLECAT": tt.polecat}))
			blocked := err != nil && strings.Contains(err.Error(), "polecats cannot hook")

			if blocked != tt.wantBlock {
				if tt.wantBlock {
					t.Errorf("expected polecat block but was not blocked (GT_ROLE=%q GT_POLECAT=%q)", tt.role, tt.polecat)
				} else {
					t.Errorf("unexpected polecat block with GT_ROLE=%q GT_POLECAT=%q", tt.role, tt.polecat)
				}
			}
		})
	}
}

// TestHookRejectsNonBeadArg pins down GH#3701: when cobra fails to match a
// subcommand and falls through to the bead-id positional, args that don't
// look like bead IDs should produce a clear error pointing at --help rather
// than the misleading "bead 'set' not found" emitted by bd show.
func TestHookRejectsNonBeadArg(t *testing.T) {
	t.Parallel()
	tests := []string{"set", "list", "delete", "nonexistentword12345"}
	for _, arg := range tests {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			err := hookBeadArgError(arg)
			if err == nil {
				t.Fatalf("hookBeadArgError(%q) returned nil, want error", arg)
			}
			if !strings.Contains(err.Error(), "is not a bead ID") {
				t.Errorf("hookBeadArgError(%q) error = %q, want substring %q", arg, err.Error(), "is not a bead ID")
			}
			if !strings.Contains(err.Error(), "--help") {
				t.Errorf("hookBeadArgError(%q) error = %q, want it to point at --help", arg, err.Error())
			}
		})
	}
}

func TestNormalizeHookShowTarget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		target string
		want   string
	}{
		{
			name:   "shorthand polecat path resolves",
			target: "gastown/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "canonical polecat path stays canonical",
			target: "gastown/polecats/toast",
			want:   "gastown/polecats/toast",
		},
		{
			name:   "unknown target stays unchanged",
			target: "this-is-not-an-agent-path",
			want:   "this-is-not-an-agent-path",
		},
		{
			name:   "path traversal shorthand stays unchanged",
			target: "../toast",
			want:   "../toast",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeHookShowTarget(session.NewPrefixRegistry(), tt.target)
			if got != tt.want {
				t.Fatalf("normalizeHookShowTarget(%q) = %q, want %q", tt.target, got, tt.want)
			}
		})
	}
}

func TestCloseCompletedHookedMoleculeClosesTheBead(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	old, err := db.Create(beads.CreateOptions{Title: "old molecule", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	if err := closeCompletedHookedMoleculeIn(db, old.ID); err != nil {
		t.Fatalf("closeCompletedHookedMolecule: %v", err)
	}
	got, err := db.Show(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.CloseReason != "Auto-replaced by gt hook (molecule complete)" {
		t.Fatalf("status %q reason %q, want closed with the auto-replace reason", got.Status, got.CloseReason)
	}
}

// TestHookThenDisplaceReleasesOnlyAfterTheNewHookCommits covers gt-34z9v: the
// bead a new hook replaces is released only once the new hook write has
// committed, and the release clears the assignee along with the status. A hook
// that fails must leave the previously hooked (naked) bead exactly as it was.
func TestHookThenDisplaceReleasesOnlyAfterTheNewHookCommits(t *testing.T) {
	t.Parallel()
	const agent = "gastown/polecats/nux"
	const oldID = "gt-oldhook"

	seed := func(t *testing.T) *beadsfake.Fake {
		t.Helper()
		db := beadsfake.New()
		db.Seed(beads.Issue{ID: oldID, Title: "previous hook", Status: string(beads.StatusHooked), Assignee: agent})
		return db
	}
	release := func(db *beadsfake.Fake) func(*beads.Issue) error {
		return func(d *beads.Issue) error { return releaseBeadToOpen(db, d.ID) }
	}

	t.Run("a failed new hook leaves the old bead hooked and assigned", func(t *testing.T) {
		t.Parallel()
		db := seed(t)
		err := hookThenDisplace(&beads.Issue{ID: oldID}, func() error { return errors.New("dolt down") }, release(db))
		if err == nil {
			t.Fatal("a failed hook write must return its error")
		}
		got, err := db.Show(oldID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != string(beads.StatusHooked) || got.Assignee != agent {
			t.Fatalf("displaced bead = status %q assignee %q, want hooked/%q", got.Status, got.Assignee, agent)
		}
	})

	t.Run("a committed new hook releases the old bead to open with no assignee", func(t *testing.T) {
		t.Parallel()
		db := seed(t)
		if err := hookThenDisplace(&beads.Issue{ID: oldID}, func() error { return nil }, release(db)); err != nil {
			t.Fatalf("hookThenDisplace: %v", err)
		}
		got, err := db.Show(oldID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != string(beads.StatusOpen) || got.Assignee != "" {
			t.Fatalf("displaced bead = status %q assignee %q, want open with no assignee", got.Status, got.Assignee)
		}
	})
}
