package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
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
// The overflow seat is a seat too (gt-jzr1): capped at max_overflow, it
// turns a full local pool into a refused sling rather than an unbounded run
// of overflow spawns (10 polecats on a 3+3 town, spend pace $2.62/h).
//
// Bead-shape routing (gt-t8h1, plan Task 4 / design B1+B3) layers on top: the
// pool used to decide from the seat count alone, so a hard bug and a doc tweak
// competed for the same seat. Now the bead's labels and type pick the seat,
// every decision line names the agent it chose, and a bead that has already
// spent a local attempt cannot spend a second one. polecat_pool.idle_fill
// switches off the one branch that hands an overflow-shaped bead a free local
// seat (gt-nn7n).

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

// poolSeatCounts counts the live polecat sessions sitting on each of the pool's
// seats, and reports the newest local spawn — the timestamp the stagger guard
// reads. A nil pool, or one with no local_agent, has no seats to count.
//
// This is the pool's one count of itself, shared by the admission decision
// (choosePoolAgent) and the idle-seat patrol (gt daemon dispatch-check). The
// patrol must not count seats its own way: a nudge that named room the next
// sling would refuse is worse than no nudge, because it spends the mayor's
// attention to produce a refusal (gt-59o9).
func poolSeatCounts(pool *config.PolecatPool, sessions []poolSession) (local, overflow int, newestLocal time.Time) {
	if pool == nil || pool.LocalAgent == "" {
		return 0, 0, time.Time{}
	}
	for _, s := range sessions {
		switch {
		case s.agent == pool.LocalAgent:
			local++
			if s.created.After(newestLocal) {
				newestLocal = s.created
			}
		case pool.OverflowAgent != "" && s.agent == pool.OverflowAgent:
			overflow++
		}
	}
	return local, overflow, newestLocal
}

// choosePoolAgent decides the agent for a new polecat given the pool, the bead
// being slung, the agent the caller asked for, and the live polecat sessions.
// It is pure: the only side effect in routing — attaching localAttemptLabel —
// belongs to the caller. It returns "" with an empty reason when the pool has
// no opinion, so the caller keeps the agent it asked for (or role_agents when
// it asked for none), and refused when no seat is free, so the caller stops the
// sling instead of spawning past a cap.
//
// requested is the agent named on the command line (--agent), including the
// agent a convoy recorded at sling time. A request that names one of the pool's
// own seats is served by that seat's own rules or refused — never by the other
// seat, which would spend on the paid provider the caller did not ask for
// (gt-x40u). An agent the pool does not own leaves it nothing to admit, and the
// request stands untouched (gt-4lbz) — ahead of the spent-local-attempt rule,
// which records an attempt on the pool's own seat and so cannot answer for one
// the pool never owned (gt-gcrk).
//
// Every reason names the agent it chose or the seat it could not take. The
// three lines that name no agent — no pool configured, no local_agent, and a
// requested agent the pool does not own — are empty or say so, because the
// caller has yet to pick one.

// poolOwnsAgent reports whether requested names a seat the pool controls. An
// empty request has no seat to leave alone, so it is trivially "owned": the
// pool is free to pick for it. Both choosePoolAgent's rule 2 and poolRoute's
// own-error fallbacks (a tmux-listing or seat-claim read failure) test this
// same question — a seat the pool never owned is not the pool's to override
// on a hiccup any more than it is the pool's to admit (gt-67fj, gt-gcrk).
func poolOwnsAgent(pool *config.PolecatPool, requested string) bool {
	return requested == "" || requested == pool.LocalAgent || requested == pool.OverflowAgent
}

