package witness

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// verifyCommitOnMainTown builds a town whose rig holds one polecat worktree
// that is a real git repo with no remotes. It returns the town root; the
// polecat's repo is at <town>/rig/polecats/nux.
func verifyCommitOnMainTown(t *testing.T) (townRoot, polecatDir string) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	polecatDir = filepath.Join(townRoot, "rig", "polecats", "nux")
	if err := os.MkdirAll(polecatDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return townRoot, polecatDir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestVerifyCommitOnMain_Verdicts pins _verifyCommitOnMain's three verdicts
// (gt-udrrw, gt-bfale): a commit confirmed on the default branch is Pass, one
// confirmed off it is Fail, and a check that could not answer at all is
// Unknown — never folded into Fail, since HandleMerged blocks on Fail but only
// warns on Unknown.
func TestVerifyCommitOnMain_Verdicts(t *testing.T) {
	t.Parallel()

	t.Run("no town is Unknown", func(t *testing.T) {
		t.Parallel()
		v := _verifyCommitOnMain(t.TempDir(), "rig", "nux")
		if !v.IsUnknown() {
			t.Fatalf("no town: want Unknown, got %v", v)
		}
	})

	t.Run("polecat dir that is not a repo is Unknown", func(t *testing.T) {
		t.Parallel()
		townRoot, _ := verifyCommitOnMainTown(t)
		v := _verifyCommitOnMain(townRoot, "rig", "nux")
		if !v.IsUnknown() {
			t.Fatalf("no git repo: want Unknown, got %v", v)
		}
	})

	t.Run("commit on local main is Pass", func(t *testing.T) {
		t.Parallel()
		townRoot, polecatDir := verifyCommitOnMainTown(t)
		gitIn(t, polecatDir, "init", "-b", "main")
		gitIn(t, polecatDir, "commit", "--allow-empty", "-m", "work")
		v := _verifyCommitOnMain(townRoot, "rig", "nux")
		if !v.IsPass() {
			t.Fatalf("HEAD on main: want Pass, got %v", v)
		}
	})

	t.Run("commit off main is Fail", func(t *testing.T) {
		t.Parallel()
		townRoot, polecatDir := verifyCommitOnMainTown(t)
		gitIn(t, polecatDir, "init", "-b", "main")
		gitIn(t, polecatDir, "commit", "--allow-empty", "-m", "base")
		gitIn(t, polecatDir, "checkout", "-b", "polecat/nux/work")
		gitIn(t, polecatDir, "commit", "--allow-empty", "-m", "unmerged work")
		v := _verifyCommitOnMain(townRoot, "rig", "nux")
		if !v.IsFail() {
			t.Fatalf("HEAD ahead of main: want Fail, got %v", v)
		}
	})

	t.Run("no default branch to compare against is Unknown, not Fail", func(t *testing.T) {
		t.Parallel()
		townRoot, polecatDir := verifyCommitOnMainTown(t)
		// The only branch is not "main", so every ancestor check errors on the
		// missing ref: nothing answered "not an ancestor", the check just
		// could not run.
		gitIn(t, polecatDir, "init", "-b", "elsewhere")
		gitIn(t, polecatDir, "commit", "--allow-empty", "-m", "work")
		v := _verifyCommitOnMain(townRoot, "rig", "nux")
		if !v.IsUnknown() {
			t.Fatalf("no main ref anywhere: want Unknown, got %v", v)
		}
		if v.Err() == nil {
			t.Fatalf("Unknown must carry a reason, got nil Err()")
		}
	})
}
