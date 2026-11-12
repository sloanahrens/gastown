//go:build integration

package done

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationCrewDoneFlagsMustMatchPresentWorktree: with the worktree present, a crew
// submission's --branch and --head are checked against the worktree's own
// branch and HEAD before anything else runs. Matching flags pass that check
// and go on to the bead lookup; a branch or HEAD that differs refuses there,
// so the gated path never submits a commit the caller did not name (gt-pyh5p6).
func TestIntegrationCrewDoneFlagsMustMatchPresentWorktree(t *testing.T) {
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	testRunGit(t, dir, "init", "--bare", remote)
	const branch = "crew/operator/gt-1"
	_, head := seedRepoWithCommit(t, dir, remote, branch)
	repo := filepath.Join(dir, "work")
	townRoot := filepath.Join(dir, "town")
	if err := os.MkdirAll(townRoot, 0o755); err != nil {
		t.Fatalf("mkdir town: %v", err)
	}

	getenv := func(key string) string {
		switch key {
		case "BD_ACTOR":
			return "gastown/crew/operator"
		case "GT_TOWN_ROOT":
			return townRoot
		}
		return ""
	}
	run := func(branch, head string) error {
		return runDoneCrew(Options{
			Branch: branch,
			Head:   head,
			Getwd:  func() (string, error) { return repo, nil },
		}, ExitCompleted, getenv)
	}

	// The flags match the worktree: the check passes, and the run stops only
	// because no bead is named (it reaches the bead lookup, not the refusal).
	err := run(branch, head)
	if err == nil || strings.Contains(err.Error(), "this worktree's") || !strings.Contains(err.Error(), "names no bead") {
		t.Errorf("matching flags: err = %v, want the bead-lookup refusal and no worktree mismatch", err)
	}

	err = run("crew/operator/other", head)
	if err == nil || !strings.Contains(err.Error(), "not this worktree's branch") {
		t.Errorf("branch mismatch: err = %v, want the worktree branch refusal", err)
	}

	err = run(branch, strings.Repeat("b2", 20))
	if err == nil || !strings.Contains(err.Error(), "not this worktree's HEAD") {
		t.Errorf("head mismatch: err = %v, want the worktree HEAD refusal", err)
	}
}
