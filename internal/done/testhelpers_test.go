package done

import (
	"github.com/steveyegge/gastown/internal/beads"
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

// doneFakeCleanupUpdater records the cleanup_status self-report: one agent
// bead whose write beads.UpdateAgentCleanupStatus performs.
type doneFakeCleanupUpdater struct {
	beads.Client
	calls  int
	id     string
	status string
	err    error
}

// Show returns the agent bead the read-modify-write reads.
func (f *doneFakeCleanupUpdater) Show(id string) (*beads.Issue, error) {
	return &beads.Issue{ID: id, Title: "Polecat worker", Labels: []string{"gt:agent"}}, nil
}

// Update records the description write and the cleanup_status it set.
func (f *doneFakeCleanupUpdater) Update(id string, opts beads.UpdateOptions) error {
	f.calls++
	f.id = id
	if opts.Description != nil {
		f.status = beads.ParseAgentFields(*opts.Description).CleanupStatus
	}
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
