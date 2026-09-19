package cmd

import (
	"fmt"
	"sort"
	"strings"
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
}

// polecatInventoryEnv carries the per-rig facts the list path has and the
// capacity path does not: one rig-wide merge-request index, and the agent-bead
// facts the spawn grace is measured against. The zero value means "no MR
// source, no grace" and reproduces the pre-gt-2540 states.
type polecatInventoryEnv struct {
	MRs   polecatMRIndex
	Spawn polecatSpawnFacts
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
		} else if running && !polecat.CleanupStatus(item.CleanupStatus).IsSafe() {
			item.State = polecat.StateReviewNeeded
		}
		facts.ActiveWorkBlocker = activeWorkEvidence.Blocker
		facts.ActiveWorkCountsTowardCapacity = activeWorkEvidence.CountsTowardCapacity
	} else if item.State == polecat.StateIdle && running && !polecat.CleanupStatus(item.CleanupStatus).IsSafe() {
		item.State = polecat.StateReviewNeeded
	}

	if fields != nil && !activeWorkEvidence.BlocksCleanup {
		if hookBead := strings.TrimSpace(fields.HookBead); hookBead != "" {
			facts.ActiveWorkBlocker = fmt.Sprintf("hook_bead=%s status=unverified", hookBead)
		}
	}
	if item.ActiveMR != "" {
		// Still blocking whenever active_mr is set (fail-closed, unchanged), but
		// the status is now the joined MR's real state when the rig-wide index
		// answered. "unknown" is what an absent index leaves behind — capacity
		// and a failed MR lookup.
		mrStatus := item.MRStatus
		if mrStatus == "" {
			mrStatus = "unknown"
		}
		facts.ActiveMRBlocker = "active_mr=" + item.ActiveMR + " status=" + mrStatus
	}

	facts.State = item.State
	item.Disposition = polecat.DecideWorkstate(polecat.NewWorkstateInput(facts))
	return item
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
