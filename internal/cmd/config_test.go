package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// setupTestTown creates a minimal Gas Town workspace for testing.
func setupTestTownForConfig(t *testing.T) string {
	t.Helper()

	townRoot := t.TempDir()

	// Create mayor directory with required files
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}

	// Create town.json
	townConfig := &config.TownConfig{
		Type:       "town",
		Version:    config.CurrentTownVersion,
		Name:       "test-town",
		PublicName: "Test Town",
		CreatedAt:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	townConfigPath := filepath.Join(mayorDir, "town.json")
	if err := config.SaveTownConfig(townConfigPath, townConfig); err != nil {
		t.Fatalf("save town.json: %v", err)
	}

	// Create empty rigs.json
	rigsConfig := &config.RigsConfig{
		Version: 1,
		Rigs:    make(map[string]config.RigEntry),
	}
	rigsPath := filepath.Join(mayorDir, "rigs.json")
	if err := config.SaveRigsConfig(rigsPath, rigsConfig); err != nil {
		t.Fatalf("save rigs.json: %v", err)
	}

	return townRoot
}

func TestConfigAgentList(t *testing.T) {
	t.Parallel()
	t.Run("lists built-in agents", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Run the command
		err := configAgentList(townConfigCmdEnv(townRoot, io.Discard))
		if err != nil {
			t.Fatalf("runConfigAgentList failed: %v", err)
		}

		// Verify settings file was created (LoadOrCreate creates it)
		if _, err := os.Stat(settingsPath); err != nil {
			// This is OK - list command works without settings file
		}
	})

	t.Run("lists built-in and custom agents", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Create settings with custom agent
		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-custom": {
					Command: "my-agent",
					Args:    []string{"--flag"},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save settings: %v", err)
		}

		// Run the command
		err := configAgentList(townConfigCmdEnv(townRoot, io.Discard))
		if err != nil {
			t.Fatalf("runConfigAgentList failed: %v", err)
		}
	})

	t.Run("JSON output", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Use a command with the --json flag registered
		e := townConfigCmdEnv(townRoot, io.Discard)
		e.agentListJSON = true
		err := configAgentList(e)
		if err != nil {
			t.Fatalf("runConfigAgentList failed: %v", err)
		}
	})
}

func TestConfigAgentGet(t *testing.T) {
	t.Parallel()
	t.Run("gets built-in agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Run the command
		args := []string{"claude"}
		err := configAgentGet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentGet failed: %v", err)
		}
	})

	t.Run("gets custom agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Create settings with custom agent
		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-custom": {
					Command: "my-agent",
					Args:    []string{"--flag1", "--flag2"},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save settings: %v", err)
		}

		// Run the command
		args := []string{"my-custom"}
		err := configAgentGet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentGet failed: %v", err)
		}
	})

	t.Run("returns error for unknown agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Run the command with unknown agent
		args := []string{"unknown-agent"}
		err := configAgentGet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err == nil {
			t.Fatal("expected error for unknown agent")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want 'not found'", err)
		}
	})
}

