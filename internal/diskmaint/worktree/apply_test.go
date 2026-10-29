package worktree

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// applyLedger returns a ledger in a temporary directory, so a test can read back
// the lines an apply appended.
func applyLedger(t *testing.T) *diskmaint.Ledger {
	t.Helper()
	return diskmaint.NewLedger(filepath.Join(t.TempDir(), "maint.jsonl"), 0)
}

// applyTarget returns a target over the fake runner and ledger, with no
// redirecting variable in its environment.
func applyTarget(root string, f *fakeGit, l *diskmaint.Ledger) *Target {
	return NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect, Ledger: l})
}

// commonOf is the shared git dir a fixture's worktrees report.
func commonOf(root string) string { return filepath.Join(root, "rig-a", ".git") }

// answerRemove answers the git reads and the removal for the worktree at path.
// When refuse is non-nil, git refuses the removal the way it does a dirty
// worktree.
func answerRemove(f *fakeGit, root, path string, refuse error) {
	f.answer("-C "+path+" rev-parse --git-common-dir", commonOf(root)+"\n", nil)
	f.answer("--git-dir "+commonOf(root)+" worktree remove "+path, "", refuse)
}

// answerBranch answers the branch delete for branch in the shared git dir.
func answerBranch(f *fakeGit, root, branch string) {
	f.answer("--git-dir "+commonOf(root)+" branch -D "+branch, "", nil)
}

// removeAction and branchAction are the two actions the planner pairs for one
// landed worktree.
func removeAction(path string) diskmaint.Action {
	return diskmaint.Action{Kind: kindRemove, Ref: path}
}

func branchAction(path, branch string) diskmaint.Action {
	return diskmaint.Action{Kind: kindDeleteBranch, Ref: path + "#" + branch}
}

// touches reports whether a recorded command names path as a whole argument.
func touches(calls []string, path string) bool {
	for _, c := range calls {
		if strings.Contains(c, path+" ") || strings.HasSuffix(c, path) {
			return true
		}
	}
	return false
}

// TestApplyDryRunRunsNothing is the dry-run rule. Apply has no Policy: a dry run
// is a Plan with Policy.Apply false, and the caller applies only when it means
// to. Planning with Apply false issues no worktree removal or branch delete.
func TestApplyDryRunRunsNothing(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	a := addCheckout(t, root, "rig-a/crew/overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	f.answer("-C "+a+" worktree list --porcelain", listing(porcelainWorktree{a, shaHead, branch}), nil)
	settle(f, a, branch, shaHead, testNow.Add(-72*time.Hour))

	cfg := Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect}
	p, err := NewTarget(cfg).Plan(context.Background(), diskmaint.Policy{Apply: false, Now: testNow})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(p.Actions) != 1 || p.Actions[0].Kind != kindRemove {
		t.Fatalf("plan actions = %+v, want the one removal the dry run would make", p.Actions)
	}
	if n := f.callsMatching("worktree remove") + f.callsMatching("branch -D"); n != 0 {
		t.Fatalf("dry run ran %v, want no removal or branch command", f.calls)
	}
}

// TestApplyRefusesPathsOutsideOverseerCrew checks that a path that is not an
// overseer-* directory under <Town>/<rig>/crew is refused, that nothing runs for
// it, and that the rest of the plan still runs.
func TestApplyRefusesPathsOutsideOverseerCrew(t *testing.T) {
	t.Parallel()
	for name, bad := range map[string]string{
		"outside crew":       "rig-a/other/overseer-x",
		"base not overseer":  "rig-a/crew/other-d",
		"the clone":          "rig-a/crew/overseer",
		"rig not configured": "rig-z/crew/overseer-z",
		"nested below crew":  "rig-a/crew/sub/overseer-x",
		"parent traversal":   "rig-a/crew/../../overseer-x",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			badPath := root + "/" + bad
			goodPath := filepath.Join(root, "rig-a", "crew", "overseer-a")
			const goodBranch = "polecat/overseer-a/x"
			f := newFakeGit(t)
			answerRemove(f, root, goodPath, nil)
			answerBranch(f, root, goodBranch)

			plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
				removeAction(badPath),
				branchAction(badPath, "polecat/other-d/x"),
				removeAction(goodPath),
				branchAction(goodPath, goodBranch),
			}}
			res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), plan)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if touches(f.calls, badPath) {
				t.Fatalf("a command named the refused path %q: %v", badPath, f.calls)
			}
			if len(res.Done) != 2 || len(res.Failed) != 2 {
				t.Fatalf("Done %d, Failed %d; want the good pair done and the refused pair failed", len(res.Done), len(res.Failed))
			}
			if f.callsMatching("worktree remove") != 1 || f.callsMatching("branch -D "+goodBranch) != 1 {
				t.Fatalf("calls %v, want the good worktree removed and its branch deleted", f.calls)
			}
		})
	}
}

