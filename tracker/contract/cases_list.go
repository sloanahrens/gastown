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
	register(Case{Name: "list hides ephemeral and infrastructure issues unless asked", Run: listDefaultsHide})
	register(Case{Name: "list filters by label (all of them), assignee, type, parent, rig, ids and priority", Run: listFilters})
	register(Case{Name: "list orders by priority then creation and pages by cursor without gaps or repeats", Run: listPaging})
	register(Case{Name: "list statuses: none means not closed, all means everything, a list is an OR", Run: listStatuses})
	register(Case{Name: "list created_after and an invalid cursor", Run: listCreatedAfterAndBadCursor})
	register(Case{Name: "list refuses assignee together with no_assignee", Run: listAssigneeConflict})
	register(Case{Name: "list scopes to several rigs at once and reports a rig it does not hold", Run: listRigs})
}

func ids(page api.IssuePage) []string {
	out := make([]string, len(page.Issues))
	for i, is := range page.Issues {
		out[i] = is.ID
	}
	return out
}

func mustList(t *testing.T, s store.Store, f api.ListFilter) api.IssuePage {
	t.Helper()
	page, err := s.ListIssues(ctx(), f)
	if err != nil {
		t.Fatalf("ListIssues(%+v): %v", f, err)
	}
	return page
}

func sameSet(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func listDefaultsHide(t *testing.T, s store.Store) {
	task := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "task"})
	agent := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "agent", Type: "agent"})
	msg := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "msg", Type: "message"})
	wisp := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "wisp", Ephemeral: true})
	sameSet(t, "default list", ids(mustList(t, s, api.ListFilter{})), task.ID)
	sameSet(t, "include_infra", ids(mustList(t, s, api.ListFilter{IncludeInfra: true})), task.ID, agent.ID, msg.ID)
	sameSet(t, "ephemeral", ids(mustList(t, s, api.ListFilter{Ephemeral: true})), wisp.ID)
	sameSet(t, "an explicit type shows that infra type", ids(mustList(t, s, api.ListFilter{IssueType: "agent"})), agent.ID)
}

func listFilters(t *testing.T, s store.Store) {
	epic := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "epic", Type: "epic", Priority: 1})
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a", Labels: []string{"x", "y"}, Assignee: "me", Priority: 2, Parent: epic.ID})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b", Labels: []string{"x"}, Priority: 3, Type: "bug"})
	c := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "c", Priority: 2})
	all := []string{epic.ID, a.ID, b.ID, c.ID}
	sameSet(t, "no filter", ids(mustList(t, s, api.ListFilter{})), all...)
	sameSet(t, "label x", ids(mustList(t, s, api.ListFilter{Labels: []string{"x"}})), a.ID, b.ID)
	sameSet(t, "labels x and y (all of them)", ids(mustList(t, s, api.ListFilter{Labels: []string{"x", "y"}})), a.ID)
	sameSet(t, "assignee me", ids(mustList(t, s, api.ListFilter{Assignee: "me"})), a.ID)
	sameSet(t, "no assignee", ids(mustList(t, s, api.ListFilter{NoAssignee: true})), epic.ID, b.ID, c.ID)
	sameSet(t, "type bug", ids(mustList(t, s, api.ListFilter{IssueType: "bug"})), b.ID)
	sameSet(t, "parent", ids(mustList(t, s, api.ListFilter{Parent: epic.ID})), a.ID)
	sameSet(t, "rig hq", ids(mustList(t, s, api.ListFilter{Rig: "hq"})), c.ID)
	sameSet(t, "ids", ids(mustList(t, s, api.ListFilter{IDs: []string{b.ID, c.ID, "gt-nope"}})), b.ID, c.ID)
	p2 := 2
	sameSet(t, "priority 2", ids(mustList(t, s, api.ListFilter{Priority: &p2})), a.ID, c.ID)
	p0 := 0
	sameSet(t, "priority 0", ids(mustList(t, s, api.ListFilter{Priority: &p0})))
	// Labels come back on listed issues, sorted.
	for _, is := range mustList(t, s, api.ListFilter{Labels: []string{"x", "y"}}).Issues {
		if !slices.Equal(is.Labels, []string{"x", "y"}) {
			t.Errorf("listed Labels = %v, want [x y]", is.Labels)
		}
	}
}

