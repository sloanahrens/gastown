package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
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

// choosePoolAgent decides the agent for a new polecat given the pool, the bead
// being slung, and the live polecat sessions. It is pure: the only side effect
// in routing — attaching localAttemptLabel — belongs to the caller. It returns
// "" when the pool does not apply (unset, or no local agent) so the caller
// falls back to role_agents, and refused when no seat is free, so the caller
// stops the sling instead of spawning past a cap.
//
// Every reason names the agent it chose or the seat it could not take, except
// the three "the pool does not apply" lines, which name no agent because the
// caller has yet to pick one.
func choosePoolAgent(pool *config.PolecatPool, bead poolBead, sessions []poolSession, now time.Time) (agent, reason string, refused bool) {
	switch {
	case pool == nil:
		return "", "pool: no polecat pool configured", false
	case pool.LocalAgent == "":
		return "", "pool: polecat_pool has no local_agent; using the role default", false
	case pool.MaxLocal <= 0:
		return "", fmt.Sprintf("pool: polecat_pool max_local is %d; using the role default", pool.MaxLocal), false
	}
	local, over := 0, 0
	var newest time.Time
	for _, s := range sessions {
		switch {
		case s.agent == pool.LocalAgent:
			local++
			if s.created.After(newest) {
				newest = s.created
			}
		case pool.OverflowAgent != "" && s.agent == pool.OverflowAgent:
			over++
		}
	}
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
	_ = os.Remove(filepath.Join(dir, id+".json"))
}

// releasePoolSeatClaim drops this process's seat claim. StartSession calls it
// when the tmux session exists: the session is now the record of that seat, and
// holding both would read one polecat as two.
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
// opens the window to claim one. The caller must call done().
//
// A dry run merges the same claims — so it prints the route a real sling would
// take — but neither cleans up nor claims: a preview must not change the pool's
// state.
func beginPoolSeatDecision(townRoot string, live bool, sessions []poolSession) ([]poolSession, *poolSeatDecision) {
	d := &poolSeatDecision{townRoot: townRoot}
	if !live {
		return append(sessions, poolSeatClaimSessions(townRoot, processPoolSeatClaims.ownID())...), d
	}
	lock, err := lockPoolDecision(townRoot)
	if err != nil {
		// The route is still decided, from live sessions alone — the behavior
		// before claims existed — and the reason says the seat could not be
		// reserved rather than passing a racy decision off as a reserved one.
		d.note = "seat not reserved: " + err.Error()
		return sessions, d
	}
	d.lock = lock
	cleanupStalePoolSeatClaims(townRoot, time.Now())
	return append(sessions, poolSeatClaimSessions(townRoot, processPoolSeatClaims.ownID())...), d
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
func poolSeatClaimSessions(townRoot, ownID string) []poolSession {
	claims := readPoolSeatClaims(townRoot)
	if len(claims) == 0 {
		return nil
	}
	out := make([]poolSession, 0, len(claims))
	for _, c := range claims {
		if c.ID == ownID || c.Agent == "" {
			continue
		}
		out = append(out, poolSession{name: "pool-claim/" + c.ID, agent: c.Agent, created: c.CreatedAt})
	}
	return out
}

// readPoolSeatClaims reads the claims on disk, discarding entries that cannot
// be read or that do not name themselves (a half-written or hand-edited file
// must not hold a seat).
func readPoolSeatClaims(townRoot string) []poolSeatClaim {
	dir := poolSeatClaimDir(townRoot)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// No directory yet means no claim has ever been made here.
		return nil
	}
	claims := make([]poolSeatClaim, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		var claim poolSeatClaim
		if err := json.Unmarshal(data, &claim); err != nil {
			_ = os.Remove(path)
			continue
		}
		if claim.ID == "" || claim.PID <= 0 || claim.CreatedAt.IsZero() || claim.ID+".json" != entry.Name() {
			_ = os.Remove(path)
			continue
		}
		claims = append(claims, claim)
	}
	return claims
}