// TestApplyDirtyWorktreeIsFailedNotForced checks that git's refusal of a dirty
// worktree is a failed action, is not retried, never gets --force, and keeps its
// branch. One failure does not stop the next worktree from being removed.
func TestApplyDirtyWorktreeIsFailedNotForced(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	dirty := filepath.Join(root, "rig-a", "crew", "overseer-b")
	clean := filepath.Join(root, "rig-a", "crew", "overseer-a")
	const dirtyBranch, cleanBranch = "polecat/overseer-b/x", "polecat/overseer-a/x"
	f := newFakeGit(t)
	answerRemove(f, root, dirty, errors.New("exit status 128"))
	answerRemove(f, root, clean, nil)
	answerBranch(f, root, cleanBranch)

	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		removeAction(dirty),
		branchAction(dirty, dirtyBranch),
		removeAction(clean),
		branchAction(clean, cleanBranch),
	}}
	res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.callsMatching("--force"); n != 0 {
		t.Fatalf("calls %v, want no --force", f.calls)
	}
	if n := f.callsMatching("worktree remove " + dirty); n != 1 {
		t.Fatalf("the dirty removal ran %d times, want once, not retried: %v", n, f.calls)
	}
	if n := f.callsMatching("branch -D " + dirtyBranch); n != 0 {
		t.Fatalf("the dirty worktree's branch was deleted: %v", f.calls)
	}
	if len(res.Failed) != 2 || res.Failed[0].Action.Ref != dirty {
		t.Fatalf("Failed = %+v, want the dirty removal and its kept branch", res.Failed)
	}
	if len(res.Done) != 2 || res.Done[0].Ref != clean {
		t.Fatalf("Done = %+v, want the clean worktree removed after the failure", res.Done)
	}
}

// TestApplyRejectsUnsafeBranchName checks that a branch name failing the allowed
// shape never reaches a command, and that its worktree is still removed.
func TestApplyRejectsUnsafeBranchName(t *testing.T) {
	t.Parallel()
	for name, branch := range map[string]string{
		"a space":          "bad branch",
		"a shell splice":   "x;rm",
		"a command subst":  "$(x)",
		"a leading dash":   "-D",
		"a quote":          `a"b`,
		"a backslash":      `back\slash`,
		"a leading option": "--force",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			path := filepath.Join(root, "rig-a", "crew", "overseer-a")
			f := newFakeGit(t)
			answerRemove(f, root, path, nil)

			plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
				removeAction(path),
				branchAction(path, branch),
			}}
			res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), plan)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if n := f.callsMatching("branch -D"); n != 0 {
				t.Fatalf("calls %v, want no branch command for an unsafe name", f.calls)
			}
			if len(res.Failed) != 1 || res.Failed[0].Action.Kind != kindDeleteBranch {
				t.Fatalf("Failed = %+v, want the one refused branch", res.Failed)
			}
			if len(res.Done) != 1 || res.Done[0].Kind != kindRemove {
				t.Fatalf("Done = %+v, want the worktree removed", res.Done)
			}
		})
	}
}

