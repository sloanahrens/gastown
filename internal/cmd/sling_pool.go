package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Polecat seat pool (gt-md4z, gt-jzr1).
//
// polecat_pool.seats caps how many polecats run on each agent at once: a seat's
// cap of live sessions, after which a sling for that agent is refused rather
// than spawning an unbounded run of paid sessions (10 polecats on a 3+3 town,
// spend pace $2.62/h).
//
// max_total (gt-bcpj5) is the cap beside those: live polecats across every
// seat, so a town whose seats are each inside their own cap has a number that
// bounds them together.

// reworkLabel marks a bead whose MR came back with findings. The landing-queue
// backpressure guard lets a rework through a full queue (sling_backpressure.go).
const reworkLabel = "rework"

// poolBead is the bead shape the sling guards read. Type and Labels come from
// `bd show --json`; both are empty when that lookup failed.
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

// poolSession is what the policy needs to know about one occupied seat: a live
// polecat session, an in-flight seat claim, a seat mid-landing, or a seat whose
// session is gone while its bead is still hooked (deadHooked).
type poolSession struct {
	name    string
	agent   string // GT_AGENT in the tmux session environment
	created time.Time
	// rig and polecat name the seat this session runs, parsed from the session's
	// GT_ROLE. Empty for a claim or a landing seat, which name no live polecat.
	rig     string
	polecat string
	// deadHooked marks a seat inside Live that no tmux session holds: its
	// polecat crashed with the bead still hooked and is waiting for its
	// supervised restart (gt-tldj4). It counts, and naming it explains a full
	// seat that has fewer live sessions than its number.
	deadHooked bool
	// parkedStale marks a parked seat with no session that has been parked
	// longer than agentpause.ParkedSeatRelease: it no longer holds its seat
	// (gt-t7k92). poolHeldCount, poolSeatCount and the roster's Live skip it;
	// the roster lists it apart so a seat that stopped counting is still visible.
	parkedStale bool
	// landing marks a seat mid-landing: its polecat ran gt done and the
	// landing worker has not closed its bead (poolLandingSessions).
	landing bool
}

// poolHeldCount counts the sessions that hold a pool seat: every session but a
// parkedStale one, whose seat is released (gt-t7k92). Every tally of the pool's
// seats reads through it or poolSeatCount, so a released seat is skipped by the
// router, the dispatch-check patrol and the roster alike without any caller
// opting in.
func poolHeldCount(sessions []poolSession) int {
	n := 0
	for _, s := range sessions {
		if !s.parkedStale {
			n++
		}
	}
	return n
}

// poolSeatCount counts the polecat sessions holding agent's seat, released
// parked seats excluded. An empty agent has no seat to count.
//
// This is the pool's one count of itself, shared by the admission decision
// (choosePoolAgent) and the idle-seat patrol (gt daemon dispatch-check). The
// patrol must not count seats its own way: a nudge that named room the next
// sling would refuse is worse than no nudge, because it spends an agent's
// attention to produce a refusal (gt-59o9).
func poolSeatCount(agent string, sessions []poolSession) int {
	if agent == "" {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.agent == agent && !s.parkedStale {
			n++
		}
	}
	return n
}

// poolSeatOf returns agent's entry in the seat map.
func poolSeatOf(pool *config.PolecatPool, agent string) (config.SeatConfig, bool) {
	if pool == nil || agent == "" {
		return config.SeatConfig{}, false
	}
	seat, ok := pool.Seats[agent]
	return seat, ok
}

