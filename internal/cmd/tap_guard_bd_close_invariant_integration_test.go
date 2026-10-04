//go:build integration

package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRunTapGuardBdCloseInvariant runs the bd-close guard end to
// end in a hermetic town with a real git checkout: the guard reads the branch
// and its commits ahead of origin/main from git. TestBdCloseInvariantRefusal
// unit-tests the decision table itself.
func TestIntegrationRunTapGuardBdCloseInvariant(t *testing.T) {
	t.Parallel()
	// ConflictTaskCloseAllowed is the false-positive
	// regression test for the scoping decision. A conflict-resolution task is
	// completed with `bd close <task-id>` after the branch is pushed (the refinery
	// unblocks the MR when the task closes), and that branch legitimately carries
	// commits the merge has not taken yet. The task id is not the id the branch was
	// cut for, so the guard must not judge it — scoping by the agent's hook_bead
	// instead of the branch would have blocked this documented workflow.
	t.Run("ConflictTaskCloseAllowed", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

		if err := f.run(t, "bd close gt-conflict-task"); err != nil {
			t.Errorf("closing a bead this branch was not cut for must be allowed, got error: %v", err)
		}
	})

	// SelfCloseBlocked is the guard's reason to
	// exist: a `bd close` naming the bead the branch was cut for, with unmerged
	// commits and nothing tracking them, must be blocked — this is the exact bypass
	// of gt-6hmz that never reaches done.go.
	t.Run("SelfCloseBlocked", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

		err := f.run(t, "bd close gt-arno")
		if err == nil {
			t.Fatal("expected a raw self-close with unmerged commits to be blocked, got nil error")
		}
		if exitErr, ok := err.(*SilentExitError); !ok || exitErr.Code != 2 {
			t.Errorf("expected exit code 2 (BLOCK), got %v", err)
		}
	})

	// ZeroCommitsAllowed covers the polecat
	// "nothing to implement" path: `bd close <id> --reason="no-changes: ..."` on a
	// branch with no commits of its own must pass, or the guard would block the
	// documented way to close a bead that turned out to need no work.
	t.Run("ZeroCommitsAllowed", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 0)

		if err := f.run(t, `bd close gt-arno --reason "no-changes: nothing to do"`); err != nil {
			t.Errorf("expected a zero-commit close to be allowed, got error: %v", err)
		}
	})

	// OperatorOverrideAllowed covers exit (c) at the
	// guard boundary — the one close gt done's own path can never take, because gt
	// done passes an empty close reason. If the guard did not read --reason, the
	// operator override would be unreachable for every raw close.
	t.Run("OperatorOverrideAllowed", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

		if err := f.run(t, `bd close gt-arno --reason "cancel: work abandoned, superseded by gt-x"`); err != nil {
			t.Errorf("expected an operator override reason to allow the close, got error: %v", err)
		}
	})

	// OtherRigsBranchUntouched pins the boundary the
	// branch rule draws for a session working in one rig on another rig's bead: a
	// bead whose id does not appear in this branch is not this guard's business,
	// however much unmerged work the branch carries.
	t.Run("OtherRigsBranchUntouched", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

		if err := f.run(t, "bd close hq-cv-vbvss gt-other"); err != nil {
			t.Errorf("expected ids outside this branch to be allowed, got error: %v", err)
		}
	})

	// OnDefaultBranchAllowed pins the wrapper's
	// "nothing to compare against itself" rule at the guard boundary: on the rig's
	// default branch the invariant is not evaluable, so a close passes.
	t.Run("OnDefaultBranchAllowed", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "main", 0)

		if err := f.run(t, "bd close gt-arno"); err != nil {
			t.Errorf("expected a close from the default branch to be allowed, got error: %v", err)
		}
	})

	// NonAgentContextAllowed pins the first scope
	// check: outside a Gas Town agent session the guard must never fire, however
	// close the command looks to the bypass it exists to stop.
	t.Run("NonAgentContextAllowed", func(t *testing.T) {
		t.Parallel()
		f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)
		// A human working in the repo: no role env, and a path with no
		// /polecats/, /crew/ or /deacon/dogs/ component.
		//
		// The fixture's worktree path does contain /polecats/, which
		// isGasTownAgentContext treats as an agent context by path alone, so
		// the case is driven from the town's plain checkout instead.
		for _, env := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS", "GT_REFINERY", "GT_DEACON", "GT_DOG_NAME"} {
			delete(f.env, env)
		}

		payload := fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":"bd close gt-arno"}}`, f.plain)
		if err := tapGuardBdCloseInvariant(strings.NewReader(payload), f.process(f.plain)); err != nil {
			t.Errorf("expected the guard to be a no-op outside an agent context, got error: %v", err)
		}

		// The same close, from the same directory, with only the agent-context
		// signal added: it must be refused. The pair is what makes the allow
		// above evidence — the plain checkout resolves to the same branch and
		// the same unmerged commits either way, so the agent-context check is
		// the whole difference between the two runs. A guard that ignored the
		// check and judged this directory an agent context would refuse the
		// first close too, failing the case rather than passing it for the
		// wrong reason (gt-22hdp.59).
		agentEnv := make(map[string]string, len(f.env)+1)
		for k, v := range f.env {
			agentEnv[k] = v
		}
		agentEnv["GT_POLECAT"] = "malachite"
		agent := f.process(f.plain)
		agent.getenv = envMap(agentEnv)
		if err := tapGuardBdCloseInvariant(strings.NewReader(payload), agent); err == nil {
			t.Error("expected the same close to be refused once the same directory is read as an agent context")
		}
	})
}

