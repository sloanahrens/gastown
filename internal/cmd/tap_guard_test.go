package cmd

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIsPRCreateCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"gh pr create", "gh pr create --title foo", true},
		{"gh pr create mixed case", "GH PR CREATE --title foo", true},
		{"chained after unrelated segment", "echo hi && gh pr create --title foo", true},
		{"git checkout -b", "git checkout -b temp origin/branch", false},
		{"git switch -c", "git switch -c temp origin/branch", false},
		{"unrelated", "echo hello", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPRCreateCommand(tt.command); got != tt.want {
				t.Errorf("isPRCreateCommand(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

// gt-cyz8: both pr-workflow exemptions must be keyed on the leading-command
// branch-creation shape, not the old line-wide isFeatureBranchCommand (now
// removed). The two matched the same leading-command shapes but differed on
// the chained case — an exemption's escape was a "git checkout -b" glued
// after a "gh pr create" on one line, which line-wide containment matches but
// a leading-command match does not.
func TestIsLeadingBranchCreation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"refinery rehearsal checkout", "git checkout -b temp origin/polecat/topaz+abc123", true},
		{"polecat session branch", "git checkout -b polecat/pearl/gt-da2x+mu6jwe92", true},
		{"switch -c", "git switch -c temp origin/branch", true},
		{"checkout -b with extra flag", "git checkout -q -b temp origin/branch", true},
		{"gh pr create", "gh pr create --title foo", false},
		{"plain checkout (no -b)", "git checkout main", false},
		{"plain switch (no -c)", "git switch main", false},
		{"no git", "checkout -b temp", false},
		{"empty", "", false},
		{"chained after unrelated segment", "echo hi && git checkout -b temp origin/branch", false},
		{"chained after pr create", "gh pr create --title foo && git checkout -b temp", false},
		{"chained before pr create", "git checkout -b temp && gh pr create --title foo", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLeadingBranchCreation(tt.command); got != tt.want {
				t.Errorf("isLeadingBranchCreation(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestIsRefineryRole(t *testing.T) {
	tests := []struct {
		name       string
		gtRefinery string
		gtRole     string
		want       bool
	}{
		{"GT_REFINERY set", "1", "", true},
		{"GT_ROLE refinery compound", "", "gastown/refinery", true},
		{"GT_ROLE polecat", "", "gastown/polecats/topaz", false},
		{"neither set", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_REFINERY", tt.gtRefinery)
			t.Setenv("GT_ROLE", tt.gtRole)
			if got := isRefineryRole(); got != tt.want {
				t.Errorf("isRefineryRole() = %v, want %v", got, tt.want)
			}
		})
	}
}

// withStdin replaces os.Stdin with content for the duration of fn, restoring
// the original afterward. Needed because runTapGuardPRWorkflow reads the
// command straight off os.Stdin (Claude Code hook protocol).
func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	done := make(chan struct{})
	go func() {
		_, _ = io.WriteString(w, content)
		w.Close()
		close(done)
	}()
	fn()
	<-done
}

func TestRunTapGuardPRWorkflow_RefineryRehearsalExemption(t *testing.T) {
	// gt-r2xm: mol-refinery-patrol step 1's mandated merge rehearsal
	// ("git checkout -b temp origin/<branch>") must be allowed under the
	// refinery role even though it matches the same "if": "Bash(git
	// checkout -b*)" hook pattern that blocks feature branches everywhere
	// else.
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"git checkout -b temp origin/polecat/topaz+abc123"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err != nil {
		t.Errorf("expected refinery merge rehearsal to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPRWorkflow_RefineryStillBlocksPRCreate(t *testing.T) {
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title foo"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected gh pr create to remain blocked for the refinery role, got nil error")
	}
}

func TestRunTapGuardPRWorkflow_NonRefineryStillBlocksCheckout(t *testing.T) {
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")
	t.Setenv("GT_POLECAT", "topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"git checkout -b temp origin/main"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected non-refinery feature-branch checkout to remain blocked, got nil error")
	}
}

// TestRunTapGuardPRWorkflow_BlocksNewlineSeparatedCommand pins the gt-3j8u
// bypass at the guard boundary rather than at the matcher: the reported
// fail-open was a multi-line Bash call whose blocked shape sat on a later
// line ("cd /tmp" + newline + "gh pr create ..."), read from the hook
// payload the harness actually sends. JSON's \n escape decodes to the real
// newline the tokenizer has to treat as a command separator.
func TestRunTapGuardPRWorkflow_BlocksNewlineSeparatedCommand(t *testing.T) {
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")
	t.Setenv("GT_POLECAT", "topaz")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"cd /tmp\ngit checkout -b feature/x"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected a blocked shape on a later line of a multi-line command to be blocked, got nil error")
	}
}

// The following two tests pin the composition of the refinery exemption
// (gt-r2xm) with the command self-filter (gt-pjeh): the exemption only
// fires for the exact feature-branch shape, and the self-filter's fail-shut
// fallback on unreadable stdin (gt-wisp-52y4) still applies under the
// refinery role — the exemption must not widen into "refinery role always
// passes."
func TestRunTapGuardPRWorkflow_RefineryUnrelatedCommandAllowed(t *testing.T) {
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err != nil {
		t.Errorf("expected unrelated command to be allowed for refinery role via self-filter, got error: %v", err)
	}
}