// poolSeatAgents lists the seat map's agents in the loader's order, sorted by
// name, so every choice made from the map is repeatable.
func poolSeatAgents(pool *config.PolecatPool) []string {
	if pool == nil {
		return nil
	}
	agents := make([]string, 0, len(pool.Seats))
	for agent := range pool.Seats {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	return agents
}

// poolSeatClosedAt reports whether agent's seat is inside its closed window at
// now. A seat with no window is always open. A window the loader would refuse
// (a bad zone or range, or no timezone) reads as closed: the town checked the
// map at start, so a window that cannot be read here is a map nobody vetted,
// and the cost rule behind a window is safer kept than guessed away.
func poolSeatClosedAt(pool *config.PolecatPool, agent string, now time.Time) bool {
	seat, ok := poolSeatOf(pool, agent)
	if !ok || seat.Closed == "" {
		return false
	}
	w, err := config.SeatWindow(pool, seat)
	if err != nil {
		return true
	}
	return w.Closed(now)
}

// poolDefaultAgent is the agent a sling that named none runs on: the seat open
// at now with the most room (its cap less its live polecats), the first by name
// when several tie. It is "" when no open seat has room. A sling that names no
// agent has no label to honor, so it goes where there is the most room, which
// spreads the town's polecats over the seats rather than filling one; the
// dispatcher, which draws at random, never asks (it names the agent it drew).
// A seat inside its closed window is no candidate: this sling chose no seat, so
// nothing in it asked to spend on a seat the town shut.
func poolDefaultAgent(pool *config.PolecatPool, sessions []poolSession, now time.Time) string {
	best, bestRoom := "", 0
	for _, agent := range poolSeatAgents(pool) {
		if poolSeatClosedAt(pool, agent, now) {
			continue
		}
		room := pool.Seats[agent].Cap - poolSeatCount(agent, sessions)
		if room > bestRoom {
			best, bestRoom = agent, room
		}
	}
	return best
}

// poolOwnsAgent reports whether requested names a seat the pool controls. An
// empty request has no seat to leave alone, so it is trivially "owned": the
// pool is free to pick for it. Both choosePoolAgent and the router's own-error
// fallbacks (a tmux-listing or seat-claim read failure) test this same
// question — a seat the pool never owned is not the pool's to override on a
// hiccup any more than it is the pool's to admit (gt-67fj, gt-gcrk).
func poolOwnsAgent(pool *config.PolecatPool, requested string) bool {
	if requested == "" {
		return true
	}
	_, ok := poolSeatOf(pool, requested)
	return ok
}

// poolSeatCapKey is the polecat_pool key that caps agent's seat, which a
// refusal names so the operator raises the number that held the sling.
func poolSeatCapKey(agent string) string {
	return "polecat_pool.seats." + agent + ".cap"
}

// choosePoolAgent decides the agent for a new polecat given the pool, the agent
// the caller asked for, and the live polecat sessions. It is pure. It returns
// "" with an empty reason when the pool has no opinion, so the caller keeps the
// agent it asked for (or the town's default agent when it asked for none), and
// refused when the seat is at its cap, so the caller stops the sling instead of
// spawning past it.
//
// requested is the agent named on the command line (--agent), including any
// agent recorded at sling time. An agent with no seat leaves the pool nothing
// to admit, and the request stands untouched (gt-4lbz). A request for an agent
// with a seat is admitted by that seat's cap like any other sling; a cap of 0
// is a closed seat and refuses. A request that names none goes to the seat with
// the most room among the seats open now (poolDefaultAgent), and is refused when
// no seat has any.
//
// raise names the polecat_pool key whose cap refused the sling — the seat's cap
// for a seat, max_total for the town's total — so the refusal can say what to
// raise rather than pointing at a key that would not move. It is empty unless
// the sling was refused.
//
// A request for an agent with no seat still answers to the town's total: a
// seat's cap is that agent's alone, so another agent's request is none of the
// pool's business, but max_total is a cap on the town, and a request for any
// agent adds one of the polecats it bounds (gt-bcpj5).
//
// A seat's closed window bounds a choice, not a request: a sling that names no
// agent is placed only on a seat open at now, while a sling that names a closed
// seat's agent is an operator or a script naming an agent, and it is admitted by
// the cap alone.
func choosePoolAgent(pool *config.PolecatPool, requested string, sessions []poolSession, now time.Time) (agent, reason string, refused bool, raise string) {
	if pool == nil {
		return "", "pool: no polecat pool configured", false, ""
	}
	if total := pool.GetMaxTotal(); total > 0 && poolHeldCount(sessions) >= total {
		agent = poolRefusalAgent(pool, requested)
		return agent, fmt.Sprintf("pool: total full (%d/%d across every seat) -> no seat for %s", poolHeldCount(sessions), total, agent), true, "polecat_pool.max_total"
	}
	switch {
	case len(pool.Seats) == 0:
		return "", "pool: polecat_pool has no seats; using the default agent", false, ""
	case !poolOwnsAgent(pool, requested):
		return "", "", false, ""
	}
	agent = requested
	if agent == "" {
		if agent = poolDefaultAgent(pool, sessions, now); agent == "" {
			// A one-seat pool shows that seat's own refusal, unless its window
			// has shut it: this sling may not take that seat.
			if len(pool.Seats) != 1 || poolSeatClosedAt(pool, poolSeatAgents(pool)[0], now) {
				return "", "pool: every seat is full or closed -> no seat for a sling that named no agent", true, "polecat_pool.seats"
			}
			// The only seat's own refusal names its count, any dead-hooked
			// seat in it, and the cap to raise.
			for only := range pool.Seats {
				agent = only
			}
		}
	}
	seat := pool.Seats[agent]
	n := poolSeatCount(agent, sessions)
	if seat.Cap <= 0 {
		return agent, fmt.Sprintf("pool: closed (cap 0) -> no seat for %s", agent), true, poolSeatCapKey(agent)
	}
	if n >= seat.Cap {
		return agent, fmt.Sprintf("pool: full (%d/%d%s) -> no seat for %s", n, seat.Cap, deadHookedNote(agent, sessions), agent), true, poolSeatCapKey(agent)
	}
	return agent, fmt.Sprintf("pool: seat %d/%d -> %s", n+1, seat.Cap, agent), false, ""
}

// poolRefusalAgent names the agent a refusal is about: the one the caller asked
// for when it asked for one, else the only seat's agent, which a request-less
// sling on a one-seat pool runs, else the default agent. It is only ever
// printed — a refusal stops the sling, so the caller takes no agent from it —
// and it names the request because the total gives no seat to point at instead.
func poolRefusalAgent(pool *config.PolecatPool, requested string) string {
	if requested != "" {
		return requested
	}
	if len(pool.Seats) == 1 {
		for only := range pool.Seats {
			return only
		}
	}
	return "the default agent"
}

// deadHookedNote names the dead-hooked seats inside an agent's live count, so a
// full-seat refusal says when a crashed seat waiting for its supervised restart
// is what filled it: the operator otherwise sees "3/3" with fewer than three
// live sessions and no reason (gt-tldj4). Empty when no dead-hooked seat holds
// a seat, so the common refusal keeps its wording.
func deadHookedNote(agent string, sessions []poolSession) string {
	n := 0
	for _, s := range sessions {
		if s.deadHooked && s.agent == agent {
			n++
		}
	}
	if n == 0 {
		return ""
	}
	return ", " + deadHookedCount(n)
}

// deadHookedCount names the seats inside a count that no tmux session holds:
// their polecat crashed with work still in hand and is waiting for its
// supervised restart. Empty when none is inside it. One wording for every count
// an operator reads — the sling refusal, and `gt pool show`'s live column — so
// the same number reads the same wherever it is met.
func deadHookedCount(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d dead-hooked", n)
}

// parkedStaleCount names the seats a count no longer holds because their
// polecat has been parked with no session past agentpause.ParkedSeatRelease. Empty when
// none is.
func parkedStaleCount(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("%d parked-stale", n)
}

// sessionLister is the slice of tmux the pool reads; a var so tests can
// substitute a fake without a tmux server.
type sessionLister interface {
	ListSessions() ([]string, error)
	GetEnvironment(session, key string) (string, error)
	GetSessionCreatedTime(name string) (time.Time, error)
}

var newPoolSessionLister = func() sessionLister { return tmux.NewTmux() }

// polecatDispositionFunc reads one polecat's own bead state for the pool. A nil
// func is a pool that cannot read any polecat's state.
type polecatDispositionFunc func(rigName, polecatName string) (poolSeatState, error)

// poolSeatReads is the town's beads reads one seat read makes, passed in rather
// than reached for so the read can be driven with no database (docs/testing.md):
// the polecat's agent bead, the work bead assigned to it, and the by-id lookup
// the hook reference and the source-issue hint need. poolSeatReadsFor wires the
// real ones; every other input of the seat read is a directory.
type poolSeatReads struct {
	agentBead func(bd beads.Client, agentID string) (*beads.Issue, *beads.AgentFields, error)
	assigned  func(bd assignedIssueLookup, rigName, polecatName string) polecatActiveWorkEvidence
	show      func(bd beads.Client, id string) (*beads.Issue, error)

	// probe measures a seat's worktree; nil means polecat.ProbeLiveGitStateLocal.
	probe func(worktreePath string) polecat.LiveGitState
	// openWorkLanded opens the repository the landed probe reads; nil means
	// the worktree's own git. Tests set both to a modeled repository.
	openWorkLanded workLandedOpener
}

// issueReadFunc is a by-id bead lookup read as a polecat.IssueReader, so the
// pool's landed evidence reads the bead through the same seam as its other
// beads reads.
type issueReadFunc func(id string) (*beads.Issue, error)

func (f issueReadFunc) Show(id string) (*beads.Issue, error) { return f(id) }

func poolSeatReadsFor() poolSeatReads {
	return poolSeatReads{
		agentBead: beads.GetAgentBead,
		assigned:  poolAssignedWorkEvidence,
		show:      func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) },
	}
}

// poolSeatState is one polecat's own seat read for the pool: the canonical
// workstate disposition, plus whether the seat still holds live work.
//
// HookHeld is what tells a crashed seat from a finished one. A polecat whose
// tmux session died with work still in flight is waiting for its supervised
// restart and still owns the seat (gt-tldj4); one whose work names nothing live
// — submitted, terminal or inert — has nothing to wait for. It is decided from
// the seat's work, not from one reference to it (gt-bc9b6).
type poolSeatState struct {
	Disposition polecat.WorkstateDisposition
	HookHeld    bool
}

