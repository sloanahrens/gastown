package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

const polecatSessionKeySep = "\x00"

// polecatSessionSet indexes live polecat sessions by rig and polecat name.
// Agent and Created are only populated when the set was built with a session
// lister (see newPolecatSessionSetFromNames).
type polecatSessionSet map[string]polecatSessionEntry

type polecatSessionEntry struct {
	Name    string
	Agent   string
	Created time.Time
}

type polecatInventoryItem struct {
	Rig   string
	Name  string
	Agent string
	// Spawning is the spawn grace verdict (see polecatSpawnFacts): work is
	// assigned, no session is live yet, and the agent bead is still inside its
	// startup window — so this polecat is starting up, not stalled.
	Spawning bool
	State    polecat.State
	Issue    string
	// MRID/MRStatus are the polecat's merge request as resolved by one bulk
	// rig-wide join (see polecatMRIndex). MRID is empty when the rig has no MR
	// for this polecat at all — MRStatus is then empty too, not "missing".
	MRID           string
	MRStatus       string
	CleanupStatus  string
	ActiveMR       string
	Branch         string
	SessionRunning bool
	SessionName    string
	Disposition    polecat.WorkstateDisposition
	// CleanupStatusSource/GitStateSource/GitStateReason are the provenance of
	// the two cleanup inputs above, copied from Disposition: cleanup_status is
	// always a recorded hint, while the git facts are live whenever a probe ran
	// (see polecat.CleanupStatusSourceRecorded / polecat.GitStateSource*).
	CleanupStatusSource string
	GitStateSource      string
	GitStateReason      string
}

// polecatInventoryEnv carries the per-rig facts the list path has and the
// capacity path does not: one rig-wide merge-request index, and the agent-bead
// facts the spawn grace is measured against. The zero value means "no MR
// source, no grace" and reproduces the pre-gt-2540 states.
type polecatInventoryEnv struct {
	MRs   polecatMRIndex
	Spawn polecatSpawnFacts
	// WorktreePath is the polecat's git worktree, probed live so the reuse
	// verdict re-derives from measured git state instead of the recorded
	// cleanup_status hint (claude-41j.1 D9). Empty means "no live probe" — the
	// recorded hint stays authoritative, which is what the counts-only capacity
	// projection (applyAgentFieldsToCapacitySnapshot) wants: it reports counts,
	// not reuse decisions, so it must not pay a git subprocess per polecat on
	// the admission path. Falling back to the recorded hint there errs *toward*
	// recovery-blocked, i.e. it under-reports free slots rather than
	// over-admitting; the actual reuse gate (Manager.ReuseDecisionForPolecat)
	// does probe.
	WorktreePath string
	// GitProbeLocalOnly drops the network from the worktree probe: branch
	// preservation is read from the rig's local remote-tracking refs instead of
	// an ls-remote against the remote. The list path sets it, because it pays
	// this probe once per seat and the dashboard polls it — an ls-remote per
	// seat is what pushed `gt polecat list --all` past 90s (gt-8q0s). Nothing
	// else changes, and the local answer is the live one's or more
	// conservative, so a caller that leaves this off is choosing accuracy over
	// cost, not a different verdict vocabulary.
	GitProbeLocalOnly bool
	// ActiveMRSource resolves the ids the active_mr policy looks up (the MR
	// itself and its source issue). Nil — the zero value, and what the capacity
	// path passes — leaves the policy reading the same fail-closed
	// "status=unverified" it read before there was a source at all, so
	// admission still pays no beads call per polecat.
	ActiveMRSource polecat.IssueReader
}

// polecatActiveMRReader resolves the active_mr policy's two lookups from the
// two places that actually hold them: the rig-wide merge-request index for MR
// ids, the rig's beads database for everything else (a source issue).
//
// MRs are ephemeral wisps — a per-polecat `bd show` finds them unreliably,
// which is why the index exists — while a source issue is an ordinary bead only
// the database has. An index that never loaded answers nothing rather than
// "gone": "the queue does not have it" is a claim only a queue that was read
// can make, and the policy fails closed on an error.
type polecatActiveMRReader struct {
	index polecatMRIndex
	bd    polecat.IssueReader
}

