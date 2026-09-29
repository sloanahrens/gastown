package beadsfake

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// RunClientContract checks the behavior every beads.Client must share: the
// fake in the unit tier, *beads.Beads against bd and a Dolt test database in
// the integration tier. newClient must return an empty database each call.
// Every behavior the fake documents is pinned here, the refusals and
// missing-issue paths included.
func RunClientContract(t *testing.T, newClient func(t *testing.T) beads.Client) {
	t.Helper()
	for _, c := range contractCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, newClient(t))
		})
	}
}

type contractCase struct {
	name string
	run  func(t *testing.T, c beads.Client)
}

var contractCases = []contractCase{
	{"create and show", contractCreateShow},
	{"missing issue", contractMissing},
	{"flag-like title", contractFlagTitle},
	{"show multiple", contractShowMultiple},
	{"update fields", contractUpdateFields},
	{"labels", contractLabels},
	{"claim fence", contractClaimFence},
	{"close and reopen", contractCloseReopen},
	{"close fence", contractCloseFence},
	{"list filters", contractListFilters},
	{"assignee queries", contractAssigneeQueries},
	{"comments", contractComments},
	{"dependencies and ready", contractDependencies},
	{"ready filter", contractReadyFilter},
	{"children", contractChildren},
	{"release", contractRelease},
}

// mustCreate creates an issue or fails the test.
func mustCreate(t *testing.T, c beads.Client, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	is, err := c.Create(opts)
	if err != nil {
		t.Fatalf("Create(%q): %v", opts.Title, err)
	}
	if is == nil || is.ID == "" {
		t.Fatalf("Create(%q) returned no ID: %+v", opts.Title, is)
	}
	return is
}

func mustShow(t *testing.T, c beads.Client, id string) *beads.Issue {
	t.Helper()
	is, err := c.Show(id)
	if err != nil {
		t.Fatalf("Show(%s): %v", id, err)
	}
	return is
}

func mustDo(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func ptr[T any](v T) *T { return &v }

// ids returns the sorted IDs of issues.
func ids(issues []*beads.Issue) []string {
	out := []string{}
	for _, is := range issues {
		out = append(out, is.ID)
	}
	sort.Strings(out)
	return out
}

func sorted(in ...string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func wantIDs(t *testing.T, what string, got []*beads.Issue, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if g, w := ids(got), sorted(want...); !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %v, want %v", what, g, w)
	}
}

func labels(is *beads.Issue) []string {
	return sorted(is.Labels...)
}

func contractCreateShow(t *testing.T, c beads.Client) {
	desc := "first line\nsecond line"
	created := mustCreate(t, c, beads.CreateOptions{
		Title: "alpha", Description: desc, Labels: []string{"x", "gt:task"}, Priority: 1,
	})
	got := mustShow(t, c, created.ID)
	if got.ID != created.ID || got.Title != "alpha" || got.Description != desc {
		t.Errorf("Show = id %q title %q description %q", got.ID, got.Title, got.Description)
	}
	if got.Status != "open" || got.Priority != 1 || got.Assignee != "" || got.Type != "task" {
		t.Errorf("Show = status %q priority %d assignee %q type %q, want open 1 \"\" task", got.Status, got.Priority, got.Assignee, got.Type)
	}
	if !reflect.DeepEqual(labels(got), []string{"gt:task", "x"}) {
		t.Errorf("labels = %v", got.Labels)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" || got.ClosedAt != "" || got.Ephemeral {
		t.Errorf("timestamps/ephemeral = created %q updated %q closed %q ephemeral %v", got.CreatedAt, got.UpdatedAt, got.ClosedAt, got.Ephemeral)
	}

	dflt := mustCreate(t, c, beads.CreateOptions{Title: "default priority", Priority: -1})
	if p := mustShow(t, c, dflt.ID).Priority; p != 2 {
		t.Errorf("priority -1 created priority %d, want bd's default 2", p)
	}
	actor := mustCreate(t, c, beads.CreateOptions{Title: "by someone", Priority: -1, Actor: "contract-actor"})
	if by := mustShow(t, c, actor.ID).CreatedBy; by != "contract-actor" {
		t.Errorf("CreatedBy = %q, want contract-actor", by)
	}
	wisp := mustCreate(t, c, beads.CreateOptions{Title: "a wisp", Priority: -1, Ephemeral: true})
	if !mustShow(t, c, wisp.ID).Ephemeral {
		t.Error("ephemeral create is not ephemeral")
	}
}

func contractMissing(t *testing.T, c beads.Client) {
	const id = "gt-nosuch"
	if _, err := c.Show(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Show(missing) = %v, want ErrNotFound", err)
	}
	if err := c.Update(id, beads.UpdateOptions{Status: ptr("in_progress")}); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Update(missing) = %v, want ErrNotFound", err)
	}
	if err := c.Close(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Close(missing) = %v, want ErrNotFound", err)
	}
	if err := c.AddComment(id, "x"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("AddComment(missing) = %v, want ErrNotFound", err)
	}
	if _, err := c.Comments(id); err == nil {
		t.Error("Comments(missing) succeeded")
	}
	real := mustCreate(t, c, beads.CreateOptions{Title: "exists", Priority: -1})
	if err := c.AddDependency(real.ID, id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("AddDependency(on missing) = %v, want ErrNotFound", err)
	}
}