// poolDispositionFor is the polecat-state read a caller in townRoot wants, or
// nil when there is no town to read (a caller that then fails open; see
// polecatSeatOccupied). Every reader of a polecat's own seat state builds it
// here, so the seat picture the dispatchers take (poolSeatSessions) and any
// other count cannot read the same polecat two ways.
func poolDispositionFor(townRoot string) polecatDispositionFunc {
	if townRoot == "" {
		return nil
	}
	return func(rigName, polecatName string) (poolSeatState, error) {
		return poolPolecatSeatState(townRoot, rigName, polecatName)
	}
}

// listPolecatSessionsWith returns every live polecat session with its agent and
// creation time, less the seats the polecat-state read says are no longer
// spending one. Polecats are identified by the GT_ROLE their session carries
// ("<rig>/polecats/<name>"), so witnesses, refineries and dogs on the same
// server are not counted. GT_AGENT is written into the session environment at
// spawn (SessionStartOptions.Agent / AgentEnv fallback).
//
// disposition is the polecat's own bead state (poolDispositionFor); a nil one
// fails open. now stands in for a session whose creation time tmux cannot
// report.
//
// A session surviving past `gt done` (preserved for recovery, or torn down
// a beat later than the polecat's own agent_state write) does not mean the
// polecat is still spending the seat: a done polecat sitting on an open MR
// is idle, waiting on the refinery, not on the flash API (gt-2nft
// — a 4/3 seat-cap refusal with only two live sessions, the third and fourth
// "occupants" both done with their MR still in the queue). polecatSeatOccupied
// reads the polecat's own bead state, the same signal `gt polecat list` and
// scheduler.max_polecats (polecat_capacity.go) already trust over a session's
// mere presence, so a session that outlives its polecat's done+MR-open state
// never counts twice against two different capacity models.
func listPolecatSessionsWith(t sessionLister, disposition polecatDispositionFunc, now time.Time) ([]poolSession, error) {
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
			// Unknown age counts as "just spawned": it forces the spec
			// dispatcher's stagger rather than silently disabling it (a zero
			// time would look two thousand years old).
			created = now
		}
		rigName, polecatName, ok := parsePolecatRole(role)
		if ok && !polecatSeatOccupied(disposition, rigName, polecatName) {
			continue
		}
		out = append(out, poolSession{name: n, agent: strings.TrimSpace(agent), created: created, rig: rigName, polecat: polecatName})
	}
	return out, nil
}

// parsePolecatRole splits a session's GT_ROLE ("<rig>/polecats/<name>") into
// the rig and polecat name poolPolecatDisposition needs to look up the bead.
// A role that does not match the shape (already filtered by the caller, but
// checked again defensively) reports ok=false rather than guessing.
func parsePolecatRole(role string) (rig, name string, ok bool) {
	const marker = "/polecats/"
	i := strings.Index(role, marker)
	if i < 0 {
		return "", "", false
	}
	rig = role[:i]
	name = role[i+len(marker):]
	if rig == "" || name == "" {
		return "", "", false
	}
	return rig, name, true
}

// polecatSeatOccupied reports whether the named polecat's own bead state
// still counts as occupying a seat. It fails open (true, "still occupied")
// on a lookup error or a caller with no way to read the state (a townRoot-less
// listPolecatSessions): a pool that cannot read a polecat's state is not a pool
// that knows the seat is free, and undercounting risks the overrun the pool
// exists to prevent (gt-md4z) rather than the seat refusal this fix targets.
func polecatSeatOccupied(disposition polecatDispositionFunc, rigName, polecatName string) bool {
	if disposition == nil {
		return true
	}
	return poolSeatStateOccupied(disposition(rigName, polecatName))
}

// poolSeatStateOccupied is polecatSeatOccupied over a state already read: the
// one seat-free predicate, which the roster's release recorder
// (releasedSeatRecorder) shares with the read that drops the seat.
func poolSeatStateOccupied(state poolSeatState, err error) bool {
	if err != nil {
		return true
	}
	d := state.Disposition
	if d.ReuseStatus == "idle-pr-open" {
		// A done seat waiting on the refinery is not spending the seat (gt-2nft).
		return false
	}
	// A seat whose only capacity-counting blocker is commits preserved on its
	// branch, and whose bead is already terminal, is not spending the seat
	// either (gt-b9ud0): nothing is in flight, and a nuke keeps those commits
	// recoverable from origin. It stays NEEDS_RECOVERY in `gt polecat list`, so
	// it is still visible and recoverable — it just stops filling the roster.
	if d.CommitsPreservedOnBranch {
		return false
	}
	return true
}

// poolPolecatSeatState classifies one polecat through the same
// WorkstateDisposition every other capacity model reads (polecat_capacity.go,
// `gt polecat list`): its agent bead, the live git facts of its worktree, the
// terminality of the bead it is assigned, and what its work names. So "done
// with an open MR", "finished with its leftover commits on its branch", and
// "crashed with the bead still hooked" each mean the same thing here as they do
// everywhere else.
//
// The seat's work is read from both places the rest of the town reads it: the
// bead assigned to the polecat (the direct-tracking model the daemon's crash
// detection, patrol_scan's AssignedWork and `gt polecat list` all decide from,
// hq-l6mm5/gt-4k3fj.1) and the hook reference its agent bead carries. The
// assignment is what holds a crashed seat whose reference was emptied on the
// way out (gt-bc9b6), and the disposition the seat is counted with has to say
// the same, which is why it adds the capacity-counting blocker the list path's
// active-work read adds (gt-tldj4).
func poolPolecatSeatState(townRoot, rigName, polecatName string) (poolSeatState, error) {
	return poolPolecatSeatStateWith(townRoot, rigName, polecatName, poolSeatReadsFor())
}

