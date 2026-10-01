//go:build integration

package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// rejectedReworkFixture is a clone on a work branch that has already been
// pushed and rejected: rejectedSHA is the tip the rejected MR was submitted
// with, which is what its MR bead records in commit_sha.
type rejectedReworkFixture struct {
	seed        string // stands in for the shared origin/main branch
	polecat     string
	branch      string
	rejectedSHA string
}

// newRejectedReworkFixture builds origin.git with a base commit, clones it into
// a seed checkout (main) and a polecat checkout, and leaves the polecat on a
// pushed work branch whose only change is a line in shared.txt.
func newRejectedReworkFixture(t *testing.T) rejectedReworkFixture {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "origin.git")
	seed := filepath.Join(dir, "seed")
	polecat := filepath.Join(dir, "polecat")
	branch := "polecat/zircon/gt-test"

	runGitCmd(t, "", "init", "--bare", remote)
	// Point the bare repo's HEAD at main explicitly: git init's default branch
	// name is host-configurable, and the clone below checks out whatever HEAD
	// names.
	runGitCmd(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", remote, seed)
	runGitCmd(t, seed, "config", "user.email", "seed@example.com")
	runGitCmd(t, seed, "config", "user.name", "Seed")
	writeTestFile(t, filepath.Join(seed, "shared.txt"), "base\n")
	writeTestFile(t, filepath.Join(seed, "keep.txt"), "keep\n")
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "base")
	runGitCmd(t, seed, "push", "origin", "main")

	runGitCmd(t, "", "clone", remote, polecat)
	runGitCmd(t, polecat, "config", "user.email", "polecat@example.com")
	runGitCmd(t, polecat, "config", "user.name", "Polecat")
	runGitCmd(t, polecat, "checkout", "-b", branch, "origin/main")

	writeTestFile(t, filepath.Join(polecat, "shared.txt"), "base\nwork\n")
	runGitCmd(t, polecat, "add", "-A")
	runGitCmd(t, polecat, "commit", "-m", "implement the feature")
	runGitCmd(t, polecat, "push", "origin", branch)

	return rejectedReworkFixture{
		seed:        seed,
		polecat:     polecat,
		branch:      branch,
		rejectedSHA: revParse(t, polecat, "HEAD"),
	}
}

// TestIntegrationReportUnchangedSinceRejection runs gt-0jzd5 against real
// repositories: the guard's verdict rests on git's patch-id being
// base-invariant (a rebase does not launder the rejected diff) and on a real
// fix changing it. The unit tests cover the guard's logic over a fake.
func TestIntegrationReportUnchangedSinceRejection(t *testing.T) {
	t.Parallel()
	check := func(f rejectedReworkFixture) error {
		tips := func(mrID string) (string, bool) { return f.rejectedSHA, mrID == "gt-wisp-v8j9" }
		return reportUnchangedSinceRejection(git.NewGit(f.polecat), rejectionNotes(f.branch, "gt-wisp-v8j9"), "gt-0jzd5", "origin/main", tips)
	}

	t.Run("no commit since the rejection is refused", func(t *testing.T) {
		t.Parallel()
		f := newRejectedReworkFixture(t)
		err := check(f)
		if err == nil || !strings.Contains(err.Error(), "no change since the rejection") {
			t.Fatalf("a rework that made no commit was not refused: %v", err)
		}
	})

	t.Run("a rebase that changed no content is refused", func(t *testing.T) {
		t.Parallel()
		f := newRejectedReworkFixture(t)
		writeTestFile(t, filepath.Join(f.seed, "keep.txt"), "keep\nmain touch\n")
		runGitCmd(t, f.seed, "add", "-A")
		runGitCmd(t, f.seed, "commit", "-m", "merged: other work")
		runGitCmd(t, f.seed, "push", "origin", "main")
		runGitCmd(t, f.polecat, "fetch", "origin")
		runGitCmd(t, f.polecat, "rebase", "origin/main")
		if err := check(f); err == nil {
			t.Fatal("a rebase that changed no content was accepted; the diff is still the rejected one")
		}
	})

	t.Run("a pushed fix is accepted", func(t *testing.T) {
		t.Parallel()
		f := newRejectedReworkFixture(t)
		writeTestFile(t, filepath.Join(f.polecat, "shared.txt"), "base\nwork\naddressed the finding\n")
		runGitCmd(t, f.polecat, "add", "-A")
		runGitCmd(t, f.polecat, "commit", "-m", "address the docs-lint finding")
		runGitCmd(t, f.polecat, "push", "origin", f.branch)
		if err := check(f); err != nil {
			t.Fatalf("a pushed fix was refused because the branch ref now holds it: %v", err)
		}
	})
}
