package contract

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "wisp read: ephemeral issues by status and age cutoffs, paged by cursor", Run: wispReadByAgeAndStatus})
	register(Case{Name: "wisp count: the whole match, and a filter carrying a page is refused", Run: wispCountAllOfTheMatch})
}

// closeAll closes ids so a closed-read case has rows at the status it reads.
func closeAll(t *testing.T, s store.Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.CloseIssues(ctx(), []string{id}, "reaped", false); err != nil {
			t.Fatalf("CloseIssues(%s): %v", id, err)
		}
	}
}

// wispReadByAgeAndStatus pins the read the wisp reaper walks: ephemeral rows
// only, narrowed by status and by the two upper age bounds, a page at a time,
// with the durable issue of the same status and age never among them.
//
// An upper bound is measured from now, because the store sets created_at and
// closed_at itself: a bound in the future keeps every row it covers and one in
// the past keeps none, which is what pins that the bound is applied at all.
func wispReadByAgeAndStatus(t *testing.T, s store.Store) {
	open := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open wisp", Ephemeral: true, Priority: 1})
	closedA := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed wisp a", Ephemeral: true, Priority: 2})
	closedB := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed wisp b", Ephemeral: true, Priority: 3})
	durable := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "durable issue", Priority: 1})
	closeAll(t, s, closedA.ID, closedB.ID, durable.ID)

	future, past := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	closedBefore := func(at time.Time) api.ListFilter {
		return api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}, ClosedBefore: at}
	}

	sameSet(t, "closed wisps before a bound after them", ids(mustList(t, s, closedBefore(future))), closedA.ID, closedB.ID)
	sameSet(t, "closed wisps before a bound before them", ids(mustList(t, s, closedBefore(past))))
	sameSet(t, "open wisps created before a bound after them",
		ids(mustList(t, s, api.ListFilter{Ephemeral: true, CreatedBefore: future})), open.ID)
	sameSet(t, "open wisps created before a bound before them",
		ids(mustList(t, s, api.ListFilter{Ephemeral: true, CreatedBefore: past})))

	// The page walk: one row at a time, no gap and no repeat, and the open
	// wisp stays out of a closed read on every page.
	var paged []string
	cursor, pages := "", 0
	for {
		f := closedBefore(future)
		f.Limit, f.Cursor = 1, cursor
		page := mustList(t, s, f)
		paged = append(paged, ids(page)...)
		pages++
		if page.NextCursor == "" {
			break
		}
		if pages > 5 {
			t.Fatal("the wisp read does not end")
		}
		cursor = page.NextCursor
	}
	if pages != 2 || !slices.Equal(paged, []string{closedA.ID, closedB.ID}) {
		t.Errorf("paged %v over %d pages, want [%s %s] over 2: no gaps, no repeats", paged, pages, closedA.ID, closedB.ID)
	}
}

// wispCountAllOfTheMatch pins the count beside the list: it answers for every
// row the same filter reads, across pages, and refuses a filter that carries a
// page because that would be a count of something a list call already shapes.
func wispCountAllOfTheMatch(t *testing.T, s store.Store) {
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open wisp", Ephemeral: true, Priority: 1})
	closed := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "closed wisp", Ephemeral: true, Priority: 2})
	durable := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "durable issue", Priority: 1})
	closeAll(t, s, closed.ID, durable.ID)

	all := api.ListFilter{Ephemeral: true, Statuses: []string{"all"}}
	allN, err := s.CountIssues(ctx(), all)
	if err != nil {
		t.Fatalf("CountIssues(%+v): %v", all, err)
	}
	if page := mustList(t, s, all); allN != 2 || allN != len(page.Issues) {
		t.Errorf("counted %d wisps while the list read %d, want 2 both: the durable issue is in neither", allN, len(page.Issues))
	}

	closedOnly := api.ListFilter{Ephemeral: true, Statuses: []string{"closed"}}
	if n, err := s.CountIssues(ctx(), closedOnly); err != nil || n != 1 {
		t.Errorf("closed wisps = %d, %v; want 1: the status narrows the count as it narrows the list", n, err)
	}

	// An upper age bound the rows predate keeps none of them, and the count
	// says so without a page to walk.
	aged := api.ListFilter{Ephemeral: true, Statuses: []string{"all"}, CreatedBefore: time.Now().Add(-time.Hour)}
	if n, err := s.CountIssues(ctx(), aged); err != nil || n != 0 {
		t.Errorf("wisps created before an hour ago = %d, %v; want 0", n, err)
	}

	for name, f := range map[string]api.ListFilter{
		"a limit":  {Ephemeral: true, Limit: 10},
		"a cursor": {Ephemeral: true, Cursor: "c1"},
	} {
		if n, err := s.CountIssues(ctx(), f); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("a count with %s = %d, %v; want api.ErrInvalid", name, n, err)
		}
	}
}
