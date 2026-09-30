package polecat

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// survivalFixture is a rig in a gitfake world: a bare "origin", a seed
// clone that writes to it, and a rig root with the shared .repo.git layout.
type survivalFixture struct {
	w                                *world
	tmp, origin, seed, rigRoot, bare string
}

func newSurvivalFixture(t *testing.T) *survivalFixture {
	t.Helper()
	tmp := t.TempDir()
	f := &survivalFixture{
		w:       newWorld(),
		tmp:     tmp,
		origin:  filepath.Join(tmp, "origin.git"),
		seed:    filepath.Join(tmp, "seed"),
		rigRoot: filepath.Join(tmp, "gastown"),
	}
	f.bare = filepath.Join(f.rigRoot, ".repo.git")
	f.w.InitBare(t, f.origin)
	f.w.Commit(t, f.origin, "main", "base", map[string]string{"base.txt": "base\n"})
	f.w.Clone(t, f.origin, f.seed)
	f.w.InitBare(t, f.bare)
	f.w.AddRemote(t, f.bare, "origin", f.origin)
	return f
}

func (f *survivalFixture) g() gitRepo { return f.w.repo(f.seed) }

func (f *survivalFixture) do(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *survivalFixture) commit(t *testing.T, file, content, msg string) {
	t.Helper()
	f.w.writeAndCommit(t, f.seed, msg, map[string]string{file: content})
}

// branchWithWork creates branch off main in the seed with one commit.
func (f *survivalFixture) branchWithWork(t *testing.T, branch, file string) {
	t.Helper()
	f.do(t, f.g().CheckoutNewBranch(branch, "main"))
	f.commit(t, file, file+"\n", "work on "+branch)
	f.do(t, f.g().Checkout("main"))
}

// branchAt points a new seed branch at main.
func (f *survivalFixture) branchAt(t *testing.T, branch string) {
	t.Helper()
	main, err := f.g().Rev("main")
	f.do(t, err)
	f.w.SetRef(t, f.seed, "refs/heads/"+branch, main)
}

func (f *survivalFixture) push(t *testing.T, refs ...string) {
	t.Helper()
	for _, ref := range refs {
		f.do(t, f.g().Push("origin", ref, false))
	}
}

// mergeIntoMain merges branch into the seed's main with a merge commit.
func (f *survivalFixture) mergeIntoMain(t *testing.T, branch string) {
	t.Helper()
	f.do(t, f.w.OpenBranchRepo(f.seed).MergeNoFF(branch, "merge"))
}

// survivingWork is SurvivingWorkForIssue over f's world.
func (f *survivalFixture) survivingWork(rigRoot string) (string, error) {
	w, err := newWorkSurvival(f.w.opener(), rigRoot, git.RemoteQueryTimeout)
	if err != nil {
		return "", err
	}
	return w.ForIssue(survivalIssue)
}

const survivalIssue = "gt-elvf4"