// TestApplyRemovesWorktreeThenItsBranch checks the order of a landed removal:
// the shared git dir is read first, the worktree goes next, and its branch last.
func TestApplyRemovesWorktreeThenItsBranch(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	answerRemove(f, root, path, nil)
	answerBranch(f, root, branch)

	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		removeAction(path),
		branchAction(path, branch),
	}}
	res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []string{
		"git -C " + path + " rev-parse --git-common-dir",
		"git --git-dir " + commonOf(root) + " worktree remove " + path,
		"git --git-dir " + commonOf(root) + " branch -D " + branch,
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	if len(res.Done) != 2 || len(res.Failed) != 0 {
		t.Fatalf("Done %d, Failed %d; want both done", len(res.Done), len(res.Failed))
	}
}

// TestApplyDoesNotDeleteBranchWhenRemovalFailed checks that a branch whose
// worktree removal failed is kept, even though its own name is safe.
func TestApplyDoesNotDeleteBranchWhenRemovalFailed(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	answerRemove(f, root, path, errors.New("exit status 128"))

	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		removeAction(path),
		branchAction(path, branch),
	}}
	res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if n := f.callsMatching("branch -D"); n != 0 {
		t.Fatalf("calls %v, want no branch delete after a failed removal", f.calls)
	}
	if len(res.Failed) != 2 {
		t.Fatalf("Failed = %+v, want the removal and the kept branch", res.Failed)
	}
}

// TestApplyAppendsOneLedgerEntry checks that each call appends exactly one line,
// including a refused plan and an empty one, and that a refused plan runs no
// command.
func TestApplyAppendsOneLedgerEntry(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	const branch = "polecat/overseer-a/x"
	f := newFakeGit(t)
	answerRemove(f, root, path, nil)
	answerBranch(f, root, branch)
	l := applyLedger(t)
	target := applyTarget(root, f, l)
	ctx := context.Background()

	if _, err := target.Apply(ctx, diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		removeAction(path),
		branchAction(path, branch),
	}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entries, err := l.Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 1 || entries[0].Mode != "apply" || entries[0].Done != 2 || entries[0].Planned != 2 {
		t.Fatalf("entries = %+v, want one apply line with 2 planned and 2 done", entries)
	}

	calls := len(f.calls)
	if _, err := target.Apply(ctx, diskmaint.Plan{Target: planTarget, Refused: "unreadable worktree state", Actions: []diskmaint.Action{
		removeAction(path),
	}}); err != nil {
		t.Fatalf("Apply refused: %v", err)
	}
	if len(f.calls) != calls {
		t.Fatalf("a refused plan ran %v, want no command", f.calls[calls:])
	}
	if _, err := target.Apply(ctx, diskmaint.Plan{Target: planTarget}); err != nil {
		t.Fatalf("Apply empty: %v", err)
	}
	entries, err = l.Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 3 || entries[1].Refused == "" || entries[2].Planned != 0 {
		t.Fatalf("entries = %+v, want the refused and the empty run each one line", entries)
	}
}

// TestApplyNeedsALedger checks that a target without a ledger runs nothing: a
// run that cannot write its record does not start.
func TestApplyNeedsALedger(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	f := newFakeGit(t)
	target := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Environ: noRedirect})
	_, err := target.Apply(context.Background(), diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{removeAction(path)}})
	if !errors.Is(err, errNoLedger) {
		t.Fatalf("Apply error = %v, want errNoLedger", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls %v, want none without a ledger", f.calls)
	}
}

// TestApplyRefusesARedirectedGit checks that an apply in a redirected git
// environment runs nothing, as planning refuses to.
func TestApplyRefusesARedirectedGit(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	f := newFakeGit(t)
	l := applyLedger(t)
	target := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Run: f.run, Ledger: l,
		Environ: func() []string { return []string{"GIT_DIR=/elsewhere/.git"} }})
	_, err := target.Apply(context.Background(), diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{removeAction(path)}})
	if !errors.Is(err, errRedirected) {
		t.Fatalf("Apply error = %v, want errRedirected", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls %v, want none in a redirected environment", f.calls)
	}
}

