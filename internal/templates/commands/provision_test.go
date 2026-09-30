package commands

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestBuildCommand_Claude(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	body, err := bodiesFS.ReadFile("bodies/done.md")
	if err != nil {
		t.Fatalf("reading the embedded done body: %v", err)
	}
	text := string(body)
	const calmWait = "`gt done` runs the local gate itself (lint, build and the unit tier of the tests; " +
		"no container slot), which can take several minutes. That is normal. Do not interrupt it " +
		"and do not close the bead."
	if !strings.Contains(text, calmWait) {
		t.Error("the /done body lacks the gt done wait sentence")
	}
	for _, want := range []string{
		"Never script a retry around `gt done`",
		"`gt escalate -s medium`",
		"No flag skips the gate.",
		"read the container-gate rule in `docs/reference.md`",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the /done body lacks %q", want)
		}
	}
}

func TestProvisionForAndMissingFor(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	configDir := getAgentConfigDir("claude")
	if configDir == "" {
		t.Fatal("claude preset has no config dir")
	}
	if !IsKnownAgent("Claude") {
		t.Error("IsKnownAgent(Claude) = false, want true (case-insensitive)")
	}

	if got := MissingFor(ws, "claude"); !slices.Equal(got, Names()) {
		t.Fatalf("MissingFor(empty) = %v, want all %v", got, Names())
	}

	// A pre-existing command file is never overwritten.
	dir := filepath.Join(ws, configDir, "commands")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "handoff.md")
	if err := os.WriteFile(custom, []byte("mine"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ProvisionFor(ws, "CLAUDE"); err != nil {
		t.Fatalf("ProvisionFor: %v", err)
	}
	if got := MissingFor(ws, "claude"); len(got) != 0 {
		t.Errorf("MissingFor after provision = %v, want none", got)
	}
	if data, err := os.ReadFile(custom); err != nil || string(data) != "mine" {
		t.Errorf("handoff.md = %q, %v; want the pre-existing content kept", data, err)
	}
	want, err := BuildCommand(*FindByName("review"), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "review.md")); err != nil || string(data) != want {
		t.Errorf("review.md = %q, %v; want BuildCommand output", data, err)
	}
}

func TestProvisionForUnknownAgent(t *testing.T) {
	t.Parallel()
	ws := t.TempDir()
	if err := ProvisionFor(ws, "no-such-agent"); err == nil {
		t.Error("ProvisionFor(unknown) = nil, want an error")
	}
	if got := MissingFor(ws, "no-such-agent"); got != nil {
		t.Errorf("MissingFor(unknown) = %v, want nil", got)
	}
	if IsKnownAgent("no-such-agent") {
		t.Error("IsKnownAgent(no-such-agent) = true")
	}
	if FindByName("no-such-command") != nil {
		t.Error("FindByName(no-such-command) != nil")
	}
	if _, err := BuildCommand(Command{Name: "no-such-body"}, "claude"); err == nil {
		t.Error("BuildCommand with no body = nil error")
	}
}

// The /done body is read while the polecat writes its last commit; it must
// carry the no-attribution rule (gt-v4ssj.10).
func TestDoneBodyForbidsAIAttribution(t *testing.T) {
	t.Parallel()
	body, err := bodiesFS.ReadFile("bodies/done.md")
	if err != nil {
		t.Fatalf("reading the embedded done body: %v", err)
	}
	const want = "NO\nCo-Authored-By trailer, no AI attribution anywhere."
	if !strings.Contains(string(body), want) {
		t.Errorf("the /done body lacks %q", want)
	}
}
