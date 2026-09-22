package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestParseBdCloseInvocations pins the command shape recognition: which
// segments count as a `bd close`, which ids they carry, and which tokens are
// flags or flag values rather than ids.
func TestParseBdCloseInvocations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    []bdCloseInvocation
	}{
		{
			name:    "bare close",
			command: "bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "multiple ids",
			command: "bd close gt-arno gt-other",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno", "gt-other"}}},
		},
		{
			name:    "reason flag before id",
			command: `bd close -r "supersede: folded into gt-x" gt-arno`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "supersede: folded into gt-x",
			}},
		},
		{
			name:    "reason flag after id",
			command: `bd close gt-arno --reason "cancel: abandoned"`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "cancel: abandoned",
			}},
		},
		{
			name:    "equals-form reason",
			command: `bd close gt-arno --reason="cancel: not needed"`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "cancel: not needed",
			}},
		},
		{
			// The unquoted space means a shell would split this into two argv
			// entries, so the test pins the parser's own behavior on the
			// mutation rather than a realistic invocation: the glued value is
			// the reason, and the following word must not become an id.
			name:    "glued short reason",
			command: `bd close gt-arno -rsupersede:`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "supersede:",
			}},
		},
		{
			name:    "boolean flags are not ids",
			command: "bd close --force gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "reason is not mistaken for an id",
			command: `bd close --reason "gt-decoy" gt-arno`,
			want: []bdCloseInvocation{{
				IDs:    []string{"gt-arno"},
				Reason: "gt-decoy",
			}},
		},
		{
			name:    "found on a later segment of a compound command",
			command: "cd /tmp && bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "env assignment prefix",
			command: "BD_ACTOR=x bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "absolute path to the binary",
			command: "/usr/local/bin/bd close gt-arno",
			want:    []bdCloseInvocation{{IDs: []string{"gt-arno"}}},
		},
		{
			name:    "two closes in one line",
			command: "bd close gt-a; bd close gt-b",
			want: []bdCloseInvocation{
				{IDs: []string{"gt-a"}},
				{IDs: []string{"gt-b"}},
			},
		},
		{
			name:    "unrelated bd subcommand",
			command: "bd update gt-arno --status=in_progress",
			want:    nil,
		},
		{
			name:    "close in a heredoc body is data, not a command",
			command: "gt mail send mayor/ -s hi --stdin <<'BODY'\nbd close gt-arno\nBODY",
			want:    nil,
		},
		{
			name:    "close mentioned inside a quoted argument",
			command: `echo "run bd close gt-arno"`,
			want:    nil,
		},
		{
			name:    "similar-looking binary is not bd",
			command: "subd close gt-arno",
			want:    nil,
		},
		{
			// A variable id is unreadable, so it cannot match a branch and
			// cannot be judged. It is still reported as an invocation, so
			// "no bd close at all" stays distinguishable from "a close whose
			// ids this parser could not read".
			name:    "variable id is reported but matches nothing",
			command: "bd close $ISSUE",
			want:    []bdCloseInvocation{{IDs: []string{"$ISSUE"}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseBdCloseInvocations(tt.command)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseBdCloseInvocations(%q) = %+v, want %+v", tt.command, got, tt.want)
			}
		})
	}
}

// TestBranchNamesBead pins the scope rule that decides which closes this guard
// judges at all: the branch is the only signal that a bead is the source_issue
// of the work in this worktree, so the match has to be exact and component-wise.
func TestBranchNamesBead(t *testing.T) {
	t.Parallel()
	const branch = "polecat/malachite/gt-arno+muck73gu"
	tests := []struct {
		name    string
		issueID string
		want    bool
	}{
		{"the bead the branch was cut for", "gt-arno", true},
		{"case-insensitive", "GT-Arno", true},
		{"leading/trailing whitespace from parsing", " gt-arno ", true},
		{"a prefix of the id is not the id", "gt-arn", false},
		{"a longer id is not the id", "gt-arnold", false},
		{"the suffix component is not a bead id", "muck73gu", false},
		{"the polecat name is not a bead id", "malachite", false},
		{"another bead entirely", "gt-other", false},
		{"a bead id cannot match a plain word", "temp", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := branchNamesBead(branch, tt.issueID); got != tt.want {
				t.Errorf("branchNamesBead(%q, %q) = %v, want %v", branch, tt.issueID, got, tt.want)
			}
		})
	}

	t.Run("default branch names no bead", func(t *testing.T) {
		t.Parallel()
		if branchNamesBead("main", "main") {
			t.Error("a default branch must not be read as naming the bead \"main\"")
		}
	})
}

