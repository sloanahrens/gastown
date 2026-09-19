package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Polecat model pool (gt-md4z).
//
// The local model serves a fixed GPU: on 2026-09-18 five local polecats
// drove 12-16 busy slots and decode fell under 1 token/s per slot with no
// merges for an hour, while three sessions ran at 4-17 tok/s. Every fresh
// session also costs a 20-25k-token prefill during which every decoding
// slot starves. So the pool caps how many polecats run locally, spaces
// their spawns, and sends the rest to the overflow agent.

// poolSession is what the policy needs to know about one live polecat.
type poolSession struct {
	name    string
	agent   string // GT_AGENT in the tmux session environment
	created time.Time
}

// choosePoolAgent decides the agent for a new polecat given the pool and
// the live polecat sessions. It returns "" when the pool does not apply
// (unset, or no local agent) so the caller falls back to role_agents.
//
// The reason is a routing line printed to the operator, so each outcome gets
// its own unambiguous wording and every line names the agent it picked:
//
//	local seat 2/2 -> local-coder-polecat       (took the last seat)
//	local full (2/2) -> overflow deepseek-flash (no seat to take)
//	local stagger (last spawn 30s ago < 4m0s gap, 1/2) -> overflow deepseek-flash
//
// "seat" and "full" differ by more than a word so a taken seat is never
// misread as an overflow (that mistake cost a flash seat 2026-09-18).
func choosePoolAgent(pool *config.PolecatPool, sessions []poolSession, now time.Time) (agent, reason string) {
	switch {
	case pool == nil:
		return "", "no polecat pool configured"
	case pool.LocalAgent == "":
		return "", "polecat_pool has no local_agent; using the role default"
	case pool.MaxLocal <= 0:
		return "", fmt.Sprintf("polecat_pool max_local is %d; using the role default", pool.MaxLocal)
	}
	local := 0
	var newest time.Time
	for _, s := range sessions {
		if s.agent != pool.LocalAgent {
			continue
		}
		local++
		if s.created.After(newest) {
			newest = s.created
		}
	}
	if local >= pool.MaxLocal {
		return pool.OverflowAgent, fmt.Sprintf("local full (%d/%d) -> overflow %s", local, pool.MaxLocal, poolAgentLabel(pool.OverflowAgent))
	}
	if gap := pool.MinSpawnGapD(); gap > 0 && local > 0 && now.Sub(newest) < gap {
		return pool.OverflowAgent, fmt.Sprintf("local stagger (last spawn %s ago < %s gap, %d/%d) -> overflow %s",
			now.Sub(newest).Round(time.Second), gap, local, pool.MaxLocal, poolAgentLabel(pool.OverflowAgent))
	}
	return pool.LocalAgent, fmt.Sprintf("local seat %d/%d -> %s", local+1, pool.MaxLocal, pool.LocalAgent)
}

// poolAgentLabel names the agent a routing decision picked. The pool lines
// must always end with the chosen agent, so an unset overflow agent has to
// say "role default" rather than trail off into an empty string.
func poolAgentLabel(agent string) string {
	if agent == "" {
		return "role default"
	}
	return agent
}

// sessionLister is the slice of tmux the pool reads; a var so tests can
// substitute a fake without a tmux server.
type sessionLister interface {
	ListSessions() ([]string, error)
	GetEnvironment(session, key string) (string, error)
	GetSessionCreatedTime(name string) (time.Time, error)
}

var newPoolSessionLister = func() sessionLister { return tmux.NewTmux() }

// listPolecatSessions returns every live polecat session with its agent and
// creation time. Polecats are identified by the GT_ROLE their session
// carries ("<rig>/polecats/<name>"), so witnesses, refineries and dogs on
// the same server are not counted. GT_AGENT is written into the session
// environment at spawn (SessionStartOptions.Agent / AgentEnv fallback).
func listPolecatSessions(t sessionLister) ([]poolSession, error) {
	names, err := t.ListSessions()
	if err != nil {
		return nil, err
	}
	var out []poolSession
	for _, n := range names {
		role, err := t.GetEnvironment(n, "GT_ROLE")
		if err != nil || !strings.Contains(role, "/polecats/") {
			continue
		}
		agent, _ := t.GetEnvironment(n, "GT_AGENT")
		created, err := t.GetSessionCreatedTime(n)
		if err != nil {
			// Unknown age counts as "just spawned": it forces the stagger
			// rather than silently disabling it (a zero time would look
			// two thousand years old).
			created = time.Now()
		}
		out = append(out, poolSession{name: n, agent: strings.TrimSpace(agent), created: created})
	}
	return out, nil
}

// resolvePolecatPoolAgent applies the town's polecat_pool to a sling that
// gave no --agent. It returns the agent to use ("" = role default) and a
// human-readable reason for the sling output.
func resolvePolecatPoolAgent(townRoot string) (agent, reason string) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil || ts == nil || ts.PolecatPool == nil {
		return "", ""
	}
	sessions, err := listPolecatSessions(newPoolSessionLister())
	if err != nil {
		// Without a session count the pool cannot be trusted: fall back to
		// the overflow agent (or the role default when none is set) rather
		// than risk over-filling the GPU.
		return ts.PolecatPool.OverflowAgent, "pool: cannot list sessions (" + err.Error() + ") -> " + poolAgentLabel(ts.PolecatPool.OverflowAgent)
	}
	agent, reason = choosePoolAgent(ts.PolecatPool, sessions, time.Now())
	return agent, "pool: " + reason
}