func contractFlagTitle(t *testing.T, c beads.Client) {
	if _, err := c.Create(beads.CreateOptions{Title: "--help", Priority: -1}); !errors.Is(err, beads.ErrFlagTitle) {
		t.Errorf("Create(--help) = %v, want ErrFlagTitle", err)
	}
}

func contractShowMultiple(t *testing.T, c beads.Client) {
	a := mustCreate(t, c, beads.CreateOptions{Title: "a", Priority: -1})
	b := mustCreate(t, c, beads.CreateOptions{Title: "b", Priority: -1})
	got, err := c.ShowMultiple([]string{a.ID, "gt-nosuch", b.ID})
	if err != nil {
		t.Fatalf("ShowMultiple: %v", err)
	}
	if len(got) != 2 || got[a.ID] == nil || got[b.ID] == nil || got[a.ID].Title != "a" {
		t.Errorf("ShowMultiple = %v, want exactly %s and %s", got, a.ID, b.ID)
	}
	empty, err := c.ShowMultiple(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("ShowMultiple(nil) = %v, %v", empty, err)
	}
}

func contractUpdateFields(t *testing.T, c beads.Client) {
	is := mustCreate(t, c, beads.CreateOptions{Title: "before", Description: "old", Priority: 3})
	desc := "new body\nwith two lines"
	mustDo(t, "Update", c.Update(is.ID, beads.UpdateOptions{
		Title: ptr("after"), Priority: ptr(0), Description: &desc,
		Assignee: ptr("alice"), Status: ptr("in_progress"),
	}))
	got := mustShow(t, c, is.ID)
	if got.Title != "after" || got.Priority != 0 || got.Description != desc || got.Assignee != "alice" || got.Status != "in_progress" {
		t.Errorf("after Update: title %q priority %d description %q assignee %q status %q",
			got.Title, got.Priority, got.Description, got.Assignee, got.Status)
	}
	mustDo(t, "clear description", c.Update(is.ID, beads.UpdateOptions{Description: ptr("")}))
	if d := mustShow(t, c, is.ID).Description; d != "" {
		t.Errorf("description after clearing = %q", d)
	}
	mustDo(t, "hooked", c.Update(is.ID, beads.UpdateOptions{Status: ptr(beads.StatusHooked)}))
	if s := mustShow(t, c, is.ID).Status; s != beads.StatusHooked {
		t.Errorf("status = %q, want hooked", s)
	}
}