func TestConfigAgentSet(t *testing.T) {
	t.Parallel()
	t.Run("sets custom agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Run the command
		args := []string{"my-agent", "my-agent --arg1 --arg2"}
		err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		// Verify settings were saved
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		if loaded.Agents == nil {
			t.Fatal("Agents map is nil")
		}
		agent, ok := loaded.Agents["my-agent"]
		if !ok {
			t.Fatal("custom agent not found in settings")
		}
		if agent.Command != "my-agent" {
			t.Errorf("Command = %q, want 'my-agent'", agent.Command)
		}
		if len(agent.Args) != 2 {
			t.Errorf("Args count = %d, want 2", len(agent.Args))
		}
	})

	t.Run("sets agent with single command (no args)", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Run the command
		args := []string{"simple-agent", "simple-agent"}
		err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		// Verify settings were saved
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		agent := loaded.Agents["simple-agent"]
		if agent.Command != "simple-agent" {
			t.Errorf("Command = %q, want 'simple-agent'", agent.Command)
		}
		if len(agent.Args) != 0 {
			t.Errorf("Args count = %d, want 0", len(agent.Args))
		}
	})

	t.Run("overrides existing agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Create initial settings
		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-agent": {
					Command: "old-command",
					Args:    []string{"--old"},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save initial settings: %v", err)
		}

		// Run the command to override
		args := []string{"my-agent", "new-command --new"}
		err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		// Verify settings were updated
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		agent := loaded.Agents["my-agent"]
		if agent.Command != "new-command" {
			t.Errorf("Command = %q, want 'new-command'", agent.Command)
		}
	})

	t.Run("keeps the other runtime fields of an existing agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"deepseek-flash": {
					Provider:      "claude",
					Command:       "old-command",
					Args:          []string{"--old"},
					Env:           map[string]string{"ANTHROPIC_BASE_URL": "https://backend.example", "ANTHROPIC_AUTH_TOKEN": "$DEEPSEEK_KEY"},
					InitialPrompt: "hello",
					Session:       &config.RuntimeSessionConfig{SessionIDEnv: "MY_SESSION_ID"},
					Tmux:          &config.RuntimeTmuxConfig{ProcessNames: []string{"node"}, ReadyDelayMs: 500},
					Instructions:  &config.RuntimeInstructionsConfig{File: "AGENTS.md"},
					ExecWrapper:   []string{"exitbox", "run", "--"},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save initial settings: %v", err)
		}

		args := []string{"deepseek-flash", "new-command --new"}
		if err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args); err != nil {
			t.Fatalf("configAgentSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		agent := loaded.Agents["deepseek-flash"]
		if agent == nil {
			t.Fatal("agent missing after set")
		}
		if agent.Command != "new-command" || len(agent.Args) != 1 || agent.Args[0] != "--new" {
			t.Errorf("Command/Args = %q %v, want new-command [--new]", agent.Command, agent.Args)
		}
		if agent.Provider != "claude" {
			t.Errorf("Provider = %q, want existing 'claude' kept", agent.Provider)
		}
		if len(agent.Env) != 2 ||
			agent.Env["ANTHROPIC_BASE_URL"] != "https://backend.example" ||
			agent.Env["ANTHROPIC_AUTH_TOKEN"] != "$DEEPSEEK_KEY" {
			t.Errorf("Env block changed")
		}
		if agent.InitialPrompt != "hello" {
			t.Errorf("InitialPrompt = %q, want 'hello'", agent.InitialPrompt)
		}
		if agent.Session == nil || agent.Session.SessionIDEnv != "MY_SESSION_ID" {
			t.Errorf("Session changed: %+v", agent.Session)
		}
		if agent.Tmux == nil || len(agent.Tmux.ProcessNames) != 1 || agent.Tmux.ReadyDelayMs != 500 {
			t.Errorf("Tmux changed: %+v", agent.Tmux)
		}
		if agent.Instructions == nil || agent.Instructions.File != "AGENTS.md" {
			t.Errorf("Instructions changed: %+v", agent.Instructions)
		}
		if len(agent.ExecWrapper) != 3 {
			t.Errorf("ExecWrapper changed: %v", agent.ExecWrapper)
		}
	})

	t.Run("explicit provider replaces the existing one", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-agent": {Provider: "claude", Command: "old", Env: map[string]string{"K": "v"}},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save initial settings: %v", err)
		}

		e := townConfigCmdEnv(townRoot, io.Discard)
		e.agentSetProvider = "codex"
		if err := configAgentSet(e, []string{"my-agent", "new"}); err != nil {
			t.Fatalf("configAgentSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		agent := loaded.Agents["my-agent"]
		if agent.Provider != "codex" {
			t.Errorf("Provider = %q, want 'codex'", agent.Provider)
		}
		if agent.Env["K"] != "v" {
			t.Errorf("Env does not keep K=v")
		}
	})
}