func (r polecatActiveMRReader) Show(issueID string) (*beads.Issue, error) {
	if mr := r.index.byID[issueID]; mr != nil {
		return mr, nil
	}
	if !r.index.loaded {
		return nil, errPolecatMRIndexUnreadable
	}
	if r.bd != nil {
		return r.bd.Show(issueID)
	}
	return nil, beads.ErrNotFound
}

// errPolecatMRIndexUnreadable reports a merge-request index that was never
// read. It is deliberately not beads.ErrNotFound: the active_mr policy treats
// "not found" as "the MR wisp is gone", and a queue nobody managed to read
// cannot say that.
var errPolecatMRIndexUnreadable = errors.New("rig merge-request index was not read")

// landedProbe supplies the active_mr policy's git evidence for a gone-or-closed
// MR: the polecat's work already contained in the integration branch on origin.
// A run with no worktree to measure (the counts-only capacity projection)
// returns nil, which the policy reads as "no evidence" without spawning git.
func (e polecatInventoryEnv) landedProbe(branch string) polecat.LandedEvidenceProbe {
	if strings.TrimSpace(e.WorktreePath) == "" {
		return nil
	}
	worktree := e.WorktreePath
	return func() polecat.LandedEvidence { return polecat.ProbeWorkLandedOnRef(worktree, branch, "origin") }
}

// polecatSpawnFacts dates the agent bead's most recent write. SpawnGrace
// compares it against the window the town's witness thresholds give a starting
// session (HeartbeatStartupGrace, default 5m).
type polecatSpawnFacts struct {
	UpdatedAt time.Time
	Grace     time.Duration
	Now       time.Time
}

// spawning reports whether this polecat's agent bead still reads as starting up.
func (f polecatSpawnFacts) spawning(agentState string) bool {
	return polecat.SpawnGrace(agentState, f.UpdatedAt, f.Now, f.Grace)
}

type polecatActiveWorkEvidence struct {
	BlocksCleanup        bool
	RequiresRestart      bool
	CountsTowardCapacity bool
	Blocker              string
	AssignedIssue        string
}

func newPolecatSessionSet(sessionNames []string) polecatSessionSet {
	return newPolecatSessionSetFromNames(nil, sessionNames)
}

// newPolecatSessionSetFromNames indexes sessions by rig/polecat and, when a
// lister is supplied, records the agent each session was spawned with.
// GT_AGENT is written into the session environment at spawn
// (SessionStartOptions.Agent / AgentEnv fallback) — the same read sling_pool
// already pays for. Callers that only need liveness (capacity) pass a nil
// lister and skip the per-session tmux reads.
func newPolecatSessionSetFromNames(t sessionLister, sessionNames []string) polecatSessionSet {
	sessions := make(polecatSessionSet, len(sessionNames))
	for _, sessionName := range sessionNames {
		rigName, polecatName, ok := parsePolecatSessionName(sessionName)
		if !ok {
			continue
		}
		entry := polecatSessionEntry{Name: sessionName}
		if t != nil {
			if agent, err := t.GetEnvironment(sessionName, "GT_AGENT"); err == nil {
				entry.Agent = strings.TrimSpace(agent)
			}
			if created, err := t.GetSessionCreatedTime(sessionName); err == nil {
				entry.Created = created
			}
		}
		sessions[polecatSessionKey(rigName, polecatName)] = entry
	}
	return sessions
}

// loadPolecatSessionSet lists live sessions once and enriches every polecat
// session with its agent and creation time.
func loadPolecatSessionSet(t sessionLister) (polecatSessionSet, error) {
	names, err := t.ListSessions()
	if err != nil {
		return nil, err
	}
	return newPolecatSessionSetFromNames(t, names), nil
}

func (s polecatSessionSet) lookup(rigName, polecatName string) (string, bool) {
	entry, ok := s.session(rigName, polecatName)
	return entry.Name, ok
}

func (s polecatSessionSet) session(rigName, polecatName string) (polecatSessionEntry, bool) {
	if s == nil {
		return polecatSessionEntry{}, false
	}
	entry, ok := s[polecatSessionKey(rigName, polecatName)]
	return entry, ok
}

func (s polecatSessionSet) namesForRig(rigName string) []string {
	if len(s) == 0 {
		return nil
	}
	var names []string
	for _, entry := range s {
		sessionRig, _, ok := parsePolecatSessionName(entry.Name)
		if ok && sessionRig == rigName {
			names = append(names, entry.Name)
		}
	}
	sort.Strings(names)
	return names
}