func TestSurvivingWorkForIssue(t *testing.T) {
	t.Parallel()
	const (
		older = "polecat/basalt/gt-elvf4+mu5wzd6q"
		newer = "polecat/agate/gt-elvf4+mu72g5cz"
	)

	t.Run("branch with an unmerged commit on origin survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		assertSurvivor(t, f, older)
	})

	t.Run("branch equal to main does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchAt(t, older)
		f.push(t, older)
		assertSurvivor(t, f, "")
	})

	t.Run("merged branch does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		f.mergeIntoMain(t, older)
		f.push(t, "main")
		assertSurvivor(t, f, "")
	})

	t.Run("rebase-merged branch does not survive (patch-id, not ancestry)", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		f.push(t, older)
		// main moves on, then takes the same patch as a new commit: the
		// branch tip is not an ancestor of main, but its patch is on main.
		f.commit(t, "other.txt", "other\n", "unrelated")
		f.commit(t, "work.txt", "work.txt\n", "work on "+older)
		f.push(t, "main")
		assertSurvivor(t, f, "")
	})

	t.Run("local-only branch with unpushed work survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "work.txt")
		// The rig repo holds the branch; origin never saw it.
		f.do(t, f.g().Push(f.bare, older+":refs/heads/"+older, false))
		assertSurvivor(t, f, older)
	})

	t.Run("newest branch merged, older one still carries work", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, older, "old.txt")
		f.branchAt(t, newer)
		f.push(t, older, newer)
		assertSurvivor(t, f, older)
	})

	t.Run("other beads' branches are ignored", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.branchWithWork(t, "polecat/basalt/gt-other+mu5wzd6q", "work.txt")
		f.push(t, "polecat/basalt/gt-other+mu5wzd6q")
		assertSurvivor(t, f, "")
	})

	t.Run("idle polecat on an epic branch merged into integration does not survive", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.do(t, f.g().CheckoutNewBranch("integration/epic-x", "main"))
		f.commit(t, "epic.txt", "epic\n", "epic groundwork")
		f.do(t, f.g().CheckoutNewBranch(older, "integration/epic-x"))
		f.commit(t, "work.txt", "work\n", "work on the epic")
		f.do(t, f.g().Checkout("integration/epic-x"))
		f.do(t, f.w.OpenBranchRepo(f.seed).MergeNoFF(older, "merge into epic"))
		f.do(t, f.g().Checkout("main"))
		f.push(t, older, "integration/epic-x")
		// The work (and the epic's own groundwork) is on the integration
		// branch, not on main: judged against main alone it would survive.
		assertSurvivor(t, f, "")
	})

	t.Run("epic branch with work not yet in integration survives", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.do(t, f.g().CheckoutNewBranch("integration/epic-x", "main"))
		f.commit(t, "epic.txt", "epic\n", "epic groundwork")
		f.do(t, f.g().CheckoutNewBranch(older, "integration/epic-x"))
		f.commit(t, "work.txt", "work\n", "work on the epic")
		f.do(t, f.g().Checkout("main"))
		f.push(t, older, "integration/epic-x")
		assertSurvivor(t, f, older)
	})

	t.Run("stale local origin ref is re-fetched", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		// The rig repo last saw the branch at main's tip (no work) ...
		f.branchAt(t, older)
		f.push(t, older)
		f.do(t, f.w.repo(f.bare).Fetch("origin"))
		// ... then work landed on origin.
		f.do(t, f.g().Checkout(older))
		f.commit(t, "work.txt", "work\n", "late work")
		f.do(t, f.g().Checkout("main"))
		f.push(t, older)
		assertSurvivor(t, f, older)
	})

	t.Run("unreachable origin with no local candidate is unknown", func(t *testing.T) {
		t.Parallel()
		f := newSurvivalFixture(t)
		f.w.RemoveRepo(t, f.origin)
		if got, err := f.survivingWork(f.rigRoot); err == nil {
			t.Fatalf("want an error for an unreachable origin, got branch %q", got)
		}
	})

	t.Run("no rig repo", func(t *testing.T) {
		t.Parallel()
		if _, err := newSurvivalFixture(t).survivingWork(t.TempDir()); !errors.Is(err, ErrNoRigRepo) {
			t.Fatalf("err = %v, want ErrNoRigRepo", err)
		}
	})
}

// A rig with no repo to protect is a definite "nothing survives", not an
// unknown: only the latter keeps a bead hooked.
func TestVerdictFromClassifiesTheAnswer(t *testing.T) {
	t.Parallel()
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	for _, tc := range []struct {
		name         string
		branch       string
		err          error
		wantSurvives string
		wantUnknown  bool
	}{
		{name: "a branch survives", branch: branch, wantSurvives: branch},
		{name: "nothing survives"},
		{name: "an unreachable origin is unknown", err: errors.New("origin unreachable"), wantUnknown: true},
		{name: "no repo to protect is nothing survives", err: ErrNoRigRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := VerdictFrom(tc.branch, tc.err)
			if v.SurvivesOn() != tc.wantSurvives || v.Unknown() != tc.wantUnknown {
				t.Fatalf("VerdictFrom(%q, %v) = %+v, want survives=%q unknown=%v",
					tc.branch, tc.err, v, tc.wantSurvives, tc.wantUnknown)
			}
			if want := tc.wantSurvives == "" && !tc.wantUnknown; v.NothingToProtect() != want {
				t.Fatalf("NothingToProtect() = %v, want %v", v.NothingToProtect(), want)
			}
		})
	}
}

func assertSurvivor(t *testing.T, f *survivalFixture, want string) {
	t.Helper()
	got, err := f.survivingWork(f.rigRoot)
	if err != nil {
		t.Fatalf("SurvivingWorkForIssue: %v", err)
	}
	if got != want {
		t.Fatalf("surviving work = %q, want %q", got, want)
	}
}

// newWorkBeadBd is the bd unassignWorkBeads sees: `list` returns one
// in_progress work bead assigned to gastown/polecats/basalt, and every other
// call succeeds silently.
func newWorkBeadBd() *fakeBd {
	return &fakeBd{answer: func(cmd string, _ []string) string {
		if cmd == "list" {
			return `[{"id":"gt-elvf4","title":"work","status":"in_progress","assignee":"gastown/polecats/basalt","issue_type":"task"}]`
		}
		return ""
	}}
}

