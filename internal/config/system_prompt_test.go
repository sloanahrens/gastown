package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptFilePath_PerRole(t *testing.T) {
	t.Parallel()
	town := "/town"
	rig := "/town/myrig"
	cases := map[string]string{
		"witness":  "/town/myrig/witness/.claude/system-prompt.md",
		"refinery": "/town/myrig/refinery/.claude/system-prompt.md",
		"mayor":    "/town/mayor/.claude/system-prompt.md",
		"deacon":   "/town/deacon/.claude/system-prompt.md",
		"dog":      "",
		"boot":     "",
	}
	for role, want := range cases {
		if got := SystemPromptFilePath(role, town, rig, ""); got != want {
			t.Errorf("SystemPromptFilePath(%s) = %q, want %q", role, got, want)
		}
	}
	if got := SystemPromptFilePath("witness", town, "", ""); got != "" {
		t.Errorf("rig-scoped role without rigPath must return empty, got %q", got)
	}
}

func TestWithRoleSystemPromptFlag_OnlyWhenFileExists(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")

	rc := &RuntimeConfig{Command: "claude", Args: []string{"--dangerously-skip-permissions"}}
	got := withRoleSystemPromptFlag(rc, "polecat", town, rig, "nux")
	for _, a := range got.Args {
		if a == "--append-system-prompt-file" {
			t.Fatalf("flag must not be added while the file does not exist: %v", got.Args)
		}
	}
	if _, ok := got.Env[EnvSystemPromptFile]; ok {
		t.Fatalf("env must not be set while the file does not exist: %v", got.Env)
	}

	path := SystemPromptFilePath("polecat", town, rig, "nux")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("static role text"), 0o644); err != nil {
		t.Fatal(err)
	}

	got = withRoleSystemPromptFlag(rc, "polecat", town, rig, "nux")
	found := false
	for i, a := range got.Args {
		if a == "--append-system-prompt-file" {
			found = true
			if i+1 >= len(got.Args) || got.Args[i+1] != path {
				t.Fatalf("flag value must be the file path, args = %v", got.Args)
			}
		}
	}
	if !found {
		t.Fatalf("expected --append-system-prompt-file in %v", got.Args)
	}
	if got.Env[EnvSystemPromptFile] != path {
		t.Fatalf("env %s must name the file, env = %v", EnvSystemPromptFile, got.Env)
	}

	// Idempotent: a second call must not add the flag twice.
	got = withRoleSystemPromptFlag(got, "polecat", town, rig, "nux")
	n := 0
	for _, a := range got.Args {
		if a == "--append-system-prompt-file" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("flag added %d times, want 1: %v", n, got.Args)
	}
}

func TestWithRoleSystemPromptFlag_SkipsNonClaude(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	path := SystemPromptFilePath("polecat", town, rig, "nux")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rc := &RuntimeConfig{Command: "codex", Provider: "codex"}
	got := withRoleSystemPromptFlag(rc, "polecat", town, rig, "nux")
	if len(got.Args) != 0 || len(got.Env) != 0 {
		t.Fatalf("non-Claude agents must be untouched: args=%v env=%v", got.Args, got.Env)
	}
}

func TestResolveRoleAgentConfig_AddsSystemPromptFlagWhenFileExists(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	if err := os.MkdirAll(filepath.Join(rig, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(filepath.Join(rig, "settings", "config.json"), NewRigSettings()); err != nil {
		t.Fatal(err)
	}
	path := SystemPromptFilePath("witness", town, rig, "")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := ResolveRoleAgentConfig("witness", town, rig)
	found := false
	for _, a := range rc.Args {
		if a == path {
			found = true
		}
	}
	if !found {
		t.Fatalf("ResolveRoleAgentConfig must carry the system-prompt flag, args=%v", rc.Args)
	}
	if rc.Env[EnvSystemPromptFile] != path {
		t.Fatalf("env not set: %v", rc.Env)
	}
}

func TestBuildStartupCommand_CarriesSystemPromptFlagAndEnv(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	if err := os.MkdirAll(filepath.Join(rig, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(filepath.Join(rig, "settings", "config.json"), NewRigSettings()); err != nil {
		t.Fatal(err)
	}
	path := SystemPromptFilePath("polecat", town, rig, "nux")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd, err := BuildStartupCommandFromConfig(AgentEnvConfig{Role: "polecat", Rig: "myrig", AgentName: "nux", TownRoot: town}, rig, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--append-system-prompt-file", path, EnvSystemPromptFile + "="} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("startup command missing %q:\n%s", want, cmd)
		}
	}
}

func TestSystemPromptFilePath_PerAgentForPolecatAndCrew(t *testing.T) {
	t.Parallel()
	town, rig := "/town", "/town/myrig"
	cases := []struct{ role, agent, want string }{
		{"polecat", "nux", "/town/myrig/polecats/.claude/system-prompt-nux.md"},
		{"crew", "sloan", "/town/myrig/crew/.claude/system-prompt-sloan.md"},
		{"polecat", "", ""}, // the template bakes in the agent's name; no shared file
		{"crew", "", ""},
		{"witness", "ignored", "/town/myrig/witness/.claude/system-prompt.md"},
		{"mayor", "", "/town/mayor/.claude/system-prompt.md"},
	}
	for _, c := range cases {
		if got := SystemPromptFilePath(c.role, town, rig, c.agent); got != c.want {
			t.Errorf("SystemPromptFilePath(%s,%q) = %q, want %q", c.role, c.agent, got, c.want)
		}
	}
}

func TestBuildStartupCommand_PolecatUsesPerAgentSystemPrompt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rig := filepath.Join(town, "myrig")
	if err := os.MkdirAll(filepath.Join(rig, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(filepath.Join(rig, "settings", "config.json"), NewRigSettings()); err != nil {
		t.Fatal(err)
	}
	other := SystemPromptFilePath("polecat", town, rig, "other")
	mine := SystemPromptFilePath("polecat", town, rig, "nux")
	if err := os.MkdirAll(filepath.Dir(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("someone else"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err := BuildStartupCommandFromConfig(AgentEnvConfig{Role: "polecat", Rig: "myrig", AgentName: "nux", TownRoot: town}, rig, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cmd, "--append-system-prompt-file") {
		t.Fatalf("another polecat's file must not be used:\n%s", cmd)
	}
	if err := os.WriteFile(mine, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, err = BuildStartupCommandFromConfig(AgentEnvConfig{Role: "polecat", Rig: "myrig", AgentName: "nux", TownRoot: town}, rig, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, mine) {
		t.Fatalf("expected the per-agent file %s in:\n%s", mine, cmd)
	}
}
