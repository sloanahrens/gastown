package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
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
	t.Run("set and get convoy.notify_on_complete", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)
		settingsPath := config.TownSettingsPath(townRoot)

		// Set convoy.notify_on_complete to true
		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"convoy.notify_on_complete", "true"})
		if err != nil {
			t.Fatalf("runConfigSet failed: %v", err)
		}

		// Verify persisted
		loaded, err := config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		if loaded.Convoy == nil {
			t.Fatal("Convoy config is nil after set")
		}
		if !loaded.Convoy.NotifyOnComplete {
			t.Error("NotifyOnComplete should be true")
		}

		// Get the value back
		err = configGet(townConfigCmdEnv(townRoot, io.Discard), []string{"convoy.notify_on_complete"})
		if err != nil {
			t.Fatalf("runConfigGet failed: %v", err)
		}

		// Set back to false
		err = configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"convoy.notify_on_complete", "false"})
		if err != nil {
			t.Fatalf("runConfigSet(false) failed: %v", err)
		}

		loaded, err = config.LoadOrCreateTownSettings(settingsPath)
		if err != nil {
			t.Fatalf("load settings: %v", err)
		}
		if loaded.Convoy != nil && loaded.Convoy.NotifyOnComplete {
			t.Error("NotifyOnComplete should be false after setting to false")
		}
	})

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

	t.Run("convoy.notify_on_complete rejects non-boolean", func(t *testing.T) {
		townRoot := setupTestTownForConfig(t)

		err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"convoy.notify_on_complete", "maybe"})
		if err == nil {
			t.Fatal("expected error for non-boolean value")
		}
		if !strings.Contains(err.Error(), "invalid value") {
			t.Errorf("error = %v, want 'invalid value'", err)
		}
	})
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

// gt config reads and writes the seat-refill dispatch policy through the same
// keys the plugin reads: get reports the effective value (the file's, else the
// default), and set refuses a value the plugin cannot act on (gt-y3pgh.12).
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
	if err := configGet(townConfigCmdEnv(townRoot, &stdout), []string{"polecat_pool.pro_label"}); err != nil {
		t.Fatalf("configGet polecat_pool.pro_label: %v", err)
	}
	if got := strings.TrimSpace(stdout.String()); got != "needs-pro" {
		t.Errorf("polecat_pool.pro_label = %q, want the default needs-pro", got)
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

	// A knob that is not one of the pool's keys is still an unknown key.
	if err := configSet(townConfigCmdEnv(townRoot, io.Discard), []string{"polecat_pool.max_bogus", "1"}); err == nil {
		t.Error("configSet polecat_pool.max_bogus = nil, want an unknown-key error")
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