func polecatSessionKey(rigName, polecatName string) string {
	return rigName + polecatSessionKeySep + polecatName
}

func buildPolecatInventoryItem(rigName, polecatName string, fields *beads.AgentFields, activeWork *beads.Issue, sessions polecatSessionSet, env polecatInventoryEnv) polecatInventoryItem {
	return buildPolecatInventoryItemFromEvidence(rigName, polecatName, fields, assessPolecatAssignedIssueWork(activeWork), sessions, env)
}

func buildPolecatInventoryItemFromEvidence(rigName, polecatName string, fields *beads.AgentFields, activeWorkEvidence polecatActiveWorkEvidence, sessions polecatSessionSet, env polecatInventoryEnv) polecatInventoryItem {
	entry, running := sessions.session(rigName, polecatName)
	item := polecatInventoryItem{
		Rig:            rigName,
		Name:           polecatName,
		Agent:          entry.Agent,
		State:          polecat.StateIdle,
		SessionRunning: running,
		SessionName:    entry.Name,
	}

	agentState := ""
	if fields != nil {
		agentState = strings.TrimSpace(fields.AgentState)
		item.MRID, item.MRStatus = env.MRs.statusFor(fields.ActiveMR, polecatName)
	}
	spawning := env.Spawn.spawning(agentState)
	item.Spawning = spawning

	facts := polecat.WorkstateFacts{State: polecat.StateIdle, HookBeadSafe: true}
	if fields != nil {
		// gt-ui2x: the bead was actually read here — see
		// ResolveIgnoreCleanupStatus's agentBeadRead/liveGitProbeRan branch.
		// Without this, a missing cleanup_status that the reuse gate
		// (Manager.WorkstateDispositionForPolecat) has cleared would still
		// display as NEEDS_RECOVERY here.
		facts.AgentBeadRead = true
		item.CleanupStatus = strings.TrimSpace(fields.CleanupStatus)
		item.ActiveMR = strings.TrimSpace(fields.ActiveMR)
		item.Branch = strings.TrimSpace(fields.Branch)
		switch beads.AgentState(strings.TrimSpace(fields.AgentState)) {
		case beads.AgentStateDone:
			item.State = polecat.StateDone
		}
		facts.CleanupStatus = polecat.CleanupStatus(item.CleanupStatus)
		facts.PushFailed = fields.PushFailed
		facts.MRFailed = fields.MRFailed
		facts.Branch = item.Branch
		facts.ActiveMR = item.ActiveMR
	}

	// claude-41j.1 D9: measure the worktree before deciding anything that
	// depends on git, so the verdict (and the review-needed rewrite below)
	// re-derives from live state. The recorded cleanup_status remains the
	// input when no probe target is available; the source label records which
	// of the two actually fed the decision.
	if env.WorktreePath != "" {
		live := probePolecatWorktree(env.WorktreePath, env.GitProbeLocalOnly)
		live.ApplyFacts(&facts)
		if live.Branch != "" {
			// A live branch supersedes the recorded one, the same way live git
			// supersedes a recorded dirty status: the bead's branch field is
			// another stale-able self-report.
			item.Branch = live.Branch
		}
	}

	if !activeWorkEvidence.BlocksCleanup && fields != nil {
		activeWorkEvidence = assessPolecatAgentStateWork(beads.AgentState(strings.TrimSpace(fields.AgentState)))
	}

	if activeWorkEvidence.BlocksCleanup {
		item.Issue = activeWorkEvidence.AssignedIssue
		if activeWorkEvidence.RequiresRestart || activeWorkEvidence.CountsTowardCapacity {
			switch {
			case running:
				item.State = polecat.StateWorking
			case spawning:
				// Dispatched seconds ago and not up yet: a session that is
				// still booting, not a stall to restart (gt-yteq).
				item.State = polecat.StateSpawning
			default:
				item.State = polecat.StateStalled
			}
		} else if running && polecat.RecordedCleanupBlocks(polecat.CleanupStatus(item.CleanupStatus), facts.GitStateSource) {
			item.State = polecat.StateReviewNeeded
		}
		facts.ActiveWorkBlocker = activeWorkEvidence.Blocker
		facts.ActiveWorkCountsTowardCapacity = activeWorkEvidence.CountsTowardCapacity
	} else if item.State == polecat.StateIdle && running && polecat.RecordedCleanupBlocks(polecat.CleanupStatus(item.CleanupStatus), facts.GitStateSource) {
		item.State = polecat.StateReviewNeeded
	}

	if fields != nil && !activeWorkEvidence.BlocksCleanup {
		if hookBead := strings.TrimSpace(fields.HookBead); hookBead != "" {
			facts.ActiveWorkBlocker = fmt.Sprintf("hook_bead=%s status=unverified", hookBead)
		}
	}
	if item.ActiveMR != "" {
		// The decision is the shared active_mr policy, so what the list shows
		// agrees with the reuse gate (Manager.ReuseDecisionForPolecat) and with
		// `gt polecat check-recovery`; the index's status label stays in the
		// blocker string because it is the vocabulary `gt mq list` uses.
		//
		// gt-wprt: a pointer whose wisp is gone used to block unconditionally,
		// which read as idle-pr-open forever and refused every spawn once the
		// rig hit its directory cap. The landed probe is the evidence that
		// clears it; capacity passes no source and no worktree, so it keeps
		// failing closed exactly as before.
		mrStatus := item.MRStatus
		if mrStatus == "" {
			mrStatus = "unknown"
		}
		assessment := polecat.AssessActiveMRWithLandedEvidence(env.ActiveMRSource, polecat.ActiveMRInput{
			ActiveMR:        item.ActiveMR,
			SourceIssueHint: agentSourceIssueHint(item.Issue, fields),
		}, env.landedProbe(item.Branch))
		if assessment.Pending {
			facts.ActiveMRBlocker = "active_mr=" + item.ActiveMR + " status=" + mrStatus
		}
	}

	facts.State = item.State
	item.Disposition = polecat.DecideWorkstate(polecat.NewWorkstateInput(facts))
	// The disposition is the one place the verdict's fact provenance is
	// labeled (labelFactSources) — copy it rather than re-deriving it here.
	item.CleanupStatusSource = item.Disposition.CleanupStatusSource
	item.GitStateSource = item.Disposition.GitStateSource
	item.GitStateReason = item.Disposition.GitStateReason
	return item
}

