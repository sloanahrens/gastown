package cmd

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

func TestPolecatSessionSet(t *testing.T) {
	t.Parallel()
	sessions := newPolecatSessionSet(polecatTestRegistry(), []string{
		"gt-thunder",
		"gt-crew-dom",
		"gp-mirelurk",
		"not-a-polecat",
	})

	if got, ok := sessions.lookup("gastown", "thunder"); !ok || got != "gt-thunder" {
		t.Fatalf("lookup gastown/thunder = %q, %v", got, ok)
	}
	if _, ok := sessions.lookup("gastown", "dom"); ok {
		t.Fatal("crew session should not be indexed as polecat")
	}
	if got := sessions.polecatsForRig("gastown"); len(got) != 1 || got[0] != "thunder" {
		t.Fatalf("polecatsForRig(gastown) = %v", got)
	}
}

// fakeSessionLister stands in for tmux: sessions plus their environments.
type fakeSessionLister struct {
	sessions []string
	env      map[string]map[string]string
	created  map[string]time.Time
}

func (f fakeSessionLister) ListSessions() ([]string, error) { return f.sessions, nil }

func (f fakeSessionLister) GetEnvironment(session, key string) (string, error) {
	if value, ok := f.env[session][key]; ok {
		return value, nil
	}
	return "", errors.New("no such variable")
}

func (f fakeSessionLister) GetSessionCreatedTime(name string) (time.Time, error) {
	if created, ok := f.created[name]; ok {
		return created, nil
	}
	return time.Time{}, errors.New("no such session")
}

// TestLoadPolecatSessionSetReadsAgent is the agent half of gt-2540: the list
// command reports the agent a polecat runs from the session environment
// (GT_AGENT, written at spawn), resolved once per list — not per polecat from
// the agent bead, which does not carry it.
func TestLoadPolecatSessionSetReadsAgent(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
	lister := fakeSessionLister{
		sessions: []string{"gt-topaz", "gt-thunder"},
		env: map[string]map[string]string{
			"gt-topaz":   {"GT_AGENT": "flash"},
			"gt-thunder": {}, // session predates GT_AGENT
		},
		created: map[string]time.Time{"gt-topaz": created},
	}

	sessions, err := loadPolecatSessionSet(polecatTestRegistry(), lister)
	if err != nil {
		t.Fatalf("loadPolecatSessionSet: %v", err)
	}
	entry, ok := sessions.session("gastown", "topaz")
	if !ok {
		t.Fatal("gastown/topaz missing from session set")
	}
	if entry.Agent != "flash" {
		t.Errorf("agent = %q, want flash", entry.Agent)
	}
	if !entry.Created.Equal(created) {
		t.Errorf("created = %s, want %s", entry.Created, created)
	}
	missing, ok := sessions.session("gastown", "thunder")
	if !ok {
		t.Fatal("gastown/thunder missing from session set")
	}
	if missing.Agent != "" {
		t.Errorf("agent = %q, want empty for a session with no GT_AGENT", missing.Agent)
	}
}

// TestBuildPolecatInventoryItemSpawnGrace drives the alarming branch of the
// inventory grace site (gt-yteq): a hooked bead whose session has not appeared
// yet must read spawning inside the agent bead's startup window and stalled
// after it. Without the grace, every dispatch is "stalled" the instant it is
// slung, and the restart paths chase a session that is still booting.
func TestBuildPolecatInventoryItemSpawnGrace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 19, 16, 32, 0, 0, time.UTC)
	const grace = 5 * time.Minute

	tests := []struct {
		name       string
		agentState string
		updatedAt  time.Time
		wantState  polecat.State
	}{
		{
			name:       "hooked bead, no session, spawning 30s ago is spawning",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-30 * time.Second),
			wantState:  polecat.StateSpawning,
		},
		{
			name:       "hooked bead, no session, spawning 6m ago is stalled",
			agentState: string(beads.AgentStateSpawning),
			updatedAt:  now.Add(-6 * time.Minute),
			wantState:  polecat.StateStalled,
		},
		{
			name:       "hooked bead, no session, working state is stalled",
			agentState: string(beads.AgentStateWorking),
			updatedAt:  now.Add(-30 * time.Second),
			wantState:  polecat.StateStalled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooked := &beads.Issue{ID: "gt-hook", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/topaz"}
			env := polecatInventoryEnv{Spawn: polecatSpawnFacts{UpdatedAt: tt.updatedAt, Grace: grace, Now: now}}
			item := buildPolecatInventoryItem(
				"gastown",
				"topaz",
				&beads.AgentFields{AgentState: tt.agentState, CleanupStatus: string(polecat.CleanupClean)},
				hooked,
				polecatSessionSet{},
				env,
			)
			if item.State != tt.wantState {
				t.Fatalf("state = %q, want %q (item %+v)", item.State, tt.wantState, item)
			}
			wantSpawning := tt.wantState == polecat.StateSpawning
			if item.Spawning != wantSpawning {
				t.Errorf("Spawning = %v, want %v — the grace verdict must travel with the item (effectivePolecatState consumes it)", item.Spawning, wantSpawning)
			}
		})
	}
}