// cleanupStalePoolSeatClaims drops claims that cannot become sessions: the
// process that made them is gone, or it has held the seat past the TTL. Both
// mean the seat is free, and a stale claim is worse than a missing one — it
// would push a bead to the overflow agent for a seat nobody is using.
func cleanupStalePoolSeatClaims(townRoot string, now time.Time) {
	dir := poolSeatClaimDir(townRoot)
	for _, claim := range readPoolSeatClaims(townRoot) {
		if processAlive(claim.PID) && now.Sub(claim.CreatedAt) <= poolSeatClaimTTL {
			continue
		}
		_ = os.Remove(filepath.Join(dir, claim.ID+".json"))
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
	if err := os.MkdirAll(dir, 0755); err != nil {
		return poolSeatClaim{}, fmt.Errorf("creating seat claim dir: %w", err)
	}
	path := filepath.Join(dir, claim.ID+".json")
	tmpPath := path + ".tmp"
	data, err := json.MarshalIndent(claim, "", "  ")
	if err != nil {
		return poolSeatClaim{}, err
	}
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return poolSeatClaim{}, fmt.Errorf("writing seat claim: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
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

// resolvePolecatPoolAgent applies the town's polecat_pool to a sling that
// gave no --agent. It reads the bead's type and labels, claims the seat it
// routes to while it decides (so a sling racing this one sees the seat as
// taken), attaches localAttemptLabel when the idle-seat fill branch fires, and
// returns the agent to use ("" = role default) with a one-line reason naming
// that agent. It returns errPoolBackpressure when every seat the bead could
// take is at its cap and force is false.
func resolvePolecatPoolAgent(townRoot, beadID string, force bool) (agent, reason string, err error) {
	return poolRoute(townRoot, beadID, true, force)
}

// peekPolecatPoolAgent is resolvePolecatPoolAgent without the side effects:
// `gt sling --dry-run` must print the route it would take — a refusal included,
// since that is the route a live sling would take — without claiming a seat or
// consuming the bead's one local attempt.
func peekPolecatPoolAgent(townRoot, beadID string, force bool) (agent, reason string, err error) {
	return poolRoute(townRoot, beadID, false, force)
}

// errPoolBackpressure identifies a pool refusal so callers and tests can match
// it with errors.Is without parsing the message.
var errPoolBackpressure = errors.New("polecat pool backpressure")

// poolBackpressureError is the typed refusal: every seat the bead could take is
// at its cap. The message leads with `sling refused:`, the marker that tells
// the convoy feeder to defer the bead instead of failing it (see
// internal/daemon/convoy_sling_backpressure.go), carries the pool's own reason
// line and names the way through.
type poolBackpressureError struct {
	Reason string
}

func (e *poolBackpressureError) Error() string {
	return "sling refused: " + e.Reason + "; pass --force to spawn anyway"
}

func (e *poolBackpressureError) Unwrap() error { return errPoolBackpressure }

// poolRoute decides the route. live distinguishes a sling that will spawn from
// a dry run: only a live sling claims a seat or writes labels.
func poolRoute(townRoot, beadID string, live, force bool) (agent, reason string, err error) {
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
	sessions, err := listPolecatSessions(newPoolSessionLister())
	if err != nil {
		// Without a session count the pool cannot be trusted: fall back to
		// the overflow agent (or the role default when none is set) rather
		// than risk over-filling the GPU. A town whose sessions cannot be
		// counted is not a town at its cap, so the overflow cap stays off
		// here too — the refusal below would otherwise stop every sling on a
		// tmux hiccup.
		fallback := "the role default"
		if ts.PolecatPool.OverflowAgent != "" {
			fallback = ts.PolecatPool.OverflowAgent
		}
		return ts.PolecatPool.OverflowAgent, "pool: cannot list sessions (" + err.Error() + "), using " + fallback, nil
	}
	if live {
		// The claim this process holds stood for its previous spawn, whose
		// session is countable by now. Drop it before counting, or a batch
		// sling reads its own last polecat as two seats.
		releasePoolSeatClaim()
	}
	sessions, seat := beginPoolSeatDecision(townRoot, live, sessions)
	defer seat.done()
	agent, reason, refused := choosePoolAgent(ts.PolecatPool, bead, sessions, time.Now())
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
		if !force {
			return agent, reason, &poolBackpressureError{Reason: reason}
		}
		reason = fmt.Sprintf("%s [--force: spawning past the overflow cap]", reason)
	}
	if live && beadID != "" && strings.Contains(reason, idleFillReason) {
		if err := poolBeadLabelAdd(townRoot, beadID, localAttemptLabel); err != nil {
			reason = fmt.Sprintf("%s [%s label failed: %v]", reason, localAttemptLabel, err)
		}
	}
	return agent, reason, nil
}
