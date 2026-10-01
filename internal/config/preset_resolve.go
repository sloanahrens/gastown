package config

import (
	"path/filepath"
	"sort"
	"strings"
)

// ResolveAgentPreset maps an agent name — a GT_AGENT value, role_agents entry,
// or default_agent — to the preset of the harness that actually runs it.
// Custom agents in rig then town settings/config.json come first (the same
// order as lookupAgentConfigIfExists), then the registry for that town and
// rig (LoadAgentRegistryFor). ok=false means no agent of that name is
// configured: callers must treat that as an unknown harness, never as
// Claude. A configured agent always resolves, to Claude when its command is
// a wrapper (claude-9a8).
func ResolveAgentPreset(name, townRoot, rigPath string) (*AgentPresetInfo, bool) {
	if name == "" {
		return nil, false
	}
	reg := AgentRegistryFor(townRoot, rigPath)
	var town *TownSettings
	if townRoot != "" {
		if ts, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot)); err == nil {
			town = ts
		}
	}
	var rig *RigSettings
	if rigPath != "" {
		if rs, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil {
			rig = rs
		}
	}
	if rc := customAgentFor(name, town, rig); rc != nil {
		return reg.HarnessPreset(rc)
	}
	if preset := reg.Preset(name); preset != nil {
		return preset, true
	}
	return nil, false
}

// HarnessPreset returns the built-in preset of the harness a RuntimeConfig
// launches. See (*AgentRegistry).HarnessPreset.
func HarnessPreset(rc *RuntimeConfig) (*AgentPresetInfo, bool) {
	return builtinAgentRegistry.HarnessPreset(rc)
}

// HarnessPreset returns the preset in r of the harness a RuntimeConfig
// launches (see harnessPresetName).
func (r *AgentRegistry) HarnessPreset(rc *RuntimeConfig) (*AgentPresetInfo, bool) {
	if rc == nil {
		return nil, false
	}
	table := r.presetTable()
	preset := table[harnessPresetName(rc.Command, rc.Args, rc.Provider, table)]
	return preset, preset != nil
}

// customAgentFor finds a custom agent definition, rig before town. It does
// not apply fillRuntimeDefaults, so an unset command stays distinguishable.
func customAgentFor(name string, town *TownSettings, rig *RigSettings) *RuntimeConfig {
	if rig != nil && rig.Agents != nil {
		if rc, ok := rig.Agents[name]; ok && rc != nil {
			return rc
		}
	}
	if town != nil && town.Agents != nil {
		if rc, ok := town.Agents[name]; ok && rc != nil {
			return rc
		}
	}
	return nil
}

// presetTable merges the built-in presets with the presets in r, so a
// registry built by hand without the built-ins still recognizes them.
func (r *AgentRegistry) presetTable() map[string]*AgentPresetInfo {
	agents := r.agents()
	table := make(map[string]*AgentPresetInfo, len(builtinPresets)+len(agents))
	for name, preset := range builtinPresets {
		table[string(name)] = preset
	}
	for name, preset := range agents {
		table[name] = preset
	}
	return table
}

// harnessPresetName names the preset for a command line. The command is
// authoritative (basename, gt- prefix and wrappers such as `env -u X claude`
// unwrapped); provider is the fallback. Anything else is Claude: the Claude CLI
// is the only runtime (D4), so an unrecognized command is a wrapper script that
// execs it (a claude-deepseek-* backend wrapper) and an unknown provider names
// a backend, not a harness.
func harnessPresetName(command string, args []string, provider string, presets map[string]*AgentPresetInfo) string {
	if command != "" {
		if name := presetForBinary(commandBinary(command, args), presets); name != "" {
			return name
		}
	}
	if _, ok := presets[provider]; ok && provider != "" {
		return provider
	}
	return string(AgentClaude)
}

// commandBinary returns the normalized binary a command line runs.
func commandBinary(command string, args []string) string {
	bin := commandBase(command)
	if wrapperCommands[bin] {
		bin = commandBase(extractWrappedBinary(bin, args))
	}
	return bin
}

func commandBase(command string) string {
	if command == "" {
		return ""
	}
	base := filepath.Base(command)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	return strings.TrimPrefix(base, "gt-")
}

// presetForBinary prefers the preset named after the binary (claude, not
// groq-compound, which also runs claude), then the first preset in sorted
// order whose command matches, so the answer never depends on map order.
func presetForBinary(bin string, presets map[string]*AgentPresetInfo) string {
	if bin == "" {
		return ""
	}
	if p, ok := presets[bin]; ok && p != nil && (p.Command == "" || commandBase(p.Command) == bin) {
		return bin
	}
	for _, name := range sortedPresetNames(presets) {
		if p := presets[name]; p != nil && p.Command != "" && commandBase(p.Command) == bin {
			return name
		}
	}
	return ""
}

func sortedPresetNames(presets map[string]*AgentPresetInfo) []string {
	names := make([]string, 0, len(presets))
	for name := range presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// canonicalFirst moves the names equal to any of want to the front,
// keeping the rest in order.
func canonicalFirst(names []string, want ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		for _, w := range want {
			if n == w {
				out = append(out, n)
				break
			}
		}
	}
	for _, n := range names {
		keep := true
		for _, w := range want {
			if n == w {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, n)
		}
	}
	return out
}
