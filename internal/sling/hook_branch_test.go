package sling

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// hookBranchTestOptions is a sling for gt-wlrgk8: the bead whose work the
// gt-u9fqky incident hooked onto a seat still sitting on gt-ug7zed's branch.
func hookBranchTestOptions() Options {
	opts := holdTestOptions()
	opts.BeadID = "gt-wlrgk8"
	return opts
}

// TestRunHookBranchGuardRefusesAnotherBeadsBranch: a seat the pool was asked
// for again may still have its worktree on the branch a first sling made for
// another bead. Hooking here would put this bead's hook onto that work, which
// is what the incident did when the other bead then landed and its branch
// deletion took this bead's only commit with it (gt-u9fqky).
func TestRunHookBranchGuardRefusesAnotherBeadsBranch(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{branch: "polecat/coma/gt-ug7zed+mwmuimru"}

	result, err := Run(context.Background(), f.deps(), hookBranchTestOptions())
	if err == nil {
		t.Fatalf("the dispatch hooked a seat whose worktree holds another bead's branch (result %+v)", result)
	}
	if !strings.Contains(err.Error(), dispatch.SlingRefusalMarker) {
		t.Errorf("err = %v, want the refusal marker %q: an automatic dispatcher must defer the bead and ask again, not count a failed dispatch",
			err, dispatch.SlingRefusalMarker)
	}
	for _, want := range []string{"polecat/coma/gt-ug7zed+mwmuimru", "gt-ug7zed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q named", err, want)
		}
	}
	if f.hooks != 0 {
		t.Errorf("the bead was hooked %d time(s), want 0", f.hooks)
	}
	if f.started != 0 {
		t.Errorf("a session was started %d time(s), want 0", f.started)
	}
	if f.rollback != 1 {
		t.Errorf("rolled back %d spawned polecat(s), want 1: the refusal is past the spawn", f.rollback)
	}
	if f.branchDir != holdTestClonePath {
		t.Errorf("read the branch of %q, want the spawn's worktree %q", f.branchDir, holdTestClonePath)
	}
}

// TestRunHookBranchGuardRefusesAResumeOfAnotherBranch: a resume names the
// branch the sling is taking over. When the worktree sits on some other
// branch, the resume did not happen and the hook would land on work the sling
// never asked for.
func TestRunHookBranchGuardRefusesAResumeOfAnotherBranch(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{branch: "polecat/coma/gt-ug7zed+mwmuimru"}

	opts := hookBranchTestOptions()
	opts.ResumeBranch = "polecat/onyx/gt-wlrgk8+mwnlwq9w"
	if _, err := Run(context.Background(), f.deps(), opts); err == nil {
		t.Fatal("a resume whose branch is not checked out was allowed to hook")
	}
	if f.hooks != 0 || f.rollback != 1 {
		t.Errorf("hooks = %d, rollback = %d, want no hook and the spawn rolled back", f.hooks, f.rollback)
	}
}

// TestRunHookBranchGuardAllowsOwnResumeAndNonPolecatBranches: the guard is for
// a seat that would take work it was not slung for. A branch named for this
// bead, a deliberate resume of the branch it names, and a branch the town does
// not name a seat by all hook as they did before (gt-u9fqky).
func TestRunHookBranchGuardAllowsOwnResumeAndNonPolecatBranches(t *testing.T) {
	t.Parallel()
	const other = "polecat/coma/gt-ug7zed+mwmuimru"
	for _, tc := range []struct {
		name, branch, resume string
	}{
		{"a branch named for this bead", "polecat/onyx/gt-wlrgk8+mwnlwq9w", ""},
		{"a resume of the branch it names", other, other},
		{"a non-polecat branch", "main", ""},
		{"a detached HEAD", "HEAD", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSlingDeps{branch: tc.branch}
			opts := hookBranchTestOptions()
			opts.ResumeBranch = tc.resume

			result, err := Run(context.Background(), f.deps(), opts)
			if err != nil {
				t.Fatalf("branch %q with resume %q was refused: %v", tc.branch, tc.resume, err)
			}
			if f.hooks != 1 || result == nil || !result.Success {
				t.Fatalf("hooks = %d, result %+v, want the dispatch to hook the bead", f.hooks, result)
			}
			if f.rollback != 0 {
				t.Errorf("rolled back %d spawn(s), want 0 for a dispatch that committed", f.rollback)
			}
		})
	}
}

// TestRunHookBranchGuardWarnsAndHooksWhenTheBranchIsUnreadable: a worktree
// whose branch cannot be read is unknown, and the dispatch keeps the behavior
// it had before this guard rather than refusing work it cannot judge. The
// warning is what an operator has to see (gt-u9fqky).
func TestRunHookBranchGuardWarnsAndHooksWhenTheBranchIsUnreadable(t *testing.T) {
	t.Parallel()
	f := &fakeSlingDeps{branchErr: errors.New("git rev-parse: not a repository")}
	d := f.deps()
	var out bytes.Buffer
	d.Out = &out

	result, err := Run(context.Background(), d, hookBranchTestOptions())
	if err != nil {
		t.Fatalf("an unreadable branch refused the dispatch: %v", err)
	}
	if f.hooks != 1 || result == nil || !result.Success {
		t.Fatalf("hooks = %d, result %+v, want the dispatch to hook the bead", f.hooks, result)
	}
	if !strings.Contains(out.String(), "git rev-parse: not a repository") {
		t.Errorf("output = %q, want the read error warned", out.String())
	}
}