// probePolecatWorktree measures one polecat worktree, picking the probe's
// fidelity from the caller's cost budget. The branch is the only place the two
// probes differ, and both classify a failed probe the same way. The cheap one
// is not a faster copy of the live answer — it reads preservation from this
// clone's tracking refs, so it reports more unpreserved work than the live
// probe when a ref was never fetched and less when a ref outlives its remote
// branch (gt-dt0k); see git.BranchPreservationStatusLocal.
func probePolecatWorktree(worktreePath string, localOnly bool) polecat.LiveGitState {
	if localOnly {
		return polecat.ProbeLiveGitStateLocal(worktreePath)
	}
	return polecat.ProbeLiveGitState(worktreePath)
}

// polecatListInventoryEnv is the per-seat fact set `gt polecat list` carves
// out of its per-rig queries. It is a named function because one field in it
// is a performance contract rather than a verdict input, and that choice is
// worth being able to assert on directly (see TestPolecatListInventoryEnvProbe).
func polecatListInventoryEnv(rigPath, rigName, polecatName string, mrIndex polecatMRIndex, activeMRSource polecat.IssueReader, spawn polecatSpawnFacts) polecatInventoryEnv {
	return polecatInventoryEnv{
		// The dashboard polls this command, so it must not pay a network round
		// trip per seat: the probe reads branch preservation from the rig's
		// local refs instead of running an ls-remote per seat (gt-8q0s).
		GitProbeLocalOnly: true,
		MRs:               mrIndex,
		// The active_mr policy resolves MR ids through the index it already has
		// and source issues through the rig's database; the counts-only
		// capacity path passes neither.
		ActiveMRSource: activeMRSource,
		Spawn:          spawn,
		// claude-41j.1 D9: probe the worktree so the listed verdict re-derives
		// from live git rather than the recorded hint. An unresolvable path (no
		// worktree behind the directory) leaves the probe off, and the verdict
		// stays on the recorded hint.
		WorktreePath: resolvePolecatWorktree(filepath.Join(rigPath, "polecats"), polecatName, rigName),
	}
}