func TestRunTapGuardPRWorkflow_RefineryEmptyStdinStillBlocks(t *testing.T) {
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	var err error
	withStdin(t, "", func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected refinery role with empty/unparsable stdin to still be blocked (fail closed), got nil error")
	}
}

// withUnreadableStdin replaces os.Stdin with a write-only file for the
// duration of fn, so io.ReadAll fails where withStdin's pipe would merely
// yield an empty payload. Reading a write-only fd returns EBADF; no payload
// content can reach the guard at all.
func withUnreadableStdin(t *testing.T, fn func()) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "stdin"), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatalf("opening write-only stdin: %v", err)
	}
	defer f.Close()

	origStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = origStdin }()

	fn()
}

// gt-hift: an io.ReadAll error returned nil straight out of the guard,
// failing it open — the one path that let a real PR command through in
// agent context, since a read error was indistinguishable from "allow" at
// the call site. An unreadable stdin is the same "we don't know what
// command this is" condition gt-wisp-52y4 gives the fail-closed fallback,
// so it must reach the unconditional context/origin check.
func TestRunTapGuardPRWorkflow_UnreadableStdinStillBlocks(t *testing.T) {
	t.Setenv("GT_POLECAT", "topaz")

	var err error
	withUnreadableStdin(t, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected unreadable stdin in agent context to fail closed (block), got nil error")
	}
}

// TestEvaluatePRWorkflowGuard_UnknownInputFailsClosed pins the decision
// itself for every shape of "no command", nil included — the value
// runTapGuardPRWorkflow substitutes on a read error. Unknown input must
// fall through to the context check, and must not widen the refinery
// exemption into "refinery always passes" (gt-r2xm composed with
// gt-wisp-52y4).
func TestEvaluatePRWorkflowGuard_UnknownInputFailsClosed(t *testing.T) {
	t.Setenv("GT_POLECAT", "topaz")
	t.Setenv("GT_REFINERY", "")
	t.Setenv("GT_ROLE", "gastown/polecats/topaz")

	unknown := []struct {
		name  string
		input []byte
	}{
		{"nil input (unreadable stdin)", nil},
		{"empty payload", []byte("")},
		{"payload with no command", []byte("not json")},
	}
	for _, tt := range unknown {
		t.Run(tt.name, func(t *testing.T) {
			if got := evaluatePRWorkflowGuard(tt.input); got != prWorkflowBlockAgentContext {
				t.Errorf("evaluatePRWorkflowGuard(%q) = %v, want prWorkflowBlockAgentContext (unknown input fails closed)", tt.input, got)
			}
		})
	}

	t.Run("refinery role does not exempt unknown input", func(t *testing.T) {
		t.Setenv("GT_REFINERY", "1")
		if got := evaluatePRWorkflowGuard(nil); got != prWorkflowBlockAgentContext {
			t.Errorf("evaluatePRWorkflowGuard(nil) under GT_REFINERY = %v, want prWorkflowBlockAgentContext", got)
		}
	})

	t.Run("known unrelated command is still allowed", func(t *testing.T) {
		hookInput := []byte(`{"tool_name":"Bash","tool_input":{"command":"ls -la"}}`)
		if got := evaluatePRWorkflowGuard(hookInput); got != prWorkflowAllow {
			t.Errorf("evaluatePRWorkflowGuard(unrelated command) = %v, want prWorkflowAllow", got)
		}
	})
}

// gt-cyz8: the refinery exemption (gt-r2xm) was keyed on isFeatureBranchCommand,
// which matches "git checkout -b" ANYWHERE on the line, while the "gh pr create
// stays blocked" guard was anchored differently. Asymmetric matchers let a
// "gh pr create ... && git checkout -b temp" chain fire the exemption (feature
// branch found) while the PR-create guard never matched, so the exemption let
// the chained PR create through. The exemption is now keyed on
// isLeadingBranchCreation — anchored to the command's first word, exactly as
// the hook "if" glob that routes the command here is anchored — so the escape
// is closed.
func TestRunTapGuardPRWorkflow_RefineryChainedPRCreateStillBlocks(t *testing.T) {
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --title foo && git checkout -b temp origin/main"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err == nil {
		t.Error("expected chained 'gh pr create && git checkout -b' to remain blocked for the refinery role, got nil error")
	}
}

// The reverse chain must keep working exactly as gt-r2xm intended: the
// rehearsal checkout first is exempt even though a gh pr create follows on
// the same line — the "if" glob (Bash(git checkout -b*)) anchors to the
// command's first word, so that line's routing decision was always the
// checkout, and the exemption honors that.
func TestRunTapGuardPRWorkflow_RefineryRehearsalFirstStillExempt(t *testing.T) {
	t.Setenv("GT_REFINERY", "1")
	t.Setenv("GT_ROLE", "gastown/refinery")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"git checkout -b temp origin/main && gh pr create --title foo"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPRWorkflow(tapGuardPRWorkflowCmd, nil)
	})
	if err != nil {
		t.Errorf("expected rehearsal-first chain to stay exempt for the refinery role, got error: %v", err)
	}
}