func TestConfigAgentSetProviderInference(t *testing.T) {
	t.Parallel()
	t.Run("infers provider from known command name", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// "claude" is a known preset — provider should be inferred
		args := []string{"claude-custom", "claude --model opus"}
		if err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args); err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		agent := loaded.Agents["claude-custom"]
		if agent == nil {
			t.Fatal("agent not found")
		}
		if agent.Provider != "claude" {
			t.Errorf("Provider = %q, want 'claude' (inferred from command)", agent.Provider)
		}
		if agent.Command != "claude" {
			t.Errorf("Command = %q, want 'claude'", agent.Command)
		}
	})

	t.Run("no provider inferred for unknown command", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// "my-custom-tool" is not a known preset — provider should remain empty
		args := []string{"my-bot", "my-custom-tool --flag"}
		if err := configAgentSet(townConfigCmdEnv(townRoot, io.Discard), args); err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		agent := loaded.Agents["my-bot"]
		if agent == nil {
			t.Fatal("agent not found")
		}
		if agent.Provider != "" {
			t.Errorf("Provider = %q, want '' (no inference for unknown command)", agent.Provider)
		}
	})

	t.Run("explicit --provider flag overrides inference", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Command name is unknown, but explicit provider is given
		e := townConfigCmdEnv(townRoot, io.Discard)
		e.agentSetProvider = "claude"
		if err := configAgentSet(e, []string{"my-claude-wrapper", "my-claude-wrapper --custom"}); err != nil {
			t.Fatalf("runConfigAgentSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		agent := loaded.Agents["my-claude-wrapper"]
		if agent == nil {
			t.Fatal("agent not found")
		}
		if agent.Provider != "claude" {
			t.Errorf("Provider = %q, want 'claude' (explicit --provider flag)", agent.Provider)
		}
	})
}

func TestConfigAgentRemove(t *testing.T) {
	t.Parallel()
	t.Run("removes custom agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Create settings with custom agent
		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-agent": {
					Command: "my-agent",
					Args:    []string{"--flag"},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save settings: %v", err)
		}

		// Run the command
		args := []string{"my-agent"}
		err := configAgentRemove(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigAgentRemove failed: %v", err)
		}

		// Verify agent was removed
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		if loaded.Agents != nil {
			if _, ok := loaded.Agents["my-agent"]; ok {
				t.Error("agent still exists after removal")
			}
		}
	})

	t.Run("rejects removing built-in agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Try to remove a built-in agent
		args := []string{"claude"}
		err := configAgentRemove(townConfigCmdEnv(townRoot, io.Discard), args)
		if err == nil {
			t.Fatal("expected error when removing built-in agent")
		}
		if !strings.Contains(err.Error(), "cannot remove built-in") {
			t.Errorf("error = %v, want 'cannot remove built-in'", err)
		}
	})

	t.Run("removes override of built-in agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"claude": {Command: "claude-glm"},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save settings: %v", err)
		}

		var out bytes.Buffer
		if err := configAgentRemove(townConfigCmdEnv(townRoot, &out), []string{"claude"}); err != nil {
			t.Fatalf("configAgentRemove failed: %v", err)
		}
		if !strings.Contains(out.String(), "built-in is restored") {
			t.Errorf("output = %q, want it to say the built-in is restored", out.String())
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		if _, ok := loaded.Agents["claude"]; ok {
			t.Error("override of claude still exists after removal")
		}
		// The override was the default agent: the default stays named, and
		// now resolves to the built-in rather than the deleted override.
		if loaded.DefaultAgent != "claude" {
			t.Errorf("DefaultAgent = %q, want claude", loaded.DefaultAgent)
		}
		if rc := config.ResolveAgentConfig(townRoot, ""); filepath.Base(rc.Command) != "claude" {
			t.Errorf("default agent resolves to command %q, want the built-in claude", rc.Command)
		}
	})

	t.Run("returns error for non-existent custom agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Try to remove a non-existent agent
		args := []string{"non-existent"}
		err := configAgentRemove(townConfigCmdEnv(townRoot, io.Discard), args)
		if err == nil {
			t.Fatal("expected error for non-existent agent")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want 'not found'", err)
		}
	})
}