// poolPolecatSeatStateWith is poolPolecatSeatState with the town's beads reads
// passed in: a test drives the whole seat read — the disposition the roster
// counts and the work that holds a crashed seat — with no database.
func poolPolecatSeatStateWith(townRoot, rigName, polecatName string, reads poolSeatReads) (poolSeatState, error) {
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	rigPath := filepath.Join(townRoot, rigName)
	bd := beads.New(rigPath)
	_, fields, err := reads.agentBead(bd, agentID)
	if err != nil {
		return poolSeatState{}, err
	}
	if fields == nil {
		// No agent bead: nothing to read, and safest read is "still occupied"
		// (see polecatSeatOccupied's fail-open note) rather than a disposition
		// that happens to read as idle-pr-open by construction. No work is
		// named, so a seat with no live session is not a held one either.
		return poolSeatState{Disposition: polecat.WorkstateDisposition{ReuseStatus: ""}}, nil
	}

	state := polecat.StateIdle
	if beads.AgentState(strings.TrimSpace(fields.AgentState)) == beads.AgentStateDone {
		state = polecat.StateDone
	}
	facts := polecat.WorkstateFacts{
		State:         state,
		CleanupStatus: polecat.CleanupStatus(fields.CleanupStatus),
		PushFailed:    fields.PushFailed,
		MRFailed:      fields.MRFailed,
		Branch:        fields.Branch,
		HookBeadSafe:  true,
	}
	if activeMR := strings.TrimSpace(fields.ActiveMR); activeMR != "" {
		facts.ActiveMR = activeMR
		facts.ActiveMRBlocker = "active_mr=" + activeMR
	}
	// The same live probe the list path runs: a reuse and capacity decision
	// comes from measured facts, not recalled ones. Without it the only trace
	// of a finished seat's leftover commits is its recorded cleanup_status,
	// which cannot tell the classifier the commits are preserved on the branch
	// — so the pool would keep filling a seat the list has already stopped
	// counting (gt-b9ud0, the gt-2nft shape of two pictures of one pool). A
	// worktree that cannot be resolved is left unprobed, and the recorded hint
	// stands.
	worktree := resolvePolecatWorktree(filepath.Join(rigPath, "polecats"), polecatName, rigName)
	if worktree != "" {
		probe := reads.probe
		if probe == nil {
			probe = polecat.ProbeLiveGitStateLocal
		}
		probe(worktree).ApplyFacts(&facts)
	}
	// The work the seat was dispatched for, read before the hook reference: the
	// bead the seat holds, read the way the list path reads it
	// (listActivePolecatWorkByName, assessPolecatAssignedIssueWork), so a seat
	// the pool counts is a seat `gt polecat list` counts.
	assigned := reads.assigned(bd, rigName, polecatName)

	// The hook, read the way the list path reads it: a submitted or terminal
	// reference names no work in flight, while an actively assigned (or
	// unreadable) one is a blocker that counts toward capacity. The hint's own
	// lookup doubles as the hook's when they name the same bead, so the common
	// case pays no second read.
	hint := agentSourceIssueHint("", fields)
	hookBead := strings.TrimSpace(fields.HookBead)
	var hookIssue *beads.Issue
	var hookErr error
	if hookBead != "" {
		if hint == hookBead {
			hookIssue, hookErr = reads.show(bd, hint)
		} else {
			hookIssue, hookErr = reads.show(beads.New(townRoot), hookBead)
		}
	}
	hd := polecat.ClassifyHookBead(hookBead, hookIssue, hookErr)
	held := poolSeatWorkFacts(&facts, hd, assigned)
	// Leftover commits the landing already took are not at risk: the same
	// landed evidence the list path applies, so a seat reusable there is not
	// git-unpushed here (gt-wfxa2).
	applyLiveWorkLandedToWorkstateFacts(&facts, reads.openWorkLanded, worktree,
		issueReadFunc(func(id string) (*beads.Issue, error) { return reads.show(bd, id) }), hookBead, facts.Branch)
	// The assignment terminality the classifier accounts that seat with, read
	// the way the list path reads it (assignedBeadTerminalForInventory: the
	// live assigned issue, else the recorded hint — the hook itself when the
	// hint names it, else the hint's own issue). A lookup that fails answers
	// false and the seat keeps counting: the exemption has to be proven, not
	// assumed.
	switch {
	case hint == "":
	case hint == hookBead:
		facts.AssignedBeadTerminal = hd.Terminal
	default:
		if issue, err := reads.show(bd, hint); err == nil && issue != nil {
			facts.AssignedBeadTerminal = beads.IssueStatus(issue.Status).IsTerminal()
		}
	}
	d := polecat.DecideWorkstate(polecat.NewWorkstateInput(facts))
	return poolSeatState{Disposition: d, HookHeld: held}, nil
}

// poolAssignedWorkEvidence reads the work bead assigned to one polecat and
// assesses it the way the list path assesses it, so the pool's reading of a
// seat cannot drift from `gt polecat list`'s (gt-tldj4).
//
// The assignment is the authority for what a seat holds (gt-4k3fj.1: agent
// beads are display mirrors and do not hold the work), and it is the read that
// survives a hand-off: gt done empties the agent bead's hook_bead slot on every
// exit — the DEFERRED exit that leaves the bead hooked included — so the slot
// alone reads a crashed seat as holding nothing (gt-bc9b6). The daemon's crash
// detection (assignedActiveWorkBead), patrol_scan (AssignedWork) and the list
// path all decide this question from this same bead.
//
// A read that fails is the list path's fail-closed evidence
// (polecatActiveWorkLookupError): the seat blocks cleanup without counting
// toward capacity, so a database hiccup frees nothing.
func poolAssignedWorkEvidence(bd assignedIssueLookup, rigName, polecatName string) polecatActiveWorkEvidence {
	issue, err := bd.GetAssignedIssue(rigName + "/polecats/" + polecatName)
	if err != nil {
		return polecatActiveWorkLookupError(err)
	}
	return assessPolecatAssignedIssueWork(issue)
}

// poolSeatWorkFacts folds the two readings of a seat's work into the facts the
// shared classifier decides from, and reports whether the seat still holds live
// work.
//
// The assignment comes first, exactly as it does in the list path
// (buildPolecatInventoryItemFromEvidence): it is the bead the seat holds, and
// the hook reference is consulted only when the assignment names nothing. A
// submitted assignment is deliberately not held work here — that seat mid-landing
// is counted by poolLandingSessions from the seat's own record, and both reads
// counting it would fill two seats with one polecat (gt-xmcyk).
func poolSeatWorkFacts(facts *polecat.WorkstateFacts, hd polecat.HookBeadDisposition, assigned polecatActiveWorkEvidence) (held bool) {
	if assigned.BlocksCleanup {
		facts.ActiveWorkBlocker = assigned.Blocker
		facts.ActiveWorkCountsTowardCapacity = assigned.CountsTowardCapacity
		return !assigned.Submitted
	}
	switch {
	case hd.Submitted:
		facts.HookBeadSubmitted = true
	case hd.Blocker != "":
		facts.ActiveWorkBlocker = hd.Blocker
		facts.ActiveWorkCountsTowardCapacity = true
		return true
	}
	return false
}

