package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentEnv_Mayor(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:     "mayor",
		TownRoot: "/town",
	})

	assertEnv(t, env, "GT_ROLE", "mayor")
	assertEnv(t, env, "BD_ACTOR", "mayor")
	assertEnv(t, env, "GIT_AUTHOR_NAME", "mayor")
	assertEnv(t, env, "GT_TOWN_ROOT", "/town")
	assertEnv(t, env, "GT_ROOT", "/town") // the alias bd reads until it migrates (gt-syhch)
	assertEnv(t, env, "GIT_CEILING_DIRECTORIES", "/town") // prevents git walking to umbrella
	assertEnv(t, env, "NODE_OPTIONS", "")                 // cleared to prevent debugger inheritance
	assertEnv(t, env, "CLAUDECODE", "")                   // cleared to prevent nested session detection
	assertNotSet(t, env, "GT_RIG")
}

func TestAgentEnv_Polecat(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
	})

	assertEnv(t, env, "GT_ROLE", "myrig/polecats/Toast") // compound format
	assertEnv(t, env, "GT_RIG", "myrig")
	assertEnv(t, env, "GT_POLECAT", "Toast")
	assertEnv(t, env, "BD_ACTOR", "myrig/polecats/Toast")
	assertEnv(t, env, "GIT_AUTHOR_NAME", "Toast")
	assertEnv(t, env, "BEADS_AGENT_NAME", "myrig/Toast")
	assertEnv(t, env, "BD_DOLT_AUTO_COMMIT", "off") // gt-5cc2p: prevent manifest contention
	assertEnv(t, env, "NODE_OPTIONS", "")           // cleared to prevent debugger inheritance
	assertEnv(t, env, "CLAUDECODE", "")             // cleared to prevent nested session detection
}

// A pin's provenance decides how a handoff treats it: only an explicit --agent
// override outranks role_agents, so only an override spawn writes the marker.
// A GT_AGENT that came from role resolution must stay indistinguishable from a
// stale one, or changing role_agents would still be unreachable (gt-di8p).
func TestAgentEnv_AgentOverrideMarker(t *testing.T) {
	t.Parallel()

	overridden := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
		Agent:     "codex",
	})
	assertEnv(t, overridden, EnvAgent, "codex")
	assertEnv(t, overridden, EnvAgentOverride, "1")

	resolved := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
	})
	assertNotSet(t, resolved, EnvAgent)
	assertNotSet(t, resolved, EnvAgentOverride)
}

func TestAgentEnv_Crew(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:      "crew",
		Rig:       "myrig",
		AgentName: "emma",
		TownRoot:  "/town",
	})

	assertEnv(t, env, "GT_ROLE", "myrig/crew/emma") // compound format
	assertEnv(t, env, "GT_RIG", "myrig")
	assertEnv(t, env, "GT_CREW", "emma")
	assertEnv(t, env, "BD_ACTOR", "myrig/crew/emma")
	assertEnv(t, env, "GIT_AUTHOR_NAME", "emma")
	assertEnv(t, env, "BEADS_AGENT_NAME", "myrig/emma")
}

// TestIdentityEnvVars_CoversAgentEnvOutput verifies that IdentityEnvVars contains
// all identity-bearing keys that AgentEnv can produce. If AgentEnv gains a new
// identity key, this test fails to remind you to add it to IdentityEnvVars.
func TestIdentityEnvVars_CoversAgentEnvOutput(t *testing.T) {
	t.Parallel()

	// Collect all identity keys produced by AgentEnv across all role types.
	// Identity keys are role/rig/agent-specific — NOT infrastructure keys like
	// GT_TOWN_ROOT, NODE_OPTIONS, CLAUDECODE, etc.
	identityKeys := map[string]bool{
		"GT_ROLE": true, "GT_RIG": true, "GT_CREW": true,
		"GT_POLECAT": true, "GT_DOG_NAME": true, "GT_SESSION": true,
		"GT_AGENT": true, "BD_ACTOR": true, "GIT_AUTHOR_NAME": true,
		"BEADS_AGENT_NAME": true,
	}

	have := make(map[string]bool, len(IdentityEnvVars))
	for _, k := range IdentityEnvVars {
		have[k] = true
	}

	for k := range identityKeys {
		if !have[k] {
			t.Errorf("IdentityEnvVars is missing %q — add it to prevent identity leakage (GH#3006)", k)
		}
	}
}

func TestAgentEnv_WithRuntimeConfigDir(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:             "polecat",
		Rig:              "myrig",
		AgentName:        "Toast",
		TownRoot:         "/town",
		RuntimeConfigDir: "/home/user/.config/claude",
	})

	assertEnv(t, env, "CLAUDE_CONFIG_DIR", "/home/user/.config/claude")
}

func TestAgentEnv_WithoutRuntimeConfigDir(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
	})

	assertNotSet(t, env, "CLAUDE_CONFIG_DIR")
}

func TestAgentEnvSimple(t *testing.T) {
	t.Parallel()
	env := AgentEnvSimple("polecat", "myrig", "Toast")

	assertEnv(t, env, "GT_ROLE", "myrig/polecats/Toast") // compound format
	assertEnv(t, env, "GT_RIG", "myrig")
	assertEnv(t, env, "GT_POLECAT", "Toast")
	// Simple doesn't set TownRoot, so key should be absent
	// (not empty string which would override tmux session environment)
	assertNotSet(t, env, "GT_TOWN_ROOT")
	assertNotSet(t, env, "GT_ROOT")
}

