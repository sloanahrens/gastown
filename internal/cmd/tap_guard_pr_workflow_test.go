package cmd

import (
	"fmt"
	"path/filepath"
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
		{"leading whitespace", "  git checkout -b feature/x", true},
		{"unrelated command", "ls -la", false},
		{"unrelated git command", "git status", false},
		{"git checkout without -b", "git checkout main", false},
		{"empty command", "", false},
		{"compound command, blocked shape in later segment", "cd foo && git checkout -b x", true},
		{"compound command, blocked shape first", "gh pr create --title foo && echo done", true},
		{"compound command with pipe", "echo x | gh pr create --title foo", true},
		{"compound command, no blocked segment", "cd foo && git status", false},
		{"different flag not mistaken for -b", "git checkout -branch feature/x", false},

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
	tests := []struct {
		name      string
		gtRole    string
		gtPolecat string
		want      bool
	}{
		{"GT_ROLE compound polecat", "gastown/polecats/topaz", "", true},
		{"GT_ROLE short polecat form", "gastown/topaz", "", true},
		{"GT_ROLE witness beats stale GT_POLECAT", "gastown/witness", "topaz", false},
		{"GT_ROLE refinery beats stale GT_POLECAT", "gastown/refinery", "topaz", false},
		{"GT_ROLE mayor beats stale GT_POLECAT", "mayor", "topaz", false},
		{"GT_ROLE crew beats stale GT_POLECAT", "gastown/crew/alice", "topaz", false},
		{"GT_ROLE unset falls back to GT_POLECAT", "", "topaz", true},
		{"neither set", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_ROLE", tt.gtRole)
			t.Setenv("GT_POLECAT", tt.gtPolecat)
			if got := isPolecatSession(); got != tt.want {
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
	town := newPolecatTestTown(t)
	t.Setenv("GT_ROLE", "gastown/polecats/"+town.name)

	allowed := []struct {
		name    string
		cwd     string
		command string
	}{
		{"checkout -b a fresh session branch", town.worktree, `git checkout -b "polecat/ruby/gt-da2x+$(openssl rand -hex 3)"`},
		{"switch -c a fresh session branch", town.worktree, `git switch -c polecat/ruby/gt-da2x+mu6jwe92`},
		{"checkout -b with an extra flag", town.worktree, `git checkout -q -b polecat/ruby/gt-da2x+mu6jwe92`},
		{"checkout -b from a subdirectory of the worktree", filepath.Join(town.worktree, "internal"), `git checkout -b polecat/ruby/gt-da2x+mu6jwe92`},
	}
	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			withStdin(t, polecatBranchPayload(tt.cwd, tt.command), func() {
				err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
			})
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
	town := newPolecatTestTown(t)
	branch := "polecat/ruby/gt-da2x+mu6jwe92"

	tests := []struct {
		name    string
		setup   func(t *testing.T)
		cwd     string
		command string
	}{
		{
			name:    "branch creation in a sibling polecat's worktree",
			setup:   func(t *testing.T) { t.Setenv("GT_ROLE", "gastown/polecats/"+town.name) },
			cwd:     town.sibling,
			command: "git checkout -b " + branch,
		},
		{
			name:    "branch creation at the rig root",
			setup:   func(t *testing.T) { t.Setenv("GT_ROLE", "gastown/polecats/"+town.name) },
			cwd:     town.rigRoot,
			command: "git switch -c " + branch,
		},
		{
			name:    "gh pr create in the polecat's own worktree",
			setup:   func(t *testing.T) { t.Setenv("GT_ROLE", "gastown/polecats/"+town.name) },
			cwd:     town.worktree,
			command: "gh pr create --title foo",
		},
		{
			name:    "branch creation chained with gh pr create in the own worktree",
			setup:   func(t *testing.T) { t.Setenv("GT_ROLE", "gastown/polecats/"+town.name) },
			cwd:     town.worktree,
			command: "git checkout -b " + branch + " && gh pr create --title foo",
		},
		{
			name:    "gh pr create chained before a branch creation in the own worktree",
			setup:   func(t *testing.T) { t.Setenv("GT_ROLE", "gastown/polecats/"+town.name) },
			cwd:     town.worktree,
			command: "gh pr create --title foo && git checkout -b " + branch,
		},
		{
			name: "coordinator carrying a stale GT_POLECAT",
			setup: func(t *testing.T) {
				t.Setenv("GT_ROLE", "gastown/witness")
				t.Setenv("GT_POLECAT", town.name) // stale, from having spawned one
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
			setup: func(t *testing.T) {
				t.Setenv("GT_ROLE", "")
				t.Setenv("GT_POLECAT", "live-fire")
				t.Setenv("GT_POLECAT_PATH", "")
			},
			cwd:     t.TempDir(),
			command: "git checkout -b gt-live-fire-probe",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup(t)
			payload := polecatBranchPayload(tt.cwd, tt.command)
			if got := evaluatePRWorkflowGuard([]byte(payload)); got != prWorkflowBlockAgentContext {
				t.Errorf("evaluatePRWorkflowGuard(%q) in %s = %v, want prWorkflowBlockAgentContext", tt.command, tt.cwd, got)
			}
		})
	}
}

// TestRunTapGuardPRWorkflow_PolecatBlockedShapeShowsBanner keeps the block
// end-to-end for a shape the exemption does not cover — the guard must still
// exit 2 with the banner, not merely decide to block.
func TestRunTapGuardPRWorkflow_PolecatBlockedShapeShowsBanner(t *testing.T) {
	town := newPolecatTestTown(t)
	t.Setenv("GT_ROLE", "gastown/polecats/"+town.name)

	var err error
	withStdin(t, polecatBranchPayload(town.sibling, "git checkout -b polecat/ruby/gt-da2x+mu6jwe92"), func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Fatal("expected branch creation in a sibling worktree to be blocked, got nil error")
	}
	exit, ok := err.(*SilentExitError)
	if !ok || exit.Code != 2 {
		t.Fatalf("expected a silent exit 2 (BLOCK), got %T: %v", err, err)
	}
}
