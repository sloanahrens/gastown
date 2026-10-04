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

// Polecat seat pool (gt-md4z, gt-jzr1).
//
// polecat_pool caps how many polecats run on the pool's agent (overflow_agent)
// at once: max_overflow live sessions, after which a sling is refused rather
// than spawning an unbounded run of paid sessions (10 polecats on a 3+3 town,
// spend pace $2.62/h). The pool once also ran a bounded local-model seat
// (local_agent, max_local) with bead-shape routing between the two; the local
// model was retired on 2026-09-27 and its seat with it (D4), so the overflow
// seat is the pool's only seat. The key keeps its overflow_agent name so
// existing settings files still load.

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

// poolSession is what the policy needs to know about one live polecat.
type poolSession struct {
	name    string
	agent   string // GT_AGENT in the tmux session environment
	created time.Time
}

// poolSeatCount counts the live polecat sessions sitting on the pool's seat. A
// nil pool, or one with no overflow_agent, has no seat to count.
//
// This is the pool's one count of itself, shared by the admission decision
// (choosePoolAgent) and the idle-seat patrol (gt daemon dispatch-check). The
// patrol must not count seats its own way: a nudge that named room the next
// sling would refuse is worse than no nudge, because it spends the mayor's
// attention to produce a refusal (gt-59o9).
func poolSeatCount(pool *config.PolecatPool, sessions []poolSession) int {
	if pool == nil || pool.OverflowAgent == "" {
		return 0
	}
	n := 0
	for _, s := range sessions {
		if s.agent == pool.OverflowAgent {
			n++
		}
	}
	return n
}

// poolOwnsAgent reports whether requested names the seat the pool controls. An
// empty request has no seat to leave alone, so it is trivially "owned": the
// pool is free to pick for it. Both choosePoolAgent and the router's own-error
// fallbacks (a tmux-listing or seat-claim read failure) test this same
// question — a seat the pool never owned is not the pool's to override on a
// hiccup any more than it is the pool's to admit (gt-67fj, gt-gcrk).
func poolOwnsAgent(pool *config.PolecatPool, requested string) bool {
	return requested == "" || requested == pool.OverflowAgent
}