func choosePoolAgent(pool *config.PolecatPool, bead poolBead, requested string, sessions []poolSession, now time.Time) (agent, reason string, refused bool) {
	switch {
	case pool == nil:
		return "", "pool: no polecat pool configured", false
	case pool.LocalAgent == "":
		return "", "pool: polecat_pool has no local_agent; using the role default", false
	}
	local, over, newest := poolSeatCounts(pool, sessions)
	gap := pool.MinSpawnGapD()
	// The prefill guard. Every fresh local session spends 20-25k tokens of
	// prefill during which the decoding slots starve, so two local spawns
	// closer together than min_spawn_gap cost more than they buy.
	tooSoon := gap > 0 && local > 0 && now.Sub(newest) < gap
	overflow := poolAgentName(pool.OverflowAgent)
	// The overflow seat's own cap (gt-jzr1). Uncapped it grows with every
	// sling a full local pool turns away, and the spend it was meant to move
	// off the GPU comes back as an unbounded bill.
	overflowFull := pool.OverflowCapped() && over >= pool.MaxOverflow

	// refuse is the one place a spent cap becomes a decision: why names what
	// pushed the bead to the overflow seat, so a bead refused by its own shape
	// reads differently from one refused because the town is out of seats.
	refuse := func(why string) (string, string, bool) {
		return pool.OverflowAgent, fmt.Sprintf("pool: overflow full (%d/%d) -> no seat (%s)", over, pool.MaxOverflow, why), true
	}
	// refuseRequest is refuse() for a seat the caller named. The line names that
	// seat, so a request the pool could not serve reads as one rather than as the
	// pool's own choice of seat.
	refuseRequest := func(why string) (string, string, bool) {
		return requested, fmt.Sprintf("pool: %s -> no seat (requested %s)", why, requested), true
	}
	// localSeat is the one place the seat line is built: it is contractual
	// output, and it is what tells a reader which seat the polecat took.
	localSeat := func(why string) (string, string, bool) {
		return pool.LocalAgent, fmt.Sprintf("pool: local seat %d/%d -> %s (%s)", local+1, pool.MaxLocal, pool.LocalAgent, why), false
	}
	// seat decides a bead that wants the local agent (or has no preference),
	// applying the seat count and then the stagger. want names the branch for
	// the reason line.
	seat := func(want string) (string, string, bool) {
		switch {
		case pool.MaxLocal <= 0:
			// The local seat is closed: max_local 0 in a configured pool reads
			// as "no local dispatch" (gt-4lbz). A bead that wants it takes the
			// overflow seat or none — handing it to the role default instead
			// would spawn on the very seat the operator just emptied, which is
			// how a max_local 0 town kept growing local polecats.
			if overflowFull {
				return refuse(fmt.Sprintf("no local seats (max_local %d)", pool.MaxLocal))
			}
			return pool.OverflowAgent, fmt.Sprintf("pool: no local seats (max_local %d) -> %s", pool.MaxLocal, overflow), false
		case local >= pool.MaxLocal:
			if overflowFull {
				return refuse("local full")
			}
			return pool.OverflowAgent, fmt.Sprintf("pool: local full (%d/%d) -> %s", local, pool.MaxLocal, overflow), false
		case tooSoon:
			if overflowFull {
				return refuse("stagger " + now.Sub(newest).Round(time.Second).String() + " since last local spawn")
			}
			return pool.OverflowAgent, fmt.Sprintf("pool: stagger %s since last local spawn -> %s", now.Sub(newest).Round(time.Second), overflow), false
		}
		return localSeat(want)
	}
	// overflowFor sends the bead to the overflow agent, refuses when that seat
	// is capped out — an explicit route:flash label names a seat class, it does
	// not license spawning past the cap — and says why the bead's own shape (not
	// the pool's state) made the call.
	overflowFor := func(why string) (string, string, bool) {
		if overflowFull {
			return refuse(why)
		}
		return pool.OverflowAgent, fmt.Sprintf("pool: overflow -> %s (%s)", overflow, why), false
	}
	// requestedLocal is seat() for a request that named the local agent: the same
	// three rules, but a seat that cannot be taken refuses rather than being
	// served by the overflow seat. The caller asked for the free seat; the paid
	// one is the pool's answer to a bead it routed itself, not to a request
	// (gt-x40u).
	requestedLocal := func() (string, string, bool) {
		switch {
		case pool.MaxLocal <= 0:
			return refuseRequest(fmt.Sprintf("no local seats (max_local %d)", pool.MaxLocal))
		case local >= pool.MaxLocal:
			return refuseRequest(fmt.Sprintf("local full (%d/%d)", local, pool.MaxLocal))
		case tooSoon:
			return refuseRequest("stagger " + now.Sub(newest).Round(time.Second).String() + " since last local spawn")
		}
		return localSeat("requested " + requested)
	}

	// 1. Explicit routing wins over everything below, including a spent local
	//    attempt: an operator who labeled the bead has already decided.
	switch {
	case bead.hasLabel(routeLocalLabel):
		return seat("label " + routeLocalLabel)
	case bead.hasLabel(routeFlashLabel):
		return overflowFor("label " + routeFlashLabel)
	}
	// 2. An agent the pool does not own is not the pool's to admit, and the
	//    request stands untouched (gt-4lbz). It runs before rule 3 because
	//    local-attempt:1 records a spent attempt on the pool's own seat and says
	//    nothing about a seat the pool never owned: answering such a request with
	//    the overflow seat, or refusing it as full, spends where the caller never
	//    asked (gt-gcrk).
	if !poolOwnsAgent(pool, requested) {
		return "", "", false
	}
	// 3. One local attempt per bead (B3). The label is attached by the idle-seat
	//    fill branch, so a bead carrying it has already tried the local seat and
	//    lost — this is the redispatch.
	if bead.hasLabel(localAttemptLabel) {
		return overflowFor(localAttemptLabel + " failed")
	}
	// 4. The agent the caller asked for, when rule 2 left it in the pool's hands.
	//    It names a seat, it does not license taking one: a request for a seat the
	//    pool owns is admitted by that seat's own rules — so `--agent=<local>` on
	//    a full local pool is refused rather than over-filling the GPU or paying
	//    for the overflow seat the caller did not ask for (gt-x40u). The switch is
	//    exhaustive over the two seats rule 2 lets through; the return after it
	//    keeps a seat added later from falling into the bead-shape rules.
	if requested != "" {
		switch requested {
		case pool.LocalAgent:
			return requestedLocal()
		case pool.OverflowAgent:
			return overflowFor("requested " + requested)
		}
		return "", "", false
	}
	// 5. Bead shape.
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
		// one local attempt. polecat_pool.idle_fill=false sends the bead to the
		// overflow agent instead and names the knob, so a seat held open by the
		// switch does not read as a bead the shape rule overflowed (gt-nn7n).
		if local < pool.MaxLocal && !tooSoon {
			if !pool.IdleFillEnabled() {
				return overflowFor("type=" + beadTypeLabel(bead) + ", idle_fill off")
			}
			return localSeat(idleFillReason)
		}
		return overflowFor("type=" + beadTypeLabel(bead))
	}
	// 6. Unknown shape (a wisp, an epic, a bead whose lookup failed): the seat
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

