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

// pruneRecorder is a Repo that counts the prune call and answers with err, so
// a sweep test can see that the prune ran, or that it did not run at all.
type pruneRecorder struct {
	Repo
	calls int
	err   error
}

func (p *pruneRecorder) WorktreePrune() error {
	p.calls++
	return p.err
}

// TestVerdictReuseSweepRemovesLeftovers: the boot sweep removes every review-*
// leftover — a directory, a regular file, and a symlink — unlinks the symlink
// without touching its target, keeps WorkRoot/wt and every other entry, and
// prunes the worktree registration a leftover held.
func TestVerdictReuseSweepRemovesLeftovers(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	g := l.open(l.Repo)
	if err := os.MkdirAll(f.workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	reviewA := filepath.Join(f.workRoot, reviewDirPrefix+"a")
	reviewB := filepath.Join(f.workRoot, reviewDirPrefix+"b")
	for _, dir := range []string{reviewA, reviewB} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// om's raw replies are written under the review directory during a review.
	if err := os.WriteFile(filepath.Join(reviewB, "reply.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A worktree still registered under a leftover, as a killed review leaves it.
	wtDir := filepath.Join(reviewA, landWorktreeName)
	if err := g.WorktreeAddDetached(wtDir, f.base); err != nil {
		t.Fatal(err)
	}
	// A leftover regular file, and a symlink whose target must survive.
	reviewFile := filepath.Join(f.workRoot, reviewDirPrefix+"file")
	if err := os.WriteFile(reviewFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(target, "canary")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	reviewLink := filepath.Join(f.workRoot, reviewDirPrefix+"link")
	if err := os.Symlink(target, reviewLink); err != nil {
		t.Fatal(err)
	}
	// The landing's own wt and a land-* directory belong to no review.
	keepDirs := []string{landWorktreeName, "land-abc"}
	for _, name := range keepDirs {
		dir := filepath.Join(f.workRoot, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	notes := filepath.Join(f.workRoot, "notes.txt")
	if err := os.WriteFile(notes, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SweepReviewDirs(f.workRoot, g); err != nil {
		t.Fatalf("SweepReviewDirs: %v", err)
	}

	for _, gone := range []string{reviewA, reviewB, reviewFile, reviewLink} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("leftover %s survived the sweep (lstat err %v)", gone, err)
		}
	}
	if _, err := os.Stat(canary); err != nil {
		t.Errorf("the symlink's target was touched: %v", err)
	}
	for _, name := range keepDirs {
		if _, err := os.Stat(filepath.Join(f.workRoot, name, "keep")); err != nil {
			t.Errorf("%s was removed by the sweep: %v", name, err)
		}
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("a non-review file was removed by the sweep: %v", err)
	}
	list, err := g.(worktreeLister).WorktreeList()
	if err != nil {
		t.Fatalf("WorktreeList: %v", err)
	}
	for _, wt := range list {
		if wt.Path == wtDir {
			t.Errorf("worktree %s still registered after the sweep", wtDir)
		}
	}
}

// TestVerdictReuseSweepMissingRoot: a WorkRoot that is not there holds no
// leftover, so the sweep is nil; a WorkRoot that cannot be read is an error
// naming it, never an empty sweep that reads as clean.
func TestVerdictReuseSweepMissingRoot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr bool
	}{
		{
			name:  "a missing WorkRoot",
			setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		},
		{
			name: "a WorkRoot that is a regular file",
			setup: func(t *testing.T) string {
				t.Helper()
				path := filepath.Join(t.TempDir(), "work")
				if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			// A prune call would make the missing-root row fail, so a Repo that
			// refuses it pins that the early return runs no git.
			g := &pruneRecorder{Repo: f.lander().open(f.repo), err: errors.New("prune must not run")}
			workRoot := tt.setup(t)
			err := SweepReviewDirs(workRoot, g)
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), workRoot) {
					t.Fatalf("SweepReviewDirs(%s) = %v, want an error naming the directory", workRoot, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SweepReviewDirs of a missing WorkRoot = %v, want nil", err)
			}
		})
	}
}

// TestVerdictReuseSweepEmptyRoot: a readable WorkRoot with no review-* entry
// keeps WorkRoot/wt and makes no git call: no registration went stale.
func TestVerdictReuseSweepEmptyRoot(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	g := &pruneRecorder{Repo: f.lander().open(f.repo), err: errors.New("prune must not run")}
	workRoot := t.TempDir()
	wt := filepath.Join(workRoot, landWorktreeName)
	if err := os.MkdirAll(wt, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := SweepReviewDirs(workRoot, g); err != nil {
		t.Fatalf("SweepReviewDirs: %v", err)
	}
	if g.calls != 0 {
		t.Errorf("WorktreePrune called %d times on a WorkRoot with no leftover, want none", g.calls)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("WorkRoot/wt was removed by an empty sweep: %v", err)
	}
}

// TestVerdictReuseSweepRemoveFails: a leftover whose removal fails is named in
// the error and the rest are still tried; the prune still runs, and its own
// failure is reported beside the removals. Removal is injected because a file
// mode does not refuse under root.
func TestVerdictReuseSweepRemoveFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failRemove string // the entry whose removal fails; empty for none
		pruneErr   error
		wantErr    []string
	}{
		{
			name:       "a leftover that cannot be removed",
			failRemove: reviewDirPrefix + "a",
			wantErr:    []string{"remove review directory", reviewDirPrefix + "a"},
		},
		{
			name:     "a prune that fails",
			pruneErr: errors.New("prune: index is locked"),
			wantErr:  []string{"prune: index is locked"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			g := &pruneRecorder{Repo: f.lander().open(f.repo), err: tt.pruneErr}
			workRoot := filepath.Join(t.TempDir(), "work")
			var leftovers []string
			for _, name := range []string{"a", "b"} {
				dir := filepath.Join(workRoot, reviewDirPrefix+name)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				leftovers = append(leftovers, dir)
			}
			removeAll := func(path string) error {
				if filepath.Base(path) == tt.failRemove {
					return errors.New("remove: permission denied")
				}
				return os.RemoveAll(path)
			}
			err := sweepReviewDirs(workRoot, g, removeAll)
			if err == nil {
				t.Fatal("sweepReviewDirs = nil, want the failure returned")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to carry %q", err, want)
				}
			}
			if g.calls != 1 {
				t.Errorf("WorktreePrune called %d times, want 1", g.calls)
			}
			for _, dir := range leftovers {
				_, statErr := os.Stat(dir)
				if filepath.Base(dir) == tt.failRemove {
					if statErr != nil {
						t.Errorf("%s was removed though its removal failed: %v", dir, statErr)
					}
					continue
				}
				if !os.IsNotExist(statErr) {
					t.Errorf("leftover %s not removed after an earlier failure (stat err %v)", dir, statErr)
				}
			}
		})
	}
}
