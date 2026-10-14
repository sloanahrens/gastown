//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// TestIntegrationEscalatedDoneWithCommitsAheadLeavesBeadOpen: a branch with
// commits the target lacks used to leave the bead open only because the
// close-time invariant refused it, under a DONE_CLOSE_SKIPPED comment that
// said nothing of the blocker. The ESCALATED exit now holds the bead itself
// and records the escalation instead (gt-tdxsx).
func TestIntegrationEscalatedDoneWithCommitsAheadLeavesBeadOpen(t *testing.T) {
	t.Parallel()
	townRoot := escalatedTown(t)
	repo := filepath.Join(townRoot, "gastown")
	testRunGit(t, repo, "init", "-b", "main")
	testRunGit(t, repo, "config", "user.email", "test@test.com")
	testRunGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Test\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "initial")
	testRunGit(t, repo, "checkout", "-b", "polecat/nux/gt-blocked+abc")
	if err := os.WriteFile(filepath.Join(repo, "work.txt"), []byte("partial\n"), 0644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, repo, "add", ".")
	testRunGit(t, repo, "commit", "-m", "partial work")

	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	seedEscalatedBead(db)

	runEscalatedDone(t, db, repo, townRoot)

	assertEscalatedBeadHeld(t, db)
}
