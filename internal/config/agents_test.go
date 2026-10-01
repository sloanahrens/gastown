package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// isClaudeCmd checks if a command resolves to the claude binary on any platform.
// Note: Named differently from loader_test.go's isClaudeCommand to avoid redeclaration.
func isClaudeCmd(cmd string) bool {
	base := filepath.Base(cmd)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return base == "claude"
}

func TestBuiltInAgentPresetSummary(t *testing.T) {
	t.Parallel()
	s := BuiltInAgentPresetSummary()
	if !strings.Contains(s, "groq-compound") || !strings.Contains(s, "claude") {
		t.Fatalf("BuiltInAgentPresetSummary() = %q, want groq-compound and claude", s)
	}
	names := strings.Split(s, ", ")
	if !sort.StringsAreSorted(names) {
		t.Errorf("BuiltInAgentPresetSummary not sorted: %q", s)
	}
}

func TestBuiltinPresets(t *testing.T) {
	t.Parallel()
	// Ensure all built-in presets are accessible
	presets := []AgentPreset{AgentClaude, AgentGroqCompound}

	for _, preset := range presets {
		info := GetAgentPreset(preset)
		if info == nil {
			t.Errorf("GetAgentPreset(%s) returned nil", preset)
			continue
		}

		if info.Command == "" {
			t.Errorf("preset %s has empty Command", preset)
		}

		// All presets should have ProcessNames for agent detection
		if len(info.ProcessNames) == 0 {
			t.Errorf("preset %s has empty ProcessNames", preset)
		}
	}
}

func TestGetAgentPresetByName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		want    AgentPreset
		wantNil bool
	}{
		{"claude", AgentClaude, false},
		{"groq-compound", AgentGroqCompound, false},
		{"gemini", "", true}, // retired with every non-Claude runtime (D4)
		{"aider", "", true},  // Not built-in, can be added via config
		{"unknown", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetAgentPresetByName(tt.name)
			if tt.wantNil && got != nil {
				t.Errorf("GetAgentPresetByName(%s) = %v, want nil", tt.name, got)
			}
			if !tt.wantNil && got == nil {
				t.Errorf("GetAgentPresetByName(%s) = nil, want preset", tt.name)
			}
			if !tt.wantNil && got != nil && got.Name != tt.want {
				t.Errorf("GetAgentPresetByName(%s).Name = %v, want %v", tt.name, got.Name, tt.want)
			}
		})
	}
}

func TestRuntimeConfigFromPreset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		preset      AgentPreset
		wantCommand string
	}{
		{AgentClaude, "claude"}, // Note: claude may resolve to full path
	}

	for _, tt := range tests {
		t.Run(string(tt.preset), func(t *testing.T) {
			rc := RuntimeConfigFromPreset(tt.preset)
			// For claude, command may be full path due to resolveClaudePath
			if tt.preset == AgentClaude {
				if !isClaudeCmd(rc.Command) {
					t.Errorf("RuntimeConfigFromPreset(%s).Command = %v, want claude or path ending in /claude",
						tt.preset, rc.Command)
				}
			} else if rc.Command != tt.wantCommand {
				t.Errorf("RuntimeConfigFromPreset(%s).Command = %v, want %v",
					tt.preset, rc.Command, tt.wantCommand)
			}
		})
	}
}

func TestRuntimeConfigFromPresetReturnsNilEnvForPresetsWithoutEnv(t *testing.T) {
	t.Parallel()
	// Built-in presets like Claude don't have Env set
	// This verifies nil Env handling in RuntimeConfigFromPreset
	rc := RuntimeConfigFromPreset(AgentClaude)
	if rc == nil {
		t.Fatal("RuntimeConfigFromPreset returned nil")
	}

	// Claude preset doesn't have Env, so it should be nil
	if rc.Env != nil && len(rc.Env) > 0 {
		t.Errorf("Expected nil/empty Env for Claude preset, got %v", rc.Env)
	}
}

