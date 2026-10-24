package cmd

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestMatchesPRWorkflowCommand covers the self-filtering this guard now does
// against tool_input.command, mirroring the "if" glob patterns in
// hooks.DefaultBase() (Bash(gh pr create*), Bash(git checkout -b*),
// Bash(git switch -c*)).
func TestMatchesPRWorkflowCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"gh pr create", "gh pr create --title foo", true},
		{"git checkout -b", "git checkout -b feature/x", true},
		{"git switch -c", "git switch -c feature/x", true},
		// The capitals create or reset the branch the lowercase flags spell
		// (gt-5wtmz).
		{"git checkout -B", "git checkout -B feature/x", true},
		{"git switch -C", "git switch -C feature/x", true},
		{"git checkout -qB clustered", "git checkout -qB feature/x", true},
		{"leading whitespace", "  git checkout -b feature/x", true},
		{"unrelated command", "ls -la", false},
		{"unrelated git command", "git status", false},
		{"git checkout without -b", "git checkout main", false},
		{"empty command", "", false},
		{"compound command, blocked shape in later segment", "cd foo && git checkout -b x", true},
		{"compound command, blocked shape first", "gh pr create --title foo && echo done", true},
		{"compound command with pipe", "echo x | gh pr create --title foo", true},
		{"compound command, no blocked segment", "cd foo && git status", false},
		// git bundles short flags and glues a value to its flag, so -branch is
		// -b with the name "ranch", the same creation as -bfeature/x.
		{"-b glued to a name that spells branch", "git checkout -branch feature/x", true},
		{"a long flag is not mistaken for -b", "git checkout --branch feature/x", false},

		// An unquoted newline separates commands exactly like ';' does, so a
		// multi-line Bash tool call must be judged line by line — before
		// gt-3j8u the whole call was one segment and only its first word was
		// ever compared against the prefixes, so a blocked shape on any later
		// line failed open.
		{"newline-separated compound, blocked shape on later line", "cd /tmp\ngh pr create --title foo", true},
		{"newline-separated compound, blocked shape on first line", "git checkout -b feature/x\necho done", true},
		{"newline-separated compound, no blocked line", "cd /tmp\nls -la", false},
		{"newline-separated compound, blocked shape after a blank line", "echo one\n\ngit switch -c feature/x", true},

		// A backslash-newline is a line continuation, not a separator: the
		// shell joins the lines before tokenizing, so the command it runs is
		// the blocked shape even though no single line spells it.
		{"line continuation joins the halves of a blocked command", "git \\\n  checkout -b feature/x", true},

		// Heredoc bodies are data being written, not shell syntax (the rule
		// evaluateDangerousCommand and checkBashCommand already apply), so a
		// body line that spells a blocked shape is not an invocation — while
		// a real command after the heredoc still is.
		{"prose in a heredoc body stays opaque", "cat > note.md <<'EOF'\nnever run gh pr create here\nEOF", false},
		{"blocked command after a heredoc body", "cat > note.md <<'EOF'\nordinary content\nEOF\ngit switch -c feature/x", true},

		// gt-q91m: a heredoc body fed to a shell invoker is not data, it's a
		// script the shell will run — the same distinction
		// shellFedHeredocBodies draws for the dangerous-command guard
		// (gt-9g0y). "bash <<EOF ... gh pr create ... EOF" must be caught the
		// same as the equivalent compound command.
		{"gh pr create inside a bash-fed heredoc", "bash <<'EOF'\ngh pr create --title foo\nEOF", true},
		{"git checkout -b inside a sh-fed heredoc", "sh <<EOF\ngit checkout -b feature/x\nEOF", true},
		{"unrelated body in a bash-fed heredoc stays unblocked", "bash <<'EOF'\necho hello\nEOF", false},
		{"bash-fed heredoc piped through cat still counts", "cat <<'EOF' | bash\ngh pr create --title foo\nEOF", true},

		// Quoted text is never a command word, newline or not.
		{"quoted mention on its own line", "git commit -m \"use gh pr create\"\nls -la", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestIsBranchCreation pins the spellings the guard reads as a branch
// creation, switch's long forms --create (-c) and --force-create (-C)
// included: the hook carries no If glob to catch them (gt-3mp1), so this
// reader is the only thing that sees them (gt-g27m7).
func TestIsBranchCreation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"git switch -c", "git switch -c feature/x", true},
		{"git switch --create", "git switch --create feature/x", true},
		{"git switch --create=", "git switch --create=feature/x", true},
		{"git switch --force-create", "git switch --force-create feature/x", true},
		{"git switch --force-create=", "git switch --force-create=feature/x", true},
		{"git switch --force-create behind another flag", "git switch -q --force-create feature/x", true},
		{"git switch -C clustered", "git switch -qC feature/x", true},
		{"git checkout -b", "git checkout -b feature/x", true},
		{"git checkout -qB clustered", "git checkout -qB feature/x", true},
		{"git switch of a branch", "git switch --detach main", false},
		{"git switch --force is not a creation", "git switch --force feature/x", false},
		{"--force-create is not a checkout flag", "git checkout --force-create feature/x", false},
		{"creation flag after --", "git switch -q -- --force-create", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBranchCreation(strings.Fields(tt.command)); got != tt.want {
				t.Errorf("isBranchCreation(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// The gt-6hg7 exemption, in three parts: the role gate, the commands it
// opens, and the commands it must leave blocked. The fixture is
// newPolecatTestTown (tap_guard_polecat_paths_test.go), a hermetic
// <root>/gt/<rig>/polecats/<name>/<repo> layout with a sibling worktree, so
// "own worktree" and "someone else's" are both real directories on disk
// rather than strings this test asserts against itself.

// polecatBranchPayload builds the PreToolUse payload the harness sends for a
// Bash call whose session cwd is dir.
func polecatBranchPayload(dir, command string) string {
	return fmt.Sprintf(`{"tool_name":"Bash","cwd":%q,"tool_input":{"command":%q}}`, dir, command)
}

// TestIsPolecatSession pins the GT_ROLE-first rule. GT_POLECAT alone is not
// evidence of a polecat session: coordinators carry a stale GT_POLECAT from
// having spawned polecats, and inheriting an exemption from it is exactly how
// the live-fire probe mis-read a refinery session (gt-xy4b).
func TestIsPolecatSession(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		gtRole    string
		gtPolecat string
		want      bool
	}{
		{"GT_ROLE compound polecat", "gastown/polecats/topaz", "", true},
		{"GT_ROLE short polecat form", "gastown/topaz", "", true},
		{"GT_ROLE witness beats stale GT_POLECAT", "gastown/witness", "topaz", false},
		{"GT_ROLE witness beats stale GT_POLECAT", "gastown/witness", "topaz", false},
		{"GT_ROLE mayor beats stale GT_POLECAT", "mayor", "topaz", false},
		{"GT_ROLE crew beats stale GT_POLECAT", "gastown/crew/alice", "topaz", false},
		{"GT_ROLE unset falls back to GT_POLECAT", "", "topaz", true},
		{"neither set", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := fakeGuardProcess(map[string]string{"GT_ROLE": tt.gtRole, "GT_POLECAT": tt.gtPolecat}, "")
			if got := isPolecatSession(proc); got != tt.want {
				t.Errorf("isPolecatSession() with GT_ROLE=%q GT_POLECAT=%q = %v, want %v", tt.gtRole, tt.gtPolecat, got, tt.want)
			}
		})
	}
}

// TestRunTapGuardPRWorkflow_PolecatSessionBranchAllowed is the escape gt-6hg7
// opens: a polecat that resumed a pre-pushed branch creates a fresh local
// session branch inside its own worktree. gt done's recoverDivergedPush
// refuses the force-push for that case by construction (mol-polecat-work's
// branch reuse rebases AND adds fix commits, so patch-ids differ), and the
// alternative the block message used to name — push to main — is blocked for
// polecats by gt-ibt8, so before this exemption the session had no open route
// at all. The command is pearl's exact gt-da2x shape.
func TestRunTapGuardPRWorkflow_PolecatSessionBranchAllowed(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t).withEnv("GT_ROLE", "gastown/polecats/ruby")

	allowed := []struct {
		name    string
		cwd     string
		command string
	}{
		{"checkout -b a fresh session branch", town.worktree, `git checkout -b "polecat/ruby/gt-da2x+$(openssl rand -hex 3)"`},
		{"switch -c a fresh session branch", town.worktree, `git switch -c polecat/ruby/gt-da2x+mu6jwe92`},
		{"checkout -b with an extra flag", town.worktree, `git checkout -q -b polecat/ruby/gt-da2x+mu6jwe92`},
		{"checkout -b from a subdirectory of the worktree", filepath.Join(town.worktree, "internal"), `git checkout -b polecat/ruby/gt-da2x+mu6jwe92`},
		// The long spellings and the clustered flags are the same creation,
		// so they carry the same exemption (gt-g27m7).
		{"switch --create a fresh session branch", town.worktree, `git switch --create polecat/ruby/gt-da2x+mu6jwe92`},
		{"switch -q --create a fresh session branch", town.worktree, `git switch -q --create polecat/ruby/gt-da2x+mu6jwe92`},
		{"switch --force-create a fresh session branch", town.worktree, `git switch --force-create polecat/ruby/gt-da2x+mu6jwe92`},
		{"checkout -qB a fresh session branch", town.worktree, `git checkout -qB polecat/ruby/gt-da2x+mu6jwe92`},
		{"switch -q --create in the worktree", town.worktree, `git switch -q --create feature/x`},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			err := tapGuardPRWorkflow(strings.NewReader(polecatBranchPayload(tt.cwd, tt.command)), io.Discard, town.proc())
			if err != nil {
				t.Errorf("expected a polecat's session branch in its own worktree to be allowed, got error: %v", err)
			}
		})
	}
}

