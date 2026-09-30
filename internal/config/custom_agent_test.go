// Test Rig-Level Custom Agent Support
//
// These tests verify that custom agents defined in rig-level
// settings/config.json are correctly loaded and used when spawning polecats.

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRigLevelCustomAgent tests rig-level custom agent resolution: a minimal
// town/rig with a custom agent configured, resolved and built into the
// polecat startup command. Custom agents are not looked up on PATH, so the
// agent's command is only a path.
func TestRigLevelCustomAgent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	stubAgentPath := "/fake/bin/stub-agent"

	// Set up town structure
	townRoot := filepath.Join(tmpDir, "town")
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)

	setupTestTownWithCustomAgent(t, townRoot, rigName, stubAgentPath)

	// Test 1: Verify ResolveAgentConfig picks up the custom agent
	t.Run("ResolveAgentConfig uses rig-level agent", func(t *testing.T) {
		rc := ResolveAgentConfig(townRoot, rigPath)
		if rc == nil {
			t.Fatal("ResolveAgentConfig returned nil")
		}

		if rc.Command != stubAgentPath {
			t.Errorf("Expected command %q, got %q", stubAgentPath, rc.Command)
		}

		// Verify args are passed through
		if len(rc.Args) != 2 || rc.Args[0] != "--test-mode" || rc.Args[1] != "--stub" {
			t.Errorf("Expected args [--test-mode --stub], got %v", rc.Args)
		}
	})

	// Test 2: Verify BuildPolecatStartupCommand includes the custom agent
	t.Run("BuildPolecatStartupCommand uses custom agent", func(t *testing.T) {
		cmd, err := BuildPolecatStartupCommand(rigName, "test-polecat", rigPath, "")
		if err != nil {
			t.Fatalf("BuildPolecatStartupCommand returned an error: %v", err)
		}

		if !strings.Contains(cmd, stubAgentPath) {
			t.Errorf("Expected command to contain stub agent path %q, got: %s", stubAgentPath, cmd)
		}

		if !strings.Contains(cmd, "--test-mode") {
			t.Errorf("Expected command to contain --test-mode, got: %s", cmd)
		}

		// Verify environment variables are set (GT_ROLE is compound format)
		if !strings.Contains(cmd, "GT_ROLE=testrig/polecats/test-polecat") {
			t.Errorf("Expected GT_ROLE=testrig/polecats/test-polecat in command, got: %s", cmd)
		}

		if !strings.Contains(cmd, "GT_POLECAT=test-polecat") {
			t.Errorf("Expected GT_POLECAT=test-polecat in command, got: %s", cmd)
		}
	})

	// Test 3: Verify ResolveAgentConfigWithOverride respects rig agents
	t.Run("ResolveAgentConfigWithOverride with rig agent", func(t *testing.T) {
		rc, agentName, err := ResolveAgentConfigWithOverride(townRoot, rigPath, "stub-agent")
		if err != nil {
			t.Fatalf("ResolveAgentConfigWithOverride failed: %v", err)
		}

		if agentName != "stub-agent" {
			t.Errorf("Expected agent name 'stub-agent', got %q", agentName)
		}

		if rc.Command != stubAgentPath {
			t.Errorf("Expected command %q, got %q", stubAgentPath, rc.Command)
		}
	})

	// Test 4: Verify unknown agent override returns error
	t.Run("ResolveAgentConfigWithOverride unknown agent errors", func(t *testing.T) {
		_, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, "nonexistent-agent")
		if err == nil {
			t.Fatal("Expected error for nonexistent agent, got nil")
		}

		if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Expected 'not found' error, got: %v", err)
		}
	})
}

