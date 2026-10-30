package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/deadseats"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// The town's polecat-seat picture: which seats a spawn could take. The spec
// dispatcher's roster and the pool's own admission read the same helpers, so
// a seat one process calls taken is taken in the other. It lived in
// daemon_dispatch.go until the idle-seat dispatch check was deleted
// (gt-rwp7z.3); the picture itself outlives the patrol.

// sandboxDeadSamples is the patrol's configured dead-sample threshold: the
// number of consecutive dead samples its intent record has to reach before the
// patrol acts on it. The roster's dead-seat read confirms at the same count, so
// a seat the patrol is still waiting on stays live here (gt-4rxdi). An
// unreadable block reads as the compiled default, which is the count the patrol
// itself falls back to.
func sandboxDeadSamples(townRoot string) int {
	cfg, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return deadseats.DefaultSamples
	}
	return daemon.PatrolScanDeadSamples(cfg)
}

// poolSeatSessions lists the seats the pool counts as taken: the live polecat
// sessions, the seats whose session is gone while their bead is still hooked
// (gt-tldj4), the in-flight seat claims a sling writes before its session
// exists (gt-t8q5), and the seats mid-landing (gt-thy6r).
//
// All of them belong in the count. The pool's own admission path counts sessions
// and claims (choosePoolAgent), so a picture taken from live sessions alone can
// name a seat free that the very next sling refuses (gt-59o9). A crashed seat
// waiting for its supervised restart is the same lie one step earlier: its
// session is gone but the polecat is coming back within minutes, so dropping
// the seat lets the restart itself push the town past its cap (gt-tldj4). A
// seat mid-landing is the lie one stage later: the polecat that ran `gt done`
// has no session and has dropped its claim, so an accounting that read live
// sessions alone would sling a second bead into a pool class that was still
// occupied (gt-2z8k1). The seat is not free until the landing worker records
// the result.
//
// A seat can appear in more than one source at once — the seconds between
// `gt done` writing its Submitted record and the session exiting leave it live
// and mid-landing together (gt-xmcyk). Each source skips the seats an earlier
// one already named, so a seat is one seat however many of them describe it.
func poolSeatSessions(t sessionLister, townRoot string, pool *config.PolecatPool) ([]poolSession, error) {
	return poolSeatSessionsWith(t, townRoot, poolDispositionFor(townRoot), poolSeatWorkFor(townRoot), pool)
}

// poolSeatSessionsWith is poolSeatSessions with the town's reads explicit: the
// per-seat polecat-state read (listPolecatSessions' disposition) and the
// batched work-bead read the landing seats are confirmed with. A caller with
// no town to read (a test) passes nil for both.
func poolSeatSessionsWith(t sessionLister, townRoot string, disposition polecatDispositionFunc, work poolSeatWorkFunc, pool *config.PolecatPool) ([]poolSession, error) {
	sessions, err := poolOccupiedSessions(t, townRoot, disposition, work, pool, time.Now, sandboxSeatDeathsFor(townRoot))
	if err != nil {
		return nil, err
	}
	claims, err := poolSeatClaimSessions(townRoot, "")
	if err != nil {
		// A claim set that cannot be read is not an empty claim set: reading it
		// as one is what makes the picture lie (gt-t8q5).
		return nil, err
	}
	return append(sessions, claims...), nil
}

// poolOccupiedSessions lists the seats the pool already holds: the live polecat
// sessions, the seats whose session is gone while their bead is still hooked
// (gt-tldj4), and the seats mid-landing (gt-thy6r). It is the one occupancy
// source the pool's own admission decision (poolRouter.route) and the spec
// dispatcher's roster both read, so a seat one counts as taken is taken in the
// other — admission that counted live sessions alone admits past the cap while
// a seat is mid-landing (gt-3o7zk), and a crashed polecat waiting for its
// supervised restart would let a restart push the town over the cap (gt-tldj4).
//
// The seat claims are deliberately not folded in here: route decides from the
// claims it reads under the seat-decision lock (poolSeatLedger.begin), and
// poolSeatSessionsWith appends the reader's own view of them
// (poolSeatClaimSessions). Counting a claim here as well would fill two seats
// with one.
//
// A failure carries which read failed (poolOccupancyError), so a caller can
// keep its own words for a lister it cannot talk to without parsing the error.
//
// deadSandbox is the liveness read a live session is dropped by (gt-tj446): a
// sandbox seat the patrol has recorded as dead is not a working seat, and the
// dead read below then counts it as held while its bead is hooked.
func poolOccupiedSessions(t sessionLister, townRoot string, disposition polecatDispositionFunc, work poolSeatWorkFunc, pool *config.PolecatPool, now func() time.Time, deadSandbox sandboxSeatDeadFunc) ([]poolSession, error) {
	live, err := listPolecatSessionsWith(t, disposition, now())
	if err != nil {
		return nil, &poolOccupancyError{sessions: true, err: err}
	}
	live = withoutDeadSandboxSeats(live, deadSandbox)
	dead, err := poolDeadHookedSessions(townRoot, pool, disposition, live, now())
	if err != nil {
		return nil, &poolOccupancyError{err: err}
	}
	landing, err := poolLandingSessions(townRoot, pool, work, live)
	if err != nil {
		return nil, &poolOccupancyError{err: err}
	}
	return append(append(live, dead...), landing...), nil
}

