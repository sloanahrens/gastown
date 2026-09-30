//go:build integration

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestIntegrationSurvivingWorkForBeadResolvesRigRepo exercises the real wiring end to
// end: routes.jsonl -> rig name -> <town>/<rig>/.repo.git -> origin refs. A
// wrong rig root here would silently disable the whole guard, so it is worth
// the git fixture.
func TestIntegrationSurvivingWorkForBeadResolvesRigRepo(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	townRoot := t.TempDir()
	writeGastownRoutes(t, townRoot)

	originDir := filepath.Join(townRoot, "origin.git")
	runGit(t, townRoot, "init", "--bare", originDir)

	seed := filepath.Join(townRoot, "seed")
	runGit(t, townRoot, "init", "--initial-branch=main", seed)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "f.txt"), []byte("hello\n"), 0644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGit(t, seed, "add", "f.txt")
	runGit(t, seed, "commit", "-m", "base")
	// Surviving work (the shared predicate, polecat.WorkSurvival): a branch
	// with a patch that is not on main. A branch equal to main carries no
	// work and must not block a re-sling.
	runGit(t, seed, "branch", "polecat/agate/gt-empty+mu72g5cz")
	runGit(t, seed, "checkout", "-q", "-b", "polecat/pearl/gt-ibt8+mu72g5cz")
	if err := os.WriteFile(filepath.Join(seed, "work.txt"), []byte("work\n"), 0644); err != nil {
		t.Fatalf("write work file: %v", err)
	}
	runGit(t, seed, "add", "work.txt")
	runGit(t, seed, "commit", "-m", "work (gt-ibt8)")
	runGit(t, seed, "remote", "add", "origin", originDir)
	runGit(t, seed, "push", "origin", "main", "polecat/pearl/gt-ibt8+mu72g5cz", "polecat/agate/gt-empty+mu72g5cz")

	// The rig root the daemon and sling both use: <town>/<rigName>.
	rigRoot := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(rigRoot, 0755); err != nil {
		t.Fatalf("mkdir rig root: %v", err)
	}
	bare := filepath.Join(rigRoot, ".repo.git")
	runGit(t, townRoot, "init", "--bare", bare)
	runGit(t, bare, "remote", "add", "origin", originDir)

	branch, err := survivingWorkForBead(townRoot, "gt-ibt8")
	if err != nil {
		t.Fatalf("survivingWorkForBead(gt-ibt8): %v", err)
	}
	if branch != "polecat/pearl/gt-ibt8+mu72g5cz" {
		t.Errorf("survivingWorkForBead() = %q, want the pushed branch", branch)
	}

	// A branch equal to main is not surviving work.
	if branch, err := survivingWorkForBead(townRoot, "gt-empty"); err != nil || branch != "" {
		t.Errorf("a branch equal to main must not count as surviving work, got %q (%v)", branch, err)
	}

	// A bead with no branch reports unknown, not a false positive.
	if branch, err := survivingWorkForBead(townRoot, "gt-nobranch"); err != nil || branch != "" {
		t.Errorf("expected no branch for gt-nobranch, got %q", branch)
	}

	// A prefix with no route reports unknown rather than guessing a path.
	if branch, err := survivingWorkForBead(townRoot, "zz-unrouted"); !errors.Is(err, errBeadRoutesToNoRig) || branch != "" {
		t.Errorf("expected no branch for unrouted prefix, got %q", branch)
	}
}