// Seat claims (gt-eoi9).
//
// The pool counts live polecat sessions, and a session appears only once its
// spawn has finished — seconds after the decision that started it. Three slings
// launched in parallel therefore each counted the same single session, each
// read a free seat, and all three spawned: the cap the pool exists to hold was
// broken by exactly the concurrency it was meant to bound (gt-eoi9 — opal,
// shale and agate all went past the cap).
//
// A claim closes that window. Before deciding, a sling writes a seat claim on
// disk while holding a lock, so the next sling — which may be reading a tmux
// server that has never heard of the first — counts the claim as a taken seat.
// The lock is what makes count-then-claim atomic; without it two slings read an
// empty claim set and both take the last seat. The claim is rebuilt as a
// poolSession (same agent, created at claim time) and merged into the session
// list, so it moves the seat count through the one policy in choosePoolAgent
// rather than through a second copy of the rules.
//
// A claim lives exactly as long as its seat is invisible to tmux. StartSession
// drops it once the real session is up, and the next decision from the same
// process drops it too, so a batch sling's second bead never reads its own
// first bead as an extra polecat. A claim left behind by a sling that died is
// removed by the next decision's cleanup: a dead PID is not about to start a
// session, so its seat is free.
const (
	// poolSeatClaimTTL bounds how long a claim held by a still-live process can
	// shadow a seat. It is the admission reservation's 30m, not the spawn gap:
	// the tmux session is started by the sling's own process minutes after the
	// decision (11m15s measured for gt-ipk7), so the claim has to outlive the
	// whole spawn. A claim whose process is gone is dropped immediately, without
	// waiting for the TTL.
	poolSeatClaimTTL = 30 * time.Minute

	// poolDecisionLockTimeout bounds the wait for the decision lock. The
	// critical section is a directory read and one small file write.
	poolDecisionLockTimeout = 5 * time.Second
)