// Polecat removal gives a hooked bead back only when its work does not
// survive, and then only through the guarded write (gt-vm5g4).
func TestUnassignWorkBeadsKeepsSurvivingWork(t *testing.T) {
	t.Parallel()
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	for _, tc := range []struct {
		name        string
		setup       func(t *testing.T, f *survivalFixture)
		wantRelease bool
	}{
		{name: "surviving work keeps the hook", setup: func(t *testing.T, f *survivalFixture) {
			f.branchWithWork(t, branch, "work.txt")
			f.push(t, branch)
		}},
		{name: "merged branch releases the hook", wantRelease: true, setup: func(t *testing.T, f *survivalFixture) {
			f.branchWithWork(t, branch, "work.txt")
			f.push(t, branch)
			f.mergeIntoMain(t, branch)
			f.push(t, "main")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSurvivalFixture(t)
			tc.setup(t, f)
			bd := newWorkBeadBd()

			mgr := newTestManager(&rig.Rig{Name: "gastown", Path: f.rigRoot}, f.w, nil, bd)
			mgr.unassignWorkBeads("basalt", nil)

			releases := guardedReleases(bd)
			if !tc.wantRelease {
				if len(releases) != 0 {
					t.Fatalf("surviving work was released: %v", releases)
				}
				return
			}
			if len(releases) != 1 || !strings.Contains(releases[0], "--if-assignee=gastown/polecats/basalt") {
				t.Fatalf("want one guarded release, got %v", releases)
			}
		})
	}
}

// guardedReleases returns the guarded release writes the manager made, one per
// bead it returned to open.
func guardedReleases(bd *fakeBd) []string {
	var releases []string
	for _, line := range bd.argvs() {
		if strings.Contains(line, "--status=open") {
			releases = append(releases, line)
		}
	}
	return releases
}

// Removal replays the verdict its caller already reached for a bead instead of
// asking the survival predicate a second time, which can answer differently and
// release work that is on a branch (gt-kud90). Each case judges the bead the
// opposite way from what the rig's own git state would say, so a re-ask is
// visible as the wrong outcome.
func TestUnassignWorkBeadsReplaysJudgedVerdict(t *testing.T) {
	t.Parallel()
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	for _, tc := range []struct {
		name       string
		seedBranch bool // the rig repo really does carry surviving work
		judged     SurvivalVerdict
		wantHold   bool
	}{
		{name: "judged surviving, rig has no branch: keeps the hook", judged: VerdictFrom(branch, nil), wantHold: true},
		{name: "judged nothing survives, rig has a branch: releases", seedBranch: true, judged: VerdictFrom("", nil)},
		{name: "judged unknown, rig has no branch: keeps the hook",
			judged: VerdictFrom("", errors.New("origin unreachable")), wantHold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSurvivalFixture(t)
			if tc.seedBranch {
				f.branchWithWork(t, branch, "work.txt")
				f.push(t, branch)
			}
			bd := newWorkBeadBd()

			mgr := newTestManager(&rig.Rig{Name: "gastown", Path: f.rigRoot}, f.w, nil, bd)
			mgr.unassignWorkBeads("basalt", map[string]SurvivalVerdict{survivalIssue: tc.judged})

			held := len(guardedReleases(bd)) == 0
			if held != tc.wantHold {
				t.Fatalf("released = %v, want held = %v (guarded releases: %v)", !held, tc.wantHold, guardedReleases(bd))
			}
		})
	}
}

// TestNewWorkSurvivalUsesTheRemoteQueryTimeout is the wiring guard for the
// fetch bound: the exported constructor bounds every remote call by git's
// RemoteQueryTimeout, the bound the stalling-origin integration test proves.
func TestNewWorkSurvivalUsesTheRemoteQueryTimeout(t *testing.T) {
	t.Parallel()
	f := newSurvivalFixture(t)
	w, err := NewWorkSurvival(f.rigRoot)
	if err != nil {
		t.Fatal(err)
	}
	if w.fetchTimeout != git.RemoteQueryTimeout {
		t.Fatalf("fetchTimeout = %v, want git.RemoteQueryTimeout (%v)", w.fetchTimeout, git.RemoteQueryTimeout)
	}
}

// Polecat removal never gives back work submitted for landing: the landing
// worker owns it, even when the branch already reads as merged (gt-v4ssj.2).
func TestUnassignWorkBeadsKeepsSubmittedWork(t *testing.T) {
	t.Parallel()
	const branch = "polecat/basalt/gt-elvf4+mu5wzd6q"
	f := newSurvivalFixture(t)
	f.branchWithWork(t, branch, "work.txt")
	f.push(t, branch)
	f.mergeIntoMain(t, branch)
	f.push(t, "main")
	bd := &fakeBd{answer: func(cmd string, _ []string) string {
		if cmd == "list" {
			return `[{"id":"gt-elvf4","title":"work","status":"hooked","assignee":"gastown/polecats/basalt","issue_type":"task","labels":["gt:ready-to-land"]}]`
		}
		return ""
	}}
	mgr := newTestManager(&rig.Rig{Name: "gastown", Path: f.rigRoot}, f.w, nil, bd)
	mgr.unassignWorkBeads("basalt", nil)
	if releases := guardedReleases(bd); len(releases) != 0 {
		t.Fatalf("submitted work was released: %v", releases)
	}
}