func listPaging(t *testing.T, s store.Store) {
	var want []string
	prios := []int{2, 1, 2, 0, 1}
	made := make([]api.Issue, len(prios))
	for i, p := range prios {
		made[i] = mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "n", Priority: p})
	}
	order := []int{3, 1, 4, 0, 2} // priority 0, then 1 (oldest first), then 2 (oldest first)
	for _, i := range order {
		want = append(want, made[i].ID)
	}
	if got := ids(mustList(t, s, api.ListFilter{})); !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v (priority, then creation)", got, want)
	}
	var got []string
	cursor, pages := "", 0
	for {
		page := mustList(t, s, api.ListFilter{Limit: 2, Cursor: cursor})
		got = append(got, ids(page)...)
		pages++
		if page.NextCursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("paging does not end")
		}
		cursor = page.NextCursor
	}
	if !slices.Equal(got, want) || pages != 3 {
		t.Errorf("paged %v over %d pages, want %v over 3: no gaps, no repeats", got, pages, want)
	}
	if page := mustList(t, s, api.ListFilter{Limit: 5}); page.NextCursor != "" {
		t.Errorf("a page that holds everything has NextCursor %q, want none", page.NextCursor)
	}
}

func listStatuses(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	sameSet(t, "open", ids(mustList(t, s, api.ListFilter{Statuses: []string{"open"}})), a.ID)
	sameSet(t, "closed", ids(mustList(t, s, api.ListFilter{Statuses: []string{"closed"}})))
	sameSet(t, "closed or open", ids(mustList(t, s, api.ListFilter{Statuses: []string{"closed", "open"}})), a.ID)
	sameSet(t, "all", ids(mustList(t, s, api.ListFilter{Statuses: []string{"all"}})), a.ID)
	sameSet(t, "none means not closed", ids(mustList(t, s, api.ListFilter{})), a.ID)
}

func listCreatedAfterAndBadCursor(t *testing.T, s store.Store) {
	first := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "first"})
	second := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "second"})
	sameSet(t, "created_after the second", ids(mustList(t, s, api.ListFilter{CreatedAfter: second.CreatedAt})), second.ID)
	sameSet(t, "created_after the first", ids(mustList(t, s, api.ListFilter{CreatedAfter: first.CreatedAt})), first.ID, second.ID)
	sameSet(t, "created_after the future", ids(mustList(t, s, api.ListFilter{CreatedAfter: time.Now().Add(time.Hour)})))
	if _, err := s.ListIssues(ctx(), api.ListFilter{Cursor: "garbage!"}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("a bad cursor error = %v, want api.ErrInvalid", err)
	}
}

func listAssigneeConflict(t *testing.T, s store.Store) {
	if _, err := s.ListIssues(ctx(), api.ListFilter{Assignee: "me", NoAssignee: true}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("assignee with no_assignee = %v, want api.ErrInvalid", err)
	}
}

func listRigs(t *testing.T, s store.Store) {
	gt := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "gt task"})
	hq := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq task"})
	sameSet(t, "gt and hq", ids(mustList(t, s, api.ListFilter{Rigs: []string{"gt", "hq"}})), gt.ID, hq.ID)
	sameSet(t, "hq alone", ids(mustList(t, s, api.ListFilter{Rigs: []string{"hq"}})), hq.ID)
	sameSet(t, "hq twice", ids(mustList(t, s, api.ListFilter{Rigs: []string{"hq", "hq"}})), hq.ID)
	for name, f := range map[string]api.ListFilter{
		"rig and rigs together":             {Rig: "gt", Rigs: []string{"hq"}},
		"a rigs list of empty strings only": {Rigs: []string{"", ""}},
	} {
		if _, err := s.ListIssues(ctx(), f); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: ListIssues = %v, want api.ErrInvalid", name, err)
		}
		// The count answers for the same rows the list reads, so it refuses
		// the same filter rather than reporting a rig's queue as empty.
		if n, err := s.CountIssues(ctx(), f); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: CountIssues = %d, %v; want api.ErrInvalid", name, n, err)
		}
	}
	// A rig the tracker does not hold is reported on its own entry: the rigs
	// it does hold still answer, and the unknown one is never an empty rig.
	page := mustList(t, s, api.ListFilter{Rigs: []string{"gt", "nope"}})
	sameSet(t, "gt beside an unknown rig", ids(page), gt.ID)
	if _, ok := page.RigErrors["nope"]; !ok || len(page.RigErrors) != 1 {
		t.Errorf("RigErrors = %v, want an entry for nope alone", page.RigErrors)
	}
	page = mustList(t, s, api.ListFilter{Rigs: []string{"nope"}})
	if len(page.Issues) != 0 || len(page.RigErrors) != 1 {
		t.Errorf("only an unknown rig asked: issues %v, RigErrors %v; want none and an entry for nope", ids(page), page.RigErrors)
	}
	// The count refuses an unknown rig rather than answering it as empty.
	if n, err := s.CountIssues(ctx(), api.ListFilter{Rigs: []string{"gt", "nope"}}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("count with an unknown rig = %d, %v; want api.ErrInvalid", n, err)
	}
	if n, err := s.CountIssues(ctx(), api.ListFilter{Rigs: []string{"gt", "hq"}}); err != nil || n != 2 {
		t.Errorf("count of gt and hq = %d, %v; want 2", n, err)
	}
}