// liveSeatKeys names the seats a live session already accounts for, keyed
// "rig/polecat". The seat sources that enumerate seats another way (the dead
// and the landing reads) skip these, so a seat stays one seat. A session that
// names no seat — a witness, refinery or crew session, or a claim, which
// carries no GT_ROLE — contributes nothing: it holds no pool seat.
func liveSeatKeys(live []poolSession) map[string]bool {
	seats := make(map[string]bool, len(live))
	for _, s := range live {
		if s.rig != "" && s.polecat != "" {
			seats[s.rig+"/"+s.polecat] = true
		}
	}
	return seats
}

// poolSeatBatch answers the seat reads of one tick from one batched read per
// rig: the rig's assigned work, which also answers the agent beads it carries
// (gt-u8rbh, gt-4efmk). The per-seat read spends two bd calls on every polecat
// directory, idle ones included. A polecat the batch did not answer, or a rig
// whose batch failed, is read per seat through the rig's own store, so a
// failure costs calls and never changes a seat's count. It holds what it read:
// build one per tick.
//
// A bead is answered from the batch only in a form the seat read can use whole:
// an agent bead is complete (listedAgentBeadComplete) or read live by id, and an
// id two rigs' lists carry is answered in a fixed order (heldIssue).
type poolSeatBatch struct {
	townRoot string
	store    func(rigName string) beads.Client
	show     func(bd beads.Client, id string) (*beads.Issue, error)
	rigs     map[string]*rigSeatBatch
}

// rigSeatBatch is one rig's reads. store is the rig's client, which the
// per-seat fallback reads too. assigned and held come from the rig's work list,
// and are nil when that read failed; agents is nil when no agent bead was
// answered. A rig whose work list failed falls back to the per-seat reads: its
// agent beads cost one batch and its assignments one live read each. held holds
// the work beads by id — those the list returned, plus the hook and source-issue
// beads a later batch added — and an id it does not hold is read live, as the
// per-seat read does.
type rigSeatBatch struct {
	store    beads.Client
	asked    map[string]bool         // agent bead ids the batch answered (found or not)
	agents   map[string]*beads.Issue // the agent beads that exist among them
	parsed   map[string]parsedAgent  // each agent bead's parse, once, by id
	assigned map[string]*beads.Issue // assignee -> the issue GetAssignedIssue picks
	held     map[string]*beads.Issue // work, hook and source-issue beads, by id
}

// parsedAgent is one batched agent bead's parse: what a live read of the bead
// would have returned. The batch parses each bead once and answers both the
// references it follows (readSourceIssues) and the seat read itself (agentBead)
// from that parse.
type parsedAgent struct {
	fields *beads.AgentFields
	err    error
}

// assignedStatuses are the statuses GetAssignedIssue considers, most preferred
// first. The batch asks bd for all three in one comma-separated list.
var assignedStatuses = []string{string(beads.StatusOpen), string(beads.StatusInProgress), beads.StatusHooked}

// newPoolSeatBatch builds the batch over the stores store returns, and show,
// the by-id read the hook reference follows.
func newPoolSeatBatch(townRoot string, store func(rigName string) beads.Client, show func(bd beads.Client, id string) (*beads.Issue, error)) *poolSeatBatch {
	return &poolSeatBatch{townRoot: townRoot, store: store, show: show, rigs: map[string]*rigSeatBatch{}}
}