// polecatSeat is one row of the list output, in output order, together with
// the facts needed to build it. The list path collects these serially (the
// per-rig queries are one-per-rig, not one-per-seat) and then resolves them on
// the pool, so a seat carries its inputs rather than being built where it is
// discovered.
type polecatSeat struct {
	rigName       string
	name          string
	fields        *beads.AgentFields
	activeWork    *beads.Issue
	activeWorkErr error
	sessions      polecatSessionSet
	env           polecatInventoryEnv
	// decided holds a row that needs no probing at all — an orphan tmux session,
	// which is classified from session/bead presence alone. Set means resolve
	// returns it untouched.
	decided *PolecatListItem
}

// resolve builds this seat's output row. It is called on the probe pool, so it
// must not touch shared state: it reads its own captured facts and writes
// nothing but its return value.
func (s polecatSeat) resolve() PolecatListItem {
	if s.decided != nil {
		return *s.decided
	}
	return buildPolecatSeatItem(s.rigName, s.name, s.fields, s.activeWork, s.activeWorkErr, s.sessions, s.env)
}

// buildPolecatSeatItem assembles one polecat's list row from its agent-bead
// facts and a live probe of its worktree. activeWorkErr is the rig-wide
// active-work query's error, carried per seat so a rig whose work could not be
// read reports every seat as unverified rather than as idle.
func buildPolecatSeatItem(rigName, name string, fields *beads.AgentFields, activeWork *beads.Issue, activeWorkErr error, sessions polecatSessionSet, env polecatInventoryEnv) PolecatListItem {
	item := buildPolecatInventoryItem(rigName, name, fields, activeWork, sessions, env)
	if activeWorkErr != nil {
		item = buildPolecatInventoryItemFromEvidence(rigName, name, fields, polecatActiveWorkLookupError(activeWorkErr), sessions, env)
	}
	disposition := item.Disposition
	state := effectivePolecatState(PolecatListItem{
		State:                item.State,
		Issue:                item.Issue,
		SessionRunning:       item.SessionRunning,
		CountsTowardCapacity: disposition.CountsTowardCapacity,
	}, item.Spawning)
	return PolecatListItem{
		Rig:                  rigName,
		Name:                 name,
		Agent:                item.Agent,
		State:                state,
		Issue:                item.Issue,
		MRID:                 item.MRID,
		MRStatus:             item.MRStatus,
		CleanupStatus:        item.CleanupStatus,
		ActiveMR:             item.ActiveMR,
		CleanupStatusSource:  item.CleanupStatusSource,
		GitStateSource:       item.GitStateSource,
		GitStateReason:       item.GitStateReason,
		Branch:               item.Branch,
		Verdict:              disposition.Verdict,
		Reason:               disposition.Reason,
		Reusable:             disposition.Reusable,
		SafeToNuke:           disposition.SafeToNuke,
		NeedsRecovery:        disposition.NeedsRecovery,
		NeedsMQSubmit:        disposition.NeedsMQSubmit,
		MQStatus:             disposition.MQStatus,
		CountsTowardCapacity: disposition.CountsTowardCapacity,
		ReuseStatus:          disposition.ReuseStatus,
		Blockers:             disposition.Blockers,
		SessionRunning:       item.SessionRunning,
		SessionName:          item.SessionName,
	}
}

// polecatSeatPoolSize bounds how many units of `gt polecat list` work run
// concurrently. It governs two pools, both of which are waiting on
// subprocesses rather than burning CPU:
//
//   - seats, in resolvePolecatSeats: each seat spawns several git subprocesses
//     against its own worktree (gt-8q0s)
//   - rigs, in buildAllRigSeats: each rig pays a handful of Dolt CLI round
//     trips, so this is also the ceiling on concurrent `bd` processes a
//     `--all` listing starts (gt-92zx)
//
// For the seat pool it is the knob that keeps a large town from forking
// hundreds of git processes in one burst: at 47 seats an unbounded fan-out is
// ~300 concurrent processes. Retuning it for one workload moves the other, so
// check both before changing it.
//
// The multiplier is deliberately modest. Both probes are mostly waiting on
// subprocesses rather than burning CPU, so more goroutines than cores is
// right; but the work is a few tens of milliseconds per unit even locally, so
// the pool only needs to overlap waves, not maximize throughput.
func polecatSeatPoolSize() int {
	size := runtime.GOMAXPROCS(0) * 2
	if size < 4 {
		size = 4
	}
	if size > 12 {
		size = 12
	}
	return size
}

