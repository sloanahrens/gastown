package plugin

import "github.com/steveyegge/gastown/internal/config"

// agentPresetResolves reports whether a dog session can start on this preset
// name, by asking the same resolver the session start asks.
//
// A narrower check — membership in TownSettings.Agents, say — admits names the
// session then rejects (a built-in preset is not in that map) and misses the
// ones it accepts, so the validation would disagree with the thing it exists
// to predict. The rig path is empty because a dog session carries no rig
// (internal/dog SessionManager.Start sets no RigPath).
func agentPresetResolves(name, townRoot string) bool {
	_, _, err := config.ResolveAgentConfigWithOverride(townRoot, "", name)
	return err == nil
}
