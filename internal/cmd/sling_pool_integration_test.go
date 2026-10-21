//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The pool's seat read with no probe and no landed opener set reads the
// worktree and its landing evidence through real git, as the production pool
// does. The seams are the modeled repository the unit tests pass in; this is
// the wiring they bypass (gt-0laxm).
func TestIntegrationPoolSeatReadsRealGitByDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		landedRef  bool
		wantSafe   bool
		wantReason string
	}{
		{name: "landed head holds the seat's commit", landedRef: true, wantSafe: true, wantReason: "landed"},
		{name: "no landed head", wantReason: "git-unpushed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			buildDefaultSeatWorktree(t, filepath.Join(town, "gastown", "polecats", "nitro", "gastown"), tt.landedRef)

			fields := &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), Branch: landedSeatBranch}
			reads := poolSeatReadsAnswering(t, fields, polecatActiveWorkEvidence{})
			reads.show = func(_ beads.Client, id string) (*beads.Issue, error) { return seatBeads(t).Show(id) }

			state, err := poolPolecatSeatStateWith(town, "gastown", "nitro", reads)
			if err != nil {
				t.Fatalf("poolPolecatSeatStateWith: %v", err)
			}
			d := state.Disposition
			if d.SafeToNuke != tt.wantSafe || d.Reason != tt.wantReason {
				t.Fatalf("pool disposition = %+v; want safe_to_nuke=%v reason=%q", d, tt.wantSafe, tt.wantReason)
			}
		})
	}
}

// buildDefaultSeatWorktree clones origin into worktree and leaves it one
// unpushed commit past origin on landedSeatBranch. With landedRef, the head
// is also recorded as the landing of gt-seat.
func buildDefaultSeatWorktree(t *testing.T, worktree string, landedRef bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatalf("mkdir seat parent: %v", err)
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGitCmd(t, "", "init", "--bare", "-b", "main", origin)
	runGitCmd(t, "", "clone", origin, worktree)
	runGitCmd(t, worktree, "config", "user.email", "polecat@example.com")
	runGitCmd(t, worktree, "config", "user.name", "Polecat Test")
	writeMQSubmitTestFile(t, worktree, "a.txt", "one\n")
	runGitCmd(t, worktree, "add", "a.txt")
	runGitCmd(t, worktree, "commit", "-m", "seed main")
	runGitCmd(t, worktree, "push", "-u", "origin", "main")
	runGitCmd(t, worktree, "switch", "-c", landedSeatBranch)
	writeMQSubmitTestFile(t, worktree, "b.txt", "work\n")
	runGitCmd(t, worktree, "add", "b.txt")
	runGitCmd(t, worktree, "commit", "-m", "feat: seat work")
	if landedRef {
		runGitCmd(t, worktree, "update-ref", land.LandedRef("gt-seat"), "HEAD")
	}
}
