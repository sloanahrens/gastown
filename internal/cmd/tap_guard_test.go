package cmd

import (
	"io"
	"os"
	"testing"
)

func TestIsPRCreateCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"gh pr create", "gh pr create --title foo", true},
		{"gh pr create mixed case", "GH PR CREATE --title foo", true},
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

func TestIsFeatureBranchCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"refinery rehearsal checkout", "git checkout -b temp origin/polecat/topaz+abc123", true},
		{"switch -c", "git switch -c temp origin/branch", true},
		{"checkout -b with extra flag", "git checkout -q -b temp origin/branch", true},
		{"gh pr create", "gh pr create --title foo", false},
		{"plain checkout (no -b)", "git checkout main", false},
		{"plain switch (no -c)", "git switch main", false},
		{"no git", "checkout -b temp", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFeatureBranchCommand(tt.command); got != tt.want {
				t.Errorf("isFeatureBranchCommand(%q) = %v, want %v", tt.command, got, tt.want)
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