// bdCloseInvariantFixture is a hermetic town: a real "mayor/town.json" marker
// so workspace.Find resolves it, a rig directory, a real git checkout at the
// polecat worktree path, and a second checkout of the same branch at a path
// with no agent component (plain). Nothing here touches the operator's town,
// so a guard run against it neither reads nor mutates production state; the
// guard itself runs no bd subprocess.
type bdCloseInvariantFixture struct {
	town string
	work string
	// plain is the same branch and history as work, checked out where
	// /polecats/, /crew/ and /deacon/dogs/ do not appear in the path: the cwd
	// the NonAgentContextAllowed case needs a non-agent session to sit in.
	plain string
	env   map[string]string
}

func (f bdCloseInvariantFixture) payload(command string) string {
	return fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":%q}}`, f.work, command)
}

func (f bdCloseInvariantFixture) run(t *testing.T, command string) error {
	t.Helper()
	return tapGuardBdCloseInvariant(strings.NewReader(f.payload(command)), f.process(f.work))
}

// process is the guard's view of a session with the fixture's environment,
// whose working directory is wd.
func (f bdCloseInvariantFixture) process(wd string) guardProcess {
	return guardProcess{getenv: envMap(f.env), getwd: func() (string, error) { return wd, nil }}
}

// newBdCloseInvariantFixture builds the fixture town on the given polecat
// branch, with commitsAhead commits of the polecat's own on top of origin/main.
//
// origin/main is materialized as a real remote-tracking ref rather than a local
// "main", because closeTimeBranchTarget resolves the target through
// CleanBaseRef("origin", ...) — the same path gt done takes, and the case
// gt-6hmz's finding 1 was about (a stale local main overcounts commits).
//
// The town is a copy of one built once per test binary for each branch and
// commit count (cachedGitFixture); the agent identity is set per test.
func newBdCloseInvariantFixture(t *testing.T, branch string, commitsAhead int) bdCloseInvariantFixture {
	t.Helper()
	const rig = "gastown"
	town, _ := cachedGitFixture(t, fmt.Sprintf("bd-close-invariant %s %d", branch, commitsAhead), func(town string) (struct{}, error) {
		buildBdCloseInvariantTown(t, town, rig, branch, commitsAhead)
		return struct{}{}, nil
	})
	work := filepath.Join(town, rig, "polecats", "malachite", rig)

	// Agent identity, as a spawned session carries it. The git checkout itself
	// supplies the branch, via the payload cwd, so nothing here depends on the
	// guard's getwd fallback.
	env := map[string]string{"GT_TOWN_ROOT": town, "GT_ROOT": town, "GT_RIG": rig, "GT_POLECAT": "malachite"}

	return bdCloseInvariantFixture{town: town, work: work, plain: filepath.Join(town, "notes"), env: env}
}

// buildBdCloseInvariantTown lays out newBdCloseInvariantFixture's town, its
// polecat checkout, and the plain checkout of the same branch.
func buildBdCloseInvariantTown(t *testing.T, town, rig, branch string, commitsAhead int) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The worktree sits where a real polecat's does, so the layout the guard
	// derives (rig + worktree) matches production.
	work := filepath.Join(town, rig, "polecats", "malachite", rig)
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		t.Fatal(err)
	}

	remote := filepath.Join(town, "remote.git")
	testRunGit(t, town, "init", "--bare", "--initial-branch", "main", remote)
	testRunGit(t, town, "clone", remote, work)
	testRunGit(t, work, "config", "user.email", "test@test.com")
	testRunGit(t, work, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testRunGit(t, work, "add", ".")
	testRunGit(t, work, "commit", "-m", "base")
	testRunGit(t, work, "push", "origin", "main")
	testRunGit(t, work, "fetch", "origin")
	if branch != "main" {
		// A clone already sits on main; checking out "-b main origin/main"
		// would fail there, and the on-default-branch case needs it.
		testRunGit(t, work, "checkout", "-b", branch, "origin/main")
	}
	for i := 0; i < commitsAhead; i++ {
		if err := os.WriteFile(filepath.Join(work, "work.txt"), []byte(strings.Repeat("x", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		testRunGit(t, work, "add", ".")
		testRunGit(t, work, "commit", "-m", "polecat work")
	}

	// The plain checkout: the branch work is on, with the same commits ahead of
	// origin/main, at a path with none of the agent components
	// isGasTownAgentContext matches. Cloning work keeps the two from drifting;
	// what NonAgentContextAllowed needs from it is that the guard resolve a
	// branch and unmerged commits from it, so the only thing standing between
	// `bd close gt-arno` and a refusal there is the agent-context check
	// (gt-22hdp.59).
	testRunGit(t, town, "clone", work, filepath.Join(town, "notes"))
}