// resolvePolecatSeats builds every seat's row, probing at most
// polecatSeatPoolSize worktrees at once. Results land in slot order, so the
// output is byte-for-byte what a serial loop produced; only the measuring is
// concurrent. A single seat, or a one-core host, falls back to the serial path
// rather than paying for goroutines that cannot overlap.
//
// CONCURRENT-USE PRECONDITION: each seat is built on its own goroutine, so
// everything a seat's build reaches for must tolerate concurrent use. That is
// satisfied today by construction rather than by luck, and it is worth stating
// because the failure would be silent: the worktree probe spawns git, and the
// beads facts come from a shared *beads.Beads whose reads are sync.Once-guarded
// or pure (getTownRoot, getResolvedBeadsDir) with the queries themselves
// running as `bd` subprocesses — beads.New sets no in-process store, so Show
// never enters the SDK storage path. Anything added here that shares mutable
// state (an in-process store, a cached cursor) has to be made safe first.
func resolvePolecatSeats(seats []polecatSeat) []PolecatListItem {
	items := make([]PolecatListItem, len(seats))
	workers := polecatSeatPoolSize()
	if workers > len(seats) {
		workers = len(seats)
	}
	if workers <= 1 {
		for i := range seats {
			items[i] = seats[i].resolve()
		}
		return items
	}

	// A shared cursor rather than a static split: seats are not uniform —
	// probing a worktree that is mid-rebase costs more than probing a clean one
	// — so whoever finishes first should take the next seat.
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(seats) {
					return
				}
				items[i] = seats[i].resolve()
			}
		}()
	}
	wg.Wait()
	return items
}

// MR status values reported on PolecatListItem.MRStatus. They are the
// vocabulary `gt mq list` already uses for a queue entry (open, ready,
// blocked) plus the two ways an MR leaves the queue and "missing" for an
// active_mr pointer whose bead is gone (reaped after the reaper's retention
// window, or never in this rig's database).
const (
	polecatMRStatusOpen     = "open"
	polecatMRStatusReady    = "ready"
	polecatMRStatusBlocked  = "blocked"
	polecatMRStatusMerged   = "merged"
	polecatMRStatusRejected = "rejected"
	polecatMRStatusMissing  = "missing"
)

// polecatMRLister is the slice of beads the inventory reads for MR state; an
// interface so tests can substitute a fake without a Dolt server.
type polecatMRLister interface {
	ListMergeRequests(opts beads.ListOptions) ([]*beads.Issue, error)
}

// polecatMRIndex is one rig's merge-request beads, read ONCE per list run and
// then looked up per polecat. MRs are ephemeral wisps: a per-polecat `bd show`
// neither finds them reliably nor scales, which is the blindness this replaces.
type polecatMRIndex struct {
	// loaded records that the rig-wide query actually answered. Without it an
	// unreadable queue (or no query at all) would be indistinguishable from
	// "this polecat has no MR", and the inventory would report "missing" —
	// a claim about a queue nobody managed to read.
	loaded bool
	// byID holds every MR the rig returned, so a polecat's own active_mr
	// resolves even after it left the queue (merged/rejected).
	byID map[string]*beads.Issue
	// byWorker holds only MRs still in the queue, keyed by MRFields.Worker
	// (the polecat name the branch carries). Restricted to open MRs so a stale
	// merged MR cannot be reported as a polecat's current work.
	byWorker map[string]*beads.Issue
}

// loadPolecatMRIndex issues the rig's one bulk merge-request query. Status
// "all" (not just open) so a polecat whose MR merged or was rejected reports
// how it ended instead of looking like a missing bead; `gt mq list` reads the
// same set with --status all.
func loadPolecatMRIndex(bd polecatMRLister, rigName string) (polecatMRIndex, error) {
	mrs, err := bd.ListMergeRequests(beads.ListOptions{
		Status:   "all",
		Label:    "gt:merge-request",
		Rig:      rigName,
		Priority: -1, // no priority filter
	})
	if err != nil {
		return polecatMRIndex{}, err
	}
	return buildPolecatMRIndex(rigName, mrs), nil
}