// TestBdCloseInvariantRefusal pins the decision table at the guard's own
// boundary: the three gt-6hmz exits, and refusal when none holds. It is the
// guard's contract that it refuses exactly what gt done refuses.
func TestBdCloseInvariantRefusal(t *testing.T) {
	t.Parallel()
	scope := func(count int, pendingMR string, mrs map[string]*beads.Issue) bdCloseInvariantScope {
		return bdCloseInvariantScope{
			branch:    "polecat/malachite/gt-arno+muck73gu",
			target:    "origin/main",
			counter:   fakeCloseTimeCommitCounter{count: count},
			mrTracker: fakeCloseTimeMRTracker{issues: mrs},
			pendingMR: pendingMR,
		}
	}

	t.Run("exit (a): zero commits ahead", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(0, "", nil), "gt-arno", ""); got != "" {
			t.Errorf("expected close allowed with zero commits ahead, got %q", got)
		}
	})

	t.Run("exit (b): live MR from the agent bead", func(t *testing.T) {
		t.Parallel()
		mrs := map[string]*beads.Issue{"gt-wisp-mr1": {ID: "gt-wisp-mr1", Status: "open"}}
		if got := bdCloseInvariantRefusal(scope(4, "gt-wisp-mr1", mrs), "gt-arno", ""); got != "" {
			t.Errorf("expected close allowed when a live MR tracks the issue, got %q", got)
		}
	})

	t.Run("exit (b) does not trust a stale pointer to a closed MR", func(t *testing.T) {
		t.Parallel()
		mrs := map[string]*beads.Issue{"gt-wisp-mr1": {ID: "gt-wisp-mr1", Status: "closed"}}
		if got := bdCloseInvariantRefusal(scope(4, "gt-wisp-mr1", mrs), "gt-arno", ""); got == "" {
			t.Error("expected refusal when the active_mr pointer names an already-closed MR")
		}
	})

	t.Run("exit (c): supersede reason", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4, "", nil), "gt-arno", "supersede: folded into gt-x"); got != "" {
			t.Errorf("expected supersede: reason to allow the close, got %q", got)
		}
	})

	t.Run("exit (c): cancel reason", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4, "", nil), "gt-arno", "cancel: abandoned"); got != "" {
			t.Errorf("expected cancel: reason to allow the close, got %q", got)
		}
	})

	t.Run("refuses unmerged work with nothing tracking it", func(t *testing.T) {
		t.Parallel()
		got := bdCloseInvariantRefusal(scope(4, "", nil), "gt-arno", "")
		if got == "" {
			t.Fatal("expected refusal for unmerged commits with no MR and no override reason")
		}
		if !strings.Contains(got, "polecat/malachite/gt-arno+muck73gu") {
			t.Errorf("refusal must name the branch, got %q", got)
		}
		if !strings.Contains(got, "4") {
			t.Errorf("refusal must name the unmerged commit count, got %q", got)
		}
	})

	t.Run("a done-style reason is not an override", func(t *testing.T) {
		t.Parallel()
		if got := bdCloseInvariantRefusal(scope(4, "", nil), "gt-arno", "done"); got == "" {
			t.Error("expected a non-prefix reason to still be refused")
		}
	})
}

// bdCloseInvariantFixture is a hermetic town: a real "mayor/town.json" marker
// so workspace.Find resolves it, a rig directory, and a real git checkout at
// the polecat worktree path. Nothing here touches the operator's town, so a
// guard run against it neither reads nor mutates production state — in
// particular the rig has no .beads database, so no bd subprocess is spawned
// (see rigBeadsWorkspaceExists).
type bdCloseInvariantFixture struct {
	town string
	work string
}

func (f bdCloseInvariantFixture) payload(command string) string {
	return fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":%q}}`, f.work, command)
}

func (f bdCloseInvariantFixture) run(t *testing.T, command string) error {
	t.Helper()
	var err error
	withStdin(t, f.payload(command), func() {
		err = runTapGuardBdCloseInvariant(tapGuardBdCloseInvariantCmd, nil)
	})
	return err
}

// newBdCloseInvariantFixture builds the fixture town on the given polecat
// branch, with commitsAhead commits of the polecat's own on top of origin/main.
//
// origin/main is materialized as a real remote-tracking ref rather than a local
// "main", because closeTimeBranchTarget resolves the target through
// CleanBaseRef("origin", ...) — the same path gt done takes, and the case
// gt-6hmz's finding 1 was about (a stale local main overcounts commits).
func newBdCloseInvariantFixture(t *testing.T, branch string, commitsAhead int) bdCloseInvariantFixture {
	t.Helper()
	const rig = "gastown"
	town := t.TempDir()

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

	// Agent identity, as a spawned session carries it. The git checkout itself
	// supplies the branch, via the payload cwd — the test process stays in the
	// repo worktree, so nothing here depends on the guard's os.Getwd fallback
	// happening to match.
	t.Setenv("GT_TOWN_ROOT", town)
	t.Setenv("GT_ROOT", town)
	t.Setenv("GT_RIG", rig)
	t.Setenv("GT_POLECAT", "malachite")

	return bdCloseInvariantFixture{town: town, work: work}
}