// choosePoolAgent decides the agent for a new polecat given the pool, the agent
// the caller asked for, and the live polecat sessions. It is pure. It returns
// "" with an empty reason when the pool has no opinion, so the caller keeps the
// agent it asked for (or role_agents when it asked for none), and refused when
// the seat is at its cap, so the caller stops the sling instead of spawning
// past it.
//
// requested is the agent named on the command line (--agent), including any
// agent recorded at sling time. An agent the pool does not own leaves
// it nothing to admit, and the request stands untouched (gt-4lbz). A request
// for the pool's own agent is admitted by the seat's cap like any other sling.
func choosePoolAgent(pool *config.PolecatPool, requested string, sessions []poolSession) (agent, reason string, refused bool) {
	switch {
	case pool == nil:
		return "", "pool: no polecat pool configured", false
	case pool.OverflowAgent == "":
		return "", "pool: polecat_pool has no overflow_agent; using the role default", false
	case !poolOwnsAgent(pool, requested):
		return "", "", false
	}
	n := poolSeatCount(pool, sessions)
	if !pool.OverflowCapped() {
		return pool.OverflowAgent, fmt.Sprintf("pool: seat %d (uncapped) -> %s", n+1, pool.OverflowAgent), false
	}
	if n >= pool.MaxOverflow {
		return pool.OverflowAgent, fmt.Sprintf("pool: full (%d/%d) -> no seat for %s", n, pool.MaxOverflow, pool.OverflowAgent), true
	}
	return pool.OverflowAgent, fmt.Sprintf("pool: seat %d/%d -> %s", n+1, pool.MaxOverflow, pool.OverflowAgent), false
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
type polecatDispositionFunc func(rigName, polecatName string) (polecat.WorkstateDisposition, error)

// poolDispositionFor is the polecat-state read a caller in townRoot wants, or
// nil when there is no town to read (a caller that then fails open; see
// polecatSeatOccupied). Every reader of a polecat's own seat state builds it
// here, so the seat picture the dispatchers take (poolSeatSessions) and any
// other count cannot read the same polecat two ways.
func poolDispositionFor(townRoot string) polecatDispositionFunc {
	if townRoot == "" {
		return nil
	}
	return func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
		return poolPolecatDisposition(townRoot, rigName, polecatName)
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
// — a 4/3 overflow refusal with only two live sessions, the third and fourth
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
		if rigName, polecatName, ok := parsePolecatRole(role); ok && !polecatSeatOccupied(disposition, rigName, polecatName) {
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
// on a lookup error or a caller with no way to read the state (a townRoot-less
// listPolecatSessions): a pool that cannot read a polecat's state is not a pool
// that knows the seat is free, and undercounting risks the overrun the pool
// exists to prevent (gt-md4z) rather than the overflow-refusal this fix targets.
func polecatSeatOccupied(disposition polecatDispositionFunc, rigName, polecatName string) bool {
	if disposition == nil {
		return true
	}
	d, err := disposition(rigName, polecatName)
	if err != nil {
		return true
	}
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

// poolPolecatDisposition classifies one polecat through the same
// WorkstateDisposition every other capacity model reads (polecat_capacity.go,
// `gt polecat list`): its agent bead, the live git facts of its worktree, and
// the terminality of the bead it is assigned. So "done with an open MR", and
// "finished with its leftover commits on its branch", each mean the same thing
// here as they do everywhere else.
func poolPolecatDisposition(townRoot, rigName, polecatName string) (polecat.WorkstateDisposition, error) {
	prefix := beads.GetPrefixForRig(townRoot, rigName)
	agentID := beads.PolecatBeadIDWithPrefix(prefix, rigName, polecatName)
	_, fields, err := beads.GetAgentBead(beads.New(filepath.Join(townRoot, rigName)), agentID)
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
	rigPath := filepath.Join(townRoot, rigName)
	// The same live probe the list path runs: a reuse and capacity decision
	// comes from measured facts, not recalled ones. Without it the only trace
	// of a finished seat's leftover commits is its recorded cleanup_status,
	// which cannot tell the classifier the commits are preserved on the branch
	// — so the pool would keep filling a seat the list has already stopped
	// counting (gt-b9ud0, the gt-2nft shape of two pictures of one pool). A
	// worktree that cannot be resolved is left unprobed, and the recorded hint
	// stands.
	if worktree := resolvePolecatWorktree(filepath.Join(rigPath, "polecats"), polecatName, rigName); worktree != "" {
		polecat.ProbeLiveGitStateLocal(worktree).ApplyFacts(&facts)
	}
	// The assignment terminality the classifier accounts that seat with, read
	// the way the reuse gate reads it (Manager.workstateInputForPolecat: the
	// source-issue hint, else the hook reference), so the pool cannot disagree
	// with the reuse gate about which bead "assigned" means. A lookup that
	// fails answers false and the seat keeps counting: the exemption has to be
	// proven, not assumed.
	if hint := agentSourceIssueHint("", fields); hint != "" {
		if issue, err := beads.New(rigPath).Show(hint); err == nil && issue != nil {
			facts.AssignedBeadTerminal = beads.IssueStatus(issue.Status).IsTerminal()
		}
	}
	return polecat.DecideWorkstate(polecat.NewWorkstateInput(facts)), nil
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

// begin merges the claims other slings hold into sessions and opens the window
// to claim one. The caller must call done(), error or not.
//
// A dry run merges the same claims — so it prints the route a real sling would
// take — but neither cleans up nor claims: a preview must not change the pool's
// state.
//
// An error means the claims could not be read: the seats other slings hold are
// then unknown rather than absent, and the caller must not decide as if the set
// were empty (gt-t8q5).
func (l *poolSeatLedger) begin(live bool, sessions []poolSession) ([]poolSession, *poolSeatDecision, error) {
	d := &poolSeatDecision{ledger: l}
	ownID := l.store.ownID()
	if !live {
		claims, err := l.claimSessions(ownID)
		return append(sessions, claims...), d, err
	}
	unlock, err := l.lock()
	if err != nil {
		// The route is still decided, from live sessions alone — the behavior
		// before claims existed — and the reason says the seat could not be
		// reserved rather than passing a racy decision off as a reserved one.
		d.note = "seat not reserved: " + err.Error()
		return sessions, d, nil
	}
	d.unlock = unlock
	l.cleanupStale()
	claims, claimErr := l.claimSessions(ownID)
	return append(sessions, claims...), d, claimErr
}

// claimFor takes a seat for the route the caller chose when that seat is
// capped. A cap nothing claims is the gt-eoi9 race again.
func (d *poolSeatDecision) claimFor(agent string, pool *config.PolecatPool, beadID string) {
	if d.unlock == nil || pool == nil {
		return
	}
	if agent != pool.OverflowAgent || !pool.OverflowCapped() {
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
	out := make([]poolSession, 0, len(claims))
	for _, c := range claims {
		if c.ID == ownID || c.Agent == "" {
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
		if l.alive(claim.PID) && now.Sub(claim.CreatedAt) <= poolSeatClaimTTL {
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
	return &poolRouter{
		townRoot: townRoot,
		pool: func() *config.PolecatPool {
			ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
			if err != nil || ts == nil {
				return nil
			}
			return ts.PolecatPool
		},
		sessions: newPoolSessionLister,
		disposition: func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
			return poolPolecatDisposition(townRoot, rigName, polecatName)
		},
		work:  poolSeatWorkFor(townRoot),
		seats: poolSeatLedgerFor(townRoot, store),
		now:   time.Now,
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
// with max_overflow 3) this guard exists to stop. Capacity is a property of
// the town, so it is raised where it is declared — polecat_pool.max_overflow —
// which the pool reads on every sling.
type poolBackpressureError struct {
	Reason string
}

func (e *poolBackpressureError) Error() string {
	return dispatch.SlingRefusalMarker + " " + e.Reason + "; raise polecat_pool.max_overflow to spawn"
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

// route decides the route. live distinguishes a sling that will spawn from a
// dry run: only a live sling claims a seat.
//
// The count it decides from is the pool's occupied seats: the live sessions and
// the seats mid-landing (poolOccupiedSessions, the same source the dispatch
// picture reads), plus the claims other slings hold, folded in under the
// seat-decision lock below. A polecat that submitted and left its seat to land
// still holds it, so a second sling admits into a full pool only when the pool
// has room (gt-3o7zk).
func (r *poolRouter) route(requested string, live bool) (agent, reason string, err error) {
	pool := r.pool()
	if pool == nil {
		return "", "", nil
	}
	// The seats the pool already holds: the live sessions and the seats
	// mid-landing, from the same occupancy source the dispatch picture reads,
	// so a live sling counts a seat a mid-landing polecat still occupies
	// (gt-3o7zk). The claims join the count under the lock below.
	sessions, err := poolOccupiedSessions(r.sessions(), r.townRoot, r.disposition, r.work, pool, r.now)
	if err != nil {
		// A seat the pool does not own is not the pool's to override on a
		// tmux hiccup any more than it is the pool's to admit (gt-67fj): the
		// request stands untouched, same as choosePoolAgent.
		if !poolOwnsAgent(pool, requested) {
			return "", "", nil
		}
		// A town whose seats cannot be counted is not a town at its cap, so
		// the cap stays off here — a refusal would otherwise stop every sling
		// on a tmux hiccup — and the reason says so.
		what := "count seats"
		var oe *poolOccupancyError
		if errors.As(err, &oe) && oe.sessions {
			what = "list sessions"
		}
		return pool.OverflowAgent,
			"pool: cannot " + what + " (" + err.Error() + "), using " + poolUncountedFallback(pool), nil
	}
	// No release of a previous claim before counting: the store is this
	// spawn's, and a spawn makes one decision. A claim from an earlier spawn
	// was dropped by StartSession or the rollback that ended it, and
	// claimSessions skips this store's own claim anyway.
	sessions, seat, claimErr := r.seats.begin(live, sessions)
	defer seat.done()
	if claimErr != nil {
		// As above: a seat the pool does not own stands untouched rather than
		// being overridden by a claims-read failure (gt-67fj).
		if !poolOwnsAgent(pool, requested) {
			return "", "", nil
		}
		// The seats other slings hold could not be read, so the count this
		// decision would run on is unknown — not zero. It falls back the same
		// way a session list that cannot be read does above, and the reason
		// names the failure rather than passing an unknown count off as a
		// clean one (gt-t8q5). No seat is claimed on this path.
		return pool.OverflowAgent,
			"pool: cannot read seat claims (" + claimErr.Error() + "), using " + poolUncountedFallback(pool), nil
	}
	agent, reason, refused := choosePoolAgent(pool, requested, sessions)
	if live && !refused {
		seat.claimFor(agent, pool, "")
	}
	if seat.note != "" {
		reason = fmt.Sprintf("%s [%s]", reason, seat.note)
	}
	if refused {
		return agent, reason, &poolBackpressureError{Reason: reason}
	}
	return agent, reason, nil
}