func TestConfigDefaultAgent(t *testing.T) {
	t.Parallel()
	t.Run("gets default agent (shows current)", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Run the command with no args (should show current default)
		args := []string{}
		err := configDefaultAgent(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigDefaultAgent failed: %v", err)
		}
	})

	t.Run("sets default agent to built-in", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Set default to groq-compound
		args := []string{"groq-compound"}
		err := configDefaultAgent(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigDefaultAgent failed: %v", err)
		}

		// Verify settings were saved
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		if loaded.DefaultAgent != "groq-compound" {
			t.Errorf("DefaultAgent = %q, want 'groq-compound'", loaded.DefaultAgent)
		}
	})

	t.Run("sets default agent to custom", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Create settings with custom agent
		settings := &config.TownSettings{
			Type:         "town-settings",
			Version:      config.CurrentTownSettingsVersion,
			DefaultAgent: "claude",
			Agents: map[string]*config.RuntimeConfig{
				"my-custom": {
					Command: "my-agent",
					Args:    []string{},
				},
			},
		}
		if err := config.SaveTownSettings(settingsPath, settings); err != nil {
			t.Fatalf("save settings: %v", err)
		}

		// Set default to custom agent
		args := []string{"my-custom"}
		err := configDefaultAgent(townConfigCmdEnv(townRoot, io.Discard), args)
		if err != nil {
			t.Fatalf("runConfigDefaultAgent failed: %v", err)
		}

		// Verify settings were saved
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}

		if loaded.DefaultAgent != "my-custom" {
			t.Errorf("DefaultAgent = %q, want 'my-custom'", loaded.DefaultAgent)
		}
	})

	t.Run("returns error for unknown agent", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Try to set default to unknown agent
		args := []string{"unknown-agent"}
		err := configDefaultAgent(townConfigCmdEnv(townRoot, io.Discard), args)
		if err == nil {
			t.Fatal("expected error for unknown agent")
		}
		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want 'not found'", err)
		}
	})
}

func TestConfigDefaultAgentList(t *testing.T) {
	t.Parallel()
	t.Run("lists available agents via default-agent list", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// runConfigAgentList is reused by default-agent list
		err := configAgentList(townConfigCmdEnv(townRoot, io.Discard))
		if err != nil {
			t.Fatalf("runConfigAgentList (via default-agent list) failed: %v", err)
		}
	})

	t.Run("JSON output via default-agent list", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		e := townConfigCmdEnv(townRoot, io.Discard)
		e.agentListJSON = true
		err := configAgentList(e)
		if err != nil {
			t.Fatalf("runConfigAgentList JSON (via default-agent list) failed: %v", err)
		}
	})
}

func TestConfigSetGet(t *testing.T) {
	t.Parallel()
	t.Run("set and get cli_theme", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"cli_theme", "dark"})
		if err != nil {
			t.Fatalf("runConfigSet failed: %v", err)
		}

		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		if loaded.CLITheme != "dark" {
			t.Errorf("CLITheme = %q, want 'dark'", loaded.CLITheme)
		}
	})

	t.Run("set cli_theme rejects invalid value", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"cli_theme", "neon"})
		if err == nil {
			t.Fatal("expected error for invalid cli_theme")
		}
		if !strings.Contains(err.Error(), "invalid cli_theme") {
			t.Errorf("error = %v, want 'invalid cli_theme'", err)
		}
	})

	t.Run("set rejects unknown key", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"nonexistent.key", "value"})
		if err == nil {
			t.Fatal("expected error for unknown key")
		}
		if !strings.Contains(err.Error(), "unknown config key") {
			t.Errorf("error = %v, want 'unknown config key'", err)
		}
	})

	t.Run("get rejects unknown key", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		err := configGet(townConfigCmdEnv(townRoot, io.Discard), []string{"nonexistent.key"})
		if err == nil {
			t.Fatal("expected error for unknown key")
		}
		if !strings.Contains(err.Error(), "unknown config key") {
			t.Errorf("error = %v, want 'unknown config key'", err)
		}
	})
}

