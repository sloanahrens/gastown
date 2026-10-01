package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// writeRigAgentOverride writes <rigPath>/settings/agents.json overriding the
// claude preset with the given command, args and process names.
func writeRigAgentOverride(t *testing.T, rigPath, command string, args, processNames []string) {
	t.Helper()
	reg := AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents: map[string]*AgentPresetInfo{
			"claude": {Name: "claude", Command: command, Args: args, ProcessNames: processNames},
		},
	}
	data, err := json.Marshal(reg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := RigAgentRegistryPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// newTwoRigTown makes a town whose rig "alpha" overrides the claude preset in
// its settings/agents.json with a wrapper command and whose rig "beta" has no
// agent registry of its own.
func newTwoRigTown(t *testing.T) (townRoot, rigA, rigB string) {
	t.Helper()
	townRoot = t.TempDir()
	rigA = filepath.Join(townRoot, "alpha")
	rigB = filepath.Join(townRoot, "beta")
	for _, rig := range []string{rigA, rigB} {
		if err := SaveRigSettings(RigSettingsPath(rig), NewRigSettings()); err != nil {
			t.Fatalf("SaveRigSettings: %v", err)
		}
	}
	writeRigAgentOverride(t, rigA, "env", []string{"-u", "ALPHA_ONLY", "claude"}, []string{"node", "claude", ".claude-alpha"})
	return townRoot, rigA, rigB
}

// gt-rg4f1: a rig's settings/agents.json must not leak into another rig
// resolved later in the same process.
func TestRigAgentRegistryDoesNotLeakAcrossRigs(t *testing.T) {
	t.Parallel()
	townRoot, rigA, rigB := newTwoRigTown(t)

	a := ResolveRoleAgentConfig(constants.RolePolecat, townRoot, rigA)
	if a.Command != "env" {
		t.Fatalf("rig alpha Command = %q, want env (its own override)", a.Command)
	}

	b := ResolveRoleAgentConfig(constants.RolePolecat, townRoot, rigB)
	if b.Command == "env" || filepath.Base(b.Command) != "claude" {
		t.Fatalf("rig beta Command = %q, want the claude binary: rig alpha's override leaked", b.Command)
	}
}

// gt-rg4f1: re-resolving a rig after another rig must reapply its own file.
func TestRigAgentRegistryReappliedAfterOtherRig(t *testing.T) {
	t.Parallel()
	townRoot, rigA, rigB := newTwoRigTown(t)
	// Beta also overrides claude, differently, so the last loader would win.
	writeRigAgentOverride(t, rigB, "nohup", []string{"claude"}, []string{"node", "claude", ".claude-beta"})

	_ = ResolveRoleAgentConfig(constants.RolePolecat, townRoot, rigA)
	_ = ResolveRoleAgentConfig(constants.RolePolecat, townRoot, rigB)
	a := ResolveRoleAgentConfig(constants.RolePolecat, townRoot, rigA)
	if a.Command != "env" {
		t.Fatalf("rig alpha re-resolved Command = %q, want env (its own override)", a.Command)
	}
}

// gt-rg4f1: GT_PROCESS_NAMES for a rig is computed from that rig's registry.
func TestStartupProcessNamesScopedToRig(t *testing.T) {
	t.Parallel()
	_, rigA, rigB := newTwoRigTown(t)
	env := map[string]string{"GT_ROLE": "polecat"}

	cmdA, err := BuildStartupCommand(env, rigA, "")
	if err != nil {
		t.Fatalf("BuildStartupCommand(alpha): %v", err)
	}
	if !strings.Contains(cmdA, ".claude-alpha") {
		t.Fatalf("alpha command lacks its own process names: %s", cmdA)
	}

	cmdB, err := BuildStartupCommand(env, rigB, "")
	if err != nil {
		t.Fatalf("BuildStartupCommand(beta): %v", err)
	}
	if strings.Contains(cmdB, ".claude-alpha") || strings.Contains(cmdB, "ALPHA_ONLY") {
		t.Fatalf("beta command carries rig alpha's agent override: %s", cmdB)
	}
}

// A town's settings/agents.json applies to every rig in that town, below the
// rig's own file (NixOS: claude runs as ".claude-unwrapped").
func TestStartupProcessNamesUseTownRegistry(t *testing.T) {
	t.Parallel()
	townRoot, rigA, rigB := newTwoRigTown(t)
	townFile := DefaultAgentRegistryPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(townFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(townFile, []byte(`{"version":1,"agents":{"claude":{"process_names":["node","claude",".claude-unwrapped"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GT_ROLE": "polecat"}

	cmdB, err := BuildStartupCommand(env, rigB, "")
	if err != nil {
		t.Fatalf("BuildStartupCommand(beta): %v", err)
	}
	if !strings.Contains(cmdB, "GT_PROCESS_NAMES=node,claude,.claude-unwrapped") {
		t.Fatalf("beta command lacks the town's process names: %s", cmdB)
	}

	// Rig alpha's own file wins over the town's for the fields it sets.
	if got := AgentRegistryFor(townRoot, rigA).ProcessNames("claude"); len(got) != 3 || got[2] != ".claude-alpha" {
		t.Fatalf("alpha ProcessNames(claude) = %v, want its own override", got)
	}
	// Nothing leaks into the built-ins.
	if got := GetProcessNames("claude"); len(got) != 2 {
		t.Fatalf("GetProcessNames(claude) = %v, want the built-in [node claude]", got)
	}
}

// A rig-level override is merged onto the town-level entry, which is merged
// onto the built-in, so each file only needs the fields it changes.
func TestAgentRegistryLayersMergeFieldwise(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "rig")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(DefaultAgentRegistryPath(townRoot), `{"version":1,"agents":{"claude":{"process_names":["node","claude","town-proc"]}}}`)
	write(RigAgentRegistryPath(rigPath), `{"version":1,"agents":{"claude":{"command":"env"}}}`)

	reg, err := LoadAgentRegistryFor(townRoot, rigPath)
	if err != nil {
		t.Fatalf("LoadAgentRegistryFor: %v", err)
	}
	claude := reg.Preset("claude")
	if claude.Command != "env" {
		t.Errorf("Command = %q, want the rig's env", claude.Command)
	}
	if len(claude.ProcessNames) != 3 || claude.ProcessNames[2] != "town-proc" {
		t.Errorf("ProcessNames = %v, want the town's", claude.ProcessNames)
	}
	if claude.SessionIDEnv != GetAgentPreset(AgentClaude).SessionIDEnv {
		t.Errorf("SessionIDEnv = %q, want the built-in's", claude.SessionIDEnv)
	}
}

// registryWith returns the built-in registry plus the given presets, the
// registry a town or rig settings/agents.json defining them would produce.
func registryWith(presets ...AgentPresetInfo) *AgentRegistry {
	reg := &AgentRegistry{
		Version: CurrentAgentRegistryVersion,
		Agents:  make(map[string]*AgentPresetInfo),
	}
	for name, preset := range builtinAgentRegistry.Agents {
		reg.Agents[name] = preset
	}
	for i := range presets {
		preset := presets[i]
		reg.Agents[string(preset.Name)] = &preset
	}
	return reg
}