// buildPolecatMRIndex indexes merge requests for one rig. MRs are wisps shared
// across every rig in the Dolt server, so an MR that names a different rig is
// dropped — including in the worker index, where two rigs could otherwise
// collide on the same polecat name (a polecat's name is only unique per rig).
func buildPolecatMRIndex(rigName string, mrs []*beads.Issue) polecatMRIndex {
	index := polecatMRIndex{
		loaded:   true,
		byID:     make(map[string]*beads.Issue, len(mrs)),
		byWorker: make(map[string]*beads.Issue, len(mrs)),
	}
	for _, mr := range mrs {
		if mr == nil || mr.ID == "" {
			continue
		}
		var worker string
		if fields := beads.ParseMRFields(mr); fields != nil {
			if fields.Rig != "" && !strings.EqualFold(fields.Rig, rigName) {
				continue
			}
			worker = strings.TrimSpace(fields.Worker)
		}
		index.byID[mr.ID] = mr
		if worker == "" || beads.IssueStatus(mr.Status).IsTerminal() {
			continue
		}
		if current := index.byWorker[worker]; current == nil || mrIsNewerThan(mr, current) {
			index.byWorker[worker] = mr
		}
	}
	return index
}

// mrIsNewerThan orders two MRs for the same worker. Several can be open at once
// after a re-submission; the newest is the one that describes the current work.
// Unparseable or equal timestamps keep the incumbent, so the pick is stable
// rather than dependent on the lister's order.
func mrIsNewerThan(mr, current *beads.Issue) bool {
	newer := beads.ParseIssueTime(mr.CreatedAt)
	older := beads.ParseIssueTime(current.CreatedAt)
	return !newer.IsZero() && newer.After(older)
}

// statusFor resolves one polecat's merge request: the agent bead's active_mr
// pointer first (that is the authoritative link), then the rig's open MRs by
// worker for a polecat whose pointer was stale or already cleared. A pointer
// with no bead behind it, in a queue that was read, is "missing"; a polecat
// with no MR at all reports neither id nor status.
//
// An index that never loaded reports nothing at all rather than "missing":
// "the queue does not have it" is a claim only a queue that was read can make.
func (ix polecatMRIndex) statusFor(activeMR, worker string) (id, status string) {
	if !ix.loaded {
		return "", ""
	}
	activeMR = strings.TrimSpace(activeMR)
	worker = strings.TrimSpace(worker)
	if mr := ix.byID[activeMR]; mr != nil {
		return mr.ID, polecatMRStatusFor(mr)
	}
	if mr := ix.byWorker[worker]; mr != nil {
		return mr.ID, polecatMRStatusFor(mr)
	}
	if activeMR == "" {
		return "", ""
	}
	return activeMR, polecatMRStatusMissing
}

// polecatMRStatusFor classifies an MR bead the way `gt mq list` presents the
// queue (internal/cmd/mq_list.go): an open MR with unresolved dependencies is
// blocked, one the refinery has claimed (assignee set) is open — in flight —
// and one free of both is ready. A terminal MR reports how it ended: merged for
// a merge, rejected for every other close reason (rejected, conflict,
// superseded), since the queue will never take it.
func polecatMRStatusFor(mr *beads.Issue) string {
	if mr == nil {
		return polecatMRStatusMissing
	}
	if beads.IssueStatus(mr.Status).IsTerminal() {
		if strings.EqualFold(mrCloseReason(mr), "merged") {
			return polecatMRStatusMerged
		}
		return polecatMRStatusRejected
	}
	if beads.HasUnresolvedBlockers(mr) {
		return polecatMRStatusBlocked
	}
	if strings.TrimSpace(mr.Assignee) != "" {
		return polecatMRStatusOpen
	}
	return polecatMRStatusReady
}

// mrCloseReason prefers the description's close_reason field (what the refinery
// writes) over the bead's own column.
func mrCloseReason(mr *beads.Issue) string {
	if fields := beads.ParseMRFields(mr); fields != nil && strings.TrimSpace(fields.CloseReason) != "" {
		return strings.TrimSpace(fields.CloseReason)
	}
	return strings.TrimSpace(mr.CloseReason)
}

