package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestStartSession_RequiresSessionID(t *testing.T) {
	_, err := StartSession(nil, SessionConfig{
		WorkDir: "/tmp",
		Role:    "polecat",
	})
	if err == nil {
		t.Fatal("expected error for missing SessionID")
	}
	if err.Error() != "SessionID is required" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStartSession_RequiresWorkDir(t *testing.T) {
	_, err := StartSession(nil, SessionConfig{
		SessionID: "gt-test",
		Role:      "polecat",
	})
	if err == nil {
		t.Fatal("expected error for missing WorkDir")
	}
	if err.Error() != "WorkDir is required" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStartSession_RequiresRole(t *testing.T) {
	_, err := StartSession(nil, SessionConfig{
		SessionID: "gt-test",
		WorkDir:   "/tmp",
	})
	if err == nil {
		t.Fatal("expected error for missing Role")
	}
	if err.Error() != "Role is required" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBuildPrompt_BeaconOnly(t *testing.T) {
	cfg := SessionConfig{
		Beacon: BeaconConfig{
			Recipient: "boot",
			Sender:    "daemon",
			Topic:     "triage",
		},
	}
	prompt := buildPrompt(cfg)
	if prompt == "" {
		t.Fatal("expected non-empty prompt")
	}
	if !contains(prompt, "[GAS TOWN]") {
		t.Errorf("prompt should contain beacon: %s", prompt)
	}
}

func TestBuildPrompt_WithInstructions(t *testing.T) {
	cfg := SessionConfig{
		Beacon: BeaconConfig{
			Recipient: "boot",
			Sender:    "daemon",
			Topic:     "triage",
		},
		Instructions: "Run gt boot triage now.",
	}
	prompt := buildPrompt(cfg)
	if !contains(prompt, "Run gt boot triage now.") {
		t.Errorf("prompt should contain instructions: %s", prompt)
	}
	if !contains(prompt, "[GAS TOWN]") {
		t.Errorf("prompt should contain beacon: %s", prompt)
	}
}

func TestBuildCommand_DefaultAgent(t *testing.T) {
	cfg := SessionConfig{
		Role:     "boot",
		TownRoot: "/tmp/town",
	}
	cmd, err := buildCommand(cfg, "test prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd == "" {
		t.Fatal("expected non-empty command")
	}
}

// A dog's kennel is reachable only through its name, so the startup command
// must carry GT_DOG_NAME — that is what lets the spawn path resolve the dog's
// per-agent system prompt file and add --append-system-prompt-file. Session
// config (not the config package's renderer) is all this test needs: the file
// already exists here, so no renderer has to be installed (gt-h7e5).
func TestBuildCommand_DogCarriesItsName(t *testing.T) {
	town := t.TempDir()
	path := config.SystemPromptFilePath("dog", town, "", "alpha")
	if path == "" {
		t.Fatal("dog must have a per-agent system prompt path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("dog role text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd, err := buildCommand(SessionConfig{
		SessionID: "hq-dog-alpha",
		WorkDir:   filepath.Join(town, "deacon", "dogs", "alpha"),
		Role:      "dog",
		AgentName: "alpha",
		TownRoot:  town,
	}, "test prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(cmd, "GT_DOG_NAME=alpha") {
		t.Errorf("dog command does not carry GT_DOG_NAME:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--append-system-prompt-file") || !strings.Contains(cmd, path) {
		t.Errorf("dog command does not carry its system prompt file %s:\n%s", path, cmd)
	}
}

func TestBuildCommand_WithAgentOverride(t *testing.T) {
	cfg := SessionConfig{
		Role:          "boot",
		TownRoot:      "/tmp/town",
		AgentOverride: "opencode",
	}
	cmd, err := buildCommand(cfg, "test prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmd == "" {
		t.Fatal("expected non-empty command")
	}
}

func TestKillExistingSession_NoSession(t *testing.T) {
	// KillExistingSession with nil tmux would panic, but we test the logic
	// by verifying it's callable. Full integration tests need a real tmux.
	// This test verifies the function signature and basic flow.
	t.Skip("requires tmux for integration testing")
}

func TestMapKeysSorted(t *testing.T) {
	got := mapKeysSorted(map[string]string{
		"GT_SESSION": "1",
		"GT_ROLE":    "polecat",
		"GT_RIG":     "alpha",
	})

	want := []string{"GT_RIG", "GT_ROLE", "GT_SESSION"}
	if len(got) != len(want) {
		t.Fatalf("mapKeysSorted() length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mapKeysSorted()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestMergeRuntimeLivenessEnv_SetsResolvedAgentAndProcessNames(t *testing.T) {
	env := map[string]string{
		"GT_ROLE": "polecat",
	}
	rc := &config.RuntimeConfig{
		Command:       "claude",
		ResolvedAgent: "claude",
	}

	got := MergeRuntimeLivenessEnv(env, rc)

	if got["GT_AGENT"] != "claude" {
		t.Fatalf("GT_AGENT = %q, want %q", got["GT_AGENT"], "claude")
	}
	if got["GT_PROCESS_NAMES"] != "node,claude" {
		t.Fatalf("GT_PROCESS_NAMES = %q, want %q", got["GT_PROCESS_NAMES"], "node,claude")
	}
}

func TestMergeRuntimeLivenessEnv_RespectsExistingValues(t *testing.T) {
	env := map[string]string{
		"GT_AGENT":         "explicit-agent",
		"GT_PROCESS_NAMES": "custom-bin,custom-agent",
	}
	rc := &config.RuntimeConfig{
		Command:       "bun",
		ResolvedAgent: "wen",
	}

	got := MergeRuntimeLivenessEnv(env, rc)

	if got["GT_AGENT"] != "explicit-agent" {
		t.Fatalf("GT_AGENT = %q, want %q", got["GT_AGENT"], "explicit-agent")
	}
	if got["GT_PROCESS_NAMES"] != "custom-bin,custom-agent" {
		t.Fatalf("GT_PROCESS_NAMES = %q, want %q", got["GT_PROCESS_NAMES"], "custom-bin,custom-agent")
	}
}

func TestMergeRuntimeLivenessEnv_UsesEffectiveAgentForProcessNames(t *testing.T) {
	// When AgentOverride sets GT_AGENT to a different agent than
	// runtimeConfig.ResolvedAgent, process names must be resolved from
	// the effective agent (GT_AGENT), not the workspace-default resolved agent.
	env := map[string]string{
		"GT_AGENT": "codex", // set by AgentEnv from AgentOverride
	}
	rc := &config.RuntimeConfig{
		Command:       "claude",
		ResolvedAgent: "claude", // workspace default, NOT the override
	}

	got := MergeRuntimeLivenessEnv(env, rc)

	if got["GT_AGENT"] != "codex" {
		t.Fatalf("GT_AGENT = %q, want %q", got["GT_AGENT"], "codex")
	}
	if got["GT_PROCESS_NAMES"] != "codex" {
		t.Fatalf("GT_PROCESS_NAMES = %q, want %q (should resolve from effective agent, not runtimeConfig)", got["GT_PROCESS_NAMES"], "codex")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
