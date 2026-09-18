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

func TestResolveRoleAgentConfigWithOverrideAppliesRoleFlags(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	ts := NewTownSettings()
	ts.DefaultAgent = "claude"
	ts.Agents = map[string]*RuntimeConfig{
		"deepseek-flash": {Command: "claude", Args: []string{"--model", "deepseek-flash"}, Provider: "claude"},
	}
	if err := SaveTownSettings(TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	if err := SaveRigSettings(RigSettingsPath(rigPath), NewRigSettings()); err != nil {
		t.Fatal(err)
	}

	// Polecat: per-agent file under the shared polecats settings dir.
	polecatPath := SystemPromptFilePath("polecat", townRoot, rigPath, "marble")
	if err := os.MkdirAll(filepath.Dir(polecatPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(polecatPath, []byte("# polecat marble\n"), 0644); err != nil {
		t.Fatal(err)
	}

	rc, err := ResolveRoleAgentConfigWithOverride("polecat", townRoot, rigPath, "deepseek-flash", "marble")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rc.ResolvedAgent != "deepseek-flash" && !containsArg(rc.Args, "deepseek-flash") {
		t.Errorf("override agent not honoured: %+v", rc)
	}
	if !containsArgPair(rc.Args, "--append-system-prompt-file", polecatPath) {
		t.Errorf("polecat override config lacks the per-agent flag: %v", rc.Args)
	}
	if rc.Env[EnvSystemPromptFile] != polecatPath {
		t.Errorf("env %s = %q, want %q", EnvSystemPromptFile, rc.Env[EnvSystemPromptFile], polecatPath)
	}

	// Deacon: town-level role, empty rigPath, no agent name.
	deaconPath := SystemPromptFilePath("deacon", townRoot, "", "")
	if err := os.MkdirAll(filepath.Dir(deaconPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deaconPath, []byte("# deacon\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rc, err = ResolveRoleAgentConfigWithOverride("deacon", townRoot, "", "deepseek-flash", "")
	if err != nil {
		t.Fatalf("resolve deacon: %v", err)
	}
	if !containsArgPair(rc.Args, "--append-system-prompt-file", deaconPath) {
		t.Errorf("deacon override config lacks the flag: %v", rc.Args)
	}

	// Missing file: unchanged config, no error.
	rc, err = ResolveRoleAgentConfigWithOverride("witness", townRoot, rigPath, "deepseek-flash", "")
	if err != nil {
		t.Fatalf("resolve witness: %v", err)
	}
	if containsArg(rc.Args, "--append-system-prompt-file") {
		t.Errorf("witness has no file yet but got the flag: %v", rc.Args)
	}

	// Unknown agent: error, like ResolveAgentConfigWithOverride.
	if _, err := ResolveRoleAgentConfigWithOverride("deacon", townRoot, "", "no-such-agent", ""); err == nil {
		t.Error("expected an error for an unknown agent override")
	}

	// The settings flag rides the same path, and applying the resolver's
	// output a second time must not duplicate either flag.
	rc, err = ResolveRoleAgentConfigWithOverride("polecat", townRoot, rigPath, "deepseek-flash", "marble")
	if err != nil {
		t.Fatalf("resolve polecat again: %v", err)
	}
	wantSettings := filepath.Join(RoleSettingsDir("polecat", rigPath), ".claude", "settings.json")
	if !containsArgPair(rc.Args, "--settings", wantSettings) {
		t.Errorf("polecat override config lacks --settings %s: %v", wantSettings, rc.Args)
	}
	rc = withRoleSettingsFlag(withRoleSystemPromptFlag(rc, "polecat", townRoot, rigPath, "marble"), "polecat", rigPath)
	if n := countArg(rc.Args, "--settings"); n != 1 {
		t.Errorf("--settings appears %d times after re-application, want 1: %v", n, rc.Args)
	}
	if n := countArg(rc.Args, "--append-system-prompt-file"); n != 1 {
		t.Errorf("--append-system-prompt-file appears %d times after re-application, want 1: %v", n, rc.Args)
	}

	// Crew: per-worker file, same as polecat.
	crewPath := SystemPromptFilePath("crew", townRoot, rigPath, "sloan")
	if err := os.MkdirAll(filepath.Dir(crewPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crewPath, []byte("# crew sloan\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rc, err = ResolveRoleAgentConfigWithOverride("crew", townRoot, rigPath, "deepseek-flash", "sloan")
	if err != nil {
		t.Fatalf("resolve crew: %v", err)
	}
	if !containsArgPair(rc.Args, "--append-system-prompt-file", crewPath) {
		t.Errorf("crew override config lacks the per-worker flag: %v", rc.Args)
	}

	// Empty override: falls back to role_agents and still carries the flags
	// exactly once (ResolveRoleAgentConfig already applies them).
	ts.RoleAgents = map[string]string{"witness": "deepseek-flash"}
	if err := SaveTownSettings(TownSettingsPath(townRoot), ts); err != nil {
		t.Fatal(err)
	}
	witnessPath := SystemPromptFilePath("witness", townRoot, rigPath, "")
	if err := os.MkdirAll(filepath.Dir(witnessPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(witnessPath, []byte("# witness\n"), 0644); err != nil {
		t.Fatal(err)
	}
	rc, err = ResolveRoleAgentConfigWithOverride("witness", townRoot, rigPath, "", "")
	if err != nil {
		t.Fatalf("resolve witness without override: %v", err)
	}
	if !containsArg(rc.Args, "deepseek-flash") {
		t.Errorf("empty override did not fall back to role_agents: %v", rc.Args)
	}
	if n := countArg(rc.Args, "--append-system-prompt-file"); n != 1 {
		t.Errorf("witness flag count %d, want 1: %v", n, rc.Args)
	}
	if n := countArg(rc.Args, "--settings"); n != 1 {
		t.Errorf("witness --settings count %d, want 1: %v", n, rc.Args)
	}
}

func countArg(args []string, want string) int {
	n := 0
	for _, a := range args {
		if a == want {
			n++
		}
	}
	return n
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func containsArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