// setupTestTownWithCustomAgent creates a minimal town/rig structure with a custom agent.
func setupTestTownWithCustomAgent(t *testing.T, townRoot, rigName, stubAgentPath string) {
	t.Helper()

	rigPath := filepath.Join(townRoot, rigName)

	// Create directory structure
	dirs := []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, "settings"),
		filepath.Join(rigPath, "settings"),
		filepath.Join(rigPath, "polecats"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}

	// Create town.json
	townConfig := map[string]interface{}{
		"type":       "town",
		"version":    2,
		"name":       "test-town",
		"created_at": time.Now().Format(time.RFC3339),
	}
	writeTownJSON(t, filepath.Join(townRoot, "mayor", "town.json"), townConfig)

	// Create town settings (empty, uses defaults)
	townSettings := map[string]interface{}{
		"type":          "town-settings",
		"version":       1,
		"default_agent": "claude",
	}
	writeTownJSON(t, filepath.Join(townRoot, "settings", "config.json"), townSettings)

	// Create rig settings with custom agent
	rigSettings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"agent":   "stub-agent",
		"agents": map[string]interface{}{
			"stub-agent": map[string]interface{}{
				"command": stubAgentPath,
				"args":    []string{"--test-mode", "--stub"},
			},
		},
	}
	writeTownJSON(t, filepath.Join(rigPath, "settings", "config.json"), rigSettings)

	// Create rigs.json
	rigsConfig := map[string]interface{}{
		"version": 1,
		"rigs": map[string]interface{}{
			rigName: map[string]interface{}{
				"git_url":  "https://github.com/test/testrepo.git",
				"added_at": time.Now().Format(time.RFC3339),
			},
		},
	}
	writeTownJSON(t, filepath.Join(townRoot, "mayor", "rigs.json"), rigsConfig)
}

// writeTownJSON writes a JSON config file.
func writeTownJSON(t *testing.T, path string, data interface{}) {
	t.Helper()

	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatalf("Failed to marshal JSON for %s: %v", path, err)
	}

	if err := os.WriteFile(path, jsonData, 0644); err != nil {
		t.Fatalf("Failed to write %s: %v", path, err)
	}
}

// TestRigAgentOverridesTownAgent verifies rig agents take precedence over town agents.
func TestRigAgentOverridesTownAgent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	townRoot := filepath.Join(tmpDir, "town")
	rigName := "testrig"
	rigPath := filepath.Join(townRoot, rigName)

	// Create directory structure
	dirs := []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, "settings"),
		filepath.Join(rigPath, "settings"),
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}

	// Town settings with a custom agent
	townSettings := map[string]interface{}{
		"type":          "town-settings",
		"version":       1,
		"default_agent": "my-agent",
		"agents": map[string]interface{}{
			"my-agent": map[string]interface{}{
				"command": "/town/path/to/agent",
				"args":    []string{"--town-level"},
			},
		},
	}
	writeTownJSON(t, filepath.Join(townRoot, "settings", "config.json"), townSettings)

	// Rig settings with SAME agent name but different config (should override)
	rigSettings := map[string]interface{}{
		"type":    "rig-settings",
		"version": 1,
		"agent":   "my-agent",
		"agents": map[string]interface{}{
			"my-agent": map[string]interface{}{
				"command": "/rig/path/to/agent",
				"args":    []string{"--rig-level"},
			},
		},
	}
	writeTownJSON(t, filepath.Join(rigPath, "settings", "config.json"), rigSettings)

	// Create town.json
	townConfig := map[string]interface{}{
		"type":       "town",
		"version":    2,
		"name":       "test-town",
		"created_at": time.Now().Format(time.RFC3339),
	}
	writeTownJSON(t, filepath.Join(townRoot, "mayor", "town.json"), townConfig)

	// Resolve agent config
	rc := ResolveAgentConfig(townRoot, rigPath)
	if rc == nil {
		t.Fatal("ResolveAgentConfig returned nil")
	}

	// Rig agent should take precedence
	if rc.Command != "/rig/path/to/agent" {
		t.Errorf("Expected rig agent command '/rig/path/to/agent', got %q", rc.Command)
	}

	if len(rc.Args) != 1 || rc.Args[0] != "--rig-level" {
		t.Errorf("Expected rig args [--rig-level], got %v", rc.Args)
	}
}