// disposition is the polecatDispositionFunc the batch answers.
func (b *poolSeatBatch) disposition(rigName, polecatName string) (poolSeatState, error) {
	rb := b.rig(rigName)
	reads := poolSeatReads{
		agentBead: func(_ beads.Client, id string) (*beads.Issue, *beads.AgentFields, error) {
			return beads.GetAgentBead(rb.store, id)
		},
		assigned: func(_ assignedIssueLookup, rig, name string) polecatActiveWorkEvidence {
			return poolAssignedWorkEvidence(rb.store, rig, name)
		},
		show: func(_ beads.Client, id string) (*beads.Issue, error) { return b.show(rb.store, id) },
	}
	if rb.agents != nil {
		reads.agentBead = rb.agentBead(reads.agentBead)
	}
	if rb.assigned != nil {
		reads.assigned = func(_ assignedIssueLookup, rig, name string) polecatActiveWorkEvidence {
			return assessPolecatAssignedIssueWork(rb.assigned[rig+"/polecats/"+name])
		}
	}
	reads.show = func(_ beads.Client, id string) (*beads.Issue, error) {
		if issue, ok := rb.held[id]; ok {
			return issue, nil
		}
		return b.show(rb.store, id)
	}
	return poolPolecatSeatStateWith(b.townRoot, rigName, polecatName, reads)
}

// landingWork is the poolSeatWorkFunc the landing seats are confirmed with: the
// beads the tick's batch already holds, and one town-level ShowMultiple for the
// rest. A landing bead is live work in its rig's database, so the rig's work
// list carries it and the tick spends no read to confirm it (gt-4efmk); a bead
// of another database — one slung from the town, carrying the hq- prefix — is
// not in any rig's list and is read by town, which routes it, as before.
func (b *poolSeatBatch) landingWork(town beads.Client) poolSeatWorkFunc {
	return func(ids []string) (map[string]*beads.Issue, error) {
		out := make(map[string]*beads.Issue, len(ids))
		var rest []string
		for _, id := range ids {
			if issue, ok := b.heldIssue(id); ok {
				out[id] = issue
				continue
			}
			rest = append(rest, id)
		}
		if len(rest) == 0 {
			return out, nil
		}
		more, err := town.ShowMultiple(rest)
		if err != nil {
			// A bead that could not be read is not a bead that landed.
			return nil, err
		}
		for id, issue := range more {
			out[id] = issue
		}
		return out, nil
	}
}

// heldIssue answers a bead id from the batches made so far. One bead is one
// bead whichever rig's list carried it, so the rigs are searched in name order:
// a bead two rigs hold is answered by the same rig on every run, where a walk
// over the map would answer by whichever rig the iteration reached first.
func (b *poolSeatBatch) heldIssue(id string) (*beads.Issue, bool) {
	names := make([]string, 0, len(b.rigs))
	for name := range b.rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if issue, ok := b.rigs[name].held[id]; ok {
			return issue, true
		}
	}
	return nil, false
}

// rig reads a rig's batches the first time one of its polecats is asked about.
func (b *poolSeatBatch) rig(rigName string) *rigSeatBatch {
	if rb, ok := b.rigs[rigName]; ok {
		return rb
	}
	rb := &rigSeatBatch{store: b.store(rigName)}
	b.rigs[rigName] = rb
	names, err := listPolecatDirectoryNames(filepath.Join(b.townRoot, rigName))
	if err != nil || len(names) == 0 {
		return rb
	}
	prefix := beads.GetPrefixForRig(b.townRoot, rigName)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		ids = append(ids, beads.PolecatBeadIDWithPrefix(prefix, rigName, name))
	}
	// The work list is made first, so the agent beads it carries cost no read
	// of their own: an agent bead is a bead like any other, filed open and
	// carrying its fields in the description the list returns.
	work, err := rb.store.List(beads.ListOptions{Status: strings.Join(assignedStatuses, ","), Priority: -1})
	if err == nil {
		rb.assigned = assignedByAssignee(work)
		rb.held = make(map[string]*beads.Issue, len(work))
		for _, issue := range work {
			rb.held[issue.ID] = issue
		}
	}
	rb.readAgents(ids)
	rb.readSourceIssues()
	return rb
}