func TestIsKnownPreset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want bool
	}{
		{"claude", true},
		{"groq-compound", true},
		{"codex", false}, // retired with every non-Claude runtime (D4)
		{"aider", false}, // Not built-in, can be added via config
		{"unknown", false},
		{"chatgpt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsKnownPreset(tt.name); got != tt.want {
				t.Errorf("IsKnownPreset(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestLoadAgentRegistryForTown(t *testing.T) {
	t.Parallel()
	// Create temp directory for test config
	tmpDir := t.TempDir()
	configPath := DefaultAgentRegistryPath(tmpDir)
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write custom agent config
	customRegistry := AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents: map[string]*AgentPresetInfo{
			"my-agent": {
				Name:    "my-agent",
				Command: "my-agent-bin",
				Args:    []string{"--auto"},
			},
		},
	}

	data, err := json.Marshal(customRegistry)
	if err != nil {
		t.Fatalf("failed to marshal test config: %v", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	// Load should succeed
	reg, err := LoadAgentRegistryFor(tmpDir, "")
	if err != nil {
		t.Fatalf("LoadAgentRegistryFor failed: %v", err)
	}

	// Check custom agent is available
	myAgent := reg.Preset("my-agent")
	if myAgent == nil {
		t.Fatal("custom agent 'my-agent' not found after loading registry")
	}

	if myAgent.Command != "my-agent-bin" {
		t.Errorf("my-agent.Command = %v, want my-agent-bin", myAgent.Command)
	}

	// Check built-ins still accessible
	claude := reg.Preset("claude")
	if claude == nil {
		t.Fatal("built-in 'claude' not found after loading registry")
	}

	// Loading a registry changes nothing process-wide (gt-rg4f1).
	if GetAgentPresetByName("my-agent") != nil {
		t.Fatal("custom agent leaked into the built-in registry")
	}
}

func TestGetProcessNamesRespectsRegistryOverride(t *testing.T) {
	// Regression test: settings/agents.json overrides must be visible to
	// GetProcessNames so that liveness checks (IsAgentAliveChecked, daemon heartbeat,
	// cleanup) respect user-configured process names.
	// Real-world case: NixOS wraps claude as ".claude-unwrapped".
	t.Parallel()

	// Before loading any registry, GetProcessNames returns the builtin default.
	builtinNames := GetProcessNames("claude")
	if len(builtinNames) != 2 || builtinNames[0] != "node" || builtinNames[1] != "claude" {
		t.Fatalf("builtin GetProcessNames(claude) = %v, want [node claude]", builtinNames)
	}

	// Write a settings/agents.json that adds ".claude-unwrapped" to process_names.
	tmpDir := t.TempDir()
	configPath := DefaultAgentRegistryPath(tmpDir)
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	customRegistry := AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents: map[string]*AgentPresetInfo{
			"claude": {
				Name:         "claude",
				Command:      "claude",
				Args:         []string{"--dangerously-skip-permissions"},
				ProcessNames: []string{"node", "claude", ".claude-unwrapped"},
			},
		},
	}

	data, err := json.Marshal(customRegistry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(configPath, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The town's registry must return the override.
	reg, err := LoadAgentRegistryFor(tmpDir, "")
	if err != nil {
		t.Fatalf("LoadAgentRegistryFor: %v", err)
	}

	got := reg.ProcessNames("claude")
	want := []string{"node", "claude", ".claude-unwrapped"}
	if len(got) != len(want) {
		t.Fatalf("ProcessNames(claude) = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("GetProcessNames(claude)[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveProcessNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		agentName string
		command   string
		want      []string
	}{
		{
			name:      "built-in preset with matching command",
			agentName: "claude",
			command:   "claude",
			want:      []string{"node", "claude"},
		},
		{
			name:      "unknown agent with known command",
			agentName: "my-custom-agent",
			command:   "claude",
			want:      []string{"node", "claude"},
		},
		{
			name:      "unknown agent with unknown command",
			agentName: "my-custom-agent",
			command:   "my-binary",
			want:      []string{"my-binary"},
		},
		{
			// gt-0sr: agents.claude.command pointed at a wrapper script
			// (~/gt/bin/claude-trusted) that execs the real claude binary.
			// The wrapper's name never exists as a process post-exec, so the
			// named preset's process names must be included alongside it.
			name:      "registered agent with unknown wrapper command unions preset names",
			agentName: "claude",
			command:   "claude-trusted",
			want:      []string{"claude-trusted", "node", "claude"},
		},
		{
			name:      "registered agent with path-resolved unknown wrapper command",
			agentName: "claude",
			command:   "/home/user/gt/bin/claude-trusted",
			want:      []string{"claude-trusted", "node", "claude"},
		},
		{
			name:      "union dedupes command basename already in preset names",
			agentName: "claude",
			command:   "node",
			want:      []string{"node", "claude"},
		},
		{
			name:      "path-resolved command matches built-in preset",
			agentName: "claude",
			command:   "/usr/local/bin/claude",
			want:      []string{"node", "claude"},
		},
		{
			name:      "path-resolved unknown command falls back to basename",
			agentName: "my-agent",
			command:   "/usr/local/bin/my-binary",
			want:      []string{"my-binary"},
		},
		{
			name:      "empty agent and command",
			agentName: "",
			command:   "",
			want:      []string{"node", "claude"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveProcessNames(tt.agentName, tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveProcessNames(%q, %q) = %v, want %v", tt.agentName, tt.command, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ResolveProcessNames(%q, %q)[%d] = %q, want %q", tt.agentName, tt.command, i, got[i], tt.want[i])
				}
			}
		})
	}

	// Test registry preset with absolute-path command.
	// Custom agents loaded from agents.json may store full paths in Command.
	// ResolveProcessNames must normalize both sides to match correctly.
	t.Run("registry preset with absolute-path command matches", func(t *testing.T) {
		reg := registryWith(AgentPresetInfo{
			Name:         "custom-tool",
			Command:      "/opt/bin/custom-tool",
			ProcessNames: []string{"custom-tool", "node"},
		})

		// Query with basename — should match via filepath.Base normalization
		got := reg.ResolveProcessNames("custom-tool", "custom-tool")
		want := []string{"custom-tool", "node"}
		if len(got) != len(want) {
			t.Fatalf("ResolveProcessNames with abs-path registry = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("registry preset with absolute-path command matches via command lookup", func(t *testing.T) {
		reg := registryWith(AgentPresetInfo{
			Name:         "abs-tool",
			Command:      "/usr/local/bin/special-binary",
			ProcessNames: []string{"special-binary", "helper"},
		})

		// Query with different agent name but matching command basename
		got := reg.ResolveProcessNames("unknown-agent", "special-binary")
		want := []string{"special-binary", "helper"}
		if len(got) != len(want) {
			t.Fatalf("ResolveProcessNames command-based lookup with abs-path = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	// Regression: custom agents wrapped in `env -u VAR <real-binary>` (or
	// nohup/sudo/etc.) used to fall through to GT_PROCESS_NAMES=<wrapper>,
	// which IsAgentAliveChecked could never match — wrapper has exec'd into the real
	// binary by then. ResolveProcessNames must look past the wrapper.
	wrapperCases := []struct {
		name    string
		agent   AgentPresetInfo
		want    []string
		command string // command passed to ResolveProcessNames
	}{
		{
			name: "env -u VAR claude unwraps to claude preset",
			agent: AgentPresetInfo{
				Name:    "claude",
				Command: "env",
				Args:    []string{"-u", "ANTHROPIC_API_KEY", "claude", "--dangerously-skip-permissions", "--effort", "high"},
			},
			command: "env",
			want:    []string{"node", "claude"},
		},
		{
			name: "env VAR=val claude unwraps past assignments",
			agent: AgentPresetInfo{
				Name:    "claude",
				Command: "env",
				Args:    []string{"FOO=bar", "BAZ=qux", "claude"},
			},
			command: "env",
			want:    []string{"node", "claude"},
		},
		{
			name: "env -- claude (separator) unwraps to claude",
			agent: AgentPresetInfo{
				Name:    "claude",
				Command: "env",
				Args:    []string{"-i", "--", "claude", "--foo"},
			},
			command: "env",
			want:    []string{"node", "claude"},
		},
		{
			name: "env wrapping unknown binary returns binary basename",
			agent: AgentPresetInfo{
				Name:    "my-agent",
				Command: "env",
				Args:    []string{"-u", "FOO", "/opt/my-tool", "--flag"},
			},
			command: "env",
			want:    []string{"my-tool"},
		},
	}
	for _, tc := range wrapperCases {
		t.Run(tc.name, func(t *testing.T) {
			reg := registryWith(tc.agent)

			got := reg.ResolveProcessNames(string(tc.agent.Name), tc.command, tc.agent.Args...)
			if len(got) != len(tc.want) {
				t.Fatalf("ResolveProcessNames(%q, %q) = %v, want %v", tc.agent.Name, tc.command, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}

	// Real-world scenario: Paul's town settings shadow the canonical claude
	// preset with a wrapper. The registry still holds the built-in claude
	// preset; Args live only on the caller's RuntimeConfig. Caller args must
	// take precedence so wrapper-unwrap finds the real binary.
	t.Run("caller args used when registry holds canonical preset", func(t *testing.T) {
		// Built-in registry only — it has canonical built-in claude
		// (Command="claude", ProcessNames=[node, claude], Args=[--dangerously-...]).
		got := ResolveProcessNames("claude", "env",
			"-u", "ANTHROPIC_API_KEY", "claude", "--dangerously-skip-permissions", "--effort", "high")
		want := []string{"node", "claude"}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func TestBuildResumeCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agentName string
		sessionID string
		wantEmpty bool
		contains  []string // strings that should appear in result
	}{
		{
			name:      "claude with session",
			agentName: "claude",
			sessionID: "session-123",
			wantEmpty: false,
			contains:  []string{"claude", "--dangerously-skip-permissions", "--resume", "session-123"},
		},
		{
			name:      "empty session ID",
			agentName: "claude",
			sessionID: "",
			wantEmpty: true,
			contains:  []string{"claude"},
		},
		{
			name:      "unknown agent",
			agentName: "unknown-agent",
			sessionID: "session-123",
			wantEmpty: true,
			contains:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildResumeCommand(tt.agentName, tt.sessionID)
			if tt.wantEmpty {
				if result != "" {
					t.Errorf("BuildResumeCommand(%s, %s) = %q, want empty", tt.agentName, tt.sessionID, result)
				}
				return
			}
			for _, s := range tt.contains {
				if !strings.Contains(result, s) {
					t.Errorf("BuildResumeCommand(%s, %s) = %q, missing %q", tt.agentName, tt.sessionID, result, s)
				}
			}
		})
	}
}

func TestSupportsSessionResume(t *testing.T) {
	t.Parallel()
	tests := []struct {
		agentName string
		want      bool
	}{
		{"claude", true},
		{"unknown", false},
	}

	for _, tt := range tests {
		t.Run(tt.agentName, func(t *testing.T) {
			if got := SupportsSessionResume(tt.agentName); got != tt.want {
				t.Errorf("SupportsSessionResume(%s) = %v, want %v", tt.agentName, got, tt.want)
			}
		})
	}
}

func TestGetSessionIDEnvVar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		agentName string
		want      string
	}{
		{"claude", "CLAUDE_SESSION_ID"},
		{"unknown", ""},
	}

	for _, tt := range tests {
		t.Run(tt.agentName, func(t *testing.T) {
			if got := GetSessionIDEnvVar(tt.agentName); got != tt.want {
				t.Errorf("GetSessionIDEnvVar(%s) = %q, want %q", tt.agentName, got, tt.want)
			}
		})
	}
}

func TestGetProcessNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		agentName string
		want      []string
	}{
		{"claude", []string{"node", "claude"}},
		{"unknown", []string{"node", "claude"}}, // Falls back to Claude's process
	}

	for _, tt := range tests {
		t.Run(tt.agentName, func(t *testing.T) {
			got := GetProcessNames(tt.agentName)
			if len(got) != len(tt.want) {
				t.Errorf("GetProcessNames(%s) = %v, want %v", tt.agentName, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("GetProcessNames(%s)[%d] = %q, want %q", tt.agentName, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestDefaultRigAgentRegistryPath verifies that the default rig agent registry path is constructed correctly.
func TestDefaultRigAgentRegistryPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rigPath      string
		expectedPath string
	}{
		{"/Users/alice/gt/myproject", "/Users/alice/gt/myproject/settings/agents.json"},
		{"/tmp/my-rig", "/tmp/my-rig/settings/agents.json"},
		{"relative/path", "relative/path/settings/agents.json"},
	}

	for _, tt := range tests {
		t.Run(tt.rigPath, func(t *testing.T) {
			got := DefaultRigAgentRegistryPath(tt.rigPath)
			want := tt.expectedPath
			if filepath.ToSlash(got) != filepath.ToSlash(want) {
				t.Errorf("DefaultRigAgentRegistryPath(%s) = %s, want %s", tt.rigPath, got, want)
			}
		})
	}
}

// TestLoadAgentRegistryForRig verifies that a rig-level agent registry is loaded correctly.
func TestLoadAgentRegistryForRig(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	registryPath := filepath.Join(tmpDir, "settings", "agents.json")
	configDir := filepath.Join(tmpDir, "settings")

	// Create settings directory
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("failed to create settings dir: %v", err)
	}

	// Write agent registry
	registryContent := `{
  "version": 1,
  "agents": {
    "claude": {
      "command": "claude",
      "args": ["--session"]
    }
  }
}`

	if err := os.WriteFile(registryPath, []byte(registryContent), 0644); err != nil {
		t.Fatalf("failed to write registry file: %v", err)
	}

	// Test 1: Load should succeed and merge agents
	t.Run("load and merge", func(t *testing.T) {
		reg, err := LoadAgentRegistryFor("", tmpDir)
		if err != nil {
			t.Fatalf("LoadAgentRegistryFor(rig %s) failed: %v", tmpDir, err)
		}

		info := reg.Preset("claude")
		if info == nil {
			t.Fatal("expected claude agent to be available after loading rig registry")
		}

		if len(info.Args) != 1 || info.Args[0] != "--session" {
			t.Errorf("expected rig override args [--session], got %v", info.Args)
		}
		if info.SessionIDEnv != "CLAUDE_SESSION_ID" {
			t.Errorf("expected claude SessionIDEnv to inherit CLAUDE_SESSION_ID, got %q", info.SessionIDEnv)
		}
		if len(info.ProcessNames) == 0 {
			t.Errorf("expected claude ProcessNames to remain populated after partial override")
		}
		if info.ReadyDelayMs != 10000 {
			t.Errorf("expected claude ReadyDelayMs to inherit 10000, got %d", info.ReadyDelayMs)
		}
	})

	// Test 2: File not found should return nil (no error)
	t.Run("file not found", func(t *testing.T) {
		otherRig := filepath.Join(tmpDir, "other-rig")
		reg, err := LoadAgentRegistryFor("", otherRig)
		if err != nil {
			t.Errorf("LoadAgentRegistryFor(rig %s) should not error for non-existent file: %v", otherRig, err)
		}

		// A rig without its own file gets the built-in claude preset, not
		// the one loaded for another rig above (gt-rg4f1).
		info := reg.Preset("claude")
		if info == nil {
			t.Fatal("expected built-in claude preset")
		}
		if len(info.Args) == 1 && info.Args[0] == "--session" {
			t.Errorf("claude override from another rig leaked: args %v", info.Args)
		}
	})

	// Test 3: Invalid JSON should error
	t.Run("invalid JSON", func(t *testing.T) {
		badRig := filepath.Join(tmpDir, "bad-rig")
		invalidRegistryPath := filepath.Join(badRig, "settings", "agents.json")
		badConfigDir := filepath.Join(tmpDir, "bad-rig", "settings")
		if err := os.MkdirAll(badConfigDir, 0755); err != nil {
			t.Fatalf("failed to create bad-rig settings dir: %v", err)
		}

		invalidContent := `{"version": 1, "agents": {invalid json}}`
		if err := os.WriteFile(invalidRegistryPath, []byte(invalidContent), 0644); err != nil {
			t.Fatalf("failed to write invalid registry file: %v", err)
		}

		reg, err := LoadAgentRegistryFor("", badRig)
		if err == nil {
			t.Errorf("LoadAgentRegistryFor(rig %s) should error for invalid JSON: got nil", badRig)
		}
		if reg == nil || reg.Preset("claude") == nil {
			t.Errorf("registry with a bad rig file should still hold the built-ins")
		}
	})
}