// listPolecatSessions returns every live polecat session with its agent and
// creation time. Polecats are identified by the GT_ROLE their session
// carries ("<rig>/polecats/<name>"), so witnesses, refineries and dogs on
// the same server are not counted. GT_AGENT is written into the session
// environment at spawn (SessionStartOptions.Agent / AgentEnv fallback).
//
// A session surviving past `gt done` (preserved for recovery, or torn down
// a beat later than the polecat's own agent_state write) does not mean the
// polecat is still spending the seat: a done polecat sitting on an open MR
// is idle, waiting on the refinery, not on the GPU or the flash API (gt-2nft
// — a 4/3 overflow refusal with only two live sessions, the third and fourth
// "occupants" both done with their MR still in the queue). polecatSeatOccupied
// reads the polecat's own bead state, the same signal `gt polecat list` and
// scheduler.max_polecats (polecat_capacity.go) already trust over a session's
// mere presence, so a session that outlives its polecat's done+MR-open state
// never counts twice against two different capacity models.
func listPolecatSessions(t sessionLister, townRoot string) ([]poolSession, error) {
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
		if rigName, polecatName, ok := parsePolecatRole(role); ok && !polecatSeatOccupied(townRoot, rigName, polecatName) {
			continue
		}
		out = append(out, poolSession{name: n, agent: strings.TrimSpace(agent), created: created})
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
// on a lookup error or a townRoot-less caller (the peek helpers in tests):
// a pool that cannot read a polecat's state is not a pool that knows the
// seat is free, and undercounting risks the GPU overrun the pool exists to
// prevent (gt-md4z) rather than the overflow-refusal this fix targets.
func polecatSeatOccupied(townRoot, rigName, polecatName string) bool {
	if townRoot == "" {
		return true
	}
	disposition, err := poolPolecatDisposition(townRoot, rigName, polecatName)
	if err != nil {
		return true
	}
	return disposition.ReuseStatus != "idle-pr-open"
}

// poolPolecatDisposition reads one polecat's own agent bead and classifies it
// through the same WorkstateDisposition every other capacity model reads
// (polecat_capacity.go, `gt polecat list`), so "done with an open MR" means
// the same thing here as it does everywhere else. A var so tests can drive
// it without a live database.
var poolPolecatDisposition = func(townRoot, rigName, polecatName string) (polecat.WorkstateDisposition, error) {
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	_, fields, err := beads.New(filepath.Join(townRoot, rigName)).ForAgentBead().GetAgentBead(agentID)
	if err != nil {
		return polecat.WorkstateDisposition{}, err
	}
	if fields == nil {
		// No agent bead: nothing to read, and safest read is "still occupied"
		// (see polecatSeatOccupied's fail-open note) rather than a disposition
		// that happens to read as idle-pr-open by construction.
		return polecat.WorkstateDisposition{ReuseStatus: ""}, nil
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
	return polecat.DecideWorkstate(polecat.NewWorkstateInput(facts)), nil
}

// Seat claims (gt-eoi9).
//
// The pool counts live polecat sessions, and a session appears only once its
// spawn has finished — seconds after the decision that started it. Three slings
// launched in parallel therefore each counted the same single local session,
// each read "local seat 1/2", and all three spawned locally: the cap the pool
// exists to hold was broken by exactly the concurrency it was meant to bound
// (gt-eoi9 — opal, shale and agate all went local).
//
// A claim closes that window. Before deciding, a sling writes a seat claim on
// disk while holding a lock, so the next sling — which may be reading a tmux
// server that has never heard of the first — counts the claim as a local seat.
// The lock is what makes count-then-claim atomic; without it two slings read an
// empty claim set and both take the last seat. The claim is rebuilt as a
// poolSession (same agent, created at claim time) and merged into the session
// list, so it moves both the seat count and the stagger clock through the one
// policy in choosePoolAgent rather than through a second copy of the rules.
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

// poolSeatClaim is one sling's claim on a local seat, on disk so a concurrent
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

// poolSeatClaimFS is the filesystem every seat claim operation goes through. A
// var, like the other seams in this file, so a test can inject one.
var poolSeatClaimFS poolSeatFS = osPoolSeatFS{}

// poolSeatClaimStore holds the claim this process made. A process holds at most
// one — a sling spawns one polecat at a time, and the claim is dropped before
// the next decision — so a package var keeps it reachable from StartSession,
// the one place that knows the session has become countable, without threading
// a handle through the spawn call chain. A var, not a value, so tests can stand
// in for a second sling process.
type poolSeatClaimStore struct {
	mu  sync.Mutex
	dir string
	id  string
}

var processPoolSeatClaims = &poolSeatClaimStore{}

// ownID returns the claim this process holds, or "" when it holds none.
func (c *poolSeatClaimStore) ownID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.id
}

func (c *poolSeatClaimStore) hold(dir, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dir, c.id = dir, id
}

// release drops this process's claim. Idempotent, and a no-op when the process
// never claimed a seat (an overflow route, an explicit --agent, a dry run).
func (c *poolSeatClaimStore) release() {
	c.mu.Lock()
	dir, id := c.dir, c.id
	c.dir, c.id = "", ""
	c.mu.Unlock()
	if dir == "" || id == "" {
		return
	}
	_ = poolSeatClaimFS.Remove(filepath.Join(dir, id+".json"))
}

// releasePoolSeatClaim drops this process's seat claim. Every path a claim
// stops standing for a polecat calls it: StartSession, once the tmux session
// exists and is the record of that seat (holding both would read one polecat as
// two); the error returns of the spawn the claim was made for; and the rollback
// of a spawn whose session never started (gt-t8q5).
func releasePoolSeatClaim() { processPoolSeatClaims.release() }

// poolSeatDecision is the locked window in which a sling counts the seats other
// slings have claimed and takes one of its own.
type poolSeatDecision struct {
	lock     *flock.Flock
	townRoot string
	// note is set when a claim could not be taken; the caller appends it to
	// the reason so a racy decision never passes for a reserved one.
	note string
}

// beginPoolSeatDecision merges the claims other slings hold into sessions and
// opens the window to claim one. The caller must call done(), error or not.
//
// A dry run merges the same claims — so it prints the route a real sling would
// take — but neither cleans up nor claims: a preview must not change the pool's
// state.
//
// An error means the claims could not be read: the seats other slings hold are
// then unknown rather than absent, and the caller must not decide as if the set
// were empty (gt-t8q5).
func beginPoolSeatDecision(townRoot string, live bool, sessions []poolSession) ([]poolSession, *poolSeatDecision, error) {
	d := &poolSeatDecision{townRoot: townRoot}
	ownID := processPoolSeatClaims.ownID()
	if !live {
		claims, err := poolSeatClaimSessions(townRoot, ownID)
		return append(sessions, claims...), d, err
	}
	lock, err := lockPoolDecision(townRoot)
	if err != nil {
		// The route is still decided, from live sessions alone — the behavior
		// before claims existed — and the reason says the seat could not be
		// reserved rather than passing a racy decision off as a reserved one.
		d.note = "seat not reserved: " + err.Error()
		return sessions, d, nil
	}
	d.lock = lock
	cleanupStalePoolSeatClaims(townRoot, time.Now())
	claims, claimErr := poolSeatClaimSessions(townRoot, ownID)
	return append(sessions, claims...), d, claimErr
}

// claimFor takes a seat for the route the caller chose: the local seat always,
// the overflow seat when it is capped. A cap nothing claims is the gt-eoi9 race
// again, on the seat whose slings are all overflow routes with no stagger to
// slow them down.
func (d *poolSeatDecision) claimFor(agent string, pool *config.PolecatPool, beadID string) {
	if d.lock == nil || pool == nil {
		return
	}
	capped := agent == pool.LocalAgent || (agent == pool.OverflowAgent && pool.OverflowCapped())
	if !capped {
		return
	}
	claim, err := writePoolSeatClaim(d.townRoot, agent, beadID)
	if err != nil {
		d.note = "seat not reserved: " + err.Error()
		return
	}
	processPoolSeatClaims.hold(poolSeatClaimDir(d.townRoot), claim.ID)
}

func (d *poolSeatDecision) done() {
	if d.lock == nil {
		return
	}
	_ = d.lock.Unlock()
}

// poolSeatClaimSessions renders the claims other slings hold as poolSessions so
// the policy in choosePoolAgent counts them. ownID is skipped: a process still
// holding a claim is about to drop it, and counting both the claim and the
// session it stands for would read one polecat as two seats.
//
// An error is the claim set failing to read, which the caller must answer for
// rather than treating as no claims (gt-t8q5).
func poolSeatClaimSessions(townRoot, ownID string) ([]poolSession, error) {
	claims, err := readPoolSeatClaims(townRoot)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, nil
	}
	out := make([]poolSession, 0, len(claims))
	for _, c := range claims {
		if c.ID == ownID || c.Agent == "" {
			continue
		}
		out = append(out, poolSession{name: "pool-claim/" + c.ID, agent: c.Agent, created: c.CreatedAt})
	}
	return out, nil
}

