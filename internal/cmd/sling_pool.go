package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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
//
// Bead-shape routing (gt-t8h1, plan Task 4 / design B1+B3) layers on top: the
// pool used to decide from the seat count alone, so a hard bug and a doc tweak
// competed for the same seat. Now the bead's labels and type pick the seat,
// every decision line names the agent it chose, and a bead that has already
// spent a local attempt cannot spend a second one.

const (
	// routeLocalLabel and routeFlashLabel are explicit overrides. A bead
	// carrying one is pinned to that seat class whatever its shape.
	routeLocalLabel = "route:local"
	routeFlashLabel = "route:flash"

	// localAttemptLabel marks a bead that has already spent its one local
	// attempt (B3). resolvePolecatPoolAgent attaches it when the idle-seat fill
	// fires, so a bead redispatched after its local MR was rejected or its
	// session stalled carries the label and goes to the overflow agent instead
	// of looping through the merge gates on the local seat.
	localAttemptLabel = "local-attempt:1"

	// reworkLabel marks a bead whose MR came back with findings. Steering a
	// rework back to the seat that already has its context beats a fresh
	// prefill on the overflow agent.
	reworkLabel = "rework"

	// idleFillReason is the parenthetical of the seat-fill branch. The
	// choosePoolAgent signature is (agent, reason) — the caller keys on this
	// exact substring to know it must attach localAttemptLabel, so it is a
	// contract with resolvePolecatPoolAgent rather than prose.
	idleFillReason = "idle-seat fill, local-attempt:1"
)

// poolBead is the bead shape the routing policy reads. Type and Labels come
// from `bd show --json`; both are empty when that lookup failed, which leaves
// the seat-count policy in charge — and the caller says so in the reason
// rather than letting a seat-only decision look like a deliberate route.
type poolBead struct {
	ID     string
	Type   string
	Labels []string
}

// hasLabel reports whether the bead carries label, ignoring case and
// surrounding space.
func (b poolBead) hasLabel(label string) bool {
	for _, l := range b.Labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return true
		}
	}
	return false
}

// beadShape is how a bead type steers the pool.
type beadShape int

const (
	shapeLocal    beadShape = iota // task, chore, docs: local work by default
	shapeOverflow                  // bug, feature: the overflow agent's job
	shapeUnknown                   // no opinion; the seat count decides
)

func (b poolBead) shape() beadShape {
	switch beadTypeLabel(b) {
	case "task", "chore", "docs":
		return shapeLocal
	case "bug", "feature":
		return shapeOverflow
	}
	return shapeUnknown
}

// beadTypeLabel renders the bead's type for a reason line, normalised the way
// the shape check reads it. An unreadable type says so instead of leaving the
// parenthetical empty.
func beadTypeLabel(b poolBead) string {
	if t := strings.ToLower(strings.TrimSpace(b.Type)); t != "" {
		return t
	}
	return "unknown"
}

// poolAgentName renders the chosen agent for a reason line. An empty
// overflow_agent means the role default resolves it, and "-> " would read as a
// missing value.
func poolAgentName(agent string) string {
	if agent == "" {
		return "the role default"
	}
	return agent
}

// poolSession is what the policy needs to know about one live polecat.
type poolSession struct {
	name    string
	agent   string // GT_AGENT in the tmux session environment
	created time.Time
}