func TestAgentEnv_EmptyTownRootOmitted(t *testing.T) {
	t.Parallel()
	// Regression test: empty TownRoot should NOT create keys in the map.
	// If it was set to empty string, ExportPrefix would generate
	// "export GT_TOWN_ROOT= ..." which overrides tmux session environment
	// where it's correctly set.
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "", // explicitly empty
	})

	// Key should be absent, not empty string
	assertNotSet(t, env, "GT_TOWN_ROOT")
	assertNotSet(t, env, "GT_ROOT")
	assertNotSet(t, env, "GIT_CEILING_DIRECTORIES") // also not set when TownRoot empty

	// Other keys should still be set
	assertEnv(t, env, "GT_ROLE", "myrig/polecats/Toast") // compound format
	assertEnv(t, env, "GT_RIG", "myrig")
}

func TestAgentEnv_WithAgentOverride(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
		Agent:     "codex",
	})

	assertEnv(t, env, "GT_AGENT", "codex")
}

func TestAgentEnv_WithoutAgentOverride(t *testing.T) {
	t.Parallel()
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
	})

	assertNotSet(t, env, "GT_AGENT")
}

// TestAgentEnv_WithoutAgentOverride_RequiresFallback documents that callers
// must set GT_AGENT from RuntimeConfig.ResolvedAgent when AgentEnvConfig.Agent
// is empty. AgentEnv intentionally omits GT_AGENT without an explicit override,
// but tmux session table consumers (IsAgentAliveChecked, GT_AGENT validation) need it.
// Regression test for PR #1776 which removed the session_manager.go fallback.
func TestAgentEnv_WithoutAgentOverride_RequiresFallback(t *testing.T) {
	t.Parallel()

	// Simulate the default polecat dispatch path (no --agent flag).
	// This is what lifecycle.go calls when gt scheduler run / gt sling dispatches.
	env := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
		Agent:     "", // no explicit override — the common case
	})

	// GT_AGENT must NOT be in the map — this confirms callers need a fallback.
	// session_manager.go must compensate by writing runtimeConfig.ResolvedAgent
	// to the tmux session table via SetEnvironment.
	if _, ok := env["GT_AGENT"]; ok {
		t.Error("AgentEnv should NOT set GT_AGENT when Agent is empty; " +
			"callers must fall back to runtimeConfig.ResolvedAgent")
	}

	// With an explicit override, GT_AGENT IS set.
	envWithOverride := AgentEnv(AgentEnvConfig{
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
		Agent:     "codex",
	})
	assertEnv(t, envWithOverride, "GT_AGENT", "codex")
}

// TestAgentEnv_AgentOverrideAllRoles verifies that GT_AGENT is emitted for
// every role that supports agent overrides. This mirrors the actual
// AgentEnvConfig constructions in each manager's Start method.
func TestAgentEnv_AgentOverrideAllRoles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  AgentEnvConfig
	}{
		{
			name: "polecat via session_manager",
			cfg: AgentEnvConfig{
				Role:      "polecat",
				Rig:       "rig1",
				AgentName: "Toast",
				TownRoot:  "/town",
				Agent:     "codex",
			},
		},
		{
			name: "witness",
			cfg: AgentEnvConfig{
				Role:     "witness",
				Rig:      "rig1",
				TownRoot: "/town",
				Agent:    "gemini",
			},
		},
		{
			name: "refinery",
			cfg: AgentEnvConfig{
				Role:     "refinery",
				Rig:      "rig1",
				TownRoot: "/town",
				Agent:    "codex",
			},
		},
		{
			name: "deacon",
			cfg: AgentEnvConfig{
				Role:     "deacon",
				TownRoot: "/town",
				Agent:    "gemini",
			},
		},
		{
			name: "crew",
			cfg: AgentEnvConfig{
				Role:             "crew",
				Rig:              "rig1",
				AgentName:        "worker1",
				TownRoot:         "/town",
				RuntimeConfigDir: "/config",
				Agent:            "codex",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := AgentEnv(tc.cfg)
			assertEnv(t, env, "GT_AGENT", tc.cfg.Agent)
		})
	}
}

