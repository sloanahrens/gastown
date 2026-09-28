package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

func TestCheckHelpFlag(t *testing.T) {
	t.Parallel()
	// Create a test command
	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Test command",
		Long:  "This is a test command for testing checkHelpFlag.",
	}

	tests := []struct {
		name        string
		args        []string
		wantHelped  bool
		description string
	}{
		{
			name:        "--help as first arg",
			args:        []string{"--help"},
			wantHelped:  true,
			description: "should show help when --help is first argument",
		},
		{
			name:        "-h as first arg",
			args:        []string{"-h"},
			wantHelped:  true,
			description: "should show help when -h is first argument",
		},
		{
			name:        "--help with other args after",
			args:        []string{"--help", "something"},
			wantHelped:  true,
			description: "should show help when --help is first, ignoring rest",
		},
		{
			name:        "no args",
			args:        []string{},
			wantHelped:  false,
			description: "should not show help with no args",
		},
		{
			name:        "regular args",
			args:        []string{"abc123", "--json"},
			wantHelped:  false,
			description: "should not show help with regular args",
		},
		{
			name:        "--help NOT first - false positive prevention",
			args:        []string{"-m", "--help"},
			wantHelped:  false,
			description: "should NOT show help when --help is not first (e.g., commit -m '--help')",
		},
		{
			name:        "-h NOT first - false positive prevention",
			args:        []string{"something", "-h"},
			wantHelped:  false,
			description: "should NOT show help when -h is not first",
		},
		{
			name:        "--help after -- separator",
			args:        []string{"--", "--help"},
			wantHelped:  false,
			description: "should NOT show help when --help is after -- (passed to underlying tool)",
		},
		{
			name:        "similar but not help flag",
			args:        []string{"--helper"},
			wantHelped:  false,
			description: "should not match --helper as help flag",
		},
		{
			name:        "help without dashes",
			args:        []string{"help"},
			wantHelped:  false,
			description: "should not match 'help' without dashes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			helped, err := checkHelpFlag(testCmd, tt.args)
			if err != nil {
				t.Errorf("checkHelpFlag() returned error: %v", err)
			}
			if helped != tt.wantHelped {
				t.Errorf("checkHelpFlag(%v) helped = %v, want %v (%s)",
					tt.args, helped, tt.wantHelped, tt.description)
			}
		})
	}
}

func TestCheckHelpFlag_EdgeCases(t *testing.T) {
	t.Parallel()
	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Test command",
	}

	// Test that we correctly handle edge cases that could cause panics or unexpected behavior
	t.Run("nil-like empty slice", func(t *testing.T) {
		var args []string
		helped, err := checkHelpFlag(testCmd, args)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if helped {
			t.Error("should not show help for nil/empty args")
		}
	})

	t.Run("single empty string arg", func(t *testing.T) {
		args := []string{""}
		helped, err := checkHelpFlag(testCmd, args)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		if helped {
			t.Error("should not show help for empty string arg")
		}
	})
}

func TestPersistentPreRunKeepsAgentRegistryScoped(t *testing.T) {
	// persistentPreRun used to merge the town's settings/agents.json into a
	// process-global registry, where a rig's file loaded later leaked into
	// every other rig's resolution (gt-rg4f1). Agents are now resolved against
	// config.AgentRegistryFor(town, rig), so the process-wide built-ins must
	// stay untouched. The NixOS ".claude-unwrapped" liveness case this test
	// used to cover is pinned where the scoped registry is read:
	// config TestStartupProcessNamesUseTownRegistry and tmux
	// TestIsAgentAliveLegacyFallbackUsesTownRegistry.
	//
	// NOTE: cannot use t.Parallel() — mutates cwd.
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}

	registry := config.AgentRegistry{
		Version: config.CurrentAgentRegistryVersion,
		Agents: map[string]*config.AgentPresetInfo{
			"claude": {
				Name:         "claude",
				Command:      "claude",
				Args:         []string{"--dangerously-skip-permissions"},
				ProcessNames: []string{"node", "claude", ".claude-unwrapped"},
			},
		},
	}
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "agents.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	// cd into the fake town root so workspace.FindFromCwd() finds it.
	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	cmd := &cobra.Command{Use: "version"}
	if err := persistentPreRun(cmd, nil); err != nil {
		t.Fatalf("persistentPreRun: %v", err)
	}

	if got := config.GetProcessNames("claude"); len(got) != 2 {
		t.Fatalf("GetProcessNames(claude) = %v after persistentPreRun, want the built-in [node claude]", got)
	}
	got := config.AgentRegistryFor(townRoot, "").ProcessNames("claude")
	if len(got) != 3 || got[2] != ".claude-unwrapped" {
		t.Fatalf("town registry ProcessNames(claude) = %v, want the settings/agents.json override", got)
	}
}

func TestPersistentPreRunMalformedAgentRegistry(t *testing.T) {
	// Verify that malformed settings/agents.json does not block persistentPreRun
	// and that the builtin defaults are preserved (graceful fallback).
	//
	// NOTE: cannot use t.Parallel() — mutates cwd.

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0755); err != nil {
		t.Fatal(err)
	}
	// Write invalid JSON to settings/agents.json.
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "agents.json"), []byte("{malformed"), 0644); err != nil {
		t.Fatal(err)
	}

	origDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(townRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origDir) })

	// persistentPreRun should succeed despite malformed agents.json.
	cmd := &cobra.Command{Use: "version"}
	if err := persistentPreRun(cmd, nil); err != nil {
		t.Fatalf("persistentPreRun should not fail on malformed agents.json: %v", err)
	}

	// Builtin defaults should still be in effect.
	got := config.GetProcessNames("claude")
	if len(got) < 2 || got[0] != "node" || got[1] != "claude" {
		t.Fatalf("GetProcessNames(claude) after malformed registry = %v, want builtin [node claude ...]", got)
	}
}