// TestConfigGetCompactorThresholdDefault: with no override, config get prints
// the daemon's own default for lifecycle.compactor.threshold. It printed 500
// from a second hardcoded copy while the daemon used 2000.
func TestConfigGetCompactorThresholdDefault(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	var out bytes.Buffer
	if err := configGet(townConfigCmdEnv(townRoot, &out), []string{"lifecycle.compactor.threshold"}); err != nil {
		t.Fatalf("configGet: %v", err)
	}
	want := fmt.Sprintf("%d (default)", daemon.DefaultCompactorCommitThreshold)
	if got := strings.TrimSpace(out.String()); got != want {
		t.Fatalf("config get lifecycle.compactor.threshold = %q, want %q", got, want)
	}
}

// TestConfigSetDefaultAgentRejectsUnknown: `gt config set default_agent` takes
// the same names `gt config default-agent` does, and stores nothing when it
// rejects one.
func TestConfigSetDefaultAgentRejectsUnknown(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)

	err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"default_agent", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("config set default_agent bogus = %v, want a not-found error", err)
	}
	loaded, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if loaded.DefaultAgent == "bogus" {
		t.Fatalf("default_agent stored %q despite the rejection", loaded.DefaultAgent)
	}

	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"default_agent", "claude"}); err != nil {
		t.Fatalf("config set default_agent claude: %v", err)
	}
	loaded, err = config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if loaded.DefaultAgent != "claude" {
		t.Fatalf("default_agent = %q, want claude", loaded.DefaultAgent)
	}
}

func TestSchedulerConfigSetZero(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)
	settingsPath := config.TownSettingsPath(townRoot)

	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"scheduler.max_polecats", "0"}); err != nil {
		t.Fatalf("runConfigSet failed: %v", err)
	}

	loaded, err := config.LoadOrCreateTownSettings(settingsPath)
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if loaded.Scheduler == nil || loaded.Scheduler.MaxPolecats == nil {
		t.Fatal("scheduler.max_polecats was not persisted")
	}
	if got := loaded.Scheduler.GetMaxPolecats(); got != 0 {
		t.Fatalf("persisted scheduler.max_polecats = %d, want 0", got)
	}

	var stdout bytes.Buffer
	if err := configGet(townConfigCmdEnv(townRoot, &stdout), []string{"scheduler.max_polecats"}); err != nil {
		t.Fatalf("configGet failed: %v", err)
	}
	if out := strings.TrimSpace(stdout.String()); out != "0" {
		t.Fatalf("config get scheduler.max_polecats = %q, want 0", out)
	}
}