// TestBuildPolecatInventoryItemSubmittedIsNotStalled pins gt-obbx2: a polecat
// whose hooked bead is gt:ready-to-land has no session because gt done ended
// it, so list must say submitted — stalled reads as "dead polecat with work"
// and got the witness to raise a second session on finished work. A live
// session still reads working, and the state survives list reconciliation.
func TestBuildPolecatInventoryItemSubmittedIsNotStalled(t *testing.T) {
	t.Parallel()
	hooked := &beads.Issue{
		ID: "gt-hook", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/topaz",
		Labels: []string{"gt:ready-to-land"},
	}
	fields := &beads.AgentFields{AgentState: string(beads.AgentStateWorking), CleanupStatus: string(polecat.CleanupClean)}

	item := buildPolecatInventoryItem("gastown", "topaz", fields, hooked, polecatSessionSet{}, polecatInventoryEnv{})
	if item.State != polecat.StateSubmitted {
		t.Fatalf("state = %q, want %q (item %+v)", item.State, polecat.StateSubmitted, item)
	}
	if item.Issue != "gt-hook" {
		t.Errorf("Issue = %q, want the submitted bead", item.Issue)
	}

	listItem := PolecatListItem{State: item.State, Issue: item.Issue, CountsTowardCapacity: true}
	if got := effectivePolecatState(listItem, false); got != polecat.StateSubmitted {
		t.Errorf("effectivePolecatState = %q, want %q", got, polecat.StateSubmitted)
	}

	live := buildPolecatInventoryItem("gastown", "topaz", fields, hooked,
		newPolecatSessionSet(polecatTestRegistry(), []string{"gt-topaz"}), polecatInventoryEnv{})
	if !live.SessionRunning || live.State != polecat.StateWorking {
		t.Errorf("live session: running=%v state=%q, want running and %q", live.SessionRunning, live.State, polecat.StateWorking)
	}
}

