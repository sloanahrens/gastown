//go:build integration

package done

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// newUnmergedScenario leaves a repo mid-merge with one UU entry (conflict.txt):
// the state of a polecat that stopped in the middle of resolving a conflict.
func newUnmergedScenario(t *testing.T) string {
	t.Helper()
	dir := newAttributionRepo(t)
	file := filepath.Join(dir, "conflict.txt")

	writeTestFile(t, file, "polecat side\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "polecat edit")

	gitOut(t, dir, "switch", "main")
	writeTestFile(t, file, "main side\n")
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-m", "main edit")
	gitOut(t, dir, "switch", "polecat/amber/gt-v4ssj.10")

	// The merge must conflict, so a zero exit is the failure.
	if out, err := exec.Command("git", "-C", dir, "merge", "main").CombinedOutput(); err == nil {
		t.Fatalf("merge produced no conflict:\n%s", out)
	}
	if got := gitOut(t, dir, "status", "--porcelain"); !strings.HasPrefix(got, "AA ") {
		t.Fatalf("scenario has no unmerged entry, porcelain = %q", got)
	}
	return dir
}

// TestIntegrationAutoSaveUnmergedFiles pins gt-v7dmq: the refusal tells the
// polecat to use --status DEFERRED, so DEFERRED and ESCALATED must be able to
// leave a worktree with unmerged files. Only COMPLETED refuses.
func TestIntegrationAutoSaveUnmergedFiles(t *testing.T) {
	t.Parallel()
	const branch = "polecat/amber/gt-v4ssj.10"

	for _, exitType := range []string{ExitDeferred, ExitEscalated} {
		t.Run(exitType+"SkipsTheSave", func(t *testing.T) {
			t.Parallel()
			dir := newUnmergedScenario(t)
			g := git.NewGit(dir)
			head := gitOut(t, dir, "rev-parse", "HEAD")

			if err := autoSaveUncommittedWork(g, dir, branch, exitType); err != nil {
				t.Fatalf("autoSaveUncommittedWork(%s) = %v, want nil: the exit path the refusal recommends must work", exitType, err)
			}
			if got := gitOut(t, dir, "rev-parse", "HEAD"); got != head {
				t.Errorf("%s committed over the conflict: HEAD moved from %s to %s", exitType, head, got)
			}
			if got := observeCleanupStatus(g, branch); got != "uncommitted" {
				t.Errorf("cleanup status = %q, want %q: the conflict must stay visible on the agent bead", got, "uncommitted")
			}
		})
	}

	t.Run("CompletedKeepsTheHardError", func(t *testing.T) {
		t.Parallel()
		dir := newUnmergedScenario(t)

		err := autoSaveUncommittedWork(git.NewGit(dir), dir, branch, ExitCompleted)
		if err == nil {
			t.Fatal("autoSaveUncommittedWork accepted a COMPLETED exit with unmerged files")
		}
		if !strings.Contains(err.Error(), "conflict.txt") {
			t.Errorf("refusal does not name the unmerged file: %s", err)
		}
		if strings.Contains(err.Error(), "--cleanup-status") {
			t.Errorf("refusal names the flag that overrides it; agents self-bypass: %s", err)
		}
	})
}