// poolSeatClaim is one sling's claim on a seat, on disk so a concurrent
// sling in another process can see it. The PID is what lets the next decision
// tell a claim that is about to become a session from one whose sling is gone.
type poolSeatClaim struct {
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	Agent     string    `json:"agent"`
	Bead      string    `json:"bead,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func poolSeatClaimDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "polecat-pool-claims")
}

// poolSeatFS is the filesystem the seat claims live on. The claims are the
// pool's own state, so the read that decides a seat is a read a test has to be
// able to stage a failure for: "the claim directory could not be read" is the
// one input that makes the seat count unknown rather than small (gt-t8q5), and
// it cannot be produced on a real filesystem without a town on disk.
type poolSeatFS interface {
	ReadDir(name string) ([]os.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Remove(name string) error
	WriteFile(name string, data []byte, perm os.FileMode) error
	MkdirAll(path string, perm os.FileMode) error
	Rename(oldpath, newpath string) error
}

// osPoolSeatFS is the real filesystem, as the rest of the package uses it.
type osPoolSeatFS struct{}

func (osPoolSeatFS) ReadDir(name string) ([]os.DirEntry, error) { return os.ReadDir(name) }
func (osPoolSeatFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osPoolSeatFS) Remove(name string) error                   { return os.Remove(name) }
func (osPoolSeatFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (osPoolSeatFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}
func (osPoolSeatFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// poolSeatClaimStore holds the claim ONE SPAWN made. A spawn takes at most one
// seat — it claims at the pool decision and drops the claim when the session
// starts or the spawn fails — so the store is created by the spawn that may
// take the seat and travels with that spawn's record, which is what reaches
// StartSession and the rollback.
//
// It is per spawn and not per process on purpose. The daemon runs dispatches
// concurrently in one process, and a single store they shared would let one
// dispatch's release drop a seat another dispatch is still standing on: the
// second claim overwrites the first's, and whichever dispatch returns first
// removes the survivor's file (gt-t8q5).
type poolSeatClaimStore struct {
	mu  sync.Mutex
	fs  poolSeatFS
	dir string
	id  string
}

// newPoolSeatClaimStore is the store for one spawn: empty until that spawn's
// pool decision claims a seat in it.
func newPoolSeatClaimStore() *poolSeatClaimStore { return &poolSeatClaimStore{} }

// ownID returns the claim this spawn holds, or "" when it holds none.
func (c *poolSeatClaimStore) ownID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *poolSeatClaimStore) hold(fsys poolSeatFS, dir, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fs, c.dir, c.id = fsys, dir, id
}

// release drops this spawn's claim. Idempotent, and a no-op when the spawn
// never claimed a seat (an uncapped pool, an --agent the pool does not own, a
// dry run) or already released one.
func (c *poolSeatClaimStore) release() {
	if c == nil {
		return
	}
	c.mu.Lock()
	fsys, dir, id := c.fs, c.dir, c.id
	c.fs, c.dir, c.id = nil, "", ""
	c.mu.Unlock()
	if dir == "" || id == "" || fsys == nil {
		return
	}
	_ = fsys.Remove(filepath.Join(dir, id+".json"))
}

// poolSeatLedger is the seat claim set of one town, with the collaborators it
// reads and writes through: the filesystem, the claim one spawn holds, the
// decision lock, the process-liveness probe, this process's PID, and the
// clock. poolSeatLedgerFor wires the real ones.
type poolSeatLedger struct {
	townRoot string
	fs       poolSeatFS
	store    *poolSeatClaimStore
	// lock takes the decision lock and returns its release.
	lock  func() (func(), error)
	alive func(pid int) bool
	pid   int
	now   func() time.Time
}

// poolSeatLedgerFor is the ledger one spawn uses: the town's claim directory on
// disk, that spawn's claim store and this process's PID, and the flock that
// makes count-then-claim atomic.
func poolSeatLedgerFor(townRoot string, store *poolSeatClaimStore) *poolSeatLedger {
	return &poolSeatLedger{
		townRoot: townRoot,
		fs:       osPoolSeatFS{},
		store:    store,
		lock: func() (func(), error) {
			l, err := lockPoolDecision(townRoot)
			if err != nil {
				return nil, err
			}
			return func() { _ = l.Unlock() }, nil
		},
		alive: processAlive,
		pid:   os.Getpid(),
		now:   time.Now,
	}
}

func (l *poolSeatLedger) dir() string { return poolSeatClaimDir(l.townRoot) }

// poolSeatDecision is the locked window in which a sling counts the seats other
// slings have claimed and takes one of its own.
type poolSeatDecision struct {
	ledger *poolSeatLedger
	unlock func()
	// note is set when a claim could not be taken; the caller appends it to
	// the reason so a racy decision never passes for a reserved one.
	note string
}

// begin merges the claims other slings hold into the pool's occupied seats and
// opens the window to claim one. The caller must call done(), error or not.
//
// occupied reads the seats the pool already holds (poolOccupiedSessions, the
// live sessions and the seats mid-landing), and it is called under the decision
// lock rather than before it: a session that starts and drops its claim in the
// window between an unlocked read and the lock appears in neither, so the count
// names a seat free that the other sling already took and the pool runs one
// over its cap (gt-8o35b). Locking both reads together is what makes the count
// and the claims one atom. The move does not lengthen the read — one agent-bead
// read per live session and per polecat directory, one batched work-bead read —
// so the store calls the lock covers stay bounded by the town's seats.
//
// A dry run reads the same seats and claims — so it prints the route a real
// sling would take — but neither cleans up nor claims: a preview must not
// change the pool's state.
//
// An error means the seats could not be counted: either the occupancy read
// failed (a *poolOccupancyError naming which read) or the claims could not be
// read. Either way the count is unknown rather than small, and the caller must
// not decide as if the set were empty (gt-t8q5).
func (l *poolSeatLedger) begin(live bool, occupied func() ([]poolSession, error)) ([]poolSession, *poolSeatDecision, error) {
	d := &poolSeatDecision{ledger: l}
	ownID := l.store.ownID()
	if !live {
		sessions, err := occupied()
		if err != nil {
			return nil, d, err
		}
		claims, err := l.claimSessions(ownID)
		return append(sessions, claims...), d, err
	}
	unlock, err := l.lock()
	if err != nil {
		// The route is still decided, from live sessions alone — the behavior
		// before claims existed — and the reason says the seat could not be
		// reserved rather than passing a racy decision off as a reserved one.
		// The fallback is deliberate (a 5s lock timeout must not fail a
		// dispatch), but a route decided without the lock is a cap that can
		// overfill, so it is said out loud: a warning is the only trace a
		// contended decision leaves once the reason it fed has scrolled away
		// (gt-u0zq0). TestResolvePoolAgentUnreservedWhenTheDecisionLockFails
		// pins the decision this warns about.
		d.note = "seat not reserved: " + err.Error()
		style.PrintWarning("polecat pool seat not reserved (%v); routing on live sessions alone, which can overfill the cap", err)
		sessions, serr := occupied()
		if serr != nil {
			return nil, d, serr
		}
		return sessions, d, nil
	}
	d.unlock = unlock
	l.cleanupStale()
	sessions, err := occupied()
	if err != nil {
		// The lock is held, so done() releases it; the count is unknown.
		return nil, d, err
	}
	claims, claimErr := l.claimSessions(ownID)
	return append(sessions, claims...), d, claimErr
}

// claimFor takes a seat for the route the caller chose when that seat is
// capped. A cap nothing claims is the gt-eoi9 race again.
//
// A seat is what a claim can hold, so a route the town's total admitted on an
// agent with no seat is not claimed: a claim is counted on a seat's agent, and
// one for an agent with none would count one polecat where another agent runs
// another (gt-bcpj5). Two such slings racing the total can both read the last
// polecat and admit, the same window the seat cap closes with a claim, and
// neither more nor less.
func (d *poolSeatDecision) claimFor(agent string, pool *config.PolecatPool, beadID string) {
	if d.unlock == nil || pool == nil {
		return
	}
	if seat, ok := poolSeatOf(pool, agent); !ok || seat.Cap <= 0 {
		return
	}
	claim, err := d.ledger.write(agent, beadID)
	if err != nil {
		d.note = "seat not reserved: " + err.Error()
		return
	}
	d.ledger.store.hold(d.ledger.fs, d.ledger.dir(), claim.ID)
}

func (d *poolSeatDecision) done() {
	if d.unlock == nil {
		return
	}
	d.unlock()
}

// poolSeatClaimSessions renders the claims other slings hold in townRoot as
// poolSessions; see poolSeatLedger.claimSessions.
func poolSeatClaimSessions(townRoot, ownID string) ([]poolSession, error) {
	// A reader, not a spawn: it claims nothing, so it gets a store of its own
	// rather than one a spawn's release is watching.
	return poolSeatLedgerFor(townRoot, newPoolSeatClaimStore()).claimSessions(ownID)
}

// claimSessions renders the claims other slings hold as poolSessions so the
// policy in choosePoolAgent counts them. ownID is skipped: a process still
// holding a claim is about to drop it, and counting both the claim and the
// session it stands for would read one polecat as two seats.
//
// A claim whose sling is gone or past the TTL is not a seat (see stale). A live
// sling's begin prunes those files before it reads; the dry run and the
// dispatcher roster read without pruning, so they skip them here rather than
// count a seat nobody holds. Nothing is deleted on this path.
//
// An error is the claim set failing to read, which the caller must answer for
// rather than treating as no claims (gt-t8q5).
func (l *poolSeatLedger) claimSessions(ownID string) ([]poolSession, error) {
	claims, err := l.read()
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, nil
	}
	now := l.now()
	out := make([]poolSession, 0, len(claims))
	for _, c := range claims {
		if c.ID == ownID || c.Agent == "" || l.stale(c, now) {
			continue
		}
		out = append(out, poolSession{name: "pool-claim/" + c.ID, agent: c.Agent, created: c.CreatedAt})
	}
	return out, nil
}

// read reads the claims on disk, discarding entries that cannot be read or that
// do not name themselves (a half-written or hand-edited file must not hold a
// seat).
//
// A missing directory is "no claim has ever been made here" and reads as an
// empty set. Any other failure to read the directory is an error, because a
// claim set that could not be read is not a claim set that is empty: the seats
// left out are the ones the cap is holding, and a decision that counted them as
// none would take the seat of a polecat another sling is already spawning
// (gt-t8q5).
func (l *poolSeatLedger) read() ([]poolSeatClaim, error) {
	dir := l.dir()
	entries, err := l.fs.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading seat claims from %s: %w", dir, err)
	}
	claims := make([]poolSeatClaim, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := l.fs.ReadFile(path)
		if err != nil {
			_ = l.fs.Remove(path)
			continue
		}
		var claim poolSeatClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			_ = l.fs.Remove(path)
			continue
		}
		if claim.ID == "" || claim.PID <= 0 || claim.CreatedAt.IsZero() || claim.ID+".json" != entry.Name() {
			_ = l.fs.Remove(path)
			continue
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

// stale is a claim that cannot become a session: the process that made it is
// gone, or it has held the seat past the TTL.
func (l *poolSeatLedger) stale(claim poolSeatClaim, now time.Time) bool {
	return !l.alive(claim.PID) || now.Sub(claim.CreatedAt) > poolSeatClaimTTL
}

// cleanupStale drops claims that cannot become sessions: the process that made
// them is gone, or it has held the seat past the TTL. Both mean the seat is
// free, and a stale claim is worse than a missing one — it would push a bead to
// a refused sling for a seat nobody is using.
func (l *poolSeatLedger) cleanupStale() {
	now := l.now()
	claims, err := l.read()
	if err != nil {
		// A claim set that could not be read is one this pass cannot prune:
		// the claims it might drop are the ones it cannot see, so it drops
		// nothing rather than guessing at file names in a directory it cannot
		// enumerate. The decision that called this reports the failure itself
		// (gt-t8q5).
		return
	}
	for _, claim := range claims {
		if !l.stale(claim, now) {
			continue
		}
		_ = l.fs.Remove(filepath.Join(l.dir(), claim.ID+".json"))
	}
}

// write claims a seat for this process and the bead it is about to spawn for.
func (l *poolSeatLedger) write(agent, beadID string) (poolSeatClaim, error) {
	now := l.now().UTC()
	return l.publish(poolSeatClaim{
		ID:        fmt.Sprintf("%d-%d", l.pid, now.UnixNano()),
		PID:       l.pid,
		Agent:     agent,
		Bead:      beadID,
		CreatedAt: now,
	})
}

// publish writes a claim atomically (write, then rename), so a concurrent
// decision never reads a half-written claim.
func (l *poolSeatLedger) publish(claim poolSeatClaim) (poolSeatClaim, error) {
	dir := l.dir()
	if err := l.fs.MkdirAll(dir, 0755); err != nil {
		return poolSeatClaim{}, fmt.Errorf("creating seat claim dir: %w", err)
	}
	path := filepath.Join(dir, claim.ID+".json")
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return poolSeatClaim{}, err
	}
	if err := l.fs.WriteFile(tmpPath, data, 0644); err != nil {
		return poolSeatClaim{}, fmt.Errorf("writing seat claim: %w", err)
	}
	if err := l.fs.Rename(tmpPath, path); err != nil {
		_ = l.fs.Remove(tmpPath)
		return poolSeatClaim{}, fmt.Errorf("publishing seat claim: %w", err)
	}
	return claim, nil
}

// lockPoolDecision takes the lock that makes count-then-claim atomic. It is a
// lock of its own, not the polecat admission lock: a formula sling takes the
// admission lock and spawns through resolveTarget while still holding it
// (sling_formula.go), so a decision that reached for the same file would wait
// on its own caller.
func lockPoolDecision(townRoot string) (*flock.Flock, error) {
	lockDir := filepath.Join(townRoot, ".runtime", "locks")
	if err := os.MkdirAll(lockDir, 0755); err != nil {
		return nil, fmt.Errorf("creating polecat pool lock dir: %w", err)
	}
	lock := flock.New(filepath.Join(lockDir, "polecat-pool.lock"))
	ctx, cancel := context.WithTimeout(context.Background(), poolDecisionLockTimeout)
	defer cancel()
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("acquiring polecat pool lock: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("polecat pool lock busy for %s", poolDecisionLockTimeout)
	}
	return lock, nil
}

// poolBeadLookup reads the type and labels the sling guards need. A var so
// tests can drive them without a live database.
var poolBeadLookup = func(townRoot, beadID string) (poolBead, error) {
	issue, err := showBead(townRoot, beadID)
	if err != nil {
		return poolBead{}, err
	}
	return poolBead{ID: issue.ID, Type: issue.Type, Labels: issue.Labels}, nil
}

// poolBeadLabelAdd attaches a label to a bead. A var so tests can watch the
// write the spec dispatcher makes without a live database.
var poolBeadLabelAdd = func(townRoot, beadID, label string) error {
	return pinnedBd(resolveBeadDirFromTownRoot(townRoot, beadID)).Update(beadID, beads.UpdateOptions{AddLabels: []string{label}})
}

// poolRouter is the pool decision for one town with its collaborators
// explicit: the town's polecat_pool, the town root the seats mid-landing are
// read from, the tmux sessions it counts, each polecat's own bead state, the
// work beads the landing seats are confirmed with, the seat claims, and the
// clock. poolRouterFor wires the real ones; the free functions below are thin
// wrappers over it.
type poolRouter struct {
	// townRoot anchors the occupancy reads that live outside tmux: the seats
	// mid-landing (poolOccupiedSessions). Empty is a router with no town to
	// read, which counts live sessions alone.
	townRoot string
	// pool returns the town's polecat_pool, nil when none is configured or the
	// settings cannot be read.
	pool        func() *config.PolecatPool
	sessions    func() sessionLister
	disposition polecatDispositionFunc
	work        poolSeatWorkFunc
	seats       *poolSeatLedger
	now         func() time.Time
}

// poolRouterFor wires the pool decision to the town on disk, the tmux server,
// bd (each polecat's own state, and the work beads a landing seat is confirmed
// with), and the seat claim of the spawn asking.
func poolRouterFor(townRoot string, store *poolSeatClaimStore) *poolRouter {
	return poolRouterForWith(townRoot, store, func(dir string) beads.Client { return beads.New(dir) })
}

// poolRouterForWith is poolRouterFor with the store for a directory passed in,
// so a test can run the router over a fake. The seat reads go through one
// poolSeatBatch: two bd calls per rig, however many polecat directories the rig
// holds, where the per-seat read spent two on every one (gt-ckz0r). The batch
// lives as long as the router, which is one routing decision: route reads the
// seats once, under the seat lock, and each spawn builds its own router. Nothing
// carries a read into the next decision, so a seat that changed since is seen
// there (gt-rcktb).
func poolRouterForWith(townRoot string, store *poolSeatClaimStore, storeFor func(dir string) beads.Client) *poolRouter {
	batch := newPoolSeatBatch(townRoot,
		func(rigName string) beads.Client { return storeFor(filepath.Join(townRoot, rigName)) },
		func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	return &poolRouter{
		townRoot: townRoot,
		pool: func() *config.PolecatPool {
			ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
			if err != nil || ts == nil {
				return nil
			}
			return ts.PolecatPool
		},
		sessions:    newPoolSessionLister,
		disposition: batch.disposition,
		work:        poolSeatWorkFor(townRoot),
		seats:       poolSeatLedgerFor(townRoot, store),
		now:         time.Now,
	}
}

// peekPolecatPoolAgent is the pool decision without the side effects:
// `gt sling --dry-run` must print the route it would take — a refusal included,
// since that is the route a live sling would take — without claiming a seat. It
// gets a store nothing else reads, because a preview must leave the pool's
// state alone.
func peekPolecatPoolAgent(townRoot, requested string) (agent, reason string, err error) {
	return poolRouterFor(townRoot, newPoolSeatClaimStore()).route(requested, false)
}

// errPoolBackpressure identifies a pool refusal so callers and tests can match
// it with errors.Is without parsing the message.
var errPoolBackpressure = errors.New("polecat pool backpressure")

// poolBackpressureError is the typed refusal: the pool's seat is at its cap. The message leads with `sling refused:`, the marker that tells
// an automatic dispatcher to defer the bead instead of failing it (internal/
// dispatch/refusal.go), and carries the pool's own reason line.
//
// There is no flag that spawns past the cap, deliberately (gt-4lbz). An
// automated path that carries --force for the safety guards it also needs opens
// the cap with that same flag, so a cap that --force opens is a cap no
// automated path is actually held by: that is the spawn (a 4th flash session
// on a seat capped at 3) this guard exists to stop. Capacity is a property of
// the town, so it is raised where it is declared — the seat's cap in
// polecat_pool.seats, or polecat_pool.max_total when the town's total is what
// held it — which the pool reads on every sling.
type poolBackpressureError struct {
	Reason string
	// Raise is the polecat_pool key the operator raises to spawn: a seat's cap
	// or max_total for the town's total. Empty is the seat map, the place a
	// seat cap is declared.
	Raise string
}

func (e *poolBackpressureError) Error() string {
	raise := e.Raise
	if raise == "" {
		raise = "polecat_pool.seats"
	}
	return dispatch.SlingRefusalMarker + " " + e.Reason + "; raise " + raise + " to spawn"
}

func (e *poolBackpressureError) Unwrap() error { return errPoolBackpressure }

// poolUncountedFallback is the agent a decision falls back to when it cannot
// count something it decides from: the one the caller asked for, else the first
// seat by name that is open (a cap above 0 and outside its closed window at
// now), else none, which leaves the caller's own default agent to run. The
// reason line names whichever it was, so a fallback never reads as a deliberate
// route.
func poolUncountedFallback(pool *config.PolecatPool, requested string, now time.Time) (agent, name string) {
	if requested != "" {
		return requested, requested
	}
	for _, a := range poolSeatAgents(pool) {
		if pool.Seats[a].Cap > 0 && !poolSeatClosedAt(pool, a, now) {
			return a, a
		}
	}
	return "", "the default agent"
}

// occupied is the count the pool decides from: the live sessions and the seats
// mid-landing, read through this router's sources and the pool as it is read
// now. route passes it to begin, not calling it, because begin runs it under the
// seat-decision lock, which is where the count has to be taken (gt-8o35b).
func (r *poolRouter) occupied() func() ([]poolSession, error) {
	return func() ([]poolSession, error) {
		return poolOccupiedSessions(r.sessions(), r.townRoot, r.disposition, r.work, r.pool(), r.now)
	}
}

// poolSeatSnapshot is the seat picture of one spec dispatch tick: the roster's
// occupied seats, the seats its read released, and when it was read.
type poolSeatSnapshot struct {
	// sessions are the roster's occupied seats.
	sessions []poolSession
	// released names the seats ("rig/polecat") whose session the roster's
	// polecat-state read found no longer spending one (polecatSeatOccupied):
	// an idle-pr-open seat, or one holding only preserved commits.
	released map[string]bool
	// at is when the roster was read. A session created after it is not the
	// one the roster released.
	at time.Time
}

// releasedAt reports whether s is a session the roster released: its seat was
// released, and the session already existed when the roster read it. A session
// whose creation time is unknown reads as created now, so it counts.
func (p poolSeatSnapshot) releasedAt(s poolSession) bool {
	return s.rig != "" && s.polecat != "" && p.released[s.rig+"/"+s.polecat] && s.created.Before(p.at)
}

// releasedSeatRecorder wraps a disposition so every seat it reports free
// (poolSeatStateOccupied, the predicate polecatSeatOccupied reads) is noted.
// The roster reads a tick's seats through it, and the slings of that tick
// count the same seats free (snapshotOccupied) without another bd read.
func releasedSeatRecorder(disposition polecatDispositionFunc) (polecatDispositionFunc, func() map[string]bool) {
	var mu sync.Mutex
	released := map[string]bool{}
	if disposition == nil {
		return nil, func() map[string]bool { return released }
	}
	recorder := func(rigName, polecatName string) (poolSeatState, error) {
		state, err := disposition(rigName, polecatName)
		if !poolSeatStateOccupied(state, err) {
			mu.Lock()
			released[rigName+"/"+polecatName] = true
			mu.Unlock()
		}
		return state, err
	}
	return recorder, func() map[string]bool {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]bool, len(released))
		for k := range released {
			out[k] = true
		}
		return out
	}
}

// snapshotOccupied is the occupancy read of a sling the spec dispatcher runs:
// the live polecat sessions of the moment (a tmux listing, no bd read) less the
// seats the tick's roster released, plus the seats the roster holds that no
// session accounts for (dead-hooked and mid-landing seats). A released seat is
// free here as it is in the roster and the pool router: counting it occupied
// would refuse a seat the budget frees, every tick, until its session ended.
// A session that is new since the roster is not a released one and counts.
// The snapshot's own live sessions and claims are dropped: the listing
// replaces the former and the ledger appends the claims of the moment under the
// decision lock (gt-t8q5).
func snapshotOccupied(snapshot poolSeatSnapshot, lister func() sessionLister, now func() time.Time) func() ([]poolSession, error) {
	return func() ([]poolSession, error) {
		listed, err := listPolecatSessionsWith(lister(), nil, now())
		if err != nil {
			return nil, &poolOccupancyError{sessions: true, err: err}
		}
		live := listed[:0:0]
		for _, s := range listed {
			if !snapshot.releasedAt(s) {
				live = append(live, s)
			}
		}
		seats := liveSeatKeys(live)
		out := live
		for _, s := range snapshot.sessions {
			if !s.deadHooked && !s.landing {
				continue
			}
			if s.rig != "" && s.polecat != "" && seats[s.rig+"/"+s.polecat] {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}
}

// route decides the route. live distinguishes a sling that will spawn from a
// dry run: only a live sling claims a seat.
//
// The count it decides from is the pool's occupied seats: the live sessions and
// the seats mid-landing (poolOccupiedSessions, the same source the dispatch
// picture reads), plus the claims other slings hold — both read inside the one
// seat-decision lock begin takes (gt-8o35b). A polecat that submitted and left
// its seat to land still holds it, so a second sling admits into a full pool
// only when the pool has room (gt-3o7zk).
func (r *poolRouter) route(requested string, live bool) (agent, reason string, err error) {
	return r.routeWith(requested, live, r.occupied())
}

// routeWith is route over a given occupancy read. The spec dispatcher passes
// snapshotOccupied over the roster it already read, so a dispatched sling
// spends no seat reads of its own; route passes the router's full read.
func (r *poolRouter) routeWith(requested string, live bool, occupied func() ([]poolSession, error)) (agent, reason string, err error) {
	pool := r.pool()
	if pool == nil {
		return "", "", nil
	}
	// No release of a previous claim before counting: the store is this
	// spawn's, and a spawn makes one decision. A claim from an earlier spawn
	// was dropped by StartSession or the rollback that ended it, and
	// claimSessions skips this store's own claim anyway.
	sessions, seat, seatErr := r.seats.begin(live, occupied)
	defer seat.done()
	if seatErr != nil {
		// A seat the pool does not own is not the pool's to override on a
		// failed read any more than it is the pool's to admit (gt-67fj): the
		// request stands untouched, same as choosePoolAgent. Both failure
		// paths — an occupancy read and a claims read — take it.
		if !poolOwnsAgent(pool, requested) {
			return "", "", nil
		}
		// A town whose seats cannot be counted is not a town at its cap, so
		// the cap stays off here — a refusal would otherwise stop every sling
		// on a tmux hiccup — and the reason names the read that failed rather
		// than passing an unknown count off as a clean one (gt-t8q5, gt-67fj).
		// No seat is claimed on either path.
		fallback, named := poolUncountedFallback(pool, requested, r.now())
		var oe *poolOccupancyError
		if errors.As(seatErr, &oe) {
			what := "count seats"
			if oe.sessions {
				what = "list sessions"
			}
			return fallback,
				"pool: cannot " + what + " (" + seatErr.Error() + "), using " + named, nil
		}
		return fallback,
			"pool: cannot read seat claims (" + seatErr.Error() + "), using " + named, nil
	}
	agent, reason, refused, raise := choosePoolAgent(pool, requested, sessions, r.now())
	if live && !refused {
		seat.claimFor(agent, pool, "")
	}
	if seat.note != "" {
		reason = fmt.Sprintf("%s [%s]", reason, seat.note)
	}
	if refused {
		return agent, reason, &poolBackpressureError{Reason: reason, Raise: raise}
	}
	return agent, reason, nil
}