func TestBuildPolecatInventoryItem(t *testing.T) {
	t.Parallel()
	sessions := newPolecatSessionSet(polecatTestRegistry(), []string{"gt-running"})
	tests := []struct {
		name         string
		polecatName  string
		fields       *beads.AgentFields
		activeWork   *beads.Issue
		wantState    polecat.State
		wantIssue    string
		wantVerdict  string
		wantReusable bool
		wantRecovery bool
		wantCapacity bool
	}{
		{
			name:         "clean idle reusable",
			polecatName:  "idle",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean)},
			wantState:    polecat.StateIdle,
			wantVerdict:  polecat.WorkstateVerdictSafeToNuke,
			wantReusable: true,
		},
		{
			name:         "hooked running is working capacity",
			polecatName:  "running",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean)},
			activeWork:   &beads.Issue{ID: "gt-hook", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/running"},
			wantState:    polecat.StateWorking,
			wantIssue:    "gt-hook",
			wantVerdict:  polecat.WorkstateVerdictWorking,
			wantCapacity: true,
		},
		{
			name:         "open stopped is stalled capacity",
			polecatName:  "stopped",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean)},
			activeWork:   &beads.Issue{ID: "gt-open", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/stopped"},
			wantState:    polecat.StateStalled,
			wantIssue:    "gt-open",
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantCapacity: true,
		},
		{
			name:         "deferred protects without capacity",
			polecatName:  "deferred",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean)},
			activeWork:   &beads.Issue{ID: "gt-deferred", Status: string(beads.StatusDeferred), Assignee: "gastown/polecats/deferred"},
			wantState:    polecat.StateIdle,
			wantIssue:    "gt-deferred",
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
		},
		{
			name:         "hook fallback protects without capacity",
			polecatName:  "hookonly",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), HookBead: "gt-old"},
			wantState:    polecat.StateIdle,
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
		},
		{
			name:         "paused agent state protects without capacity",
			polecatName:  "paused",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStatePaused), CleanupStatus: string(polecat.CleanupClean)},
			wantState:    polecat.StateIdle,
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
		},
		{
			name:        "active mr is pending non capacity",
			polecatName: "pendingmr",
			fields:      &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), ActiveMR: "gt-mr"},
			wantState:   polecat.StateIdle,
			wantVerdict: polecat.WorkstateVerdictPendingMR,
		},
		{
			name:         "done without active mr and clean cleanup is reusable",
			polecatName:  "done",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupClean)},
			wantState:    polecat.StateDone,
			wantVerdict:  polecat.WorkstateVerdictSafeToNuke,
			wantReusable: true,
		},
		{
			name:         "done without active mr blocks reuse when cleanup is dirty",
			polecatName:  "donedirty",
			fields:       &beads.AgentFields{AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupUnpushed)},
			wantState:    polecat.StateDone,
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantCapacity: true,
		},
		{
			name:        "done with active mr remains pending",
			polecatName: "donepending",
			fields:      &beads.AgentFields{AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupClean), ActiveMR: "gt-mr"},
			wantState:   polecat.StateDone,
			wantVerdict: polecat.WorkstateVerdictPendingMR,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := buildPolecatInventoryItem("gastown", tt.polecatName, tt.fields, tt.activeWork, sessions, polecatInventoryEnv{})
			if item.State != tt.wantState || item.Issue != tt.wantIssue || item.Disposition.Verdict != tt.wantVerdict || item.Disposition.Reusable != tt.wantReusable || item.Disposition.NeedsRecovery != tt.wantRecovery || item.Disposition.CountsTowardCapacity != tt.wantCapacity {
				t.Fatalf("item = %+v disposition=%+v", item, item.Disposition)
			}
		})
	}
}

// TestBuildPolecatInventoryItemResolvesHookBead is the `gt polecat list` half
// of gt-eqiid. The list used to stamp every hook_bead "status=unverified"
// without looking it up, so a reference that outlived its work (flint/garnet on
// gt-2xqtj, granite on gt-gyw5w — all status=deferred) showed up as recovery
// work in the same rig whose check-recovery reported it safe. The two paths
// have to read one policy.
//
// The nil-source case is not an oversight: the counts-only capacity projection
// passes no source on purpose, and it keeps the fail-closed wording it had
// before it could resolve anything.
func TestBuildPolecatInventoryItemResolvesHookBead(t *testing.T) {
	t.Parallel()
	fields := func(hookBead string) *beads.AgentFields {
		return &beads.AgentFields{
			AgentState:    string(beads.AgentStateIdle),
			CleanupStatus: string(polecat.CleanupClean),
			HookBead:      hookBead,
		}
	}

	tests := []struct {
		name         string
		hookBead     string
		source       polecat.IssueReader
		wantVerdict  string
		wantReusable bool
		wantRecovery bool
		wantBlocker  string
	}{
		{
			// The reported false positive, at the list path.
			name:         "a deferred hook reference clears",
			hookBead:     "gt-2xqtj",
			source:       fakeIssueShower{issue: &beads.Issue{ID: "gt-2xqtj", Status: string(beads.StatusDeferred)}},
			wantVerdict:  polecat.WorkstateVerdictSafeToNuke,
			wantReusable: true,
		},
		{
			name:         "a blocked hook reference clears",
			hookBead:     "gt-gyw5w",
			source:       fakeIssueShower{issue: &beads.Issue{ID: "gt-gyw5w", Status: string(beads.StatusBlocked)}},
			wantVerdict:  polecat.WorkstateVerdictSafeToNuke,
			wantReusable: true,
		},
		{
			name:         "a live hook reference still blocks",
			hookBead:     "gt-work",
			source:       fakeIssueShower{issue: &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)}},
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantBlocker:  "hook_bead=gt-work status=hooked",
		},
		{
			name:         "a submitted hook reference is left to the landing worker",
			hookBead:     "gt-acdfp",
			source:       fakeIssueShower{issue: &beads.Issue{ID: "gt-acdfp", Status: string(beads.IssueStatusHooked), Labels: []string{"gt:ready-to-land"}}},
			wantVerdict:  polecat.WorkstateVerdictSubmitted,
			wantReusable: false,
		},
		{
			name:         "a lookup error still fails closed",
			hookBead:     "gt-work",
			source:       fakeIssueShower{err: errors.New("bd exploded")},
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantBlocker:  "status=lookup_error",
		},
		{
			name:         "an unmodeled status still fails closed",
			hookBead:     "gt-mystery",
			source:       fakeIssueShower{issue: &beads.Issue{ID: "gt-mystery", Status: "quarantined"}},
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantBlocker:  "status=quarantined",
		},
		{
			// The capacity projection's shape: no source, so no lookup and the
			// same "unverified" refusal this path has always reported.
			name:         "no issue source keeps the fail-closed unverified refusal",
			hookBead:     "gt-old",
			source:       nil,
			wantVerdict:  polecat.WorkstateVerdictNeedsRecovery,
			wantRecovery: true,
			wantBlocker:  "hook_bead=gt-old status=unverified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := buildPolecatInventoryItem(
				"gastown", "seat", fields(tt.hookBead), nil,
				newPolecatSessionSet(polecatTestRegistry(), nil),
				polecatInventoryEnv{IssueSource: tt.source},
			)
			if item.Disposition.Verdict != tt.wantVerdict ||
				item.Disposition.Reusable != tt.wantReusable ||
				item.Disposition.NeedsRecovery != tt.wantRecovery {
				t.Fatalf("disposition = %+v, want verdict=%s reusable=%v needs_recovery=%v",
					item.Disposition, tt.wantVerdict, tt.wantReusable, tt.wantRecovery)
			}
			if tt.wantBlocker != "" && !slices.ContainsFunc(item.Disposition.Blockers, func(b string) bool {
				return strings.Contains(b, tt.wantBlocker)
			}) {
				t.Fatalf("Blockers = %v, want %q among them", item.Disposition.Blockers, tt.wantBlocker)
			}
		})
	}
}