// TestEvaluatePRWorkflowGuard_PolecatBranchCreationStillBlocked is the other
// half of gt-6hg7: the exemption is narrowly scoped, so the shapes the guard
// exists to block stay blocked.
func TestEvaluatePRWorkflowGuard_PolecatBranchCreationStillBlocked(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	branch := "polecat/ruby/gt-da2x+mu6jwe92"

	tests := []struct {
		name    string
		env     []string
		cwd     string
		command string
	}{
		{
			name:    "branch creation in a sibling polecat's worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.sibling,
			command: "git checkout -b " + branch,
		},
		{
			name:    "branch creation at the rig root",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.rigRoot,
			command: "git switch -c " + branch,
		},
		// The extra-flag and long spellings are the same creation; the polecat's
		// own worktree exempts them (see the allowed table), a sibling's does not.
		{
			name:    "checkout -b with an extra flag in a sibling polecat's worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.sibling,
			command: "git checkout -q -b feature/x",
		},
		{
			name:    "switch --create at the rig root",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.rigRoot,
			command: "git switch --create feature/x",
		},
		{
			// The judge reads --force-create (gt-g27m7), so the exemption's
			// narrow scope is what keeps this blocked, not a missed spelling.
			name:    "switch --force-create at the rig root",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.rigRoot,
			command: "git switch --force-create feature/x",
		},
		{
			name:    "switch --force-create in a sibling polecat's worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.sibling,
			command: "git switch --force-create feature/x",
		},
		{
			name:    "checkout -b glued to its name in a sibling polecat's worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.sibling,
			command: "git checkout -bfeature/x",
		},
		{
			name:    "gh pr create in the polecat's own worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.worktree,
			command: "gh pr create --title foo",
		},
		{
			name:    "branch creation chained with gh pr create in the own worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.worktree,
			command: "git checkout -b " + branch + " && gh pr create --title foo",
		},
		{
			name:    "gh pr create chained before a branch creation in the own worktree",
			env:     []string{"GT_ROLE", "gastown/polecats/" + town.name},
			cwd:     town.worktree,
			command: "gh pr create --title foo && git checkout -b " + branch,
		},
		{
			name: "coordinator carrying a stale GT_POLECAT",
			env: []string{
				"GT_ROLE", "gastown/witness",
				"GT_POLECAT", town.name, // stale, from having spawned one
			},
			cwd:     town.worktree,
			command: "git checkout -b " + branch,
		},
		{
			// The doctor's live-fire probe (gt-xy4b): GT_POLECAT is set but
			// the session has no worktree — GT_POLECAT_PATH is stripped and
			// the cwd is a disposable sandbox. The probe reads a successful
			// 'git checkout -b' here as broken matcher wiring, so the
			// exemption must not fire on role alone.
			name: "live-fire probe sandbox with GT_POLECAT and no worktree",
			env: []string{
				"GT_ROLE", "",
				"GT_POLECAT", "live-fire",
				"GT_POLECAT_PATH", "",
			},
			cwd:     t.TempDir(),
			command: "git checkout -b gt-live-fire-probe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := polecatBranchPayload(tt.cwd, tt.command)
			if got := evaluatePRWorkflowGuard([]byte(payload), town.withEnv(tt.env...).proc()); got != prWorkflowBlockAgentContext {
				t.Errorf("evaluatePRWorkflowGuard(%q) in %s = %v, want prWorkflowBlockAgentContext", tt.command, tt.cwd, got)
			}
		})
	}
}

