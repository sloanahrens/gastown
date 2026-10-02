package done

import (
	"github.com/steveyegge/gastown/internal/role"
	"github.com/steveyegge/gastown/internal/session"
)

// doneTestRegistry maps the rig prefixes the session-retirement tests use.
func doneTestRegistry() *session.PrefixRegistry {
	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")
	registry.Register("do", "coder_dotfiles")
	registry.Register("mr", "myrig")
	return registry
}

// doneFakeCleanupUpdater records the cleanup_status self-report.
type doneFakeCleanupUpdater struct {
	calls  int
	id     string
	status string
	err    error
}

func (f *doneFakeCleanupUpdater) UpdateAgentCleanupStatus(id, status string) error {
	f.calls++
	f.id = id
	f.status = status
	return f.err
}

// detectFromEnv is a Detect standing in for the CLI layer's role detection:
// it reads GT_ROLE/GT_RIG/GT_POLECAT and reports the role as unnamed when
// GT_ROLE names nothing.
func detectFromEnv(cwd, townRoot string, getenv func(string) string) (Agent, string, bool) {
	parsed, rig, name := role.Parse(getenv("GT_ROLE"))
	if rig == "" {
		rig = getenv("GT_RIG")
	}
	if name == "" {
		name = getenv("GT_POLECAT")
	}
	agent := Agent{Role: parsed, Rig: rig, Polecat: name, TownRoot: townRoot, WorkDir: cwd}
	if parsed == role.Unknown {
		return agent, "", false
	}
	return agent, agent.Actor(), true
}