// TestAgentEnv_NoAgentOverrideOmitsKey verifies GT_AGENT is absent when
// Agent is empty, for all roles. This is the default behavior.
func TestAgentEnv_NoAgentOverrideOmitsKey(t *testing.T) {
	t.Parallel()
	roles := []string{"polecat", "witness", "refinery", "deacon", "crew"}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			t.Parallel()
			env := AgentEnv(AgentEnvConfig{
				Role:     role,
				TownRoot: "/town",
			})
			assertNotSet(t, env, "GT_AGENT")
		})
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple value no quoting",
			input:    "foobar",
			expected: "foobar",
		},
		{
			name:     "alphanumeric and underscore",
			input:    "FOO_BAR_123",
			expected: "FOO_BAR_123",
		},
		// CRITICAL: These values are used by existing agents and must NOT be quoted
		{
			name:     "path with slashes (GT_ROOT, CLAUDE_CONFIG_DIR)",
			input:    "/home/user/.config/claude",
			expected: "/home/user/.config/claude", // NOT quoted
		},
		{
			name:     "BD_ACTOR with slashes",
			input:    "myrig/polecats/Toast",
			expected: "myrig/polecats/Toast", // NOT quoted
		},
		{
			name:     "value with hyphen",
			input:    "deacon-boot",
			expected: "deacon-boot", // NOT quoted
		},
		{
			name:     "value with dots",
			input:    "user.name",
			expected: "user.name", // NOT quoted
		},
		{
			name:     "value with spaces",
			input:    "hello world",
			expected: "'hello world'",
		},
		{
			name:     "value with double quotes",
			input:    `say "hello"`,
			expected: `'say "hello"'`,
		},
		{
			name:     "JSON object",
			input:    `{"*":"allow"}`,
			expected: `'{"*":"allow"}'`,
		},
		{
			name:     "OPENCODE_PERMISSION value",
			input:    `{"*":"allow"}`,
			expected: `'{"*":"allow"}'`,
		},
		{
			name:     "value with single quote",
			input:    "it's a test",
			expected: `'it'\''s a test'`,
		},
		{
			name:     "value with dollar sign",
			input:    "$HOME",
			expected: "'$HOME'",
		},
		{
			name:     "value with backticks",
			input:    "`whoami`",
			expected: "'`whoami`'",
		},
		{
			name:     "value with asterisk",
			input:    "*.txt",
			expected: "'*.txt'",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ShellQuote(tt.input)
			if result != tt.expected {
				t.Errorf("ShellQuote(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestExportPrefix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		expected string
	}{
		{
			name:     "empty",
			env:      map[string]string{},
			expected: "",
		},
		{
			name:     "single var",
			env:      map[string]string{"FOO": "bar"},
			expected: "export FOO=bar && ",
		},
		{
			name: "multiple vars sorted",
			env: map[string]string{
				"ZZZ": "last",
				"AAA": "first",
				"MMM": "middle",
			},
			expected: "export AAA=first MMM=middle ZZZ=last && ",
		},
		{
			name: "JSON value is quoted",
			env: map[string]string{
				"OPENCODE_PERMISSION": `{"*":"allow"}`,
			},
			expected: `export OPENCODE_PERMISSION='{"*":"allow"}' && `,
		},
		{
			name: "mixed simple and complex values",
			env: map[string]string{
				"SIMPLE":  "value",
				"COMPLEX": `{"key":"val"}`,
				"GT_ROLE": "polecat",
			},
			expected: `export COMPLEX='{"key":"val"}' GT_ROLE=polecat SIMPLE=value && `,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ExportPrefix(tt.env)
			if result != tt.expected {
				t.Errorf("ExportPrefix() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestBuildStartupCommandWithEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		env      map[string]string
		agentCmd string
		prompt   string
		expected string
	}{
		{
			name:     "no env no prompt",
			env:      map[string]string{},
			agentCmd: "claude",
			prompt:   "",
			expected: "claude",
		},
		{
			name:     "env no prompt",
			env:      map[string]string{"GT_ROLE": "polecat"},
			agentCmd: "claude",
			prompt:   "",
			expected: "export GT_ROLE=polecat && claude",
		},
		{
			name:     "env with prompt",
			env:      map[string]string{"GT_ROLE": "polecat"},
			agentCmd: "claude",
			prompt:   "gt prime",
			expected: `export GT_ROLE=polecat && claude "gt prime"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildStartupCommandWithEnv(tt.env, tt.agentCmd, tt.prompt)
			if result != tt.expected {
				t.Errorf("BuildStartupCommandWithEnv() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestMergeEnv(t *testing.T) {
	t.Parallel()
	a := map[string]string{"A": "1", "B": "2"}
	b := map[string]string{"B": "override", "C": "3"}

	result := MergeEnv(a, b)

	assertEnv(t, result, "A", "1")
	assertEnv(t, result, "B", "override")
	assertEnv(t, result, "C", "3")
}

func TestFilterEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{"A": "1", "B": "2", "C": "3"}

	result := FilterEnv(env, "A", "C")

	assertEnv(t, result, "A", "1")
	assertNotSet(t, result, "B")
	assertEnv(t, result, "C", "3")
}

func TestWithoutEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{"A": "1", "B": "2", "C": "3"}

	result := WithoutEnv(env, "B")

	assertEnv(t, result, "A", "1")
	assertNotSet(t, result, "B")
	assertEnv(t, result, "C", "3")
}

func TestEnvToSlice(t *testing.T) {
	t.Parallel()
	env := map[string]string{"A": "1", "B": "2"}

	result := EnvToSlice(env)

	if len(result) != 2 {
		t.Errorf("EnvToSlice() returned %d items, want 2", len(result))
	}

	// Check both entries exist (order not guaranteed)
	found := make(map[string]bool)
	for _, s := range result {
		found[s] = true
	}
	if !found["A=1"] || !found["B=2"] {
		t.Errorf("EnvToSlice() = %v, want [A=1, B=2]", result)
	}
}

// Helper functions

func assertEnv(t *testing.T, env map[string]string, key, expected string) {
	t.Helper()
	if got := env[key]; got != expected {
		t.Errorf("env[%q] = %q, want %q", key, got, expected)
	}
}

func assertNotSet(t *testing.T, env map[string]string, key string) {
	t.Helper()
	if _, ok := env[key]; ok {
		t.Errorf("env[%q] should not be set, but is %q", key, env[key])
	}
}

func TestSanitizeAgentEnv(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		resolvedEnv map[string]string
		callerEnv   map[string]string
		wantKey     bool   // expect NODE_OPTIONS to be present in resolvedEnv
		wantValue   string // expected value if present
	}{
		{
			name:        "neither map has NODE_OPTIONS — sets empty",
			resolvedEnv: map[string]string{"GT_ROLE": "polecat"},
			callerEnv:   map[string]string{"GT_ROLE": "polecat"},
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "caller provides NODE_OPTIONS — preserved",
			resolvedEnv: map[string]string{"NODE_OPTIONS": "--max-old-space-size=4096"},
			callerEnv:   map[string]string{"NODE_OPTIONS": "--max-old-space-size=4096"},
			wantKey:     true,
			wantValue:   "--max-old-space-size=4096",
		},
		{
			name:        "rc.Env provides NODE_OPTIONS in resolvedEnv — preserved",
			resolvedEnv: map[string]string{"NODE_OPTIONS": "--max-old-space-size=8192"},
			callerEnv:   map[string]string{},
			wantKey:     true,
			wantValue:   "--max-old-space-size=8192",
		},
		{
			name:        "empty maps — sets empty",
			resolvedEnv: map[string]string{},
			callerEnv:   map[string]string{},
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "same map without NODE_OPTIONS — sets empty (lifecycle.go pattern)",
			resolvedEnv: map[string]string{"GT_ROLE": "polecat", "GT_RIG": "myrig"},
			callerEnv:   nil, // will be set to same map below
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "AgentEnv output with empty callerEnv — preserves empty NODE_OPTIONS",
			resolvedEnv: map[string]string{"GT_ROLE": "polecat", "NODE_OPTIONS": ""},
			callerEnv:   map[string]string{},
			wantKey:     true,
			wantValue:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callerEnv := tt.callerEnv
			if callerEnv == nil {
				// same-map pattern: pass resolvedEnv as both args (lifecycle.go pattern)
				callerEnv = tt.resolvedEnv
			}
			SanitizeAgentEnv(tt.resolvedEnv, callerEnv)
			val, ok := tt.resolvedEnv["NODE_OPTIONS"]
			if ok != tt.wantKey {
				t.Errorf("NODE_OPTIONS present=%v, want %v", ok, tt.wantKey)
			}
			if ok && val != tt.wantValue {
				t.Errorf("NODE_OPTIONS=%q, want %q", val, tt.wantValue)
			}
		})
	}
}

func TestSanitizeAgentEnv_ClearsClaudeCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		resolvedEnv map[string]string
		callerEnv   map[string]string
		wantKey     bool   // expect CLAUDECODE to be present in resolvedEnv
		wantValue   string // expected value if present
	}{
		{
			name:        "neither map has CLAUDECODE — sets empty",
			resolvedEnv: map[string]string{"GT_ROLE": "polecat"},
			callerEnv:   map[string]string{"GT_ROLE": "polecat"},
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "caller provides CLAUDECODE — preserved",
			resolvedEnv: map[string]string{"CLAUDECODE": "1"},
			callerEnv:   map[string]string{"CLAUDECODE": "1"},
			wantKey:     true,
			wantValue:   "1",
		},
		{
			name:        "inherited CLAUDECODE not in callerEnv — cleared",
			resolvedEnv: map[string]string{"CLAUDECODE": "1"},
			callerEnv:   map[string]string{},
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "empty maps — sets empty",
			resolvedEnv: map[string]string{},
			callerEnv:   map[string]string{},
			wantKey:     true,
			wantValue:   "",
		},
		{
			name:        "same map without CLAUDECODE — sets empty (lifecycle.go pattern)",
			resolvedEnv: map[string]string{"GT_ROLE": "polecat", "GT_RIG": "myrig"},
			callerEnv:   nil, // will be set to same map below
			wantKey:     true,
			wantValue:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			callerEnv := tt.callerEnv
			if callerEnv == nil {
				callerEnv = tt.resolvedEnv
			}
			SanitizeAgentEnv(tt.resolvedEnv, callerEnv)
			val, ok := tt.resolvedEnv["CLAUDECODE"]
			if ok != tt.wantKey {
				t.Errorf("CLAUDECODE present=%v, want %v", ok, tt.wantKey)
			}
			if ok && val != tt.wantValue {
				t.Errorf("CLAUDECODE=%q, want %q", val, tt.wantValue)
			}
		})
	}
}

func TestSanitizeAgentEnv_ClearsBDTargetSelectors(t *testing.T) {
	t.Parallel()

	resolvedEnv := map[string]string{
		"GT_DOLT_PORT":             "13307",
		"GT_DOLT_HOST":             "dolt.example",
		"BEADS_DOLT_PORT":          "13307",
		"BEADS_DOLT_SERVER_PORT":   "13307",
		"BEADS_DOLT_SERVER_HOST":   "dolt.example",
		"BEADS_DOLT_AUTO_START":    "0",
		"BEADS_DOLT_SERVER_SOCKET": "/tmp/stale.sock",
	}
	callerEnv := map[string]string{}
	for _, key := range bdTargetSelectorEnvVars {
		resolvedEnv[key] = "stale"
		callerEnv[key] = "caller-stale"
	}

	SanitizeAgentEnv(resolvedEnv, callerEnv)

	for _, key := range bdTargetSelectorEnvVars {
		assertEnv(t, resolvedEnv, key, "")
	}
	assertEnv(t, resolvedEnv, "GT_DOLT_PORT", "13307")
	assertEnv(t, resolvedEnv, "GT_DOLT_HOST", "dolt.example")
	assertEnv(t, resolvedEnv, "BEADS_DOLT_PORT", "13307")
	assertEnv(t, resolvedEnv, "BEADS_DOLT_SERVER_PORT", "13307")
	assertEnv(t, resolvedEnv, "BEADS_DOLT_SERVER_HOST", "dolt.example")
	assertEnv(t, resolvedEnv, "BEADS_DOLT_AUTO_START", "0")
}

func TestAgentEnv_ExcludesAnthropicBaseURL(t *testing.T) {
	t.Parallel()
	// Even when ANTHROPIC_BASE_URL is set in the process environment,
	// AgentEnv must NOT forward it. Agents that need a custom base URL
	// get it from their agent config's Env block (rc.Env), not inheritance.
	// Passthrough caused cross-provider contamination: a MiniMax deacon's
	// base URL leaked into Claude polecats, causing 401 auth failures.
	getenv := envOf("ANTHROPIC_BASE_URL", "https://api.minimax.io/anthropic")

	env := AgentEnv(AgentEnvConfig{Getenv: getenv, Role: "polecat", Rig: "testrig", AgentName: "ember"})
	if val, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("AgentEnv should not forward ANTHROPIC_BASE_URL, got %q", val)
	}
}

func TestAgentEnv_IncludesNodeOptionsClearing(t *testing.T) {
	t.Parallel()
	// Verify AgentEnv always includes NODE_OPTIONS="" regardless of role.
	// This protects tmux SetEnvironment and EnvForExecCommand paths.
	roles := []struct {
		role      string
		rig       string
		agentName string
	}{
		{"mayor", "", ""},
		{"deacon", "", ""},
		{"boot", "", ""},
		{"witness", "myrig", ""},
		{"refinery", "myrig", ""},
		{"polecat", "myrig", "Toast"},
		{"crew", "myrig", "emma"},
	}
	for _, r := range roles {
		t.Run(r.role, func(t *testing.T) {
			env := AgentEnv(AgentEnvConfig{
				Role:      r.role,
				Rig:       r.rig,
				AgentName: r.agentName,
				TownRoot:  "/town",
			})
			assertEnv(t, env, "NODE_OPTIONS", "")
		})
	}
}

func TestAgentEnv_IncludesClaudeCodeClearing(t *testing.T) {
	t.Parallel()
	// Verify AgentEnv always includes CLAUDECODE="" regardless of role.
	// This prevents nested session detection when gt sling is invoked
	// from within a Claude Code session (issue #1666).
	roles := []struct {
		role      string
		rig       string
		agentName string
	}{
		{"mayor", "", ""},
		{"deacon", "", ""},
		{"boot", "", ""},
		{"witness", "myrig", ""},
		{"refinery", "myrig", ""},
		{"polecat", "myrig", "Toast"},
		{"crew", "myrig", "emma"},
	}
	for _, r := range roles {
		t.Run(r.role, func(t *testing.T) {
			env := AgentEnv(AgentEnvConfig{
				Role:      r.role,
				Rig:       r.rig,
				AgentName: r.agentName,
				TownRoot:  "/town",
			})
			assertEnv(t, env, "CLAUDECODE", "")
		})
	}
}

func TestAgentEnv_ClearsBDTargetSelectors(t *testing.T) {
	t.Parallel()
	var kv []string
	for _, key := range bdTargetSelectorEnvVars {
		kv = append(kv, key, "stale")
	}
	getenv := envOf(kv...)

	env := AgentEnv(AgentEnvConfig{
		Getenv:    getenv,
		Role:      "polecat",
		Rig:       "myrig",
		AgentName: "Toast",
		TownRoot:  "/town",
	})
	for _, key := range bdTargetSelectorEnvVars {
		assertEnv(t, env, key, "")
	}
	assertEnv(t, env, "BEADS_DOLT_AUTO_START", "0")
}

func TestAgentEnv_DisablesBdBackup(t *testing.T) {
	t.Parallel()
	// Verify AgentEnv always includes BD_BACKUP_ENABLED=false regardless of role.
	// In Gas Town, Dolt is the persistent data store and the daemon provides
	// centralized backups (nightly Dolt backup, jsonl_git_backup). bd's per-repo
	// auto-backup is redundant and pollutes rig git history via git add -f.
	// See: https://github.com/steveyegge/beads/issues/2241
	roles := []struct {
		role      string
		rig       string
		agentName string
	}{
		{"mayor", "", ""},
		{"deacon", "", ""},
		{"boot", "", ""},
		{"witness", "myrig", ""},
		{"refinery", "myrig", ""},
		{"polecat", "myrig", "Toast"},
		{"crew", "myrig", "emma"},
	}
	for _, r := range roles {
		t.Run(r.role, func(t *testing.T) {
			env := AgentEnv(AgentEnvConfig{
				Role:      r.role,
				Rig:       r.rig,
				AgentName: r.agentName,
				TownRoot:  "/town",
			})
			assertEnv(t, env, "BD_BACKUP_ENABLED", "false")
		})
	}
}

// AgentEnv takes the Dolt endpoint from the town's config, never from the
// environment it inherits (gt-y3pgh.3).
func TestAgentEnv_IgnoresInheritedDoltEndpoint(t *testing.T) {
	t.Parallel()
	getenv := envOf("GT_DOLT_PORT", "13307", "GT_DOLT_HOST", "127.0.0.2",
		"BEADS_DOLT_SERVER_PORT", "88888", "BEADS_DOLT_PORT", "99999", "BEADS_DOLT_SERVER_HOST", "stale-host")
	env := AgentEnv(AgentEnvConfig{Getenv: getenv, Role: "polecat", Rig: "myrig", AgentName: "Toast", TownRoot: t.TempDir()})
	for _, key := range append([]string{"GT_DOLT_HOST", "GT_DOLT_PORT"}, DoltEndpointEnvKeys...) {
		assertNotSet(t, env, key)
	}
}

func TestBuildStartupCommandWithEnv_IncludesNodeOptions(t *testing.T) {
	t.Parallel()
	// Integration test: verify BuildStartupCommandWithEnv output includes NODE_OPTIONS=
	// when the env map has it set to empty (as AgentEnv produces).
	env := map[string]string{
		"GT_ROLE":      "polecat",
		"NODE_OPTIONS": "",
	}
	result := BuildStartupCommandWithEnv(env, "claude", "")
	expected := "export GT_ROLE=polecat NODE_OPTIONS= && claude"
	if result != expected {
		t.Errorf("BuildStartupCommandWithEnv() = %q, want %q", result, expected)
	}
}

// ---------------------------------------------------------------------------
// Dolt port injection tests (GH #2405 / GH #2406)
// ---------------------------------------------------------------------------

func TestParsePortFromConfigYAML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		want int
	}{
		{
			name: "standard gt-generated config",
			yaml: "log_level: warning\n\nlistener:\n  port: 3307\n  max_connections: 1000\n",
			want: 3307,
		},
		{
			name: "custom port",
			yaml: "listener:\n  port: 3308\n",
			want: 3308,
		},
		{
			name: "no listener block",
			yaml: "log_level: warning\n",
			want: 0,
		},
		{
			name: "listener without port",
			yaml: "listener:\n  max_connections: 1000\n",
			want: 0,
		},
		{
			name: "empty file",
			yaml: "",
			want: 0,
		},
		{
			name: "port in non-listener block ignored",
			yaml: "other:\n  port: 9999\n",
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parsePortFromConfigYAML([]byte(tt.yaml))
			if got != tt.want {
				t.Errorf("parsePortFromConfigYAML() = %d, want %d", got, tt.want)
			}
		})
	}
}

// writeDoltTown writes a town with the given mayor/town.json and
// .dolt-data/config.yaml bodies; an empty body leaves that file out.
func writeDoltTown(t *testing.T, townJSON, configYAML string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range map[string]string{"mayor/town.json": townJSON, ".dolt-data/config.yaml": configYAML} {
		if body == "" {
			continue
		}
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const testTownJSON = `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"`

func TestResolveDoltEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		townJSON, yamlBody string
		want               DoltEndpoint
		wantOK             bool
	}{
		{"town.json wins over config.yaml", testTownJSON + `,"dolt":{"host":"127.0.0.2","port":5507}}`, "listener:\n  host: 127.0.0.9\n  port: 3309\n", DoltEndpoint{Host: "127.0.0.2", Port: 5507}, true},
		{"config.yaml when town.json has no endpoint", testTownJSON + `}`, "listener:\n  host: 127.0.0.2\n  port: 3309\n", DoltEndpoint{Host: "127.0.0.2", Port: 3309}, true},
		{"config.yaml without host", "", "listener:\n  port: 3309\n", DoltEndpoint{Port: 3309}, true},
		{"no endpoint anywhere", testTownJSON + `}`, "", DoltEndpoint{}, false},
		{"config.yaml without a port", "", "log_level: warning\n", DoltEndpoint{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := ResolveDoltEndpoint(writeDoltTown(t, tt.townJSON, tt.yamlBody))
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ResolveDoltEndpoint = %+v, %v; want %+v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
	if _, ok := ResolveDoltEndpoint(""); ok {
		t.Error(`ResolveDoltEndpoint("") ok`)
	}
}

// The transient state file is not config: a town with only a running
// server's state has no endpoint.
func TestResolveDoltEndpoint_IgnoresStateFile(t *testing.T) {
	t.Parallel()
	root := writeDoltTown(t, "", "")
	if err := os.MkdirAll(filepath.Join(root, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "daemon", "dolt-state.json"), []byte(`{"running":true,"port":4417}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ep, ok := ResolveDoltEndpoint(root); ok {
		t.Errorf("ResolveDoltEndpoint = %+v, want no endpoint", ep)
	}
}

func TestNormalizeConfiguredDoltEnv_TownEndpointReplacesStaleEnv(t *testing.T) {
	t.Parallel()
	root := writeDoltTown(t, testTownJSON+`,"dolt":{"host":"127.0.0.2","port":5507}}`, "")
	got := envSliceMap(NormalizeConfiguredDoltEnv([]string{
		"GT_DOLT_HOST=stale-host",
		"GT_DOLT_PORT=9999",
		"BEADS_DOLT_SERVER_HOST=stale-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"BEADS_DOLT_PORT=9999",
		"KEEP=1",
	}, root))
	// GT_DOLT_* is no longer exported (gt-y3pgh.9), so a stale inherited
	// value passes through untouched: nothing reads it.
	want := map[string]string{
		"GT_DOLT_HOST": "stale-host", "BEADS_DOLT_SERVER_HOST": "127.0.0.2",
		"GT_DOLT_PORT": "9999", "BEADS_DOLT_SERVER_PORT": "5507", "BEADS_DOLT_PORT": "5507",
		"KEEP": "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeConfiguredDoltEnv = %v, want %v", got, want)
	}
}

func TestNormalizeConfiguredDoltEnv_EndpointWithoutHostClearsStaleHost(t *testing.T) {
	t.Parallel()
	root := writeDoltTown(t, "", "listener:\n  port: 5507\n")
	got := envSliceMap(NormalizeConfiguredDoltEnv([]string{"BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_PORT=9999"}, root))
	want := map[string]string{"BEADS_DOLT_SERVER_PORT": "5507", "BEADS_DOLT_PORT": "5507"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NormalizeConfiguredDoltEnv = %v, want %v", got, want)
	}
}

func TestNormalizeConfiguredDoltEnv_NoEndpointLeavesBase(t *testing.T) {
	t.Parallel()
	base := []string{"GT_DOLT_PORT=1", "BEADS_DOLT_PORT=1", "KEEP=1"}
	if got := NormalizeConfiguredDoltEnv(base, t.TempDir()); !reflect.DeepEqual(got, base) {
		t.Fatalf("NormalizeConfiguredDoltEnv = %v, want base unchanged", got)
	}
}

func TestConfiguredDoltEnv(t *testing.T) {
	t.Parallel()
	got := ConfiguredDoltEnv(writeDoltTown(t, "", "listener:\n  port: 5507\n"))
	want := map[string]string{"BEADS_DOLT_SERVER_PORT": "5507", "BEADS_DOLT_PORT": "5507"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfiguredDoltEnv = %v, want %v", got, want)
	}
	if got := ConfiguredDoltEnv(t.TempDir()); len(got) != 0 {
		t.Fatalf("ConfiguredDoltEnv(no endpoint) = %v, want empty", got)
	}
}

func envSliceMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestAgentEnv_InjectsDoltPort(t *testing.T) {
	t.Parallel()
	getenv := envOf("BEADS_DOLT_SERVER_HOST", "stale-host")
	tmpDir := t.TempDir()
	doltDataDir := filepath.Join(tmpDir, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(doltDataDir, "config.yaml"),
		[]byte("listener:\n  host: 127.0.0.2\n  port: 3307\n"),
		0644,
	); err != nil {
		t.Fatal(err)
	}

	roles := []struct {
		name string
		cfg  AgentEnvConfig
	}{
		{"mayor", AgentEnvConfig{Role: "mayor", TownRoot: tmpDir}},
		{"witness", AgentEnvConfig{Role: "witness", Rig: "myrig", TownRoot: tmpDir}},
		{"refinery", AgentEnvConfig{Role: "refinery", Rig: "myrig", TownRoot: tmpDir}},
		{"polecat", AgentEnvConfig{Role: "polecat", Rig: "myrig", AgentName: "Toast", TownRoot: tmpDir}},
		{"crew", AgentEnvConfig{Role: "crew", Rig: "myrig", AgentName: "emma", TownRoot: tmpDir}},
		{"deacon", AgentEnvConfig{Role: "deacon", TownRoot: tmpDir}},
	}

	for _, tc := range roles {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.Getenv = getenv
			env := AgentEnv(cfg)
			assertEnv(t, env, "BEADS_DOLT_SERVER_HOST", "127.0.0.2")
			assertNotSet(t, env, "GT_DOLT_HOST")
			assertNotSet(t, env, "GT_DOLT_PORT")
			assertEnv(t, env, "BEADS_DOLT_SERVER_PORT", "3307")
			assertEnv(t, env, "BEADS_DOLT_PORT", "3307")
		})
	}
}

func TestAgentEnv_NoDoltPortWithoutTownRoot(t *testing.T) {
	t.Parallel()
	getenv := envOf()
	env := AgentEnv(AgentEnvConfig{
		Getenv: getenv,
		Role:   "mayor",
	})
	assertNotSet(t, env, "GT_DOLT_PORT")
	assertNotSet(t, env, "BEADS_DOLT_PORT")
}

func TestAgentEnv_NoDoltPortWithoutConfig(t *testing.T) {
	t.Parallel()
	getenv := envOf()
	tmpDir := t.TempDir()
	env := AgentEnv(AgentEnvConfig{
		Getenv:   getenv,
		Role:     "mayor",
		TownRoot: tmpDir,
	})
	assertNotSet(t, env, "GT_DOLT_PORT")
	assertNotSet(t, env, "BEADS_DOLT_PORT")
}

func TestClaudeConfigDir_Default(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	got, err := claudeConfigDir(envOf(), func() (string, error) { return home, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(home, ".claude")
	if got != want {
		t.Errorf("ClaudeConfigDir() = %q, want %q", got, want)
	}
}

func TestClaudeConfigDir_EnvVar(t *testing.T) {
	t.Parallel()
	customDir := t.TempDir()
	getenv := envOf("CLAUDE_CONFIG_DIR", customDir)

	got, err := claudeConfigDir(getenv, func() (string, error) { return "", errors.New("home must not be read") })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != customDir {
		t.Errorf("ClaudeConfigDir() = %q, want %q", got, customDir)
	}
}

func TestAgentEnv_EffortLevel(t *testing.T) {
	t.Parallel()
	t.Run("defaults to high when no config exists", func(t *testing.T) {
		getenv := envOf()
		env := AgentEnv(AgentEnvConfig{
			Getenv:   getenv,
			Role:     "crew",
			TownRoot: "/tmp/nonexistent-town",
		})
		if got := env["CLAUDE_CODE_EFFORT_LEVEL"]; got != "high" {
			t.Errorf("CLAUDE_CODE_EFFORT_LEVEL = %q, want %q", got, "high")
		}
	})

	t.Run("ignores shell env var", func(t *testing.T) {
		// The env var is deprecated — config takes over, falling back to "high"
		getenv := envOf("CLAUDE_CODE_EFFORT_LEVEL", "max")
		env := AgentEnv(AgentEnvConfig{
			Getenv:   getenv,
			Role:     "crew",
			TownRoot: "/tmp/nonexistent-town",
		})
		if got := env["CLAUDE_CODE_EFFORT_LEVEL"]; got != "high" {
			t.Errorf("CLAUDE_CODE_EFFORT_LEVEL = %q, want %q (env var should be ignored)", got, "high")
		}
	})

	t.Run("always sets the key", func(t *testing.T) {
		getenv := envOf()
		env := AgentEnv(AgentEnvConfig{
			Getenv: getenv,
			Role:   "witness",
		})
		if _, ok := env["CLAUDE_CODE_EFFORT_LEVEL"]; !ok {
			t.Error("CLAUDE_CODE_EFFORT_LEVEL should always be set")
		}
	})
}

func TestExpandEnvRefs(t *testing.T) {
	t.Parallel()
	getenv := envOf("GT_TEST_KEY", "gsk_live_value", "GT_TEST_URL", "https://example.test/v1")

	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "nil env is returned unchanged",
			in:   nil,
			want: nil,
		},
		{
			name: "bare dollar sign is left for ShellQuote to quote",
			in:   map[string]string{"LITERAL": "$NOT_A_REF", "PLAIN": "no refs"},
			want: map[string]string{"LITERAL": "$NOT_A_REF", "PLAIN": "no refs"},
		},
		{
			name: "braced reference is replaced with the environment value",
			in:   map[string]string{"ANTHROPIC_API_KEY": "${GT_TEST_KEY}"},
			want: map[string]string{"ANTHROPIC_API_KEY": "gsk_live_value"},
		},
		{
			name: "reference embedded in a larger value is replaced in place",
			in:   map[string]string{"JOINED": "prefix-${GT_TEST_KEY}-suffix"},
			want: map[string]string{"JOINED": "prefix-gsk_live_value-suffix"},
		},
		{
			name: "several references in one value all resolve",
			in:   map[string]string{"BOTH": "${GT_TEST_KEY}@${GT_TEST_URL}"},
			want: map[string]string{"BOTH": "gsk_live_value@https://example.test/v1"},
		},
		{
			name: "unset reference expands to empty rather than the sentinel text",
			in:   map[string]string{"MISSING": "${GT_TEST_UNSET_VAR}"},
			want: map[string]string{"MISSING": ""},
		},
		{
			name: "shell parameter expansion is not a reference",
			in:   map[string]string{"SHELL": "${GT_TEST_UNSET_VAR:-fallback}"},
			want: map[string]string{"SHELL": "${GT_TEST_UNSET_VAR:-fallback}"},
		},
		{
			name: "unterminated brace is left alone",
			in:   map[string]string{"BROKEN": "${GT_TEST_KEY"},
			want: map[string]string{"BROKEN": "${GT_TEST_KEY"},
		},
		{
			name: "empty braces are not a reference",
			in:   map[string]string{"EMPTY": "${}"},
			want: map[string]string{"EMPTY": "${}"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expandEnvRefsIn(tt.in, getenv)
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("ExpandEnvRefs(nil) = %v, want empty", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ExpandEnvRefs(%v) = %v, want %v", tt.in, got, tt.want)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("ExpandEnvRefs(%v)[%q] = %q, want %q", tt.in, k, got[k], want)
				}
			}
		})
	}
}

func TestExpandEnvRefsDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	getenv := envOf("GT_TEST_KEY", "gsk_live_value")
	in := map[string]string{"ANTHROPIC_API_KEY": "${GT_TEST_KEY}"}

	expandEnvRefsIn(in, getenv)

	if in["ANTHROPIC_API_KEY"] != "${GT_TEST_KEY}" {
		t.Errorf("input mutated: ANTHROPIC_API_KEY = %q, want the reference preserved", in["ANTHROPIC_API_KEY"])
	}
}

func TestEnvRefNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "no references", in: "plain", want: nil},
		{name: "bare dollar is not a reference", in: "$PLAIN", want: nil},
		{name: "one reference", in: "${ALPHA}", want: []string{"ALPHA"}},
		{name: "duplicates collapse", in: "${ALPHA}-${ALPHA}", want: []string{"ALPHA"}},
		{name: "order of appearance is kept", in: "${BETA}${ALPHA}", want: []string{"BETA", "ALPHA"}},
		{name: "shell default syntax is not a reference", in: "${ALPHA:-x}", want: nil},
		{name: "leading digit is not a name", in: "${1ALPHA}", want: nil},
		{name: "underscores and digits are a name", in: "${A_1}", want: []string{"A_1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := envRefNames(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("envRefNames(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("envRefNames(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// envOf is a getenv over the given key/value pairs; every other variable is
// unset.
func envOf(kv ...string) func(string) string {
	m := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return func(key string) string { return m[key] }
}