<<<<<<< HEAD
// choosePoolAgent decides the agent for a new polecat given the pool and
// the live polecat sessions. It returns "" when the pool does not apply
// (unset, or no local agent) so the caller falls back to role_agents.
// rigPath is used to read pending markers that track in-progress spawns.
func choosePoolAgent(pool *config.PolecatPool, sessions []poolSession, now time.Time, rigPath string) (agent, reason string) {
=======
// choosePoolAgent decides the agent for a new polecat given the pool, the bead
// being slung, and the live polecat sessions. It is pure: the only side effect
// in routing — attaching localAttemptLabel — belongs to the caller. It returns
// "" when the pool does not apply (unset, or no local agent) so the caller
// falls back to role_agents.
//
// Every reason names the agent it chose, except the three "the pool does not
// apply" lines, which name no agent because the caller has yet to pick one.
func choosePoolAgent(pool *config.PolecatPool, bead poolBead, sessions []poolSession, now time.Time) (agent, reason string) {
>>>>>>> origin/main
	switch {
	case pool == nil:
		return "", "pool: no polecat pool configured"
	case pool.LocalAgent == "":
		return "", "pool: polecat_pool has no local_agent; using the role default"
	case pool.MaxLocal <= 0:
		return "", fmt.Sprintf("pool: polecat_pool max_local is %d; using the role default", pool.MaxLocal)
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
<<<<<<< HEAD
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
=======
	gap := pool.MinSpawnGapD()
	// The prefill guard. Every fresh local session spends 20-25k tokens of
	// prefill during which the decoding slots starve, so two local spawns
	// closer together than min_spawn_gap cost more than they buy.
	tooSoon := gap > 0 && local > 0 && now.Sub(newest) < gap
	overflow := poolAgentName(pool.OverflowAgent)

	// localSeat is the one place the seat line is built: it is contractual
	// output, and it is what tells a reader which seat the polecat took.
	localSeat := func(why string) (string, string) {
		return pool.LocalAgent, fmt.Sprintf("pool: local seat %d/%d -> %s (%s)", local+1, pool.MaxLocal, pool.LocalAgent, why)
	}
	// seat decides a bead that wants the local agent (or has no preference),
	// applying the seat count and then the stagger. want names the branch for
	// the reason line.
	seat := func(want string) (string, string) {
		switch {
		case local >= pool.MaxLocal:
			return pool.OverflowAgent, fmt.Sprintf("pool: local full (%d/%d) -> %s", local, pool.MaxLocal, overflow)
		case tooSoon:
			return pool.OverflowAgent, fmt.Sprintf("pool: stagger %s since last local spawn -> %s", now.Sub(newest).Round(time.Second), overflow)
		}
		return localSeat(want)
>>>>>>> origin/main
	}
	// overflow sends the bead to the overflow agent and says why the bead's own
	// shape (not the pool's state) made that call.
	overflowFor := func(why string) (string, string) {
		return pool.OverflowAgent, fmt.Sprintf("pool: overflow -> %s (%s)", overflow, why)
	}

	// 1. Explicit routing wins over everything below, including a spent local
	//    attempt: an operator who labeled the bead has already decided.
	switch {
	case bead.hasLabel(routeLocalLabel):
		return seat("label " + routeLocalLabel)
	case bead.hasLabel(routeFlashLabel):
		return overflowFor("label " + routeFlashLabel)
	}
	// 2. One local attempt per bead (B3). The label is attached by the idle-seat
	//    fill branch, so a bead carrying it has already tried the local seat and
	//    lost — this is the redispatch.
	if bead.hasLabel(localAttemptLabel) {
		return overflowFor(localAttemptLabel + " failed")
	}
	// 3. Bead shape.
	if bead.hasLabel(reworkLabel) {
		return seat(reworkLabel)
	}
	switch bead.shape() {
	case shapeLocal:
		return seat("type=" + beadTypeLabel(bead))
	case shapeOverflow:
		// B3 idle-seat fill: local work displaces overflow spend, so an
		// overflow-shaped bead takes a seat that has been free for longer than
		// min_spawn_gap rather than leaving the GPU idle while flash is billed.
		// The caller attaches localAttemptLabel, which bounds the bead to this
		// one local attempt.
		if local < pool.MaxLocal && !tooSoon {
			return localSeat(idleFillReason)
		}
		return overflowFor("type=" + beadTypeLabel(bead))
	}
	// 4. Unknown shape (a wisp, an epic, a bead whose lookup failed): the seat
	//    count alone decides, as it did before B1.
	return seat("type=" + beadTypeLabel(bead))
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

// poolBeadLookup reads the type and labels the routing policy needs. A var so
// tests can drive the policy without a live database.
var poolBeadLookup = func(townRoot, beadID string) (poolBead, error) {
	out, err := bdShowBeadOutputFromTownRoot(townRoot, beadID)
	if err != nil {
		return poolBead{}, err
	}
	var issues []beads.Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return poolBead{}, fmt.Errorf("parsing bead %s: %w", beadID, err)
	}
	if len(issues) == 0 {
		return poolBead{}, fmt.Errorf("bead %s not found", beadID)
	}
	return poolBead{ID: issues[0].ID, Type: issues[0].Type, Labels: issues[0].Labels}, nil
}

// poolBeadLabelAdd attaches a label to a bead. A var so tests can watch the
// write the fill rule makes without a live database.
var poolBeadLabelAdd = func(townRoot, beadID, label string) error {
	return BdCmd("label", "add", beadID, label).
		Dir(resolveBeadDirFromTownRoot(townRoot, beadID)).
		StripBeadsDir().
		Run()
}

// resolvePolecatPoolAgent applies the town's polecat_pool to a sling that
<<<<<<< HEAD
// gave no --agent. It returns the agent to use ("" = role default) and a
// human-readable reason for the sling output.
// It writes a pending marker before making the decision to prevent concurrent
// slings from exceeding the local pool cap.
// rigPath is the path to the rig directory, used to write pending markers.
func resolvePolecatPoolAgent(townRoot, rigPath string) (agent, reason string) {
=======
// gave no --agent. It reads the bead's type and labels, attaches
// localAttemptLabel when the idle-seat fill branch fires, and returns the agent
// to use ("" = role default) with a one-line reason naming that agent.
func resolvePolecatPoolAgent(townRoot, beadID string) (agent, reason string) {
	return poolRoute(townRoot, beadID, true)
}

// peekPolecatPoolAgent is resolvePolecatPoolAgent without the label write:
// `gt sling --dry-run` must print the route it would take without consuming
// the bead's one local attempt.
func peekPolecatPoolAgent(townRoot, beadID string) (agent, reason string) {
	return poolRoute(townRoot, beadID, false)
}

func poolRoute(townRoot, beadID string, attachLabel bool) (agent, reason string) {
>>>>>>> origin/main
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil || ts == nil || ts.PolecatPool == nil {
		return "", ""
	}
<<<<<<< HEAD
	// Write a pending marker BEFORE counting sessions.
	// This ensures concurrent slings see each other's pending spawns.
	if err := writePendingMarker(ts.PolecatPool.LocalAgent, rigPath); err != nil {
		// Non-fatal: we can still proceed without the marker,
		// but the pool cap protection won't work as well.
		reason = "pool: warning: could not write pending marker (" + err.Error() + ")"
	}
	// Clean up the marker when we return (best-effort)
	defer removePendingMarker(ts.PolecatPool.LocalAgent, rigPath)
=======
	// A bead lookup failure is not fatal: the policy falls back to the seat
	// count and the reason says so, so a seat-only decision never passes for a
	// deliberate route (B1).
	var bead poolBead
	var beadErr error
	if beadID != "" {
		bead, beadErr = poolBeadLookup(townRoot, beadID)
	}
>>>>>>> origin/main
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
<<<<<<< HEAD
	agent, reason = choosePoolAgent(ts.PolecatPool, sessions, time.Now(), rigPath)
	if reason == "" {
		reason = "pool: local pool decision"
	}
	return agent, "pool: " + reason
=======
	agent, reason = choosePoolAgent(ts.PolecatPool, bead, sessions, time.Now())
	if beadErr != nil {
		reason = fmt.Sprintf("%s [bead %s unreadable: %v]", reason, beadID, beadErr)
	}
	if attachLabel && beadID != "" && strings.Contains(reason, idleFillReason) {
		if err := poolBeadLabelAdd(townRoot, beadID, localAttemptLabel); err != nil {
			reason = fmt.Sprintf("%s [%s label failed: %v]", reason, localAttemptLabel, err)
		}
	}
	return agent, reason
>>>>>>> origin/main
}