var polecatSummaryWorkStatuses = []beads.IssueStatus{
	beads.IssueStatusHooked,
	beads.StatusInProgress,
	beads.StatusOpen,
	beads.StatusBlocked,
	beads.StatusDeferred,
}

var polecatSummaryWorkStatusRank = func() map[string]int {
	ranks := make(map[string]int, len(polecatSummaryWorkStatuses))
	for i, status := range polecatSummaryWorkStatuses {
		ranks[string(status)] = i
	}
	return ranks
}()

func listActivePolecatWorkByName(bd *beads.Beads, rigName string) (map[string]*beads.Issue, error) {
	byName := make(map[string]*beads.Issue)
	issues, err := bd.ListIssueStatuses(polecatSummaryWorkStatuses...)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		evidence := assessPolecatAssignedIssueWork(issue)
		if !evidence.BlocksCleanup {
			continue
		}
		name, ok := polecatNameFromAssignee(rigName, issue.Assignee)
		if !ok {
			continue
		}
		if current := byName[name]; current == nil || polecatSummaryIssueRank(issue) < polecatSummaryIssueRank(current) {
			byName[name] = issue
		}
	}
	return byName, nil
}

func polecatSummaryIssueRank(issue *beads.Issue) int {
	if issue == nil {
		return len(polecatSummaryWorkStatuses)
	}
	if rank, ok := polecatSummaryWorkStatusRank[issue.Status]; ok {
		return rank
	}
	return len(polecatSummaryWorkStatuses)
}

func polecatNameFromAssignee(rigName, assignee string) (string, bool) {
	prefix := rigName + "/polecats/"
	if !strings.HasPrefix(assignee, prefix) {
		return "", false
	}
	name := strings.TrimPrefix(assignee, prefix)
	if name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return name, true
}

func assessPolecatAssignedIssueWork(issue *beads.Issue) polecatActiveWorkEvidence {
	if issue == nil || beads.IsAgentBead(issue) || beads.IsProtectedBead(issue) || beads.IssueStatus(issue.Status).IsTerminal() {
		return polecatActiveWorkEvidence{}
	}
	requiresRestart := polecatSummaryIssueRequiresRestart(beads.IssueStatus(issue.Status))
	return polecatActiveWorkEvidence{
		BlocksCleanup:        true,
		RequiresRestart:      requiresRestart,
		CountsTowardCapacity: requiresRestart,
		Blocker:              fmt.Sprintf("assigned_work=%s status=%s", issue.ID, issue.Status),
		AssignedIssue:        issue.ID,
	}
}

func polecatSummaryIssueRequiresRestart(status beads.IssueStatus) bool {
	switch status {
	case beads.IssueStatusHooked, beads.StatusInProgress, beads.StatusOpen:
		return true
	default:
		return false
	}
}

func assessPolecatAgentStateWork(state beads.AgentState) polecatActiveWorkEvidence {
	if state == "" || state == beads.AgentStateIdle || state == beads.AgentStateDone || state == beads.AgentStateNuked {
		return polecatActiveWorkEvidence{}
	}
	if state.IsActive() {
		return polecatActiveWorkEvidence{
			BlocksCleanup:        true,
			RequiresRestart:      true,
			CountsTowardCapacity: true,
			Blocker:              fmt.Sprintf("agent_state=%s", state),
		}
	}
	if state.ProtectsFromCleanup() || state == beads.AgentStateEscalated {
		return polecatActiveWorkEvidence{
			BlocksCleanup: true,
			Blocker:       fmt.Sprintf("agent_state=%s", state),
		}
	}
	return polecatActiveWorkEvidence{}
}

func polecatActiveWorkLookupError(err error) polecatActiveWorkEvidence {
	if err == nil {
		return polecatActiveWorkEvidence{}
	}
	return polecatActiveWorkEvidence{
		BlocksCleanup: true,
		Blocker:       fmt.Sprintf("assigned_work status=lookup_error: %v", err),
	}
}

func parsePolecatAgentFields(issue *beads.Issue) *beads.AgentFields {
	if issue == nil {
		return nil
	}
	fields := beads.ParseAgentFields(issue.Description)
	fields.AgentState = beads.ResolveAgentState(issue.Description, issue.AgentState)
	return fields
}
