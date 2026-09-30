package config

import (
	"os"
	"os/exec"
)

// host is what agent resolution reads from the running process: its
// environment (GT_COST_TIER, ${VAR} references in agent env), its PATH (is an
// agent's binary installed?), its working directory (the town root when a
// caller gives none) and the system prompt renderer internal/cmd installs.
// Production resolves against processHost; unit tests pass a scripted host
// instead of setting env, stubbing PATH, chdir-ing or swapping the renderer.
type host struct {
	getenv   func(string) string
	lookPath func(string) (string, error)
	getwd    func() (string, error)
	// renderSystemPrompt renders a role's system prompt file; nil means
	// SystemPromptRenderer, read at call time because internal/cmd installs
	// it at init.
	renderSystemPrompt func(role, townRoot, rigPath, agentName, path string) error
}

// systemPromptRenderer returns h's renderer, or SystemPromptRenderer (which
// may be nil) when h has none of its own.
func (h host) systemPromptRenderer() func(role, townRoot, rigPath, agentName, path string) error {
	if h.renderSystemPrompt != nil {
		return h.renderSystemPrompt
	}
	return SystemPromptRenderer
}

// processHost reads the running process.
var processHost = host{getenv: os.Getenv, lookPath: exec.LookPath, getwd: os.Getwd}

// host returns the host r resolves against: processHost unless r was loaded
// for another one. A nil registry resolves against the process too.
func (r *AgentRegistry) host() host {
	if r == nil || r.env == nil {
		return processHost
	}
	return *r.env
}
