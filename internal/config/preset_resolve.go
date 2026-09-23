package config

import (
	"path/filepath"
	"sort"
	"strings"
)

// ResolveAgentPreset maps an agent name — a GT_AGENT value, role_agents entry,
// or default_agent — to the preset of the harness that actually runs it.
// Custom agents in rig then town settings/config.json come first (the same
// order as lookupAgentConfigIfExists), then the registry. ok=false means the
// name could not be identified: callers must treat that as an unknown
// harness, never as Claude. (claude-9a8)
func ResolveAgentPreset(name, townRoot, rigPath string) (*AgentPresetInfo, bool) {
	if name == "" {
		return nil, false
	}
	if townRoot != "" {
		_ = LoadAgentRegistry(DefaultAgentRegistryPath(townRoot))
	}
	if rigPath != "" {
		_ = LoadRigAgentRegistry(RigAgentRegistryPath(rigPath))
	}
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
		return HarnessPreset(rc)
	}
	if preset := GetAgentPresetByName(name); preset != nil {
		return preset, true
	}
	return nil, false
}

// HarnessPreset returns the preset of the harness a RuntimeConfig launches,
// by the same command-wins rule as isClaudeAgent.
func HarnessPreset(rc *RuntimeConfig) (*AgentPresetInfo, bool) {
	if rc == nil {
		return nil, false
	}
	table := presetTable()
	name := harnessPresetName(rc.Command, rc.Args, rc.Provider, table)
	if name == "" {
		return nil, false
	}
	preset := table[name]
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

// presetTable merges the immutable built-in presets with a snapshot of the
// registry. Built-ins are copied first so a concurrent registry reset in
// tests cannot empty the table (gt-5v82).
func presetTable() map[string]*AgentPresetInfo {
	registryMu.Lock()
	defer registryMu.Unlock()
	initRegistryLocked()
	table := make(map[string]*AgentPresetInfo, len(builtinPresets)+len(globalRegistry.Agents))
	for name, preset := range builtinPresets {
		table[string(name)] = preset
	}
	for name, preset := range globalRegistry.Agents {
		table[name] = preset
	}
	return table
}

// harnessPresetName names the preset for a command line. The command is
// authoritative (basename, gt- prefix and wrappers such as `env -u X claude`
// unwrapped); provider is the fallback. An empty command and provider means
// Claude, the historical default. Returns "" when nothing matches.
func harnessPresetName(command string, args []string, provider string, presets map[string]*AgentPresetInfo) string {
	if command == "" {
		if provider == "" {
			return string(AgentClaude)
		}
		if _, ok := presets[provider]; ok {
			return provider
		}
		return ""
	}
	if name := presetForBinary(commandBinary(command, args), presets); name != "" {
		return name
	}
	if _, ok := presets[provider]; ok && provider != "" {
		return provider
	}
	return ""
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
