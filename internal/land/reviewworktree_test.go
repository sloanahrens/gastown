package land

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	wt, dir, cleanup, err := l.addReviewWorktree(l.open(l.Repo), f.work, f.base)
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

// TestVerdictReuseWorktreeOpensWithLandingEnv: the Repo a review worktree
// returns is opened with the landing's committing identity, read from the
// head's author as Land reads it. The row checks the wiring only; the merge
// commit the identity makes is asserted by the integration bead (gt-4eg2d3).
func TestVerdictReuseWorktreeOpensWithLandingEnv(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var got []string
	l.landingEnv = func(env []string) { got = env }

	_, _, cleanup, err := l.addReviewWorktree(l.open(l.Repo), f.work, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	defer cleanup()

	want, err := CommittingEnv(Identity{Name: "Seat Opal", Email: "opal@gastown.local"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("review worktree handle env = %q, want %q", got, want)
	}
}

// TestVerdictReuseWorktreeRefusesUnreadableAuthor: the identity is read before
// the review directory is made, so a failed author read is returned and leaves
// nothing under WorkRoot (gt-4eg2d3).
func TestVerdictReuseWorktreeRefusesUnreadableAuthor(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.authorOf = func(Work) (Identity, error) { return Identity{}, errors.New("git log: bad revision") }

	wt, dir, cleanup, err := l.addReviewWorktree(l.open(l.Repo), f.work, f.base)
	if err == nil || !strings.Contains(err.Error(), "bad revision") {
		t.Fatalf("addReviewWorktree with an unreadable author = %v (dir %q), want the read's error", err, dir)
	}
	if wt != nil || dir != "" || cleanup != nil {
		t.Errorf("refusal returned wt=%v dir=%q cleanup=%v; want none", wt, dir, cleanup != nil)
	}
	entries, err := os.ReadDir(f.workRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), reviewDirPrefix) {
			t.Errorf("review directory %s made before the author read", e.Name())
		}
	}
}

func TestVerdictReuseWorktreeCleanup(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	g := l.open(l.Repo)
	_, dir, cleanup, err := l.addReviewWorktree(g, f.work, f.base)
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
	_, dir, cleanup, err := l.addReviewWorktree(g, f.work, f.base)
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
	_, _, cleanup, err := l.addReviewWorktree(probe, f.work, f.base)
	if err != nil {
		t.Fatalf("addReviewWorktree: %v", err)
	}
	cleanup()
	cleanup()
	if got, want := strings.Join(probe.ops, ","), "WorktreeAddDetached,WorktreeRemove,WorktreePrune"; got != want {
		t.Errorf("git calls = %s, want %s (cleanup runs once)", got, want)
	}
}

// addFails is a Repo whose worktree add fails, as git does for a ref it cannot
// check out.
type addFails struct{ Repo }

func (addFails) WorktreeAddDetached(string, string) error {
	return errors.New("fatal: invalid reference")
}

// TestVerdictReuseWorktreeAddFails: a review worktree that cannot be added is
// refused with the failure returned, leaves no review directory, and leaves
// gitMu takable for the next caller. A regular file where WorkRoot goes refuses
// the mkdir for every uid, which keeps the failure deterministic in the
// root-owned checkout and sandbox without a file mode.
func TestVerdictReuseWorktreeAddFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup returns the lander, the Repo it is called with, and the
		// directory scanned for a review directory left behind.
		setup   func(t *testing.T, f *landFixture) (l *Lander, g Repo, scan string)
		wantErr string
	}{
		{
			name: "WorkRoot under a regular file",
			setup: func(t *testing.T, f *landFixture) (*Lander, Repo, string) {
				t.Helper()
				l := f.lander()
				l.WorkRoot = filepath.Join(t.TempDir(), "work")
				if err := os.WriteFile(l.WorkRoot, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return l, l.open(l.Repo), filepath.Dir(l.WorkRoot)
			},
		},
		{
			name: "a worktree add that fails",
			setup: func(t *testing.T, f *landFixture) (*Lander, Repo, string) {
				t.Helper()
				l := f.lander()
				return l, addFails{Repo: l.open(l.Repo)}, f.workRoot
			},
			wantErr: "invalid reference",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l, g, scan := tt.setup(t, f)
			wt, dir, cleanup, err := l.addReviewWorktree(g, f.work, f.base)
			if err == nil {
				t.Fatalf("addReviewWorktree = nil error, dir %q; want the failure returned", dir)
			}
			if tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("addReviewWorktree error = %v, want it to carry %q", err, tt.wantErr)
			}
			if wt != nil || dir != "" || cleanup != nil {
				t.Errorf("refusal returned wt=%v dir=%q cleanup=%v; want none", wt, dir, cleanup != nil)
			}
			entries, err := os.ReadDir(scan)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), reviewDirPrefix) {
					t.Errorf("review directory %s left behind after a failed add", filepath.Join(scan, e.Name()))
				}
			}
			if !l.gitMu.TryLock() {
				t.Fatal("gitMu is still held after a failed add")
			}
			l.gitMu.Unlock()
		})
	}
}
