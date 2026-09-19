package reaper

import (
	"testing"
	"time"
)

func TestDebugMoleculeStepCandidates(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"mol-closed":               {id: "mol-closed", status: "closed", issueType: "molecule", createdAt: now},
			"mol-open":                 {id: "mol-open", status: "open", issueType: "molecule", createdAt: now},
			"closed-epic":              {id: "closed-epic", status: "closed", issueType: "epic", createdAt: now},
			"step-closed-mol-recent":   {id: "step-closed-mol-recent", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
			"step-closed-mol-old":      {id: "step-closed-mol-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-mixed-parent-old":    {id: "step-mixed-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-external-parent-old": {id: "step-external-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-open-parent-old":     {id: "step-open-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-non-molecule-parent": {id: "step-non-molecule-parent", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"agent-step":               {id: "agent-step", status: "open", issueType: "agent", createdAt: now.Add(-48 * time.Hour)},
			"stale-orphan":             {id: "stale-orphan", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"fresh-orphan":             {id: "fresh-orphan", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
			"step-absent-parent-old":   {id: "step-absent-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-absent-parent-recent":{id: "step-absent-parent-recent", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
		},
		deps: []fakeDep{
			{issueID: "step-closed-mol-recent", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-closed-mol-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnExternal: "external:other", depType: "parent-child"},
			{issueID: "step-open-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-non-molecule-parent", dependsOnID: "closed-epic", depType: "parent-child"},
			{issueID: "agent-step", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-absent-parent-old", dependsOnID: "mol-purged", depType: "parent-child"},
			{issueID: "step-absent-parent-recent", dependsOnID: "mol-purged-recent", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}

	c := &fakeReaperConn{id: 1, state: state}
	state.nextConn = 2

	var ids []string
	for id := range state.wisps {
		if c.state.isMoleculeStepCandidateLocked(id) {
			ids = append(ids, id)
		}
	}
	t.Logf("moleculeStepCandidates (%d): %v", len(ids), ids)

	// Also check stale candidates
	maxAge := 24 * time.Hour
	stale := state.staleCandidatesLocked(now.Add(-maxAge), true)
	t.Logf("staleCandidates (%d): %v", len(stale), stale)

	// Check which are open
	var openIds []string
	for id, w := range state.wisps {
		if isOpenWispStatus(w.status) {
			openIds = append(openIds, id)
		}
	}
	t.Logf("open wisps (%d): %v", len(openIds), openIds)
}
