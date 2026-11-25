package land

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// worktreeLister is what the gitfake handle adds to Repo: the worktree
// registry, which the cleanup test reads to see that no entry is left.
type worktreeLister interface {
	WorktreeList() ([]git.Worktree, error)
}

// gitMuProbe is a Repo that records each worktree call and fails the test when
// the call is made without the lander's gitMu held. The landing and the review
// share one repository, so every git call the review makes must hold it.
type gitMuProbe struct {
	Repo
	t   *testing.T
	l   *Lander
	ops []string
}

func (p *gitMuProbe) check(op string) {
	p.ops = append(p.ops, op)
	if p.l.gitMu.TryLock() {
		p.l.gitMu.Unlock()
		p.t.Errorf("%s ran without gitMu held", op)
	}
}

func (p *gitMuProbe) WorktreeAddDetached(path, ref string) error {
	p.check("WorktreeAddDetached")
	return p.Repo.WorktreeAddDetached(path, ref)
}

func (p *gitMuProbe) WorktreeRemove(path string, force bool) error {
	p.check("WorktreeRemove")
	return p.Repo.WorktreeRemove(path, force)
}

func (p *gitMuProbe) WorktreePrune() error {
	p.check("WorktreePrune")
	return p.Repo.WorktreePrune()
}

func TestVerdictReuseWorktreeNeverWT(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	wt, dir, cleanup, err := l.addReviewWorktree(l.open(l.Repo), f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	defer cleanup()
	if wt == nil {
		t.Error("addReviewWorktree returned a nil Repo")
	}
	review := filepath.Dir(dir)
	if filepath.Dir(review) != f.workRoot || !strings.HasPrefix(filepath.Base(review), reviewDirPrefix) {
		t.Errorf("review worktree %s is not under %s/%s*", dir, f.workRoot, reviewDirPrefix)
	}
	if dir == filepath.Join(f.workRoot, landWorktreeName) {
		t.Errorf("review worktree is the landing's %s", dir)
	}
	info, err := os.Stat(review)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Errorf("review directory mode %o, want 0700", mode)
	}
}

func TestVerdictReuseWorktreeCleanup(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	g := l.open(l.Repo)
	_, dir, cleanup, err := l.addReviewWorktree(g, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	review := filepath.Dir(dir)
	// om's raw replies are written under the review directory during a review.
	if err := os.WriteFile(filepath.Join(review, "reply.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanup()
	cleanup()
	if _, err := os.Stat(review); !os.IsNotExist(err) {
		t.Errorf("review directory %s left behind after cleanup (stat err %v)", review, err)
	}
	list, err := g.(worktreeLister).WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	for _, wt := range list {
		if wt.Path == dir {
			t.Errorf("worktree %s still registered after cleanup", dir)
		}
	}
}

// lockedRemove is a Repo whose worktree removal fails without touching the
// worktree, as git does for a locked one.
type lockedRemove struct{ Repo }

func (lockedRemove) WorktreeRemove(string, bool) error {
	return errors.New("worktree is locked")
}

// TestVerdictReuseWorktreeCleanupAfterFailedRemove: when git cannot remove the
// worktree, cleanup still removes the review directory, prunes the registration
// the directory's removal left stale, and logs git's error.
func TestVerdictReuseWorktreeCleanupAfterFailedRemove(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var out bytes.Buffer
	l.Out = &out
	g := lockedRemove{Repo: l.open(l.Repo)}
	_, dir, cleanup, err := l.addReviewWorktree(g, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(dir)); !os.IsNotExist(err) {
		t.Errorf("review directory left behind after a failed remove (stat err %v)", err)
	}
	list, err := g.Repo.(worktreeLister).WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	for _, wt := range list {
		if wt.Path == dir {
			t.Errorf("stale registration %s left after cleanup", dir)
		}
	}
	if !strings.Contains(out.String(), "worktree is locked") {
		t.Errorf("log = %q, want git's remove error in it", out.String())
	}
}

func TestVerdictReuseWorktreeHoldsGitMu(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	probe := &gitMuProbe{Repo: l.open(l.Repo), t: t, l: l}
	_, _, cleanup, err := l.addReviewWorktree(probe, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	cleanup()
	cleanup()
	if got, want := strings.Join(probe.ops, ","), "WorktreeAddDetached,WorktreeRemove,WorktreePrune"; got != want {
		t.Errorf("git calls = %s, want %s (cleanup runs once)", got, want)
	}
}

func TestVerdictReuseWorktreeRefusesFileWorkRoot(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	// A file where WorkRoot should be refuses the MkdirAll for every uid.
	l.WorkRoot = filepath.Join(t.TempDir(), "work")
	if err := os.WriteFile(l.WorkRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wt, dir, cleanup, err := l.addReviewWorktree(l.open(l.Repo), f.base)
	if err == nil {
		t.Fatalf("addReviewWorktree under a file WorkRoot = %s, nil; want an error", dir)
	}
	if wt != nil || dir != "" || cleanup != nil {
		t.Errorf("refusal returned wt=%v dir=%q cleanup=%v; want none", wt, dir, cleanup != nil)
	}
}
