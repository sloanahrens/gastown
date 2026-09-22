package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsePluginMD_AgentKey(t *testing.T) {
	content := []byte(`+++
name = "judgment"
description = "Needs a capable model"
version = 1
agent = "claude-sonnet"

[execution]
timeout = "5m"
+++

# Instructions
`)

	p, err := parsePluginMD(content, "/test/path", LocationTown, "")
	if err != nil {
		t.Fatalf("parsePluginMD failed: %v", err)
	}
	if p.Agent != "claude-sonnet" {
		t.Errorf("Agent = %q, want %q", p.Agent, "claude-sonnet")
	}
}

func TestSummary_IncludesAgent(t *testing.T) {
	p := &Plugin{Name: "judgment", Agent: "claude-sonnet"}
	if got := p.Summary().Agent; got != "claude-sonnet" {
		t.Errorf("Summary().Agent = %q, want %q", got, "claude-sonnet")
	}

	unset := &Plugin{Name: "script-runner"}
	if got := unset.Summary().Agent; got != "" {
		t.Errorf("Summary().Agent = %q for a plugin with no agent key, want empty", got)
	}
}

// A preset name the town resolves reaches the Plugin untouched, so dispatch
// has something to override role_agents.dog with.
func TestLoadPlugin_KeepsResolvableAgentPreset(t *testing.T) {
	dir := writeAgentPlugin(t, "script-runner", "gemini")
	s := &Scanner{townRoot: t.TempDir(), presetExists: func(string, string) bool { return true }}

	p, err := s.loadPlugin(dir, LocationTown, "")
	if err != nil {
		t.Fatalf("loadPlugin failed: %v", err)
	}
	if p == nil {
		t.Fatal("loadPlugin returned nil plugin")
	}
	if p.Agent != "gemini" {
		t.Errorf("Agent = %q, want %q", p.Agent, "gemini")
	}
}

// A name that does not resolve falls back to role_agents.dog instead of
// wedging the plugin: dispatch would fail the session start and retry it
// every heartbeat.
func TestLoadPlugin_DropsUnresolvableAgentPreset(t *testing.T) {
	dir := writeAgentPlugin(t, "script-runner", "no-such-model")
	s := &Scanner{townRoot: t.TempDir(), presetExists: func(string, string) bool { return false }}

	p, err := s.loadPlugin(dir, LocationTown, "")
	if err != nil {
		t.Fatalf("loadPlugin failed: %v", err)
	}
	if p == nil {
		t.Fatal("loadPlugin returned nil plugin")
	}
	if p.Agent != "" {
		t.Errorf("Agent = %q, want empty so dispatch uses role_agents.dog", p.Agent)
	}
	if p.Name != "script-runner" {
		t.Errorf("Name = %q, want the plugin to survive validation", p.Name)
	}
}

// The resolver the scanner uses by default must agree with the session start
// it predicts: built-in presets and town custom agents resolve, a typo does not.
func TestAgentPresetResolves(t *testing.T) {
	townRoot := t.TempDir()
	settingsDir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatalf("creating settings dir: %v", err)
	}
	settings := []byte(`{"type":"town-settings","version":1,"agents":{"local-trial":{"command":"llama-server"}}}`)
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), settings, 0644); err != nil {
		t.Fatalf("writing town settings: %v", err)
	}

	cases := []struct {
		name string
		want bool
	}{
		// built-in preset
		{"gemini", true},
		// town custom agent
		{"local-trial", true},
		// typo
		{"no-such-model", false},
	}
	for _, tc := range cases {
		if got := agentPresetResolves(tc.name, townRoot); got != tc.want {
			t.Errorf("agentPresetResolves(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// writeAgentPlugin creates a town-level plugin directory whose frontmatter
// names an agent preset.
func writeAgentPlugin(t *testing.T, name, agent string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "plugins", name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("creating plugin dir: %v", err)
	}
	content := strings.Join([]string{
		"+++",
		`name = "` + name + `"`,
		`description = "test plugin"`,
		"version = 1",
		`agent = "` + agent + `"`,
		"+++",
		"",
		"# Instructions",
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(dir, "plugin.md"), []byte(content), 0644); err != nil {
		t.Fatalf("writing plugin.md: %v", err)
	}
	return dir
}