// readAgents answers the polecats' agent beads from the work list and, for the
// beads that list does not carry — one filed closed, deferred or pinned, none
// of which an assigned-work list asks for, or one whose list record is not
// whole (listedAgentBeadComplete) — one ShowMultiple. An agent bead the batch
// asked for and did not find does not exist; one it could not ask about goes to
// the live per-seat read, so a failed read costs calls and never changes a
// seat's count.
func (rb *rigSeatBatch) readAgents(ids []string) {
	agents := make(map[string]*beads.Issue, len(ids))
	asked := make(map[string]bool, len(ids))
	var missing []string
	for _, id := range ids {
		if issue, ok := rb.held[id]; ok && listedAgentBeadComplete(issue) {
			agents[id] = issue
			asked[id] = true
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) > 0 {
		if found, err := rb.store.ShowMultiple(missing); err == nil {
			for _, id := range missing {
				asked[id] = true
			}
			for id, issue := range found {
				agents[id] = issue
			}
		}
	}
	if len(asked) == 0 {
		return
	}
	rb.asked = asked
	rb.agents = agents
	rb.parsed = make(map[string]parsedAgent, len(agents))
	for id, agent := range agents {
		// The shared agent-bead read, over a store that holds this one issue,
		// so the bead is validated and parsed exactly as a live read would.
		_, fields, err := beads.GetAgentBead(heldIssueStore{Client: rb.store, issue: agent}, id)
		rb.parsed[id] = parsedAgent{fields: fields, err: err}
	}
}

// listedAgentBeadComplete reports whether a work-list record carries every field
// the seat read takes from an agent bead: the gt:agent label (or the legacy
// agent type), the id and status, and the role_type and agent_state lines every
// agent description is written with (beads.FormatAgentDescription). A record
// missing one would read as an agent bead holding nothing — its hook_bead and
// agent_state parsed empty — and its seat as free, so readAgents asks the store
// for that bead by id instead. TestListedAgentBeadCarriesTheShowFields holds a
// recorded bd list and bd show of one bead to this: the list form is whole.
func listedAgentBeadComplete(issue *beads.Issue) bool {
	if issue == nil || !beads.IsAgentBead(issue) {
		return false
	}
	if strings.TrimSpace(issue.ID) == "" || strings.TrimSpace(issue.Status) == "" {
		return false
	}
	fields := beads.ParseAgentFields(issue.Description)
	return strings.TrimSpace(fields.RoleType) != "" && strings.TrimSpace(fields.AgentState) != ""
}

// readSourceIssues reads the beads the polecats' hook references and source-issue
// hints name, so the seat read answers them from the batch. The work list holds
// every live one (open, in progress or hooked), so only the rest, the closed
// ones a polecat last worked, cost a read: one batched read for them, which a
// failure leaves out so each is read live. A rig whose agent beads did not batch
// reads nothing here.
func (rb *rigSeatBatch) readSourceIssues() {
	if rb.agents == nil {
		return
	}
	if rb.held == nil {
		rb.held = map[string]*beads.Issue{}
	}
	var missing []string
	seen := map[string]bool{}
	for id := range rb.agents {
		read := rb.parsed[id]
		if read.err != nil || read.fields == nil {
			continue
		}
		// The same two references the seat read follows, named the same way, so
		// each one it reads is one the batch already holds or asks for.
		for _, ref := range []string{agentSourceIssueHint("", read.fields), strings.TrimSpace(read.fields.HookBead)} {
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			if _, ok := rb.held[ref]; !ok {
				missing = append(missing, ref)
			}
		}
	}
	if len(missing) == 0 {
		return
	}
	if issues, err := rb.store.ShowMultiple(missing); err == nil {
		for id, issue := range issues {
			rb.held[id] = issue
		}
	}
}

// assignedByAssignee keys each assignee to the issue GetAssignedIssue picks for
// it: the first of the most-preferred status.
func assignedByAssignee(work []*beads.Issue) map[string]*beads.Issue {
	out := map[string]*beads.Issue{}
	for _, status := range assignedStatuses {
		for _, issue := range work {
			if issue.Status != status || issue.Assignee == "" {
				continue
			}
			if _, taken := out[issue.Assignee]; !taken {
				out[issue.Assignee] = issue
			}
		}
	}
	return out
}

// agentBead is the agent-bead read answered from the batch. An agent bead the
// batch asked for and did not find does not exist, which beads.GetAgentBead
// reports as no bead and no error; one it did not ask for goes to live.
func (rb *rigSeatBatch) agentBead(live func(bd beads.Client, agentID string) (*beads.Issue, *beads.AgentFields, error)) func(bd beads.Client, agentID string) (*beads.Issue, *beads.AgentFields, error) {
	return func(bd beads.Client, agentID string) (*beads.Issue, *beads.AgentFields, error) {
		if !rb.asked[agentID] {
			return live(bd, agentID)
		}
		issue, ok := rb.agents[agentID]
		if !ok {
			return nil, nil, nil
		}
		// The parse the batch already made, which is what a live read of the
		// bead returns: validated and parsed the same way, once.
		read := rb.parsed[agentID]
		if read.err != nil {
			return nil, nil, read.err
		}
		return issue, read.fields, nil
	}
}

// heldIssueStore answers Show with the batched issue, so beads.GetAgentBead
// validates and parses it the way a live read does. beads has no exported
// parser for an issue already in hand.
type heldIssueStore struct {
	beads.Client
	issue *beads.Issue
}

func (s heldIssueStore) Show(string) (*beads.Issue, error) { return s.issue, nil }

// poolOwnsSeats reports whether the pool names a seat for a sessionless polecat
// to be counted on: a seat map.
func poolOwnsSeats(pool *config.PolecatPool) bool {
	return pool != nil && len(pool.Seats) > 0
}

// polecatIntentSeat is the intent-record key of a polecat seat, built through
// the same constructor the writers use (supervisor.SeatFor), so the reader of a
// seat's record names it the way the writer does.
func polecatIntentSeat(rigName, polecatName string) intent.Seat {
	return supervisor.IntentSeat(supervisor.SeatFor(rigName, constants.RolePolecat, polecatName))
}

// slungAgent is the agent a sessionless seat is counted on: the one its sling
// recorded (intent.MarkSlung), or fallback when the record names none.
//
// fallback is unrecordedSeatAgent. A seat slung before the record existed, or
// one a sling left on the role default (which the record stores as no agent at
// all), is counted on it (gt-2q283).
func slungAgent(rec intent.Record, fallback string) string {
	if agent := strings.TrimSpace(rec.Agent); agent != "" {
		return agent
	}
	return fallback
}

// polecatStarter is the one SessionManager verb restartPolecatOnSlungAgent
// needs, so the start paths can be tested without tmux.
type polecatStarter interface {
	Start(name string, opts polecat.SessionStartOptions) error
}

// seatBackendResolver resolves the session backend for the agent a seat is
// restarted on. Tests inject one, so the restart needs neither town settings
// nor a tmux client.
type seatBackendResolver func(agent string) (session.Backend, error)

// townSeatBackend resolves a seat's backend from the town settings the way the
// spawn path does (sessionBackendForAgent): the agent's seat in
// polecat_pool.seats picks sandbox or host, and an agent that is no seat gets
// the host's tmux. An unreadable settings file, or an openshell backend that
// cannot be built, is an error: the restart refuses rather than start a
// sandboxed seat on the host unfenced (gt-da3ea). A missing settings file reads
// as the defaults, and nothing is written.
func townSeatBackend(townRoot string, host *tmux.Tmux) seatBackendResolver {
	return func(agent string) (session.Backend, error) {
		ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
		if err != nil {
			return nil, fmt.Errorf("loading town settings to pick the session backend for %q: %w", agent, err)
		}
		return sessionBackendForAgent(ts, townRoot, agent, host)
	}
}

// restartPolecatOnSlungAgent starts a polecat session on the agent the seat's
// intent record says it was slung on (intent.MarkSlung), so a seat slung on
// deepseek-flash does not come back as the polecat role default. A seat with
// no record, or a record naming no agent, starts with opts as given: the role
// default, as before. opts.Agent, when set, wins over the record.
//
// The backend follows that agent too, so a sandboxed seat never starts on host
// tmux (gt-da3ea). backendFor answers the error that refuses such a restart.
func restartPolecatOnSlungAgent(mgr polecatStarter, backendFor seatBackendResolver, townRoot, rigName, polecatName string, opts polecat.SessionStartOptions) error {
	if opts.Agent == "" && townRoot != "" {
		// An unreadable record reads as one that names no agent (Read's
		// fail-closed record carries none), so the error is not acted on.
		rec, _ := intent.Read(townRoot, polecatIntentSeat(rigName, polecatName))
		opts.Agent = slungAgent(rec, "")
	}
	// A restart with no town root has no settings to read, so no seat map
	// names a sandboxed seat: the session manager's tmux default stands.
	// With a town root every seat reads the settings, since only they say
	// whether the agent is sandboxed (townSeatBackend).
	if opts.Backend == nil && townRoot != "" {
		backend, err := backendFor(opts.Agent)
		if err != nil {
			return fmt.Errorf("resolving the session backend for %s/%s: %w", rigName, polecatName, err)
		}
		opts.Backend = backend
	}
	return mgr.Start(polecatName, opts)
}

// unrecordedSeatAgent returns, per rig, the agent a sessionless seat with no
// recorded agent is counted on. Such a seat is dead-hooked and waiting for its
// supervised restart, or mid-landing, and the restart starts it on the polecat
// role default (restartPolecatOnSlungAgent), so that is the seat it holds.
//
// A map of several seats resolves the role default the way the restart does,
// rig then town (config.ResolveRoleAgentName). PolecatPool.UnrecordedAgent
// answers config.UnknownSeatAgent there, which fills no seat and lets the
// dispatcher sling a second polecat onto the seat the restart brings back. A
// pool with no seat map, or one seat, keeps UnrecordedAgent.
//
// The role default read returns no error: config.ResolveRoleAgentName answers
// a name for unreadable settings too, as the restart resolves them, and the
// result it discards is isRoleSpecific, not an error.
//
// A role default that is no seat in the map is not a seat the restart fills
// either, so the seat is counted under config.UnknownSeatAgent as in
// PolecatPool.UnrecordedAgent: it counts toward max_total, fills no seat, and
// the tick lists it.
func unrecordedSeatAgent(townRoot string, pool *config.PolecatPool) func(rigName string) string {
	if pool == nil || len(pool.Seats) < 2 {
		agent := pool.UnrecordedAgent()
		return func(string) string { return agent }
	}
	byRig := map[string]string{}
	return func(rigName string) string {
		agent, ok := byRig[rigName]
		if !ok {
			agent, _ = config.ResolveRoleAgentName(constants.RolePolecat, townRoot, filepath.Join(townRoot, rigName))
			if _, isSeat := pool.Seats[agent]; !isSeat {
				agent = config.UnknownSeatAgent
			}
			byRig[rigName] = agent
		}
		return agent
	}
}

// poolDeadHookedSessions lists the seats whose tmux session is gone but whose
// agent bead still names hooked, non-terminal work: a polecat that crashed and
// whose supervised restart is due (gt-tldj4). Such a seat keeps holding the
// pool's seat while it waits — the supervisor brings the polecat back within
// minutes, so dropping the seat in the gap is what let a restart push the town
// past its cap (the 4/3 on 2026-10-04: sapphire's session died at 18:06 with
// gt-rwp7z.10 still hooked, the roster read 2/3 at 18:07 and slung a fourth
// polecat, and the restart at 18:08 made that four).
//
// Seats are enumerated the way `gt polecat list` and the capacity snapshot
// enumerate them — the polecat directories under each rig (listPolecatDirectoryNames)
// — less the seats a live session already accounts for. The decision is the
// shared seat-state read (polecatDispositionFunc), so a seat whose bead is
// terminal, or whose work names nothing live — the bead assigned to it and the
// hook reference its agent bead carries, read the way the list path reads both
// (gt-bc9b6) — is not counted here any more than it is by a live session
// (gt-b9ud0).
//
// A seat whose state cannot be read is skipped: it is not proven held, the live
// seats are already counted, and counting every unreadable directory would let
// a bead-database hiccup fill every seat and refuse every sling.
//
// The seat is rendered on the agent it was slung on, read from the seat's
// intent record (intent.MarkSlung) — a dead seat's tmux session is what carried
// its GT_AGENT, and the record is the copy that outlives it. A seat whose
// record names no agent keeps the seat its restart lands on
// (unrecordedSeatAgent): the pool's own seat, which is what every dead seat was
// counted on before the record existed, and the polecat role default in a map
// of several seats. A seat slung before then, or one a sling left on the role
// default, reads as it did (gt-2q283).
//
// The agent read is best-effort where the seat-state read above is not: it
// decides only which seat the polecat is counted against, never whether it
// counts, so a record that cannot be read falls back rather than failing the
// picture. The fallback is a real seat, which can only over-count it — a
// refused sling rather than an admitted one.
//
// A seat parked longer than agentpause.ParkedSeatRelease stops holding its seat:
// it comes back as a parkedStale session instead of a dead-hooked one, which the
// roster lists apart from Live and poolHeldCount does not count (gt-t7k92).
func poolDeadHookedSessions(townRoot string, pool *config.PolecatPool, disposition polecatDispositionFunc, live []poolSession, now time.Time) ([]poolSession, error) {
	if townRoot == "" || !poolOwnsSeats(pool) || disposition == nil {
		return nil, nil
	}
	fallback := unrecordedSeatAgent(townRoot, pool)
	liveSeats := liveSeatKeys(live)
	rigs, err := knownRigNames(townRoot)
	if err != nil {
		return nil, err
	}
	var out []poolSession
	for _, rigName := range rigs {
		names, err := listPolecatDirectoryNames(filepath.Join(townRoot, rigName))
		if err != nil {
			return nil, fmt.Errorf("listing polecats in %s: %w", rigName, err)
		}
		for _, name := range names {
			if liveSeats[rigName+"/"+name] {
				continue
			}
			state, err := disposition(rigName, name)
			if err != nil || !state.HookHeld || !state.Disposition.CountsTowardCapacity {
				continue
			}
			// The seat's own record, read after the seat is known to hold
			// one: only an occupant's agent is worth a read.
			rec, _ := intent.Read(townRoot, polecatIntentSeat(rigName, name))
			stale := parkedStale(townRoot, rigName, name, now)
			out = append(out, poolSession{
				name:        "dead/" + rigName + "/" + name,
				agent:       slungAgent(rec, fallback(rigName)),
				rig:         rigName,
				polecat:     name,
				deadHooked:  !stale,
				parkedStale: stale,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// parkedStale reports whether the seat was parked more than
// agentpause.ParkedSeatRelease before now. A seat that is not parked, or whose
// park time cannot be read or is unset, is not stale: it keeps counting, as it
// did before the release. The daemon's release line asks the same
// agentpause.ParkedStale, so the two never disagree.
func parkedStale(townRoot, rig, name string, now time.Time) bool {
	_, st, err := agentpause.IsPaused(townRoot, rig, constants.RolePolecat, name)
	return err == nil && agentpause.ParkedStale(st, now)
}

// poolOccupancyError labels a poolOccupiedSessions failure by the read that
// produced it: sessions distinguishes a session list the pool could not read
// from landing seats it could not confirm. Error and Unwrap delegate to the
// underlying error, so a caller that does not care about the source sees the
// failure it would have seen directly.
type poolOccupancyError struct {
	sessions bool
	err      error
}

func (e *poolOccupancyError) Error() string { return e.err.Error() }
func (e *poolOccupancyError) Unwrap() error { return e.err }

// poolSeatWorkFunc reads work beads by ID in one call, keyed by ID. Missing
// IDs are left out (beads.Client.ShowMultiple). It is the batched read the
// landing seats are confirmed with; nil is a caller with no store to read.
type poolSeatWorkFunc func(ids []string) (map[string]*beads.Issue, error)

// poolSeatWorkFor is the batched work-bead read a caller in townRoot wants, or
// nil when there is no town to read. The town's client routes each ID to its
// own database, so a landing seat's bead is read wherever it lives (a bead
// slung from the town carries the hq- prefix).
func poolSeatWorkFor(townRoot string) poolSeatWorkFunc {
	if townRoot == "" {
		return nil
	}
	return func(ids []string) (map[string]*beads.Issue, error) {
		return beads.New(townRoot).ShowMultiple(ids)
	}
}

// poolLandingSessions lists the seats mid-landing as occupants of the pool's
// seat: the polecats whose work bead still carries gt:ready-to-land and whose
// session is gone because `gt done` ended it.
//
// The seat's intent record is the cheap half of the signal, and the reason
// this costs no store call in the common case: it is a small JSON file per
// seat under .runtime/agents, written by `gt done` (desired=submitted) and
// cleared by the landing worker when it records the result, or by the next
// sling onto the seat. The record alone is not trusted — it can outlive a
// submission that was pulled back for rework, and the patrol scan corrects
// that a tick later (gt-xs1ni) — so the seats it names are confirmed against
// the bead in ONE batched read, and a seat whose bead no longer reads as
// submitted is a free seat.
//
// A record whose polecat directory is gone is left out, the way townhealth
// reads the same directory: the seat is gone, and its record outlived it.
//
// A record for a seat live names is left out too (gt-xmcyk): `gt done` writes
// the record before its session exits, so for those seconds one seat is live
// and mid-landing at once, and counting both read 4/3 on a full pool and
// refused the tick a seat (2026-10-04 19:17:35). Live already holds the seat;
// the record adds nothing while it does.
//
// pool is the seat the town's polecats run, and the record's own agent field is
// the agent the seat was slung on — which is the seat a landing polecat is
// counted against, whatever the pool runs (gt-2q283). A record that names no
// agent, from a seat slung before that field existed, keeps the seat its
// restart lands on (unrecordedSeatAgent), the pool's own seat as it read then.
// A nil pool (or one with no agent) owns no seat, so nothing
// is rendered and no bead is read.
func poolLandingSessions(townRoot string, pool *config.PolecatPool, work poolSeatWorkFunc, live []poolSession) ([]poolSession, error) {
	if townRoot == "" || !poolOwnsSeats(pool) || work == nil {
		return nil, nil
	}
	fallback := unrecordedSeatAgent(townRoot, pool)
	liveSeats := liveSeatKeys(live)

	type landing struct {
		rig, name, bead, agent string
	}
	var candidates []landing
	dir := filepath.Join(constants.TownRuntimePath(townRoot), "agents")
	err := filepath.WalkDir(dir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && path == dir {
				// No seat has ever recorded an intent: nothing is mid-landing.
				return filepath.SkipDir
			}
			return err
		}
		if e.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rig, name, ok := polecatSeatRecord(dir, path)
		if !ok {
			return nil
		}
		rec, rerr := intent.ReadPath(path)
		if rerr != nil {
			// A record that cannot be read is not a seat that is free: the
			// picture fails rather than undersell the occupied seats, as the
			// claim set does (gt-t8q5).
			return fmt.Errorf("reading intent record for %s/%s: %w", rig, name, rerr)
		}
		if !rec.Submitted() || rec.WorkBead == "" {
			// A submission with no bead names nothing to confirm: it holds no
			// seat.
			return nil
		}
		if _, serr := os.Stat(filepath.Join(townRoot, rig, "polecats", name)); serr != nil {
			return nil // the seat is gone; the record outlived it
		}
		if liveSeats[rig+"/"+name] {
			return nil // a live session already holds the seat
		}
		candidates = append(candidates, landing{
			rig:   rig,
			name:  name,
			bead:  rec.WorkBead,
			agent: slungAgent(rec, fallback(rig)),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("reading landing seats from %s: %w", dir, err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if _, ok := seen[c.bead]; ok {
			continue
		}
		seen[c.bead] = struct{}{}
		ids = append(ids, c.bead)
	}
	sort.Strings(ids)
	issues, err := work(ids)
	if err != nil {
		// A bead that could not be read is not a bead that landed.
		return nil, fmt.Errorf("reading landing seats' work beads: %w", err)
	}

	out := make([]poolSession, 0, len(candidates))
	for _, c := range candidates {
		if !polecat.IsSubmittedWork(issues[c.bead]) {
			continue // the bead no longer waits to land: the seat is free
		}
		// created is left zero on purpose: a landing seat is not a spawn, and
		// the spec dispatcher's stagger (min_spawn_gap) must not arm from a
		// submission.
		out = append(out, poolSession{name: "landing/" + c.rig + "/" + c.name, agent: c.agent, rig: c.rig, polecat: c.name, landing: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// polecatSeatRecord splits an intent record's path under dir
// ("<rig>/polecat.<name>.json") into the rig and polecat it names, reporting
// ok=false for a record that is not a polecat's — a witness, refinery or
// crew seat holds no pool seat.
func polecatSeatRecord(dir, path string) (rigName, polecatName string, ok bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", "", false
	}
	rigName = filepath.Dir(rel)
	stem := strings.TrimSuffix(filepath.Base(rel), ".json")
	role, name, named := strings.Cut(stem, ".")
	if rigName == "." || !named || role != constants.RolePolecat || name == "" || strings.Contains(name, ".") {
		return "", "", false
	}
	return rigName, name, true
}

// knownRigNames reads the rig registry.
func knownRigNames(townRoot string) ([]string, error) {
	rigsConfig, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return nil, fmt.Errorf("loading the rig registry: %w", err)
	}
	names := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}
