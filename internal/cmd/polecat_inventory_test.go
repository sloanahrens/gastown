package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
)

func TestPolecatSessionSet(t *testing.T) {
	setupPolecatTestRegistry(t)
	sessions := newPolecatSessionSet([]string{
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
	if got := sessions.namesForRig("gastown"); len(got) != 1 || got[0] != "gt-thunder" {
		t.Fatalf("namesForRig(gastown) = %v", got)
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
	setupPolecatTestRegistry(t)
	created := time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)
	lister := fakeSessionLister{
		sessions: []string{"gt-topaz", "gt-thunder"},
		env: map[string]map[string]string{
			"gt-topaz":   {"GT_AGENT": "flash"},
			"gt-thunder": {}, // session predates GT_AGENT
		},
		created: map[string]time.Time{"gt-topaz": created},
	}

	sessions, err := loadPolecatSessionSet(lister)
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
	setupPolecatTestRegistry(t)
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

func TestBuildPolecatInventoryItem(t *testing.T) {
	setupPolecatTestRegistry(t)
	sessions := newPolecatSessionSet([]string{"gt-running"})
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
