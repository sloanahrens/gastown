package polecat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// sandboxSeatTimeout bounds the one gateway question a seat's git state asks.
const sandboxSeatTimeout = 15 * time.Second

// SandboxSeatProbe answers the git state of a sandbox seat: agent is the agent
// the polecat's agent bead recorded and sessionName its session, which is also
// its sandbox's name. ok is false for a host seat, whose worktree the caller
// measures as before. A sandbox seat's host worktree is a stale leftover (its
// commits were pushed from inside the sandbox), so it is never measured: the
// gateway's answer decides (SandboxSeatGitState, gt-qq6rg).
type SandboxSeatProbe func(agent, sessionName string) (state LiveGitState, ok bool)

// NewSandboxSeatProbe reads the seat map of the town at townRoot once, on the
// first question, and asks the gateway through exists (nil means the sandbox
// CLI). A settings file or seat map that cannot be read leaves every seat
// unclassifiable, so each answers unknown rather than falling back to a host
// seat's measured worktree, which for a sandbox seat is the stale leftover this
// probe exists to avoid. The daemon's GitState fails closed the same way.
//
// A seat is found by agent name alone because polecat_pool.seats is keyed by
// agent (config.Seats yields one seat per agent), so the recorded agent names
// exactly one seat. The sandbox is named for the seat's session, as the
// daemon's GitState and the reaper name it.
func NewSandboxSeatProbe(townRoot string, exists func(gateway, name string) (bool, error)) SandboxSeatProbe {
	if exists == nil {
		exists = func(gateway, name string) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), sandboxSeatTimeout)
			defer cancel()
			return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
		}
	}
	var (
		once    sync.Once
		ts      *config.TownSettings
		seats   []config.Seat
		loadErr error
	)
	load := func() {
		ts, loadErr = config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
		if loadErr != nil {
			loadErr = fmt.Errorf("reading town settings: %w", loadErr)
			return
		}
		if seats, loadErr = config.Seats(ts, nil); loadErr != nil {
			loadErr = fmt.Errorf("reading polecat_pool.seats: %w", loadErr)
		}
	}
	return func(agent, sessionName string) (LiveGitState, bool) {
		if agent == "" {
			return LiveGitState{}, false
		}
		once.Do(load)
		if loadErr != nil {
			return LiveGitState{
				Source:       GitStateSourceUnknown,
				FailedReason: fmt.Sprintf("could not tell whether agent %s is a sandbox seat: %v", agent, loadErr),
			}, true
		}
		for _, seat := range seats {
			if seat.Agent != agent {
				continue
			}
			if seat.Backend != config.BackendOpenShell {
				return LiveGitState{}, false
			}
			gateway := ts.Sandboxes[seat.Sandbox].WithDefaults().Gateway
			found, existsErr := exists(gateway, sessionName)
			return SandboxSeatGitState("", found, existsErr, sessionName), true
		}
		return LiveGitState{}, false
	}
}

// sandboxSeatState is the sandbox probe's answer for the polecat's agent bead:
// a nil probe, or a polecat with no recorded agent bead, is a host seat.
func (m *Manager) sandboxSeatState(fields *beads.AgentFields, name string) (LiveGitState, bool) {
	if fields == nil {
		return LiveGitState{}, false
	}
	probe := m.sandboxSeat
	if probe == nil {
		if m.townRoot == "" {
			return LiveGitState{}, false
		}
		m.sandboxSeatOnce.Do(func() { m.sandboxSeatDefault = NewSandboxSeatProbe(m.townRoot, nil) })
		probe = m.sandboxSeatDefault
	}
	return probe(fields.Agent, m.sessionName(name))
}

// applySandboxSeatFacts puts a sandbox seat's gateway-derived git state onto
// facts. The branch is the one the agent bead recorded (what the sandbox
// pushed), and a state the gateway could not give stays unknown.
func applySandboxSeatFacts(facts *WorkstateFacts, live LiveGitState, fields *beads.AgentFields) {
	live.ApplyFacts(facts)
	if facts.Branch == "" && fields != nil {
		facts.Branch = fields.Branch
	}
}