func TestBuildPolecatInventoryItemActiveWorkLookupErrorFailsClosed(t *testing.T) {
	t.Parallel()
	item := buildPolecatInventoryItemFromEvidence(
		"gastown",
		"lookup",
		&beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean)},
		polecatActiveWorkLookupError(errors.New("bd failed")),
		polecatSessionSet{},
		polecatInventoryEnv{},
	)

	if item.Disposition.Reusable || item.Disposition.SafeToNuke || !item.Disposition.NeedsRecovery || item.Disposition.CountsTowardCapacity {
		t.Fatalf("lookup error disposition = %+v", item.Disposition)
	}
	if item.Disposition.Reason != "active-work" {
		t.Fatalf("reason = %q, want active-work", item.Disposition.Reason)
	}
	if len(item.Disposition.Blockers) != 1 || !strings.Contains(item.Disposition.Blockers[0], "lookup_error") {
		t.Fatalf("blockers = %v, want lookup_error", item.Disposition.Blockers)
	}
}

func TestPolecatSummaryIssueRankPrefersActiveWork(t *testing.T) {
	t.Parallel()
	ordered := []*beads.Issue{
		{ID: "hook", Status: string(beads.IssueStatusHooked)},
		{ID: "progress", Status: string(beads.StatusInProgress)},
		{ID: "open", Status: string(beads.StatusOpen)},
		{ID: "blocked", Status: string(beads.StatusBlocked)},
		{ID: "deferred", Status: string(beads.StatusDeferred)},
	}
	for i := 1; i < len(ordered); i++ {
		if polecatSummaryIssueRank(ordered[i-1]) >= polecatSummaryIssueRank(ordered[i]) {
			t.Fatalf("rank(%s) should be before rank(%s)", ordered[i-1].Status, ordered[i].Status)
		}
	}
}

func TestPolecatNameFromAssignee(t *testing.T) {
	t.Parallel()
	tests := []struct {
		assignee string
		wantName string
		wantOK   bool
	}{
		{assignee: "gastown/polecats/thunder", wantName: "thunder", wantOK: true},
		{assignee: "other/polecats/thunder"},
		{assignee: "gastown/crew/dom"},
		{assignee: "gastown/polecats/"},
		{assignee: "gastown/polecats/a/b"},
	}
	for _, tt := range tests {
		got, ok := polecatNameFromAssignee("gastown", tt.assignee)
		if got != tt.wantName || ok != tt.wantOK {
			t.Fatalf("polecatNameFromAssignee(%q) = %q, %v", tt.assignee, got, ok)
		}
	}
}

