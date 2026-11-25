//go:build integration

package land

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newReviewWorktreeRepo builds newRealLandFixture's real repository and cuts
// one review worktree at its base through addReviewWorktree: the handle into
// the checkout, the checkout's directory and the base commit it is detached
// at. The worktree and its review directory are removed when the test ends.
func newReviewWorktreeRepo(t *testing.T) (wt Repo, dir, base string) {
	t.Helper()
	f := newRealLandFixture(t)
	l := f.lander()
	wt, dir, cleanup, err := l.addReviewWorktree(l.open(f.repo), f.work, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree at %s: %v", f.base, err)
	}
	t.Cleanup(cleanup)
	return wt, dir, f.base
}

// TestIntegrationVerdictReuseWorktreeRealCheckout: over real git the review
// worktree addReviewWorktree hands back is a checkout git itself reports at
// the base commit, and it is the review's own directory — the landing's
// WorkRoot/wt is never cut (gt-53l7iv). An integration file's tests carry the
// TestIntegration prefix (docs/testing.md).
func TestIntegrationVerdictReuseWorktreeRealCheckout(t *testing.T) {
	t.Parallel()

	wt, dir, base := newReviewWorktreeRepo(t)

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
