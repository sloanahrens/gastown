//go:build integration

package land

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newReviewWorktreeRepo builds newRealLandFixture's real repository and cuts
// one review worktree at its base through addReviewWorktree: the handle into
// the checkout, the checkout's directory, the base commit it is detached at,
// and the work the fixture declared for it. The worktree and its review
// directory are removed when the test ends.
func newReviewWorktreeRepo(t *testing.T) (wt Repo, dir, base string, w Work) {
	t.Helper()
	f := newRealLandFixture(t)
	l := f.lander()
	wt, dir, cleanup, err := l.addReviewWorktree(l.open(f.repo), f.work, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree at %s: %v", f.base, err)
	}
	t.Cleanup(cleanup)
	return wt, dir, f.base, f.work
}

// TestIntegrationVerdictReuseWorktreeRealCheckout: over real git the review
// worktree addReviewWorktree hands back is a checkout git itself reports at
// the base commit, and it is the review's own directory — the landing's
// WorkRoot/wt is never cut (gt-53l7iv). An integration file's tests carry the
// TestIntegration prefix (docs/testing.md).
func TestIntegrationVerdictReuseWorktreeRealCheckout(t *testing.T) {
	t.Parallel()

	wt, dir, base, _ := newReviewWorktreeRepo(t)

	if name := filepath.Base(filepath.Dir(dir)); !strings.HasPrefix(name, reviewDirPrefix) {
		t.Fatalf("review worktree %s is not in a %s* directory", dir, reviewDirPrefix)
	}
	landing := filepath.Join(filepath.Dir(filepath.Dir(dir)), landWorktreeName)
	switch _, err := os.Stat(landing); {
	case err == nil:
		t.Fatalf("addReviewWorktree cut the landing's worktree %s", landing)
	case !errors.Is(err, fs.ErrNotExist):
		t.Fatalf("stat %s: %v", landing, err)
	}

	if got := gitT(t, dir, "rev-parse", "HEAD"); got != base {
		t.Fatalf("review worktree HEAD = %s, want the base %s", got, base)
	}
	if got := gitT(t, dir, "rev-parse", "--show-prefix"); got != "" {
		t.Fatalf("git -C %s reports the checkout under %q, want the directory itself", dir, got)
	}
	head, err := wt.Rev("HEAD")
	if err != nil {
		t.Fatalf("the returned handle's HEAD: %v", err)
	}
	if head != base {
		t.Fatalf("the returned handle reads HEAD = %s, want the base %s", head, base)
	}
}

// TestIntegrationVerdictReuseWorktreeCanCommit: over real git a review can
// make the merge commit a landing makes, through the Repo addReviewWorktree
// returns. mergeWork of the fixture's clean head onto the base neither
// rejects nor fails: HEAD becomes a new merge commit whose parents are the
// base and the head, carrying the head's change, and git records the landing
// identity as its committer, the values CommittingEnv sets. An integration
// file's tests carry the TestIntegration prefix (docs/testing.md).
func TestIntegrationVerdictReuseWorktreeCanCommit(t *testing.T) {
	t.Parallel()

	wt, dir, base, w := newReviewWorktreeRepo(t)

	out, err := mergeWork(wt, w, base)
	if err != nil {
		t.Fatalf("mergeWork of the clean head: %v", err)
	}
	if out.Rejection != nil {
		t.Fatalf("mergeWork rejected the clean head: %+v", out.Rejection)
	}
	if out.Commit == "" || out.Commit == base {
		t.Fatalf("mergeWork made commit %q, want a new merge commit off the base %s", out.Commit, base)
	}
	// Non-empty: the merge commit joins the base and the head, and its tree
	// carries the head's change. A commit with the base's tree is the empty
	// merge the landing refuses.
	parents := strings.Fields(gitT(t, dir, "rev-list", "--parents", "-n", "1", out.Commit))
	if want := []string{out.Commit, base, w.Head}; !slices.Equal(parents, want) {
		t.Fatalf("merge commit parents = %v, want %v", parents, want)
	}
	if got := gitT(t, dir, "show", out.Commit+":b.txt"); got != "work" {
		t.Fatalf("merge commit b.txt = %q, want the head's change", got)
	}
	// The committer is the landing's own identity, which CommittingEnv sets on
	// every call through the handle: the clone's git config cannot reach the
	// commit (gt-dc4y3, gt-vuv2n).
	if got, want := gitT(t, dir, "show", "-s", "--format=%cn%n%ce", out.Commit),
		LandingCommitterName+"\n"+LandingCommitterEmail; got != want {
		t.Fatalf("merge commit committer = %q, want the landing identity %q", got, want)
	}
}