// A git global option is not a creation flag, so a leading segment that only
// switches branches behind one exempts nothing: the branch creation glued
// after it stays blocked (gt-oyvpp). The option is skipped rather than stopped
// at, so a real creation behind one keeps its exemption.
func TestEvaluatePRWorkflowGuard_GlobalOptionIsNotACreationFlag(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/polecats/"+town.name)
	branch := "polecat/ruby/gt-da2x+mu6jwe92"

	blocked := []string{
		"git -C . switch main && git checkout -b " + branch,
		"git -c core.pager=cat switch main && git checkout -b " + branch,
	}
	for _, command := range blocked {
		payload := polecatBranchPayload(town.worktree, command)
		if got := evaluatePRWorkflowGuard([]byte(payload), town.proc()); got != prWorkflowBlockAgentContext {
			t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want prWorkflowBlockAgentContext", command, got)
		}
	}

	allowed := []string{
		"git -C . checkout -b " + branch,
		"git -c core.pager=cat switch -c " + branch,
	}
	for _, command := range allowed {
		payload := polecatBranchPayload(town.worktree, command)
		if got := evaluatePRWorkflowGuard([]byte(payload), town.proc()); got != prWorkflowAllow {
			t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want prWorkflowAllow", command, got)
		}
	}
}

// TestRunTapGuardPRWorkflow_PolecatBlockedShapeShowsBanner keeps the block
// end-to-end for a shape the exemption does not cover — the guard must still
// exit 2 with the banner, not merely decide to block.
func TestRunTapGuardPRWorkflow_PolecatBlockedShapeShowsBanner(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/polecats/"+town.name)

	var stderr strings.Builder
	payload := polecatBranchPayload(town.sibling, "git checkout -b polecat/ruby/gt-da2x+mu6jwe92")
	err := tapGuardPRWorkflow(strings.NewReader(payload), &stderr, town.proc())
	if err == nil {
		t.Fatal("expected branch creation in a sibling worktree to be blocked, got nil error")
	}
	exit, ok := err.(*SilentExitError)
	if !ok || exit.Code != 2 {
		t.Fatalf("expected a silent exit 2 (BLOCK), got %T: %v", err, err)
	}
	if !strings.Contains(stderr.String(), "PR WORKFLOW BLOCKED") {
		t.Errorf("expected the PR WORKFLOW BLOCKED banner on stderr, got:\n%s", stderr.String())
	}
}