func TestConfigMaintenanceSetGet(t *testing.T) {
	t.Parallel()
	t.Run("set and get maintenance.window", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Set maintenance window
		err := setMaintenanceConfig(townRoot, "maintenance.window", "03:00")
		if err != nil {
			t.Fatalf("setMaintenanceConfig failed: %v", err)
		}

		// Get it back
		err = getMaintenanceConfig(io.Discard, townRoot, "maintenance.window")
		if err != nil {
			t.Fatalf("getMaintenanceConfig failed: %v", err)
		}
	})

	// A section this writer creates names the town-state backup root as
	// explicitly empty: an unset key alerts every night (gt-l9qnva).
	t.Run("a section the writer creates names the backup root explicitly empty", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		if err := setMaintenanceConfig(townRoot, "maintenance.window", "03:00"); err != nil {
			t.Fatalf("setMaintenanceConfig failed: %v", err)
		}
		pc, err := daemon.ReadPatrolConfig(townRoot)
		if err != nil || pc == nil || pc.Patrols == nil || pc.Patrols.ScheduledMaintenance == nil {
			t.Fatalf("ReadPatrolConfig = %+v, %v; want the section the writer created", pc, err)
		}
		if root := pc.Patrols.ScheduledMaintenance.TownStateBackupRoot; root == nil || *root != "" {
			t.Errorf("town_state_backup_root = %v, want explicitly empty", root)
		}
	})

	// gt-vn7ffl: a town whose scheduled_maintenance section already exists
	// without the root (the live shape) had no gt config way to set or turn
	// off the nightly backup. These rows write that shape directly.
	const backupRootKey = "maintenance.town_state_backup_root"
	writeLiveSection := func(t *testing.T, townRoot, section string) string {
		t.Helper()
		path := config.TownSettingsPath(townRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir settings: %v", err)
		}
		doc := `{"type":"town-settings","version":1,"daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":` + section + `}}}`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write settings: %v", err)
		}
		return path
	}
	readSection := func(t *testing.T, townRoot string) *daemon.ScheduledMaintenanceConfig {
		t.Helper()
		pc, err := daemon.ReadPatrolConfig(townRoot)
		if err != nil {
			t.Fatalf("ReadPatrolConfig: %v", err)
		}
		if pc == nil || pc.Patrols == nil || pc.Patrols.ScheduledMaintenance == nil {
			t.Fatalf("ReadPatrolConfig = %+v, want the scheduled_maintenance section", pc)
		}
		return pc.Patrols.ScheduledMaintenance
	}
	getRoot := func(t *testing.T, townRoot string) string {
		t.Helper()
		var out bytes.Buffer
		if err := configGet(townConfigCmdEnv(townRoot, &out), []string{backupRootKey}); err != nil {
			t.Fatalf("configGet: %v", err)
		}
		return strings.TrimSpace(out.String())
	}

	t.Run("set on a section that lacks the key leaves window and enabled alone", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		writeLiveSection(t, townRoot, `{"enabled":true,"window":"02:00"}`)

		if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{backupRootKey, "/srv/gt-backups"}); err != nil {
			t.Fatalf("config set %s: %v", backupRootKey, err)
		}

		mc := readSection(t, townRoot)
		if mc.TownStateBackupRoot == nil || *mc.TownStateBackupRoot != "/srv/gt-backups" {
			t.Errorf("town_state_backup_root = %v, want /srv/gt-backups", mc.TownStateBackupRoot)
		}
		if !mc.Enabled || mc.Window != "02:00" {
			t.Errorf("enabled, window = %v, %q; want true, 02:00 (the root is independent of the window)", mc.Enabled, mc.Window)
		}
	})

	t.Run("the empty string is the explicit off state and leaves a disabled patrol disabled", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		writeLiveSection(t, townRoot, `{"enabled":false,"window":"02:00"}`)

		if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{backupRootKey, ""}); err != nil {
			t.Fatalf("config set %s \"\": %v", backupRootKey, err)
		}
		mc := readSection(t, townRoot)
		if mc.TownStateBackupRoot == nil || *mc.TownStateBackupRoot != "" || mc.Enabled {
			t.Errorf("town_state_backup_root, enabled = %v, %v; want set and empty (off), false", mc.TownStateBackupRoot, mc.Enabled)
		}
	})

	t.Run("a relative path is refused and the file is unchanged", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		path := writeLiveSection(t, townRoot, `{"enabled":true,"window":"02:00"}`)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}

		err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{backupRootKey, "gt-backups/town-state"})
		if err == nil || !strings.Contains(err.Error(), "want an absolute path") {
			t.Fatalf("config set %s with a relative path = %v, want the daemon's want-an-absolute-path refusal", backupRootKey, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("settings file changed by a refused set:\nbefore: %s\nafter:  %s", before, after)
		}
	})

	t.Run("an absolute path is stored cleaned", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		writeLiveSection(t, townRoot, `{"enabled":true,"window":"02:00"}`)

		if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{backupRootKey, "/srv//gt-backups/../town-state/"}); err != nil {
			t.Fatalf("config set %s: %v", backupRootKey, err)
		}
		if root := readSection(t, townRoot).TownStateBackupRoot; root == nil || *root != "/srv/town-state" {
			t.Errorf("town_state_backup_root = %v, want /srv/town-state", root)
		}
	})

	t.Run("get tells not set, off and a path apart", func(t *testing.T) {
		absent := setupTestTownForConfig(t)
		if got := getRoot(t, absent); got != "(not set)" {
			t.Errorf("get with no settings file = %q, want (not set)", got)
		}

		noKey := setupTestTownForConfig(t)
		writeLiveSection(t, noKey, `{"enabled":true,"window":"02:00"}`)
		if got := getRoot(t, noKey); got != "(not set)" {
			t.Errorf("get with the key absent = %q, want (not set)", got)
		}

		off := setupTestTownForConfig(t)
		writeLiveSection(t, off, `{"enabled":true,"window":"02:00"}`)
		if err := configSet(townConfigCmdEnv(off, io.Discard), []string{backupRootKey, ""}); err != nil {
			t.Fatalf("config set %s \"\": %v", backupRootKey, err)
		}
		if got := getRoot(t, off); got != "(off)" {
			t.Errorf("get with the explicit empty string = %q, want (off)", got)
		}

		on := setupTestTownForConfig(t)
		writeLiveSection(t, on, `{"enabled":true,"window":"02:00"}`)
		if err := configSet(townConfigCmdEnv(on, io.Discard), []string{backupRootKey, "/srv/gt-backups"}); err != nil {
			t.Fatalf("config set %s: %v", backupRootKey, err)
		}
		if got := getRoot(t, on); got != "/srv/gt-backups" {
			t.Errorf("get with a directory set = %q, want /srv/gt-backups", got)
		}
	})

	t.Run("the host file's other section and its mode survive the set", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		path := config.TownSettingsPath(townRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir settings: %v", err)
		}
		doc := `{"type":"town-settings","version":1,"cli_theme":"dark","daemon":{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_maintenance":{"enabled":true,"window":"02:00"}}}}`
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write settings: %v", err)
		}

		if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{backupRootKey, "/srv/gt-backups"}); err != nil {
			t.Fatalf("config set %s: %v", backupRootKey, err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		var top map[string]interface{}
		if err := json.Unmarshal(data, &top); err != nil {
			t.Fatalf("settings file does not parse after the set: %v\n%s", err, data)
		}
		if top["cli_theme"] != "dark" {
			t.Errorf("cli_theme = %v, want dark unchanged", top["cli_theme"])
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat settings: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("settings mode = %o, want 600", got)
		}
	})

	t.Run("set maintenance.window validates format", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		// Valid windows
		for _, w := range []string{"03:00", "00:00", "23:59", "12:30"} {
			if err := setMaintenanceConfig(townRoot, "maintenance.window", w); err != nil {
				t.Errorf("setMaintenanceConfig(%q) unexpected error: %v", w, err)
			}
		}

		// Invalid windows
		for _, w := range []string{"25:00", "12:60", "abc", "12", ""} {
			if err := setMaintenanceConfig(townRoot, "maintenance.window", w); err == nil {
				t.Errorf("setMaintenanceConfig(%q) expected error", w)
			}
		}
	})

	t.Run("maintenance config routes through runConfigSet", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"maintenance.window", "04:00"})
		if err != nil {
			t.Fatalf("runConfigSet(maintenance.window) failed: %v", err)
		}

		err = configGet(townConfigCmdEnv(townRoot, io.Discard), []string{"maintenance.window"})
		if err != nil {
			t.Fatalf("runConfigGet(maintenance.window) failed: %v", err)
		}
	})

	// The schedule is policy (gt-8z769.3): the retired mode and trigger keys
	// are refused rather than written into a daemon.json nothing reads.
	t.Run("retired maintenance keys are unknown", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		for _, key := range []string{"maintenance.interval", "maintenance.threshold", "maintenance.mode",
			"maintenance.gc_min_bytes", "maintenance.gc_growth_ratio"} {
			err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{key, "1"})
			if err == nil || !strings.Contains(err.Error(), "unknown config key") {
				t.Errorf("config set %s = %v, want unknown config key", key, err)
			}
		}
	})
}

