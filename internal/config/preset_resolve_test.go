package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessPresetName(t *testing.T) {
	t.Parallel()
	presets := map[string]*AgentPresetInfo{
		"claude":        {Name: "claude", Command: "claude"},
		"groq-compound": {Name: "groq-compound", Command: "claude"},
		"codex":         {Name: "codex", Command: "codex"},
		"kiro":          {Name: "kiro", Command: "kiro-cli"},
	}
	tests := []struct {
		name, command, provider string
		args                    []string
		want                    string
	}{
		{"bare claude beats groq-compound", "claude", "", nil, "claude"},
		{"path to claude", "/usr/local/bin/claude", "", nil, "claude"},
		{"gt- wrapper", "gt-claude", "", nil, "claude"},
		{"env wrapper", "env", "", []string{"-u", "X", "claude", "--flag"}, "claude"},
		{"command wins over provider", "claude", "deepseek", nil, "claude"},
		{"command basename maps to preset name", "kiro-cli", "", nil, "kiro"},
		{"empty command uses provider", "", "codex", nil, "codex"},
		{"empty command and provider default to claude", "", "", nil, "claude"},
		{"empty command unknown provider", "", "generic", nil, ""},
		{"unknown command falls back to provider", "my-wrapper.sh", "claude", nil, "claude"},
		{"unknown command no provider", "aider", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := harnessPresetName(tt.command, tt.args, tt.provider, presets); got != tt.want {
				t.Errorf("harnessPresetName(%q, %v, %q) = %q, want %q", tt.command, tt.args, tt.provider, got, tt.want)
			}
		})
	}
}

func writeTestSettings(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveAgentPreset(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	writeTestSettings(t, TownSettingsPath(town), `{"type":"town-settings","version":1,"agents":{
		"test-9a8-flash":{"provider":"claude","command":"claude"},
		"test-9a8-dog":{"provider":"deepseek","command":"claude"},
		"test-9a8-over":{"command":"claude"},
		"test-9a8-script":{"command":"/opt/bin/mystery"}}}`)
	writeTestSettings(t, RigSettingsPath(rig), `{"type":"rig-settings","version":1,"agents":{
		"test-9a8-over":{"command":"codex"}}}`)

	tests := []struct {
		name, agent, rigPath, want string
		ok                         bool
	}{
		{"custom claude agent", "test-9a8-flash", "", "claude", true},
		{"provider deepseek command claude", "test-9a8-dog", "", "claude", true},
		{"rig definition wins over town", "test-9a8-over", rig, "codex", true},
		{"town definition without rig", "test-9a8-over", "", "claude", true},
		{"builtin passes through", "codex", "", "codex", true},
		{"unknown name", "test-9a8-nope", "", "", false},
		{"unrecognised command", "test-9a8-script", "", "", false},
		{"empty name", "", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveAgentPreset(tt.agent, town, tt.rigPath)
			if ok != tt.ok {
				t.Fatalf("ResolveAgentPreset(%q) ok = %v, want %v", tt.agent, ok, tt.ok)
			}
			if ok && string(got.Name) != tt.want {
				t.Errorf("ResolveAgentPreset(%q) = %q, want %q", tt.agent, got.Name, tt.want)
			}
		})
	}
	if p, _ := ResolveAgentPreset("test-9a8-flash", town, ""); !p.EscapeCancelsRequest {
		t.Error("custom claude agent must carry the claude preset's EscapeCancelsRequest (gt-cyyg)")
	}
}