func contractLabels(t *testing.T, c beads.Client) {
	is := mustCreate(t, c, beads.CreateOptions{Title: "labels", Labels: []string{"a", "b"}, Priority: -1})
	mustDo(t, "add/remove", c.Update(is.ID, beads.UpdateOptions{AddLabels: []string{"c"}, RemoveLabels: []string{"a"}}))
	if got := labels(mustShow(t, c, is.ID)); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("after add c, remove a: %v", got)
	}
	mustDo(t, "set", c.Update(is.ID, beads.UpdateOptions{SetLabels: []string{"z", "y"}}))
	if got := labels(mustShow(t, c, is.ID)); !reflect.DeepEqual(got, []string{"y", "z"}) {
		t.Errorf("after set y,z: %v", got)
	}
	single := mustCreate(t, c, beads.CreateOptions{Title: "single", Label: "gt:one", Priority: -1})
	if got := labels(mustShow(t, c, single.ID)); !reflect.DeepEqual(got, []string{"gt:one"}) {
		t.Errorf("Label create: %v", got)
	}
	typed := mustCreate(t, c, beads.CreateOptions{Title: "typed", Type: "bug", Priority: -1})
	if got := labels(mustShow(t, c, typed.ID)); !reflect.DeepEqual(got, []string{"gt:bug"}) {
		t.Errorf("Type create: %v", got)
	}
}