// TestApplyRecordsEachRefusal drives the failures a removal can hit: a shared git
// dir that cannot be read or is empty, a branch delete git refuses, a delete
// ref with no separator, and an action kind this target does not carry. Each
// fails its own action, and the failure is recorded as the sentinel's text.
func TestApplyRecordsEachRefusal(t *testing.T) {
	t.Parallel()
	const branch = "polecat/overseer-a/x"
	for name, c := range map[string]struct {
		setup   func(f *fakeGit, root, path string)
		actions func(path string) []diskmaint.Action
		want    error
	}{
		"the git dir cannot be read": {
			setup: func(f *fakeGit, _, path string) {
				f.answerErr("-C "+path+" rev-parse --git-common-dir", errors.New("exit status 128"))
			},
			actions: func(path string) []diskmaint.Action { return []diskmaint.Action{removeAction(path)} },
			want:    errCommonDir,
		},
		"the git dir is empty": {
			setup: func(f *fakeGit, _, path string) {
				f.answer("-C "+path+" rev-parse --git-common-dir", "\n", nil)
			},
			actions: func(path string) []diskmaint.Action { return []diskmaint.Action{removeAction(path)} },
			want:    errCommonDir,
		},
		"the branch delete is refused": {
			setup: func(f *fakeGit, root, path string) {
				answerRemove(f, root, path, nil)
				f.answerErr("--git-dir "+commonOf(root)+" branch -D "+branch, errors.New("exit status 1"))
			},
			actions: func(path string) []diskmaint.Action {
				return []diskmaint.Action{removeAction(path), branchAction(path, branch)}
			},
			want: errBranchFailed,
		},
		"a branch ref with no separator": {
			setup: func(f *fakeGit, root, path string) { answerRemove(f, root, path, nil) },
			actions: func(path string) []diskmaint.Action {
				return []diskmaint.Action{removeAction(path), {Kind: kindDeleteBranch, Ref: branch}}
			},
			want: errBadRef,
		},
		"an action kind this target does not carry": {
			setup: func(*fakeGit, string, string) {},
			actions: func(path string) []diskmaint.Action {
				return []diskmaint.Action{{Kind: "delete-manifest", Ref: path}}
			},
			want: errUnknownKind,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := townRoot(t)
			path := filepath.Join(root, "rig-a", "crew", "overseer-a")
			f := newFakeGit(t)
			c.setup(f, root, path)
			res, err := applyTarget(root, f, applyLedger(t)).Apply(context.Background(), diskmaint.Plan{Target: planTarget, Actions: c.actions(path)})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if len(res.Failed) == 0 || res.Failed[len(res.Failed)-1].Err != c.want.Error() {
				t.Fatalf("Failed = %+v, want %q recorded", res.Failed, c.want)
			}
		})
	}
}

// TestApplyStopsOnACancelledContext checks that a cancelled run starts no
// command, and returns the cancellation rather than a clean result.
func TestApplyStopsOnACancelledContext(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	f := newFakeGit(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := applyTarget(root, f, applyLedger(t)).Apply(ctx, diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{removeAction(path)}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply error = %v, want context.Canceled", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls %v, want none after cancellation", f.calls)
	}
}

// TestApplyNeedsARunner checks that a target with no git runner refuses the run
// and records the refusal, rather than deleting nothing silently.
func TestApplyNeedsARunner(t *testing.T) {
	t.Parallel()
	root := townRoot(t)
	path := filepath.Join(root, "rig-a", "crew", "overseer-a")
	l := applyLedger(t)
	target := NewTarget(Config{Town: root, Rigs: []string{"rig-a"}, Environ: noRedirect, Ledger: l})
	_, err := target.Apply(context.Background(), diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{removeAction(path)}})
	if !errors.Is(err, errNoRunner) {
		t.Fatalf("Apply error = %v, want errNoRunner", err)
	}
	entries, err := l.Tail(1)
	if err != nil || len(entries) != 1 || len(entries[0].Errors) != 1 {
		t.Fatalf("ledger = %+v (%v), want the refusal recorded", entries, err)
	}
}
