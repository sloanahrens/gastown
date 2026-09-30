package cmd

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// twoStoreClient models a rig whose listing sees only rig-local beads while
// prefix routing (ShowMultiple) still reaches the town store.
type twoStoreClient struct {
	beads.Client
	rigListing []*beads.Issue
	routed     map[string]*beads.Issue
	showErr    error
	showCalls  [][]string
}

func (c *twoStoreClient) ListIssueStatuses(...beads.IssueStatus) ([]*beads.Issue, error) {
	return c.rigListing, nil
}

func (c *twoStoreClient) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	c.showCalls = append(c.showCalls, ids)
	if c.showErr != nil {
		return nil, c.showErr
	}
	found := make(map[string]*beads.Issue)
	for _, id := range ids {
		if issue, ok := c.routed[id]; ok {
			found[id] = issue
		}
	}
	return found, nil
}

func TestListActivePolecatWorkByNameSeesTownPrefixedHook(t *testing.T) {
	townHook := &beads.Issue{ID: "hq-90m15", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/agate"}
	bd := &twoStoreClient{
		rigListing: []*beads.Issue{
			{ID: "gt-0sjf", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/amber"},
		},
		routed: map[string]*beads.Issue{"hq-90m15": townHook},
	}
	hooks := map[string]string{"agate": "hq-90m15", "amber": "gt-0sjf"}

	got, err := listActivePolecatWorkByName(bd, "gastown", hooks)
	if err != nil {
		t.Fatalf("listActivePolecatWorkByName: %v", err)
	}
	if got["agate"] != townHook {
		t.Fatalf("agate's hq- hook = %v, want %s", got["agate"], townHook.ID)
	}
	if got["amber"] == nil || got["amber"].ID != "gt-0sjf" {
		t.Fatalf("amber's rig-local hook = %v, want gt-0sjf", got["amber"])
	}
	if len(bd.showCalls) != 1 || len(bd.showCalls[0]) != 1 || bd.showCalls[0][0] != "hq-90m15" {
		t.Fatalf("routed reads = %v, want one batch for the hook the listing missed", bd.showCalls)
	}

	// The hq- hook must surface as the same cleanup blocker a rig-local hook does.
	evidence := assessPolecatAssignedIssueWork(got["agate"])
	if !evidence.BlocksCleanup || evidence.Blocker != "assigned_work=hq-90m15 status=hooked" {
		t.Fatalf("evidence = %+v, want an assigned_work blocker for hq-90m15", evidence)
	}
}

func TestListActivePolecatWorkByNameSkipsRoutedReadWhenListingHasHooks(t *testing.T) {
	bd := &twoStoreClient{
		rigListing: []*beads.Issue{
			{ID: "gt-0sjf", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/amber"},
		},
	}
	if _, err := listActivePolecatWorkByName(bd, "gastown", map[string]string{"amber": "gt-0sjf"}); err != nil {
		t.Fatalf("listActivePolecatWorkByName: %v", err)
	}
	if len(bd.showCalls) != 0 {
		t.Fatalf("routed reads = %v, want none when every hook was listed", bd.showCalls)
	}
}

func TestListActivePolecatWorkByNameIgnoresStaleTownHook(t *testing.T) {
	tests := []struct {
		name  string
		issue *beads.Issue
	}{
		{"closed", &beads.Issue{ID: "hq-done", Status: string(beads.StatusClosed), Assignee: "gastown/polecats/agate"}},
		{"reassigned", &beads.Issue{ID: "hq-moved", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/onyx"}},
		{"other rig", &beads.Issue{ID: "hq-other", Status: string(beads.IssueStatusHooked), Assignee: "longeye/polecats/agate"}},
		{"gone", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bd := &twoStoreClient{routed: map[string]*beads.Issue{}}
			hookID := "hq-gone"
			if tt.issue != nil {
				hookID = tt.issue.ID
				bd.routed[hookID] = tt.issue
			}
			got, err := listActivePolecatWorkByName(bd, "gastown", map[string]string{"agate": hookID})
			if err != nil {
				t.Fatalf("listActivePolecatWorkByName: %v", err)
			}
			if got["agate"] != nil {
				t.Fatalf("agate work = %v, want none for a stale hook_bead", got["agate"])
			}
		})
	}
}

func TestListActivePolecatWorkByNameFailsOnUnreadableHook(t *testing.T) {
	bd := &twoStoreClient{showErr: errors.New("town store unreachable")}
	if _, err := listActivePolecatWorkByName(bd, "gastown", map[string]string{"agate": "hq-90m15"}); err == nil {
		t.Fatal("want an error so callers fail closed, got nil")
	}
}

func TestPolecatHookBeads(t *testing.T) {
	agentID := func(name string) string { return "gt-gastown-polecat-" + name }
	agents := map[string]*beads.Issue{
		"gt-gastown-polecat-agate": {ID: "gt-gastown-polecat-agate", Description: "hook_bead: hq-90m15\n"},
		"gt-gastown-polecat-amber": {ID: "gt-gastown-polecat-amber", Description: "hook_bead: null\n"},
	}
	got := polecatHookBeads([]string{"agate", "amber", "onyx"}, agentID, agents)
	if len(got) != 1 || got["agate"] != "hq-90m15" {
		t.Fatalf("polecatHookBeads = %v, want only agate -> hq-90m15", got)
	}
}
