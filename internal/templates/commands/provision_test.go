package commands

import (
	"slices"
	"strings"
	"testing"
)

func TestBuildCommand_Claude(t *testing.T) {
	cmd := FindByName("handoff")
	if cmd == nil {
		t.Fatal("handoff command not found")
	}
	content, err := BuildCommand(*cmd, "claude")
	if err != nil {
		t.Fatalf("BuildCommand failed: %v", err)
	}

	// Check frontmatter
	if !strings.Contains(content, "description: Hand off to fresh session") {
		t.Error("missing description")
	}
	if !strings.Contains(content, "allowed-tools: Bash(gt handoff:*)") {
		t.Error("missing allowed-tools for Claude")
	}
	if !strings.Contains(content, "argument-hint: [message]") {
		t.Error("missing argument-hint for Claude")
	}

	// Check body
	if !strings.Contains(content, "$ARGUMENTS") {
		t.Error("missing $ARGUMENTS in body")
	}
	if strings.Contains(content, "Ready to hand off?") {
		t.Error("Claude handoff command should not require an interactive confirmation prompt")
	}
	if !strings.Contains(content, "gt handoff -y") {
		t.Error("Claude handoff command should use gt handoff -y")
	}
}

func TestBuildCommand_OpenCode(t *testing.T) {
	cmd := FindByName("handoff")
	if cmd == nil {
		t.Fatal("handoff command not found")
	}
	content, err := BuildCommand(*cmd, "opencode")
	if err != nil {
		t.Fatalf("BuildCommand failed: %v", err)
	}

	// Check frontmatter - only description, no Claude-specific fields
	if !strings.Contains(content, "description: Hand off to fresh session") {
		t.Error("missing description")
	}
	if strings.Contains(content, "allowed-tools") {
		t.Error("OpenCode should not have allowed-tools")
	}
	if strings.Contains(content, "argument-hint") {
		t.Error("OpenCode should not have argument-hint")
	}

	// Check body
	if !strings.Contains(content, "$ARGUMENTS") {
		t.Error("missing $ARGUMENTS in body")
	}
}

func TestBuildCommand_Copilot(t *testing.T) {
	cmd := FindByName("handoff")
	if cmd == nil {
		t.Fatal("handoff command not found")
	}
	content, err := BuildCommand(*cmd, "copilot")
	if err != nil {
		t.Fatalf("BuildCommand failed: %v", err)
	}

	// Check frontmatter - only description, no Claude-specific fields
	if !strings.Contains(content, "description: Hand off to fresh session") {
		t.Error("missing description")
	}
	if strings.Contains(content, "allowed-tools") {
		t.Error("Copilot should not have allowed-tools")
	}
	if strings.Contains(content, "argument-hint") {
		t.Error("Copilot should not have argument-hint")
	}

	// Check body
	if !strings.Contains(content, "$ARGUMENTS") {
		t.Error("missing $ARGUMENTS in body")
	}
}

func TestBuildCommand_Review_Claude(t *testing.T) {
	cmd := FindByName("review")
	if cmd == nil {
		t.Fatal("review command not found")
	}
	content, err := BuildCommand(*cmd, "claude")
	if err != nil {
		t.Fatalf("BuildCommand failed: %v", err)
	}

	// Check frontmatter
	if !strings.Contains(content, "description: Review code changes with structured grading") {
		t.Error("missing description")
	}
	if !strings.Contains(content, "allowed-tools:") {
		t.Error("missing allowed-tools for Claude")
	}
	if !strings.Contains(content, "argument-hint:") {
		t.Error("missing argument-hint for Claude")
	}

	// Check body
	if !strings.Contains(content, "$ARGUMENTS") {
		t.Error("missing $ARGUMENTS in body")
	}
	if !strings.Contains(content, "CRITICAL") {
		t.Error("missing CRITICAL severity in body")
	}
	if !strings.Contains(content, "Grade") {
		t.Error("missing Grade in body")
	}
}

func TestNames(t *testing.T) {
	names := Names()
	if len(names) < 2 {
		t.Errorf("expected at least 2 commands, got %d", len(names))
	}
	if !slices.Contains(names, "handoff") {
		t.Error("missing handoff command")
	}
	if !slices.Contains(names, "review") {
		t.Error("missing review command")
	}
}

// The /done command body is the second text the gt-7dxw ruling names: the
// guidance a polecat reads when it deliberately reaches for `gt done`. It has
// to carry both the calm-wait sentence and the rule that a gate failure is
// escalated rather than retried, because the /done body is short enough to be
// read in full at exactly the moment the polecat is deciding whether to wait
// or to improvise.
func TestDoneBodyCarriesTheSlotLoopRule(t *testing.T) {
	body, err := bodiesFS.ReadFile("bodies/done.md")
	if err != nil {
		t.Fatalf("reading the embedded done body: %v", err)
	}
	text := string(body)
	const calmWait = "`gt done` waits for the container-gate slot before it runs the container suites, " +
		"printing a `still waiting for the container-gate slot …` line every couple of minutes while " +
		"it does. That is normal. Do not interrupt it, do not close the bead, do not retry. It gives " +
		"up with a slot-acquire timeout once the cap expires."
	if !strings.Contains(text, calmWait) {
		t.Error("the /done body lacks the slot-wait sentence")
	}
	for _, want := range []string{
		"Never poll the slot, and never script a retry around `gt done`",
		"`gt escalate -s medium`",
		"--skip-verify",
		"read the container-gate rule in `docs/reference.md`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the /done body lacks %q", want)
		}
	}
}
