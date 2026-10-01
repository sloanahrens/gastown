package config

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// fakeHost is a host whose environment is env, whose PATH holds exactly bins
// (each found at /fake/bin/<name>, which lookPath also accepts as a path),
// and which has no working directory; see inDir. Tests resolve agents against it instead of setting env, writing PATH
// stubs or chdir-ing.
func fakeHost(env map[string]string, bins ...string) host {
	onPath := make(map[string]bool, len(bins))
	for _, b := range bins {
		onPath[b] = true
	}
	return host{
		getenv: func(key string) string { return env[key] },
		lookPath: func(name string) (string, error) {
			if onPath[name] || onPath[strings.TrimPrefix(name, "/fake/bin/")] {
				return "/fake/bin/" + strings.TrimPrefix(name, "/fake/bin/"), nil
			}
			return "", &os.PathError{Op: "lookpath", Path: name, Err: os.ErrNotExist}
		},
		getwd: func() (string, error) { return "", errors.New("fakeHost has no working directory") },
	}
}

// inDir returns h with dir as its working directory.
func (h host) inDir(dir string) host {
	h.getwd = func() (string, error) { return dir, nil }
	return h
}

// agentBins are the agent binaries the old TestMain stubbed onto PATH.
var agentBins = []string{"claude", "gemini", "codex", "cursor-agent", "auggie", "amp", "opencode"}

// agentHost is fakeHost with every agent in agentBins on PATH, plus extra.
func agentHost(env map[string]string, extra ...string) host {
	return fakeHost(env, append(append([]string(nil), agentBins...), extra...)...)
}

// writeRoutingAgents writes a town settings/agents.json defining agents named
// after other binaries. The built-in non-Claude presets are retired (D4), but
// the agent-name routing tests need agents whose command is not claude to tell
// one resolution from another, so they define them as registry entries.
func writeRoutingAgents(t *testing.T, townRoot string) {
	t.Helper()
	writeTestSettings(t, DefaultAgentRegistryPath(townRoot), `{"version":1,"agents":{
		"gemini":{"command":"gemini","args":["--approval-mode","yolo"],"process_names":["gemini"]},
		"codex":{"command":"codex","args":["--dangerously-bypass-approvals-and-sandbox"],"process_names":["codex"]},
		"opencode":{"command":"opencode","process_names":["opencode","node","bun"]},
		"amp":{"command":"amp","process_names":["amp"]},
		"cursor":{"command":"cursor-agent","process_names":["cursor-agent"]}}}`)
}
