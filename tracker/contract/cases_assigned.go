package contract

import (
	"errors"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "assigned answers each assignee's issues by status in one call", Run: assignedByStatus})
}

func mustAssigned(t *testing.T, s store.Store, req api.AssignedRequest) api.AssignedPage {
	t.Helper()
	page, err := s.Assigned(ctx(), req)
	if err != nil {
		t.Fatalf("Assigned(%+v): %v", req, err)
	}
	return page
}

// field returns the ids of one assignee's rows, in the order the server sent.
func field(page api.AssignedPage, assignee string) []string {
	out := make([]string, len(page.Assigned[assignee]))
	for i, is := range page.Assigned[assignee] {
		out[i] = is.ID
	}
	return out
}

func assignedByStatus(t *testing.T, s store.Store) {
	// seat-a holds one issue per status the request can name; seat-b holds one
	// open; seat-c exists only as an assignee nobody named.
	aOpen := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a open", Assignee: "seat-a", Priority: 1})
	aHook := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a hooked", Assignee: "seat-a", Priority: 2})
	mustUpdate(t, s, aHook.ID, api.UpdatePatch{Status: str("hooked")}, api.Guard{})
	aBusy := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a in progress", Assignee: "seat-a", Priority: 3})
	mustUpdate(t, s, aBusy.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	aShut := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a closed", Assignee: "seat-a", Priority: 0})
	// force: closing a seat's issue is normally refused without it.
	if err := s.CloseIssues(ctx(), []string{aShut.ID}, "r", true); err != nil {
		t.Fatal(err)
	}
	bOpen := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b open", Assignee: "seat-b", Labels: []string{"x"}})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "c open", Assignee: "seat-c"})
	hqOpen := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq open", Assignee: "seat-a"})
	aWisp := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a wisp", Assignee: "seat-a", Ephemeral: true})
	mustUpdate(t, s, aWisp.ID, api.UpdatePatch{Status: str("hooked")}, api.Guard{})

	page := mustAssigned(t, s, api.AssignedRequest{Assignees: []string{"seat-a", "seat-b"}, Statuses: []string{"hooked", "open"}})
	sameSet(t, "seat-a", field(page, "seat-a"), aOpen.ID, aHook.ID, hqOpen.ID)
	sameSet(t, "seat-b", field(page, "seat-b"), bOpen.ID)
	if _, ok := page.Assigned["seat-c"]; ok {
		t.Errorf("the answer holds the unrequested seat-c")
	}
	if page.Count != 4 || page.NextCursor != "" {
		t.Errorf("count/cursor = %d/%q, want 4 and none", page.Count, page.NextCursor)
	}
	if len(page.Assigned["seat-b"]) == 1 && !slices.Equal(page.Assigned["seat-b"][0].Labels, []string{"x"}) {
		t.Errorf("listed Labels = %v, want [x]: a fan-out row carries what a list row does", page.Assigned["seat-b"][0].Labels)
	}

	sameSet(t, "in_progress is not asked for", field(mustAssigned(t, s, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"open"}}), "seat-a"), aOpen.ID, hqOpen.ID)
	sameSet(t, "one rig", field(mustAssigned(t, s, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"open"}, Rig: "hq"}), "seat-a"), hqOpen.ID)
	sameSet(t, "a wisp asked for", field(mustAssigned(t, s, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"hooked"}, IncludeWisps: true}), "seat-a"), aHook.ID, aWisp.ID)

	for name, req := range map[string]api.AssignedRequest{
		"no assignees":   {Statuses: []string{"open"}},
		"no statuses":    {Assignees: []string{"seat-a"}},
		"unknown status": {Assignees: []string{"seat-a"}, Statuses: []string{"wobbly"}},
		"bad cursor":     {Assignees: []string{"seat-a"}, Statuses: []string{"open"}, Cursor: "garbage!"},
	} {
		if _, err := s.Assigned(ctx(), req); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s = %v, want api.ErrInvalid", name, err)
		}
	}

	// Paging: one page holds only part of a seat's work, and the cursor
	// resumes after it, in list order, without a repeat or a gap.
	for _, p := range []int{4, 5} {
		mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "more", Assignee: "seat-a", Priority: p})
	}
	want := mustList(t, s, api.ListFilter{Assignee: "seat-a", Statuses: []string{"all"}})
	var wantIDs []string
	for _, is := range want.Issues {
		wantIDs = append(wantIDs, is.ID)
	}
	var got []string
	cursor, pages := "", 0
	for {
		p := mustAssigned(t, s, api.AssignedRequest{Assignees: []string{"seat-a"}, Statuses: []string{"open", "hooked", "in_progress", "closed"}, Limit: 2, Cursor: cursor})
		got = append(got, field(p, "seat-a")...)
		pages++
		if p.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		cursor = p.NextCursor
	}
	if wantPages := (len(wantIDs) + 1) / 2; !slices.Equal(got, wantIDs) || pages != wantPages {
		t.Errorf("paged %v over %d pages, want %v over %d: no gaps, no repeats", got, pages, wantIDs, wantPages)
	}
}