// readPoolSeatClaims reads the claims on disk, discarding entries that cannot
// be read or that do not name themselves (a half-written or hand-edited file
// must not hold a seat).
//
// A missing directory is "no claim has ever been made here" and reads as an
// empty set. Any other failure to read the directory is an error, because a
// claim set that could not be read is not a claim set that is empty: the seats
// left out are the ones the cap is holding, and a decision that counted them as
// none would take the seat of a polecat another sling is already spawning
// (gt-t8q5).
func readPoolSeatClaims(townRoot string) ([]poolSeatClaim, error) {
	dir := poolSeatClaimDir(townRoot)
	entries, err := poolSeatClaimFS.ReadDir(dir)
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
		data, err := poolSeatClaimFS.ReadFile(path)
		if err != nil {
			_ = poolSeatClaimFS.Remove(path)
			continue
		}
		var claim poolSeatClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			_ = poolSeatClaimFS.Remove(path)
			continue
		}
		if claim.ID == "" || claim.PID <= 0 || claim.CreatedAt.IsZero() || claim.ID+".json" != entry.Name() {
			_ = poolSeatClaimFS.Remove(path)
			continue
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

// cleanupStalePoolSeatClaims drops claims that cannot become sessions: the
// process that made them is gone, or it has held the seat past the TTL. Both
// mean the seat is free, and a stale claim is worse than a missing one — it
// would push a bead to the overflow agent for a seat nobody is using.
func cleanupStalePoolSeatClaims(townRoot string, now time.Time) {
	dir := poolSeatClaimDir(townRoot)
	claims, err := readPoolSeatClaims(townRoot)
	if err != nil {
		// A claim set that could not be read is one this pass cannot prune:
		// the claims it might drop are the ones it cannot see, so it drops
		// nothing rather than guessing at file names in a directory it cannot
		// enumerate. The decision that called this reports the failure itself
		// (gt-t8q5).
		return
	}
	for _, claim := range claims {
		if processAlive(claim.PID) && now.Sub(claim.CreatedAt) <= poolSeatClaimTTL {
			continue
		}
		_ = poolSeatClaimFS.Remove(filepath.Join(dir, claim.ID+".json"))
	}
}

// writePoolSeatClaim claims a seat for this process and the bead it is about
// to spawn for.
func writePoolSeatClaim(townRoot, agent, beadID string) (poolSeatClaim, error) {
	now := time.Now().UTC()
	return publishPoolSeatClaim(townRoot, poolSeatClaim{
		ID:        fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano()),
		PID:       os.Getpid(),
		Agent:     agent,
		Bead:      beadID,
		CreatedAt: now,
	})
}