// gt config reads and writes the pool's dispatch policy through the same keys
// the spec dispatcher reads: get reports the effective value (the file's, else
// the default), and set refuses a value the dispatcher cannot act on
// (gt-y3pgh.12).
func TestConfigSetGetPolecatPoolPolicy(t *testing.T) {
	t.Parallel()
	townRoot := setupTestTownForConfig(t)

	// Get before anything is set: the default, not an empty string.
	var stdout bytes.Buffer
	if err := configGet(townConfigCmdEnv(townRoot, &stdout), []string{"polecat_pool.max_priority"}); err != nil {
		t.Fatalf("configGet polecat_pool.max_priority: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "2" {
		t.Errorf("polecat_pool.max_priority = %q, want the default 2", got)
	}
	stdout.Reset()
	if err := configGet(townConfigCmdEnv(townRoot, &stdout), []string{"polecat_pool.shape_gate"}); err != nil {
		t.Fatalf("configGet polecat_pool.shape_gate: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "warn" {
		t.Errorf("polecat_pool.shape_gate = %q, want the default warn", got)
	}

	// Set the ceiling the 2026-10-01 incident had to reach through an env var.
	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.max_priority", "3"}); err != nil {
		t.Fatalf("configSet polecat_pool.max_priority: %v", err)
	}
	loaded, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if got := loaded.PolecatPool.GetMaxPriority(); got != 3 {
		t.Errorf("saved max_priority = %d, want 3", got)
	}
	stdout.Reset()
	if err := configGet(townConfigCmdEnv(townRoot, &stdout), []string{"polecat_pool.max_priority"}); err != nil {
		t.Fatalf("configGet after set: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "3" {
		t.Errorf("polecat_pool.max_priority after set = %q, want 3", got)
	}

	// A value the plugin cannot act on never reaches the file.
	err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.max_priority", "-1"})
	if err == nil || !strings.Contains(err.Error(), "polecat_pool.max_priority") {
		t.Errorf("configSet max_priority -1 = %v, want a refusal naming the key", err)
	}
	err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.top_candidates", "0"})
	if err == nil || !strings.Contains(err.Error(), "polecat_pool.top_candidates") {
		t.Errorf("configSet top_candidates 0 = %v, want a refusal naming the key", err)
	}
	err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.max_priority", "high"})
	if err == nil || !strings.Contains(err.Error(), "expected integer") {
		t.Errorf("configSet max_priority high = %v, want an integer refusal", err)
	}
	err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.shape_gate", "block"})
	if err == nil || !strings.Contains(err.Error(), "polecat_pool.shape_gate") {
		t.Errorf("configSet shape_gate block = %v, want a refusal naming the key", err)
	}
	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.shape_gate", "refuse"}); err != nil {
		t.Fatalf("configSet shape_gate refuse: %v", err)
	}
	loaded, err = config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		t.Fatalf("load settings: %v", err)
	}
	if got := loaded.PolecatPool.GetShapeGate(); got != "refuse" {
		t.Errorf("saved shape_gate = %q, want refuse", got)
	}

	// A knob that is not one of the pool's keys is still an unknown key: the
	// retired pro seat's keys included, since the seat they configured is gone.
	for _, key := range []string{"polecat_pool.max_bogus", "polecat_pool.pro_max", "polecat_pool.pro_agent", "polecat_pool.pro_label"} {
		if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{key, "1"}); err == nil || !strings.Contains(err.Error(), "unknown config key") {
			t.Errorf("configSet %s = %v, want an unknown-key error", key, err)
		}
	}
}

func TestParseBool(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
		err   bool
	}{
		{"true", true, false},
		{"True", true, false},
		{"TRUE", true, false},
		{"yes", true, false},
		{"1", true, false},
		{"on", true, false},
		{"false", false, false},
		{"False", false, false},
		{"no", false, false},
		{"0", false, false},
		{"off", false, false},
		{"maybe", false, true},
		{"", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseBool(tt.input)
			if (err != nil) != tt.err {
				t.Errorf("parseBool(%q) error = %v, wantErr %v", tt.input, err, tt.err)
				return
			}
			if got != tt.want {
				t.Errorf("parseBool(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