func contractClaimFence(t *testing.T, c beads.Client) {
	is := mustCreate(t, c, beads.CreateOptions{Title: "claimed", Priority: -1})
	mustDo(t, "claim", c.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr("alice")}))
	if err := c.Update(is.ID, beads.UpdateOptions{Assignee: ptr("bob")}); err == nil {
		t.Error("reassigning alice's in_progress claim to bob succeeded without Force")
	}
	if a := mustShow(t, c, is.ID).Assignee; a != "alice" {
		t.Errorf("refused reassign changed assignee to %q", a)
	}
	mustDo(t, "same holder", c.Update(is.ID, beads.UpdateOptions{Assignee: ptr("alice"), Priority: ptr(1)}))
	mustDo(t, "forced reassign", c.Update(is.ID, beads.UpdateOptions{Assignee: ptr("bob"), Force: true}))
	if a := mustShow(t, c, is.ID).Assignee; a != "bob" {
		t.Errorf("forced reassign left assignee %q", a)
	}
	open := mustCreate(t, c, beads.CreateOptions{Title: "open, assigned", Priority: -1})
	mustDo(t, "assign open", c.Update(open.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	mustDo(t, "reassign open", c.Update(open.ID, beads.UpdateOptions{Assignee: ptr("bob")}))
}

func contractCloseReopen(t *testing.T, c beads.Client) {
	a := mustCreate(t, c, beads.CreateOptions{Title: "a", Priority: -1})
	b := mustCreate(t, c, beads.CreateOptions{Title: "b", Priority: -1})
	mustDo(t, "Close", c.Close(a.ID))
	got := mustShow(t, c, a.ID)
	if got.Status != "closed" || got.CloseReason != "Closed" || got.ClosedAt == "" {
		t.Errorf("Close: status %q reason %q closed_at %q, want closed \"Closed\" set", got.Status, got.CloseReason, got.ClosedAt)
	}
	mustDo(t, "CloseWithReason", c.CloseWithReason("finished", b.ID))
	if r := mustShow(t, c, b.ID).CloseReason; r != "finished" {
		t.Errorf("CloseWithReason reason = %q", r)
	}
	mustDo(t, "reopen", c.Update(a.ID, beads.UpdateOptions{Status: ptr("open")}))
	got = mustShow(t, c, a.ID)
	if got.Status != "open" || got.ClosedAt != "" || got.CloseReason != "" {
		t.Errorf("reopen: status %q closed_at %q reason %q", got.Status, got.ClosedAt, got.CloseReason)
	}
	x := mustCreate(t, c, beads.CreateOptions{Title: "x", Priority: -1})
	y := mustCreate(t, c, beads.CreateOptions{Title: "y", Priority: -1})
	mustDo(t, "close two", c.CloseWithReason("batch", x.ID, y.ID))
	for _, id := range []string{x.ID, y.ID} {
		if s := mustShow(t, c, id).Status; s != "closed" {
			t.Errorf("%s status %q after batch close", id, s)
		}
	}
	mustDo(t, "Close()", c.Close())
}

func contractCloseFence(t *testing.T, c beads.Client) {
	parent := mustCreate(t, c, beads.CreateOptions{Title: "parent", Priority: -1})
	child := mustCreate(t, c, beads.CreateOptions{Title: "child", Parent: parent.ID, Priority: -1})
	if err := c.CloseWithReason("r", parent.ID); err == nil {
		t.Error("closing a parent with an open child succeeded")
	}
	if err := c.Update(parent.ID, beads.UpdateOptions{Status: ptr("closed")}); err == nil {
		t.Error("Update(status=closed) of a parent with an open child succeeded")
	}
	if s := mustShow(t, c, parent.ID).Status; s != "open" {
		t.Errorf("refused close left status %q", s)
	}
	mustDo(t, "ForceCloseWithReason", c.ForceCloseWithReason("forced", parent.ID))
	if got := mustShow(t, c, parent.ID); got.Status != "closed" || got.CloseReason != "forced" {
		t.Errorf("forced close: %q %q", got.Status, got.CloseReason)
	}
	other := mustCreate(t, c, beads.CreateOptions{Title: "other parent", Priority: -1})
	kid := mustCreate(t, c, beads.CreateOptions{Title: "closed kid", Parent: other.ID, Priority: -1})
	mustDo(t, "close kid", c.Close(kid.ID))
	mustDo(t, "close parent of closed child", c.Close(other.ID))
	_ = child

	// bd refuses to close an issue an open issue blocks, by close or by
	// update.
	blocker := mustCreate(t, c, beads.CreateOptions{Title: "blocker", Priority: -1})
	blocked := mustCreate(t, c, beads.CreateOptions{Title: "blocked", Priority: -1})
	mustDo(t, "AddDependency", c.AddDependency(blocked.ID, blocker.ID))
	if err := c.Close(blocked.ID); err == nil {
		t.Error("closing an issue with an open blocker succeeded")
	}
	if err := c.Update(blocked.ID, beads.UpdateOptions{Status: ptr("closed")}); err == nil {
		t.Error("Update(status=closed) of an issue with an open blocker succeeded")
	}
	mustDo(t, "force close blocked", c.ForceCloseWithReason("forced", blocked.ID))
	mustDo(t, "close the blocker itself", c.Close(blocker.ID))

	// bd refuses to close an issue assigned to someone other than the
	// actor. "alice" is never the actor of either implementation.
	theirs := mustCreate(t, c, beads.CreateOptions{Title: "alice's", Priority: -1})
	mustDo(t, "assign", c.Update(theirs.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	if err := c.Close(theirs.ID); err == nil {
		t.Error("closing an issue assigned to another actor succeeded")
	}
	if s := mustShow(t, c, theirs.ID).Status; s != "open" {
		t.Errorf("refused close left status %q", s)
	}
	mustDo(t, "ForceCloseWithReason theirs", c.ForceCloseWithReason("done", theirs.ID))
	if s := mustShow(t, c, theirs.ID).Status; s != "closed" {
		t.Errorf("forced close of alice's issue left status %q", s)
	}
	// The assignee fence is close's alone: an update to status closed
	// passes it.
	also := mustCreate(t, c, beads.CreateOptions{Title: "alice's too", Priority: -1})
	mustDo(t, "assign also", c.Update(also.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	mustDo(t, "Update(status=closed) of alice's issue", c.Update(also.ID, beads.UpdateOptions{Status: ptr("closed")}))
	if s := mustShow(t, c, also.ID).Status; s != "closed" {
		t.Errorf("Update(status=closed) left status %q", s)
	}
}

func contractListFilters(t *testing.T, c beads.Client) {
	a := mustCreate(t, c, beads.CreateOptions{Title: "a", Labels: []string{"gt:task", "L"}, Priority: 1})
	mustDo(t, "assign a", c.Update(a.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	b := mustCreate(t, c, beads.CreateOptions{Title: "b", Labels: []string{"gt:bug"}, Priority: 3})
	cl := mustCreate(t, c, beads.CreateOptions{Title: "closed", Priority: 3})
	mustDo(t, "close", c.Close(cl.ID))
	w := mustCreate(t, c, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})

	list := func(opts beads.ListOptions) ([]*beads.Issue, error) { return c.List(opts) }
	all := beads.ListOptions{Priority: -1}
	got, err := list(all)
	wantIDs(t, "List{}", got, err, a.ID, b.ID)
	got, err = list(beads.ListOptions{Status: "all", Priority: -1})
	wantIDs(t, "List{Status:all}", got, err, a.ID, b.ID, cl.ID)
	got, err = list(beads.ListOptions{Status: "closed", Priority: -1})
	wantIDs(t, "List{Status:closed}", got, err, cl.ID)
	got, err = list(beads.ListOptions{Status: "open", Priority: -1})
	wantIDs(t, "List{Status:open}", got, err, a.ID, b.ID)
	got, err = list(beads.ListOptions{Label: "L", Priority: -1})
	wantIDs(t, "List{Label:L}", got, err, a.ID)
	got, err = list(beads.ListOptions{Type: "bug", Priority: -1})
	wantIDs(t, "List{Type:bug}", got, err, b.ID)
	got, err = list(beads.ListOptions{Assignee: "alice", Priority: -1})
	wantIDs(t, "List{Assignee:alice}", got, err, a.ID)
	got, err = list(beads.ListOptions{NoAssignee: true, Priority: -1})
	wantIDs(t, "List{NoAssignee}", got, err, b.ID)
	got, err = list(beads.ListOptions{Priority: 3, Status: "all"})
	wantIDs(t, "List{Priority:3,all}", got, err, b.ID, cl.ID)
	got, err = list(beads.ListOptions{Limit: 1, Priority: -1})
	if err != nil || len(got) != 1 {
		t.Errorf("List{Limit:1} = %v, %v; want one issue", ids(got), err)
	}
	got, err = list(beads.ListOptions{Ephemeral: true, Priority: -1})
	wantIDs(t, "List{Ephemeral}", got, err, w.ID)
	parent := mustCreate(t, c, beads.CreateOptions{Title: "p", Priority: -1})
	kid := mustCreate(t, c, beads.CreateOptions{Title: "kid", Parent: parent.ID, Priority: -1})
	got, err = list(beads.ListOptions{Parent: parent.ID, Priority: -1})
	wantIDs(t, "List{Parent}", got, err, kid.ID)
}

func contractAssigneeQueries(t *testing.T, c beads.Client) {
	if is, err := c.GetAssignedIssue("alice"); err != nil || is != nil {
		t.Errorf("GetAssignedIssue(nobody's) = %v, %v; want nil, nil", is, err)
	}
	hooked := mustCreate(t, c, beads.CreateOptions{Title: "hooked", Priority: -1})
	mustDo(t, "hook", c.Update(hooked.ID, beads.UpdateOptions{Status: ptr(beads.StatusHooked), Assignee: ptr("alice")}))
	if is, err := c.GetAssignedIssue("alice"); err != nil || is == nil || is.ID != hooked.ID {
		t.Fatalf("GetAssignedIssue with only a hooked issue = %v, %v", is, err)
	}
	working := mustCreate(t, c, beads.CreateOptions{Title: "working", Priority: -1})
	mustDo(t, "claim", c.Update(working.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr("alice")}))
	if is, err := c.GetAssignedIssue("alice"); err != nil || is == nil || is.ID != working.ID {
		t.Errorf("GetAssignedIssue prefers in_progress over hooked: got %v, %v", is, err)
	}
	open := mustCreate(t, c, beads.CreateOptions{Title: "open", Priority: -1})
	mustDo(t, "assign", c.Update(open.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	if is, err := c.GetAssignedIssue("alice"); err != nil || is == nil || is.ID != open.ID {
		t.Errorf("GetAssignedIssue prefers open: got %v, %v", is, err)
	}
	done := mustCreate(t, c, beads.CreateOptions{Title: "done", Priority: -1})
	mustDo(t, "assign done", c.Update(done.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	mustDo(t, "close done", c.ForceCloseWithReason("done", done.ID))
	wisp := mustCreate(t, c, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "assign wisp", c.Update(wisp.ID, beads.UpdateOptions{Assignee: ptr("alice")}))
	bobs := mustCreate(t, c, beads.CreateOptions{Title: "bob's", Priority: -1})
	mustDo(t, "assign bob", c.Update(bobs.ID, beads.UpdateOptions{Assignee: ptr("bob")}))

	got, err := c.ListByAssignee("alice")
	wantIDs(t, "ListByAssignee(alice)", got, err, hooked.ID, working.ID, open.ID, done.ID)
	got, err = c.ListIssueStatuses(beads.StatusOpen, beads.StatusInProgress)
	wantIDs(t, "ListIssueStatuses(open,in_progress)", got, err, working.ID, open.ID, bobs.ID)
	got, err = c.ListIssueStatuses()
	wantIDs(t, "ListIssueStatuses()", got, err)
	got, err = c.ListAssignedIssueStatuses("alice", beads.StatusOpen, beads.StatusInProgress)
	wantIDs(t, "ListAssignedIssueStatuses(alice, open, in_progress)", got, err, working.ID, open.ID, wisp.ID)
	got, err = c.ListAssignedIssueStatuses("", beads.StatusOpen)
	wantIDs(t, "ListAssignedIssueStatuses(\"\")", got, err)
}

func contractComments(t *testing.T, c beads.Client) {
	is := mustCreate(t, c, beads.CreateOptions{Title: "talk", Priority: -1})
	if got, err := c.Comments(is.ID); err != nil || len(got) != 0 {
		t.Errorf("Comments(new) = %v, %v", got, err)
	}
	mustDo(t, "first", c.AddComment(is.ID, "first"))
	mustDo(t, "second", c.AddComment(is.ID, "second one"))
	got, err := c.Comments(is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, cm := range got {
		texts = append(texts, cm.Text)
		if cm.IssueID != is.ID || cm.Author == "" || cm.CreatedAt == "" || cm.ID == "" {
			t.Errorf("comment = %+v", cm)
		}
	}
	if !reflect.DeepEqual(texts, []string{"first", "second one"}) {
		t.Errorf("comment texts = %q", texts)
	}
}

// depOn reports whether is lists a dependency on id of relation typ.
func depOn(is *beads.Issue, id, typ string) bool {
	for _, d := range is.Dependencies {
		if d.ID == id && d.DependencyType == typ {
			return true
		}
	}
	return false
}

func contractDependencies(t *testing.T, c beads.Client) {
	blocker := mustCreate(t, c, beads.CreateOptions{Title: "blocker", Priority: -1})
	blocked := mustCreate(t, c, beads.CreateOptions{Title: "blocked", Priority: -1})
	mustDo(t, "AddDependency", c.AddDependency(blocked.ID, blocker.ID))
	mustDo(t, "AddDependency again", c.AddDependency(blocked.ID, blocker.ID))
	if got := mustShow(t, c, blocked.ID); !depOn(got, blocker.ID, "blocks") || len(got.Dependencies) != 1 {
		t.Errorf("Show(blocked).Dependencies = %+v, want one blocks on %s", got.Dependencies, blocker.ID)
	}
	ready, err := c.Ready()
	wantIDs(t, "Ready while blocked", ready, err, blocker.ID)
	mustDo(t, "close blocker", c.Close(blocker.ID))
	ready, err = c.Ready()
	wantIDs(t, "Ready after blocker closed", ready, err, blocked.ID)
	mustDo(t, "reopen blocker", c.Update(blocker.ID, beads.UpdateOptions{Status: ptr("open")}))
	mustDo(t, "RemoveDependency", c.RemoveDependency(blocked.ID, blocker.ID))
	if got := mustShow(t, c, blocked.ID); len(got.Dependencies) != 0 {
		t.Errorf("Dependencies after remove = %+v", got.Dependencies)
	}
	ready, err = c.Ready()
	wantIDs(t, "Ready after RemoveDependency", ready, err, blocker.ID, blocked.ID)
}

func contractReadyFilter(t *testing.T, c beads.Client) {
	work := mustCreate(t, c, beads.CreateOptions{Title: "work", Labels: []string{"gt:task"}, Priority: -1})
	mail := mustCreate(t, c, beads.CreateOptions{Title: "mail", Labels: []string{"gt:message"}, Priority: -1})
	agent := mustCreate(t, c, beads.CreateOptions{Title: "agent", Labels: []string{"gt:agent"}, Priority: -1})
	busy := mustCreate(t, c, beads.CreateOptions{Title: "busy", Priority: -1})
	mustDo(t, "claim", c.Update(busy.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr("alice")}))
	done := mustCreate(t, c, beads.CreateOptions{Title: "done", Priority: -1})
	mustDo(t, "close", c.Close(done.ID))
	parent := mustCreate(t, c, beads.CreateOptions{Title: "parent", Priority: -1})
	kid := mustCreate(t, c, beads.CreateOptions{Title: "kid", Parent: parent.ID, Priority: -1})
	ready, err := c.Ready()
	wantIDs(t, "Ready", ready, err, work.ID, parent.ID, kid.ID)
	_, _ = mail, agent
}

func contractChildren(t *testing.T, c beads.Client) {
	parent := mustCreate(t, c, beads.CreateOptions{Title: "parent", Priority: -1})
	a := mustCreate(t, c, beads.CreateOptions{Title: "child a", Parent: parent.ID, Priority: -1})
	b := mustCreate(t, c, beads.CreateOptions{Title: "child b", Parent: parent.ID, Priority: -1})
	mustCreate(t, c, beads.CreateOptions{Title: "unrelated", Priority: -1})
	if !strings.HasPrefix(a.ID, parent.ID+".") {
		t.Errorf("child ID %q is not under %q", a.ID, parent.ID)
	}
	got := mustShow(t, c, a.ID)
	if got.Parent != parent.ID || !depOn(got, parent.ID, "parent-child") {
		t.Errorf("Show(child): parent %q deps %+v", got.Parent, got.Dependencies)
	}
	mustDo(t, "close b", c.Close(b.ID))
	kids, err := c.Children(parent.ID)
	wantIDs(t, "Children", kids, err, a.ID, b.ID)
	none, err := c.Children(a.ID)
	wantIDs(t, "Children(leaf)", none, err)
}

func contractRelease(t *testing.T, c beads.Client) {
	is := mustCreate(t, c, beads.CreateOptions{Title: "stuck", Priority: -1})
	mustDo(t, "claim", c.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr("dead-worker")}))
	mustDo(t, "ReleaseWithReason", c.ReleaseWithReason(is.ID, "worker died"))
	got := mustShow(t, c, is.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Errorf("after release: status %q assignee %q, want open and none", got.Status, got.Assignee)
	}
	if !strings.Contains(got.Notes, "worker died") {
		t.Errorf("notes = %q, want the release reason", got.Notes)
	}
	other := mustCreate(t, c, beads.CreateOptions{Title: "stuck 2", Priority: -1})
	mustDo(t, "claim 2", c.Update(other.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr("dead-worker")}))
	mustDo(t, "Release", c.Release(other.ID))
	if got := mustShow(t, c, other.ID); got.Status != "open" || got.Assignee != "" {
		t.Errorf("after Release: status %q assignee %q", got.Status, got.Assignee)
	}
	if err := c.Release("gt-nosuch"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Release(missing) = %v, want ErrNotFound", err)
	}
}