// publishPoolSeatClaim writes a claim atomically (write, then rename), so a
// concurrent decision never reads a half-written claim.
func publishPoolSeatClaim(townRoot string, claim poolSeatClaim) (poolSeatClaim, error) {
	dir := poolSeatClaimDir(townRoot)
	if err := poolSeatClaimFS.MkdirAll(dir, 0755); err != nil {
		return poolSeatClaim{}, fmt.Errorf("creating seat claim dir: %w", err)
	}
	path := filepath.Join(dir, claim.ID+".json")
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return poolSeatClaim{}, err
	}
	if err := poolSeatClaimFS.WriteFile(tmpPath, data, 0644); err != nil {
		return poolSeatClaim{}, fmt.Errorf("writing seat claim: %w", err)
	}
	if err := poolSeatClaimFS.Rename(tmpPath, path); err != nil {
		_ = poolSeatClaimFS.Remove(tmpPath)
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

// resolvePolecatPoolAgent applies the town's polecat_pool to a sling, whatever
// agent it asked for. It reads the bead's type and labels, claims the seat it
// routes to while it decides (so a sling racing this one sees the seat as
// taken), attaches localAttemptLabel when the idle-seat fill branch fires, and
// returns the agent to use with a one-line reason naming that agent. An empty
// agent with an empty reason means the pool has no opinion and the caller's own
// choice stands. It returns errPoolBackpressure when every seat the bead could
// take is at its cap.
func resolvePolecatPoolAgent(townRoot, beadID, requested string) (agent, reason string, err error) {
	return poolRoute(townRoot, beadID, requested, true)
}

// peekPolecatPoolAgent is resolvePolecatPoolAgent without the side effects:
// `gt sling --dry-run` must print the route it would take — a refusal included,
// since that is the route a live sling would take — without claiming a seat or
// consuming the bead's one local attempt.
func peekPolecatPoolAgent(townRoot, beadID, requested string) (agent, reason string, err error) {
	return poolRoute(townRoot, beadID, requested, false)
}

// errPoolBackpressure identifies a pool refusal so callers and tests can match
// it with errors.Is without parsing the message.
var errPoolBackpressure = errors.New("polecat pool backpressure")

// poolBackpressureError is the typed refusal: every seat the bead could take is
// at its cap. The message leads with `sling refused:`, the marker that tells
// the convoy feeder to defer the bead instead of failing it (see
// internal/daemon/convoy_sling_backpressure.go), and carries the pool's own
// reason line.
//
// There is no flag that spawns past the cap, deliberately (gt-4lbz). Every
// automated redispatch path — the convoy feeder, the deacon's RECOVERED_BEAD
// redispatch, the dead-holder auto-force in sling.go — carries --force for the
// safety guards it also needs, so a cap that --force opens is a cap no
// automated path is actually held by: those are the two spawns (a 4th flash
// session with max_overflow 3, and local spawns with max_local 0) this guard
// exists to stop. Capacity is a property of the town, so it is raised where it
// is declared — polecat_pool.max_local / max_overflow — which the pool reads on
// every sling.
type poolBackpressureError struct {
	Reason string
}

func (e *poolBackpressureError) Error() string {
	return dispatch.SlingRefusalMarker + " " + e.Reason + "; raise polecat_pool.max_local/max_overflow to spawn"
}

func (e *poolBackpressureError) Unwrap() error { return errPoolBackpressure }

// poolUncountedFallback names the agent a decision falls back to when it cannot
// count something it decides from: the overflow agent, or the role default when
// the pool has none, which the caller resolves. The reason line names whichever
// it was, so a fallback never reads as a deliberate route.
func poolUncountedFallback(pool *config.PolecatPool) string {
	if pool.OverflowAgent != "" {
		return pool.OverflowAgent
	}
	return "the role default"
}

// poolRoute decides the route. live distinguishes a sling that will spawn from
// a dry run: only a live sling claims a seat or writes labels.
func poolRoute(townRoot, beadID, requested string, live bool) (agent, reason string, err error) {
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil || ts == nil || ts.PolecatPool == nil {
		return "", "", nil
	}
	// A bead lookup failure is not fatal: the policy falls back to the seat
	// count and the reason says so, so a seat-only decision never passes for a
	// deliberate route (B1).
	var bead poolBead
	var beadErr error
	if beadID != "" {
		bead, beadErr = poolBeadLookup(townRoot, beadID)
	}
	sessions, err := listPolecatSessions(newPoolSessionLister(), townRoot)
	if err != nil {
		// A seat the pool does not own is not the pool's to override on a
		// tmux hiccup any more than it is the pool's to admit (gt-67fj): the
		// request stands untouched, same as choosePoolAgent's rule 2.
		if !poolOwnsAgent(ts.PolecatPool, requested) {
			return "", "", nil
		}
		// Without a session count the pool cannot be trusted: fall back to
		// the overflow agent (or the role default when none is set) rather
		// than risk over-filling the GPU. A town whose sessions cannot be
		// counted is not a town at its cap, so the overflow cap stays off
		// here too — the refusal below would otherwise stop every sling on a
		// tmux hiccup.
		return ts.PolecatPool.OverflowAgent,
			"pool: cannot list sessions (" + err.Error() + "), using " + poolUncountedFallback(ts.PolecatPool), nil
	}
	if live {
		// The claim this process holds stood for its previous spawn, whose
		// session is countable by now. Drop it before counting, or a batch
		// sling reads its own last polecat as two seats.
		releasePoolSeatClaim()
	}
	sessions, seat, claimErr := beginPoolSeatDecision(townRoot, live, sessions)
	defer seat.done()
	if claimErr != nil {
		// As above: a seat the pool does not own stands untouched rather than
		// being overridden by a claims-read failure (gt-67fj).
		if !poolOwnsAgent(ts.PolecatPool, requested) {
			return "", "", nil
		}
		// The seats other slings hold could not be read, so the count this
		// decision would run on is unknown — not zero. The seat a failure to
		// read can hide is the local one, and taking it on a count that cannot
		// see the claims is the gt-eoi9 race with its guard removed, so this
		// sling does not take it: it falls back the same way a session list
		// that cannot be read does above, and the reason names the failure
		// rather than passing an unknown count off as a clean one (gt-t8q5).
		// No seat is claimed on this path — the fallback is a route taken
		// without a reservation, and the cap it bypasses is the one that could
		// not be counted.
		return ts.PolecatPool.OverflowAgent,
			"pool: cannot read seat claims (" + claimErr.Error() + "), using " + poolUncountedFallback(ts.PolecatPool), nil
	}
	agent, reason, refused := choosePoolAgent(ts.PolecatPool, bead, requested, sessions, time.Now())
	if live && !refused {
		seat.claimFor(agent, ts.PolecatPool, beadID)
	}
	if beadErr != nil {
		reason = fmt.Sprintf("%s [bead %s unreadable: %v]", reason, beadID, beadErr)
	}
	if seat.note != "" {
		reason = fmt.Sprintf("%s [%s]", reason, seat.note)
	}
	if refused {
		return agent, reason, &poolBackpressureError{Reason: reason}
	}
	if live && beadID != "" && strings.Contains(reason, idleFillReason) {
		if err := poolBeadLabelAdd(townRoot, beadID, localAttemptLabel); err != nil {
			reason = fmt.Sprintf("%s [%s label failed: %v]", reason, localAttemptLabel, err)
		}
	}
	return agent, reason, nil
}