// TestPRWorkflowGuard_WrappedShapes: a launcher, assignment, group opener,
// path-qualified binary or gh flag in front of the command does not hide it
// from the guard (gt-zf2vy). The commands are only tokenized, never run.
func TestPRWorkflowGuard_WrappedShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"env assignment prefix", "GH_REPO=o/r gh pr create --title x", true},
		{"env launcher", "env X=1 gh pr create --title x", true},
		{"bash -c payload", "bash -c 'gh pr create --title x'", true},
		{"eval string", `eval "gh pr create"`, true},
		{"subshell", "( gh pr create )", true},
		{"brace group", "{ gh pr create; }", true},
		{"if then", "if true; then gh pr create; fi", true},
		{"time", "time gh pr create", true},
		{"path-qualified binary", "/usr/bin/gh pr create", true},
		{"gh -R global flag", "gh -R o/r pr create", true},

		{"gh pr view stays allowed", "gh pr view 12", false},
		{"gh pr list stays allowed", "gh pr list", false},
		{"git checkout main stays allowed", "git checkout main", false},
		{"echo of the command stays allowed", "echo gh pr create", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestPRWorkflowGuard_WrappedVariants covers the other spellings of the same
// wrappers: the branch-creation commands behind them, launcher flags, nested
// shells and the shapes that must stay allowed.
func TestPRWorkflowGuard_WrappedVariants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"git checkout -b behind an assignment", "GIT_TRACE=1 git checkout -b feature/x", true},
		{"git switch -c behind env", "env -i git switch -c feature/x", true},
		{"git checkout -b path-qualified", "/usr/bin/git checkout -b feature/x", true},
		{"git checkout -b behind -C", "git -C /tmp/repo checkout -b feature/x", true},
		{"git switch -c behind -c k=v", "git -c user.name=x switch -c feature/x", true},
		{"git checkout -b in bash -c", `bash -c "git checkout -b feature/x"`, true},
		{"git switch -c in sh -lc", `sh -lc 'git switch -c feature/x'`, true},
		{"git checkout -b behind another flag", "git checkout -q -b feature/x", true},
		{"git checkout -b glued to its name", "git checkout -bfeature/x", true},
		{"git checkout -b clustered with -q", "git checkout -qb feature/x", true},
		{"git checkout -B behind another flag", "git checkout -q -B feature/x", true},
		{"git switch -C behind another flag", "git switch -q -C feature/x", true},
		{"git checkout -b after the start point", "git checkout --track -b feature/x origin/main", true},
		{"git switch --create", "git switch --create feature/x", true},
		{"git switch --create=", "git switch --create=feature/x", true},
		{"git checkout of a branch named -B after --", "git checkout -q -- -B", false},
		{"git switch -c behind another flag", "git switch -q -c feature/x", true},
		{"git switch -c glued to its name", "git switch -cfeature/x", true},
		{"git checkout -b behind -C and another flag", "git -C /tmp/repo checkout -q -b feature/x", true},
		{"git checkout of a branch named like a flag after --", "git checkout -q -- -b", false},
		{"git switch of a path after --", "git switch -q -- -c", false},
		{"git checkout of a branch", "git checkout -q main", false},
		{"git switch of a branch", "git switch --detach main", false},
		{"git checkout --orphan stays allowed", "git checkout --orphan feature/x", false},

		{"env with a flag value", "env -u FOO gh pr create", true},
		{"env -S carries the command", `env -S "gh pr create --title x"`, true},
		{"env -S glued", `env -S'gh pr create'`, true},
		{"env after assignments", "A=1 env B=2 gh pr create", true},
		{"nice with a flag value", "nice -n 5 gh pr create", true},
		{"timeout with an operand", "timeout 30 gh pr create", true},
		{"timeout with a flag and operand", "timeout -s KILL 30 gh pr create", true},
		{"sudo with a user", "sudo -u bob gh pr create", true},
		{"exec", "exec gh pr create", true},
		{"command builtin", "command gh pr create", true},
		{"env after a double dash", "env -- gh pr create", true},
		{"negation", "! gh pr create", true},
		{"setsid", "setsid gh pr create", true},
		{"doas with a user", "doas -u bob gh pr create", true},
		{"ionice with a class", "ionice -c 3 gh pr create", true},
		{"caffeinate with a duration", "caffeinate -t 60 gh pr create", true},
		{"xargs with a flag value", "echo x | xargs -n 1 gh pr create", true},
		{"git checkout -b behind setsid", "setsid git checkout -b feature/x", true},
		{"setsid of an unrelated command", "setsid make build", false},

		{"gh --repo separate", "gh --repo o/r pr create", true},
		{"gh --repo glued", "gh --repo=o/r pr create", true},
		{"gh -R glued", "gh -Ro/r pr create", true},
		{"gh flag after pr", "gh pr -R o/r create", true},
		{"path-qualified gh with -R", "/opt/homebrew/bin/gh -R o/r pr create", true},

		{"while condition", "while ! gh pr create; do sleep 1; done", true},
		{"for body", "for i in 1 2; do gh pr create; done", true},
		{"glued subshell", "(gh pr create)", true},
		{"after cd in a subshell", "(cd /tmp && gh pr create)", true},
		{"nested bash -c", `bash -c "bash -c 'gh pr create'"`, true},
		{"bash -c behind env", `env FOO=1 bash -c 'gh pr create'`, true},
		{"eval with several words", "eval gh pr create --title x", true},
		{"sudo bash -c", `sudo bash -c 'gh pr create'`, true},
		{"bash -c in a later segment", `cd /tmp && bash -c 'gh pr create'`, true},
		{"bash -c with a heredoc body", "bash <<'EOF'\nGH_REPO=o/r gh pr create\nEOF", true},

		// A command substitution runs its body, whatever word it hangs from.
		{"assignment from a substitution", "PR_URL=$(gh pr create --fill)", true},
		{"substitution as an echo argument", `echo "$(gh pr create)"`, true},
		{"unquoted substitution argument", "echo $(gh pr create)", true},
		{"backtick substitution", "PR_URL=`gh pr create --fill`", true},
		{"branch creation in a substitution", `echo "$(git checkout -b x)"`, true},
		{"wrapped gh in a substitution", `x=$(env GH_REPO=o/r gh pr create)`, true},
		{"substitution after a separator", "cd /tmp && url=$(gh pr create)", true},
		{"nested payload in a substitution", `x=$(bash -c 'gh pr create')`, true},
		{"substitution in an unquoted heredoc body", "cat > f <<EOF\n$(gh pr create)\nEOF", true},
		{"substitution of an unrelated command", "PR_URL=$(gh pr view 12 --json url)", false},
		{"single-quoted substitution is data", `echo '$(gh pr create)'`, false},
		{"escaped substitution is data", `echo "\$(gh pr create)"`, false},
		{"substitution in a quoted heredoc body is data", "cat > f <<'EOF'\n$(gh pr create)\nEOF", false},

		// The words behind a substitution's ")" are the shell's next words:
		// "x=$(echo hi) gh pr create" assigns x and then runs gh pr create, so
		// the assignment must not swallow the command (gt-4t56h).
		{"command after a substitution on the same line", "x=$(echo hi) gh pr create", true},
		{"branch creation after a substitution on the same line", "x=$(echo hi) git checkout -b feature/x", true},
		{"unrelated gh after a substitution", "x=$(echo hi) gh pr view 12", false},
		{"unrelated command after a substitution", "x=$(date) ls -la", false},
		// The substitution stands in one word's place wherever it lands, so the
		// words behind it are judged as a command line in each position the
		// shell runs them from, and words that are not a command stay unread
		// (gt-qgya2).
		{"substitution as the command word", "$(echo hi) gh pr create", true},
		{"branch creation behind a substitution command word", "$(echo hi) git checkout -b feature/x", true},
		{"unrelated gh behind a substitution command word", "$(echo hi) gh pr view 12", false},
		{"unrelated command behind a substitution command word", "$(echo hi) ls -la", false},
		{"substitution ending a segment", "x=$(echo hi) && gh pr create", true},
		{"substitution behind a separator", "cd /tmp && x=$(echo hi) gh pr create", true},
		{"launcher behind a substitution", "x=$(echo hi) env GH_REPO=o/r gh pr create", true},
		{"substitution in a later assignment", "GH_REPO=o/r url=$(echo hi) gh pr create", true},
		{"substitution after the command's own words", `gh pr create --title "$(date)"`, true},
		{"substitution among the arguments starts no command", "echo $(date) gh pr create", false},

		// A body holding parentheses of its own still runs to its balancing ")".
		{"substitution holding a nested substitution", `x=$(gh pr create --title "$(date)")`, true},
		{"substitution holding a parenthesis in a quote", `x=$(gh pr create --title "fix (x)")`, true},
		{"substitution holding a lone quoted parenthesis", `x=$(gh pr create --title ")")`, true},
		{"substitution holding a single-quoted parenthesis", `x=$(gh pr create --title 'a)b')`, true},
		{"substitution holding an escaped parenthesis", `x=$(gh pr create --title a\)b)`, true},
		{"substitution holding a subshell", `x=$( (gh pr create) )`, true},
		{"substitution nested in a substitution", `echo "$(echo "$(gh pr create)")"`, true},
		{"backtick substitution holding parentheses", "x=`gh pr create --title \"fix (x)\"`", true},
		{"backtick body with an escaped backtick", "x=`echo \\`gh pr create\\``", true},
		{"unterminated substitution", "x=$(gh pr create --fill", true},
		{"unterminated backtick substitution", "x=`gh pr create --fill", true},
		// A body that runs to the end of the text hands its ")" over as a word
		// of its own, so the word the ")" touched is judged whole and the gh
		// behind it is seen (gt-4t56h). Each ")" of a trailing run hides a word
		// of its own, so every one of them is handed over that way (gt-qgya2).
		{"body running to the end of the text keeps its last word whole", "x=$(echo `gh pr create)", true},
		{"body running to the end of the text on two closing parens", "x=$(echo `gh pr create))", true},
		{"body running to the end of the text on three closing parens", "x=$(echo `gh pr create)))", true},
		{"unterminated backtick body on two closing parens", "x=`gh pr create))", true},
		{"substitution after a quoted dollar-paren", `echo '$(date)' "$(gh pr create)"`, true},
		{"substitution after an escaped dollar-paren", `echo \$(date) $(gh pr create)`, true},
		{"empty substitution", "x=$()", false},
		{"blank backtick substitution", "x=` `", false},
		{"substitution holding a parenthesis around an unrelated command", `x=$(gh pr view --json "a (b)")`, false},
		{"quoted parenthesis does not end a body before a later one", `x=$(echo ")") && echo $(gh pr create)`, true},
		{"substitution past the nesting cap naming gh", `x=$(bash -c "bash -c \"bash -c \\\"x=\$(gh pr create)\\\"\"")`, true},

		// Past the nesting cap the payload is unread; one that still names gh
		// or git is refused, one that does not is allowed.
		{"too deep and naming gh", `bash -c "bash -c \"bash -c \\\"bash -c 'gh pr create'\\\"\""`, true},
		{"too deep and naming neither", `bash -c "bash -c \"bash -c \\\"bash -c 'ls -la'\\\"\""`, false},

		{"env without a command", "env", false},
		{"assignment alone", "GH_REPO=o/r", false},
		{"gh pr view behind env", "GH_REPO=o/r gh pr view 12", false},
		{"gh pr list with -R", "gh -R o/r pr list", false},
		{"gh pr create-like subcommand", "gh pr created", false},
		{"gh other command", "gh -R o/r issue create", false},
		{"gh api mentioning pr create", "gh api repos/o/r/pulls -f title='pr create'", false},
		{"git checkout without -b behind env", "env X=1 git checkout main", false},
		{"git -C status", "git -C /tmp/repo status", false},
		{"git checkout -b glued to a name behind env", "env X=1 git checkout -branch x", true},
		{"bash -c of an unrelated command", `bash -c "git status"`, false},
		{"echo of a bash -c payload", `echo bash -c 'gh pr create'`, false},
		{"echo of eval", `echo eval "gh pr create"`, false},
		{"printf of a wrapped command", `printf '%s\n' "GH_REPO=o/r gh pr create"`, false},
		{"commit message naming the command", `git commit -m "env X=1 gh pr create"`, false},
		{"script file named by bash", "bash gh-pr-create.sh", false},
		{"a gh in a longer word", "github pr create", false},
		{"time of an unrelated command", "time make build", false},
		{"bash script then -c belongs to the script", "bash script.sh -c 'gh pr create'", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestPRWorkflowGuard_WrappedBlockedInAgentContext runs the wrapped shapes
// through the full decision: blocked for a polecat in its own worktree, where
// the session-branch exemption must not carry a wrapped gh pr create along,
// and still allowed when the command is not one of the three.
func TestPRWorkflowGuard_WrappedBlockedInAgentContext(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/polecats/"+town.name)
	branch := "polecat/ruby/gt-zf2vy+mvax487p"
	tooDeepGh := `bash -c "bash -c \"bash -c \\\"bash -c 'gh pr create'\\\"\""`
	tooDeepNeither := `bash -c "bash -c \"bash -c \\\"bash -c 'ls -la'\\\"\""`

	tests := []struct {
		name    string
		command string
		want    prWorkflowGuardDecision
	}{
		{"env-prefixed gh pr create", "GH_REPO=o/r gh pr create --title x", prWorkflowBlockAgentContext},
		{"bash -c gh pr create", "bash -c 'gh pr create --title x'", prWorkflowBlockAgentContext},
		{"session branch chained with an env-prefixed gh pr create", "git checkout -b " + branch + " && env X=1 gh pr create", prWorkflowBlockAgentContext},
		{"session branch chained with a bash -c gh pr create", "git checkout -b " + branch + "; bash -c 'gh pr create'", prWorkflowBlockAgentContext},
		{"session branch chained with a path-qualified gh pr create", "git switch -c " + branch + " && /usr/bin/gh pr create", prWorkflowBlockAgentContext},
		{"gh pr create assigned from a substitution", "PR_URL=$(gh pr create --fill)", prWorkflowBlockAgentContext},
		{"gh pr create in an echoed substitution", `echo "$(gh pr create)"`, prWorkflowBlockAgentContext},
		{"session branch with a substituted gh pr create", "git checkout -b " + branch + ` "$(gh pr create)"`, prWorkflowBlockAgentContext},
		{"gh pr view in a substitution", "url=$(gh pr view 12 --json url)", prWorkflowAllow},
		{"gh pr create after a substitution on the same line", "x=$(echo hi) gh pr create", prWorkflowBlockAgentContext},
		{"gh pr create behind a substitution command word", "$(echo hi) gh pr create", prWorkflowBlockAgentContext},
		{"session branch behind an env prefix keeps the exemption", "GIT_TRACE=1 git checkout -b " + branch, prWorkflowAllow},
		{"session branch behind env keeps the exemption", "env GIT_TRACE=1 git switch -c " + branch, prWorkflowAllow},
		{"gh pr view behind env", "GH_REPO=o/r gh pr view 12", prWorkflowAllow},
		{"echo of a wrapped command", "echo env X=1 gh pr create", prWorkflowAllow},
		// A payload past the nesting cap is unread, so one still naming gh is
		// refused rather than allowed through the self-filter.
		{"too deep and naming gh", tooDeepGh, prWorkflowBlockAgentContext},
		{"session branch chained with a too-deep payload naming gh", "git checkout -b " + branch + " && " + tooDeepGh, prWorkflowBlockAgentContext},
		{"too deep and naming neither", tooDeepNeither, prWorkflowAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := polecatBranchPayload(town.worktree, tt.command)
			if got := evaluatePRWorkflowGuard([]byte(payload), town.proc()); got != tt.want {
				t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// An apostrophe in a "#" comment is plain text and a "\'" inside $'...' does not
// close the string, so neither hides a live "$(gh pr create)" behind it. Quotes
// elsewhere read as before: a "$(" in a comment is still judged, and a quoted
// one stays data (gt-4vj8j).
func TestPRWorkflowSubstitutionCommentAndANSIQuote(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/polecats/"+town.name)

	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"plain substitution", "x=$(gh pr create --fill)", true},
		{"apostrophe in a comment", "# don't\nx=$(gh pr create --fill)", true},
		{"apostrophe in a trailing comment", "echo hi # don't\nx=$(gh pr create --fill)", true},
		{"comment after a separator", "true;# don't\nx=$(gh pr create --fill)", true},
		{"double quote in a comment", "# say \"hi\nx=$(gh pr create --fill)", true},
		{"backtick substitution after a comment", "# don't\nx=`gh pr create --fill`", true},
		{"escaped quote in an ANSI-C string", `echo $'it\'s' $(gh pr create --fill)`, true},
		{"substitution in a comment", "echo hi # $(gh pr create --fill)", true},
		{"apostrophe comment then bare gh pr create", "# don't\ngh pr create --fill", true},
		{"hash inside a word", `echo a#b $(gh pr create --fill)`, true},
		{"single-quoted substitution stays data", `echo '$(gh pr create --fill)'`, false},
		{"ANSI-C string hiding a substitution", `echo $'$(gh pr create)'`, false},
		{"apostrophe after the comment ended", "# note\necho 'a $(gh pr create)'", false},
		{"hash after an escaped space", `echo \ #b '$(gh pr create)'`, false},
		{"gh pr view after a comment", "# don't\nx=$(gh pr view 12)", false},
		// A heredoc body in a double-quoted substitution ends at its terminator,
		// so the ")" that closes the substitution is read and the line after it
		// is live. Its apostrophe used to open a quote that swallowed the rest
		// of the line (gt-lx8i8).
		{"gh pr create after a heredoc body in a double-quoted substitution", "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"; gh pr create --fill", true},
		{"quoted gh pr create after a heredoc body in a double-quoted substitution is text", "x=\"$(cat <<'EOF'\nit's a note\nEOF\n)\"; echo \"run gh pr create later\"", false},
		{"gh pr create in a heredoc body in a double-quoted substitution is data", "x=\"$(cat <<'EOF'\nit's gh pr create --fill\nEOF\n)\"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
			want := prWorkflowAllow
			if tt.want {
				want = prWorkflowBlockAgentContext
			}
			payload := polecatBranchPayload(town.worktree, tt.command)
			if got := evaluatePRWorkflowGuard([]byte(payload), town.proc()); got != want {
				t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want %v", tt.command, got, want)
			}
		})
	}
}

// TestPRWorkflowGuard_SwitchAbbreviatedCreate pins the abbreviations of
// switch's --create and --force-create (gt-osc72). git's option parser takes
// any unambiguous prefix, so "git switch --cre x" creates a branch (verified on
// git 2.42.0) and the guard blocks it for a non-polecat agent. The guard reads
// every prefix of the option as the creation, the prefixes one version refuses
// as ambiguous included — on git 2.42.0 "--c" is ambiguous with --conflict and
// "--f" and "--fo" with --force, so that version creates no branch from them
// while a version whose option set leaves them unambiguous does (gt-19s3v).
// "--force" exact-matches its own option, and --orphan, --detach and
// --discard-changes are not creations.
func TestPRWorkflowGuard_SwitchAbbreviatedCreate(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/crew/sloan")

	tests := []struct {
		name    string
		command string
		want    prWorkflowGuardDecision
	}{
		{"--cre", "git switch --cre feature/x", prWorkflowBlockAgentContext},
		{"--force-cr", "git switch --force-cr feature/x", prWorkflowBlockAgentContext},
		{"--cr", "git switch --cr feature/x", prWorkflowBlockAgentContext},
		{"--crea=", "git switch --crea=feature/x", prWorkflowBlockAgentContext},
		{"--force-", "git switch --force- feature/x", prWorkflowBlockAgentContext},
		{"--force-c=", "git switch --force-c=feature/x", prWorkflowBlockAgentContext},
		{"--force-creat behind another flag", "git switch -q --force-creat feature/x", prWorkflowBlockAgentContext},
		{"--cre behind git -C", "git -C /tmp/repo switch --cre feature/x", prWorkflowBlockAgentContext},
		{"--cre in bash -c", `bash -c "git switch --cre feature/x"`, prWorkflowBlockAgentContext},
		{"--c, ambiguous on 2.42", "git switch --c feature/x", prWorkflowBlockAgentContext},
		{"--f, ambiguous on 2.42", "git switch --f feature/x", prWorkflowBlockAgentContext},
		{"--fo, ambiguous on 2.42", "git switch --fo feature/x", prWorkflowBlockAgentContext},
		{"--for, ambiguous on 2.42", "git switch --for feature/x", prWorkflowBlockAgentContext},
		{"--forc, ambiguous on 2.42", "git switch --forc feature/x", prWorkflowBlockAgentContext},
		{"--force is its own option", "git switch --force feature/x", prWorkflowAllow},
		{"--conflict", "git switch --conflict=merge feature/x", prWorkflowAllow},
		{"--orphan", "git switch --orphan feature/x", prWorkflowAllow},
		{"--or", "git switch --or feature/x", prWorkflowAllow},
		{"--detach", "git switch --detach main", prWorkflowAllow},
		{"--discard-changes", "git switch --discard-changes main", prWorkflowAllow},
		{"a longer word that only starts like --create", "git switch --createx feature/x", prWorkflowAllow},
		{"--cre after --", "git switch -q -- --cre", prWorkflowAllow},
		{"--cre is not a checkout flag", "git checkout --cre feature/x", prWorkflowAllow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := polecatBranchPayload(town.worktree, tt.command)
			if got := evaluatePRWorkflowGuard([]byte(payload), town.proc()); got != tt.want {
				t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// A polecat's exemption reads the same abbreviations the judge does, so it can
// open its own worktree's session branch with "git switch --cre" and is still
// blocked at the rig root (gt-osc72).
func TestPRWorkflowGuard_SwitchAbbreviatedCreatePolecat(t *testing.T) {
	t.Parallel()
	town := newPolecatTestTown(t)
	town = town.withEnv("GT_ROLE", "gastown/polecats/"+town.name)
	branch := "polecat/ruby/gt-osc72+mvbikfnl"

	for _, flag := range []string{"--cre", "--force-cr"} {
		command := "git switch " + flag + " " + branch
		if got := evaluatePRWorkflowGuard([]byte(polecatBranchPayload(town.worktree, command)), town.proc()); got != prWorkflowAllow {
			t.Errorf("own worktree %q = %v, want allow", command, got)
		}
		if got := evaluatePRWorkflowGuard([]byte(polecatBranchPayload(town.rigRoot, command)), town.proc()); got != prWorkflowBlockAgentContext {
			t.Errorf("rig root %q = %v, want block", command, got)
		}
	}
}

// A command substitution that expands to gh or git, or sits where an argument
// goes, is judged as the command it may expand to (gt-n2gt8). An empty
// expansion vanishes ("gh pr $(echo) create" is gh pr create); one that names
// gh or git leaves the words behind it as that command's arguments; one in a
// subcommand slot may be the subcommand ("gh pr $(echo create)").
func TestPRWorkflowSubstitutionInArgumentSlot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"empty expansion between gh's subcommands", "gh pr $(echo) create", true},
		{"empty expansion before gh's subcommands", "gh $(echo) pr create --fill", true},
		{"empty expansion after gh pr create", "gh pr create $(echo) --fill", true},
		{"empty expansion before git's subcommand", "git $(echo) checkout -b x", true},
		{"empty expansion between git's subcommand and flag", "git checkout $(echo) -b x", true},
		{"backtick expansion in an argument slot", "gh pr `echo` create", true},
		{"expansion naming gh", "$(echo gh) pr create --fill", true},
		{"expansion naming git", "$(echo git) checkout -b x", true},
		{"expansion naming git behind a launcher", "env A=1 $(echo git) switch -c x", true},
		{"expansion naming gh with the flag first", "$(which gh) -R o/r pr create", true},
		{"expansion as the value of git -C keeps the subcommand", "git -C $(pwd) checkout -b x", true},
		{"expansion as the value of gh -R keeps the subcommand", "gh -R $(echo o/r) pr create", true},
		{"expansion as the value of git -C, no creation", "git -C $(pwd) checkout main", false},
		{"expansion as a gh argument, no creation", "gh pr view $(echo 12)", false},
		{"expansion naming the gh subcommand", "gh pr $(echo create) --fill", true},
		{"expansion naming the gh subcommand, backticks", "gh pr `echo create`", true},
		{"expansion naming the gh group", "gh $(echo pr) create", true},
		{"expansion naming git's subcommand", "git $(echo checkout) -b x", true},
		{"expansion naming git's subcommand, switch", "git -C d $(echo) $(echo switch) -c x", true},
		{"expansion naming git's subcommand, no creation", "git $(echo checkout) main", false},
		{"expansion in the pull request number slot", "gh pr view $(echo 12)", false},
		{"expansion after a gh subcommand that is not create", "gh pr list $(echo create)", false},
		{"expansion as a git argument, no creation", "git checkout $(git rev-parse HEAD)", false},
		{"expansion before an unrelated subcommand", "$(echo gh) pr view 12", false},
		{"expansion before an unrelated command", "$(echo hi) ls -la", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := matchesPRWorkflowCommand(tt.command); got != tt.want {
				t.Errorf("matchesPRWorkflowCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}