// TestRunTapGuardBdCloseInvariant_ConflictTaskCloseAllowed is the false-positive
// regression test for the scoping decision. A conflict-resolution task is
// completed with `bd close <task-id>` after the branch is pushed (the refinery
// unblocks the MR when the task closes), and that branch legitimately carries
// commits the merge has not taken yet. The task id is not the id the branch was
// cut for, so the guard must not judge it — scoping by the agent's hook_bead
// instead of the branch would have blocked this documented workflow.
func TestRunTapGuardBdCloseInvariant_ConflictTaskCloseAllowed(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

	if err := f.run(t, "bd close gt-conflict-task"); err != nil {
		t.Errorf("closing a bead this branch was not cut for must be allowed, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_SelfCloseBlocked is the guard's reason to
// exist: a `bd close` naming the bead the branch was cut for, with unmerged
// commits and nothing tracking them, must be blocked — this is the exact bypass
// of gt-6hmz that never reaches done.go.
func TestRunTapGuardBdCloseInvariant_SelfCloseBlocked(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

	err := f.run(t, "bd close gt-arno")
	if err == nil {
		t.Fatal("expected a raw self-close with unmerged commits to be blocked, got nil error")
	}
	if exitErr, ok := err.(*SilentExitError); !ok || exitErr.Code != 2 {
		t.Errorf("expected exit code 2 (BLOCK), got %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_ZeroCommitsAllowed covers the polecat
// "nothing to implement" path: `bd close <id> --reason="no-changes: ..."` on a
// branch with no commits of its own must pass, or the guard would block the
// documented way to close a bead that turned out to need no work.
func TestRunTapGuardBdCloseInvariant_ZeroCommitsAllowed(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 0)

	if err := f.run(t, `bd close gt-arno --reason "no-changes: nothing to do"`); err != nil {
		t.Errorf("expected a zero-commit close to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_OperatorOverrideAllowed covers exit (c) at the
// guard boundary — the one close gt done's own path can never take, because gt
// done passes an empty close reason. If the guard did not read --reason, the
// operator override would be unreachable for every raw close.
func TestRunTapGuardBdCloseInvariant_OperatorOverrideAllowed(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

	if err := f.run(t, `bd close gt-arno --reason "cancel: work abandoned, superseded by gt-x"`); err != nil {
		t.Errorf("expected an operator override reason to allow the close, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_OtherRigsBranchUntouched pins the boundary the
// branch rule draws for a session working in one rig on another rig's bead: a
// bead whose id does not appear in this branch is not this guard's business,
// however much unmerged work the branch carries.
func TestRunTapGuardBdCloseInvariant_OtherRigsBranchUntouched(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)

	if err := f.run(t, "bd close hq-cv-vbvss gt-other"); err != nil {
		t.Errorf("expected ids outside this branch to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_OnDefaultBranchAllowed pins the wrapper's
// "nothing to compare against itself" rule at the guard boundary: on the rig's
// default branch the invariant is not evaluable, so a close passes.
func TestRunTapGuardBdCloseInvariant_OnDefaultBranchAllowed(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "main", 0)

	if err := f.run(t, "bd close gt-arno"); err != nil {
		t.Errorf("expected a close from the default branch to be allowed, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_NonAgentContextAllowed pins the first scope
// check: outside a Gas Town agent session the guard must never fire, however
// close the command looks to the bypass it exists to stop.
func TestRunTapGuardBdCloseInvariant_NonAgentContextAllowed(t *testing.T) {
	f := newBdCloseInvariantFixture(t, "polecat/malachite/gt-arno+muck73gu", 3)
	// A human working in the repo: no role env, and a path with no
	// /polecats/, /crew/ or /deacon/dogs/ component.
	//
	// The fixture's worktree path does contain /polecats/, which
	// isGasTownAgentContext treats as an agent context by path alone, so this
	// case is driven from a plain directory inside the same town instead.
	plain := filepath.Join(f.town, "notes")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"GT_POLECAT", "GT_CREW", "GT_WITNESS", "GT_REFINERY", "GT_MAYOR", "GT_DEACON", "GT_DOG_NAME"} {
		t.Setenv(env, "")
	}

	payload := fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":"bd close gt-arno"}}`, plain)
	var err error
	withStdin(t, payload, func() {
		err = runTapGuardBdCloseInvariant(tapGuardBdCloseInvariantCmd, nil)
	})
	if err != nil {
		t.Errorf("expected the guard to be a no-op outside an agent context, got error: %v", err)
	}
}

// TestRunTapGuardBdCloseInvariant_UnrelatedCommandAllowed pins the self-filter:
// the guard is on every Bash call in the town, so anything that is not a
// bd close must pass without even resolving git scope.
func TestRunTapGuardBdCloseInvariant_UnrelatedCommandAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "malachite")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"ls -la && bd list --status=open"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardBdCloseInvariant(tapGuardBdCloseInvariantCmd, nil)
	})
	if err != nil {
		t.Errorf("expected a non-close command to be allowed, got error: %v", err)
	}
}

// TestRigBeadsWorkspaceExists pins the precondition that keeps the guard from
// shelling out to bd in a directory with no database — the state a fixture town
// (and a broken rig) is in.
func TestRigBeadsWorkspaceExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if rigBeadsWorkspaceExists(dir) {
		t.Error("a directory with no .beads database must report false")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !rigBeadsWorkspaceExists(dir) {
		t.Error("a directory holding .beads must report true")
	}
}