// TestBuildPolecatInventoryItemStaleAgentState is the agent_state half of
// gt-eqiid (gt-is9jb): flint's agent bead still said working after its session
// died, and its hook named a deferred bead. With no live bead assigned, the
// state alone read as a stall the witness may restart. The record is debris
// only when the hook reference is readable and names nothing at risk; every
// other shape keeps reading as a stall.
func TestBuildPolecatInventoryItemStaleAgentState(t *testing.T) {
	t.Parallel()
	sessions := func(running bool) polecatSessionSet {
		if running {
			return newPolecatSessionSet(polecatTestRegistry(), []string{"gt-running"})
		}
		return polecatSessionSet{}
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		agentState beads.AgentState
		hookBead   string
		source     polecat.IssueReader
		running    bool
		updatedAt  time.Time
		wantState  polecat.State
		wantCap    bool
	}{
		{
			// The reported case.
			name:       "working, no session, deferred hook is idle debris",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-2xqtj",
			source:     fakeIssueShower{issue: &beads.Issue{ID: "gt-2xqtj", Status: string(beads.StatusDeferred)}},
			wantState:  polecat.StateIdle,
		},
		{
			name:       "working, no session, no hook reference is idle debris",
			agentState: beads.AgentStateWorking,
			source:     fakeIssueShower{},
			wantState:  polecat.StateIdle,
		},
		{
			name:       "working, no session, closed hook is idle debris",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-done",
			source:     fakeIssueShower{issue: &beads.Issue{ID: "gt-done", Status: string(beads.StatusClosed)}},
			wantState:  polecat.StateIdle,
		},
		{
			name:       "working, no session, live hook is still a stall",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-work",
			source:     fakeIssueShower{issue: &beads.Issue{ID: "gt-work", Status: string(beads.IssueStatusHooked)}},
			wantState:  polecat.StateStalled,
			wantCap:    true,
		},
		{
			name:       "working, no session, unreadable hook is still a stall",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-work",
			source:     fakeIssueShower{err: errors.New("bd exploded")},
			wantState:  polecat.StateStalled,
			wantCap:    true,
		},
		{
			name:       "working, no session, missing hook bead is still a stall",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-gone",
			source:     fakeIssueShower{},
			wantState:  polecat.StateStalled,
			wantCap:    true,
		},
		{
			// The capacity projection passes no source: it cannot read the
			// hook, so it keeps counting the seat.
			name:       "working, no session, no issue source is still a stall",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-2xqtj",
			wantState:  polecat.StateStalled,
			wantCap:    true,
		},
		{
			name:       "working with a live session is working",
			agentState: beads.AgentStateWorking,
			hookBead:   "gt-2xqtj",
			source:     fakeIssueShower{issue: &beads.Issue{ID: "gt-2xqtj", Status: string(beads.StatusDeferred)}},
			running:    true,
			wantState:  polecat.StateWorking,
			wantCap:    true,
		},
		{
			name:       "spawning inside its grace window is spawning",
			agentState: beads.AgentStateSpawning,
			source:     fakeIssueShower{},
			updatedAt:  now.Add(-30 * time.Second),
			wantState:  polecat.StateSpawning,
			wantCap:    true,
		},
		{
			name:       "spawning past its grace window with an inert hook is idle debris",
			agentState: beads.AgentStateSpawning,
			hookBead:   "gt-2xqtj",
			source:     fakeIssueShower{issue: &beads.Issue{ID: "gt-2xqtj", Status: string(beads.StatusDeferred)}},
			updatedAt:  now.Add(-6 * time.Minute),
			wantState:  polecat.StateIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := polecatInventoryEnv{
				IssueSource: tt.source,
				Spawn:       polecatSpawnFacts{UpdatedAt: tt.updatedAt, Grace: 5 * time.Minute, Now: now},
			}
			item := buildPolecatInventoryItem(
				"gastown", "running",
				&beads.AgentFields{AgentState: string(tt.agentState), CleanupStatus: string(polecat.CleanupClean), HookBead: tt.hookBead},
				nil, sessions(tt.running), env,
			)
			if item.State != tt.wantState {
				t.Fatalf("state = %q, want %q (item %+v)", item.State, tt.wantState, item)
			}
			if item.Disposition.CountsTowardCapacity != tt.wantCap {
				t.Errorf("CountsTowardCapacity = %v, want %v (disposition %+v)", item.Disposition.CountsTowardCapacity, tt.wantCap, item.Disposition)
			}
		})
	}
}
