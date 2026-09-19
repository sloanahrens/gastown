package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
// rigPath is used to read pending markers that track in-progress spawns.
func choosePoolAgent(pool *config.PolecatPool, sessions []poolSession, now time.Time, rigPath string) (agent, reason string) {
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
	// Count pending markers (reservation markers) younger than the stagger gap
	// as local sessions to prevent concurrent slings from exceeding the cap.
	gap := pool.MinSpawnGapD()
	pendingCount := countPendingMarkers(pool.LocalAgent, gap, now, rigPath)
	local += pendingCount
	if local >= pool.MaxLocal {
		return pool.OverflowAgent, fmt.Sprintf("local pool full (%d/%d on %s)", local, pool.MaxLocal, pool.LocalAgent)
	}
	if gap > 0 && local > 0 && now.Sub(newest) < gap {
		return pool.OverflowAgent, fmt.Sprintf("stagger: last local spawn %s ago, gap %s", now.Sub(newest).Round(time.Second), gap)
	}
	return pool.LocalAgent, fmt.Sprintf("local pool %d/%d on %s", local+1, pool.MaxLocal, pool.LocalAgent)
}

// sessionLister is the slice of tmux the pool reads; a var so tests can
// substitute a fake without a tmux server.
type sessionLister interface {
	ListSessions() ([]string, error)
	GetEnvironment(session, key string) (string, error)
	GetSessionCreatedTime(name string) (time.Time, error)
}

var newPoolSessionLister = func() sessionLister { return tmux.NewTmux() }

// pendingMarkerPath returns the path to a pending marker file for the given agent.
// These markers are written inside the pool lock by resolvePolecatPoolAgent;
// removed by SpawnPolecatForSling after the polecat directory is created.
// They prevent concurrent slings from exceeding the local pool cap.
func pendingMarkerPath(agent string) string {
	// Use the rig root's .runtime/pending-spawns/ directory
	// This is a best-effort location - we use the town root's rig path
	// by convention, but this function doesn't have access to townRoot.
	// The caller (resolvePolecatPoolAgent) writes the marker to the rig
	// directory via the polecat manager's pendingPath mechanism.
	return ""
}

// pendingMarkersDir returns the directory for pending pool spawn markers.
func pendingMarkersDir(rigPath string) string {
	return filepath.Join(rigPath, ".runtime", "pending-pool-spawns")
}

// countPendingMarkers counts pending reservation markers for the given agent
// that are younger than the specified gap duration. These markers represent
// spawns that are in progress but haven't created a tmux session yet.
func countPendingMarkers(agent string, gap time.Duration, now time.Time, rigPath string) int {
	if agent == "" || gap <= 0 || rigPath == "" {
		return 0
	}
	dir := pendingMarkersDir(rigPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// If the directory doesn't exist or can't be read, no markers exist
		return 0
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// Markers are named: <agent>.pending.<pid>.<timestamp>
		if !strings.HasPrefix(name, agent+".pending.") {
			continue
		}
		// Extract timestamp from the end of the filename
		parts := strings.Split(name, ".")
		if len(parts) < 4 {
			continue
		}
		// Last part is the timestamp
		tsStr := parts[len(parts)-1]
		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			continue
		}
		created := time.Unix(ts, 0)
		// Count markers younger than the gap
		if now.Sub(created) < gap {
			count++
		}
	}
	return count
}

// writePendingMarker creates a reservation marker for the given agent.
// This must be called before spawning a polecat, while holding the pool lock.
// The marker is removed by SpawnPolecatForSling after the polecat directory is created.
func writePendingMarker(agent string, rigPath string) error {
	if agent == "" || rigPath == "" {
		return nil
	}
	dir := pendingMarkersDir(rigPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating pending markers dir: %w", err)
	}
	// Name format: <agent>.pending.<pid>.<timestamp>
	name := fmt.Sprintf("%s.pending.%d.%d", agent, os.Getpid(), time.Now().Unix())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("writing pending marker: %w", err)
	}
	return nil
}

// removePendingMarker removes a pending marker for the given agent.
// It searches for the marker by agent prefix and removes the oldest one.
// This is a best-effort cleanup - errors are ignored.
func removePendingMarker(agent string, rigPath string) {
	if agent == "" || rigPath == "" {
		return
	}
	dir := pendingMarkersDir(rigPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Find and remove the oldest marker for this agent
	var oldestPath string
	var oldestTime int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, agent+".pending.") {
			parts := strings.Split(name, ".")
			if len(parts) >= 4 {
				tsStr := parts[len(parts)-1]
				ts, err := strconv.ParseInt(tsStr, 10, 64)
				if err == nil {
					if oldestPath == "" || ts < oldestTime {
						oldestPath = filepath.Join(dir, name)
						oldestTime = ts
					}
				}
			}
		}
	}
	if oldestPath != "" {
		_ = os.Remove(oldestPath)
	}
}

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
// It writes a pending marker before making the decision to prevent concurrent
// slings from exceeding the local pool cap.
// rigPath is the path to the rig directory, used to write pending markers.
func resolvePolecatPoolAgent(townRoot, rigPath string) (agent, reason string) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil || ts == nil || ts.PolecatPool == nil {
		return "", ""
	}
	// Write a pending marker BEFORE counting sessions.
	// This ensures concurrent slings see each other's pending spawns.
	if err := writePendingMarker(ts.PolecatPool.LocalAgent, rigPath); err != nil {
		// Non-fatal: we can still proceed without the marker,
		// but the pool cap protection won't work as well.
		reason = "pool: warning: could not write pending marker (" + err.Error() + ")"
	}
	// Clean up the marker when we return (best-effort)
	defer removePendingMarker(ts.PolecatPool.LocalAgent, rigPath)
	sessions, err := listPolecatSessions(newPoolSessionLister())
	if err != nil {
		// Without a session count the pool cannot be trusted: fall back to
		// the overflow agent (or the role default when none is set) rather
		// than risk over-filling the GPU.
		fallback := "the role default"
		if ts.PolecatPool.OverflowAgent != "" {
			fallback = ts.PolecatPool.OverflowAgent
		}
		return ts.PolecatPool.OverflowAgent, "pool: cannot list sessions (" + err.Error() + "), using " + fallback
	}
	agent, reason = choosePoolAgent(ts.PolecatPool, sessions, time.Now(), rigPath)
	if reason == "" {
		reason = "pool: local pool decision"
	}
	return agent, "pool: " + reason
}
