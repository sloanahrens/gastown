package beadsfake

import (
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// RunClientContract checks the behavior every beads.Client must share: the
// fake in the unit tier, *beads.Beads against bd and a Dolt test database in
// the integration tier. Every behavior the fake documents is pinned here,
// the refusals and missing-issue paths included.
//
// newClient may return the same database to every case: cases run in
// parallel and each asserts only on the issues it created, under its own
// assignee names and labels (see scope). The integration tier shares one
// database per run, so a run costs one bd init, not one per case.
func RunClientContract(t *testing.T, newClient func(t *testing.T) beads.Client) {
	t.Helper()
	for _, c := range contractCases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.run(t, newScope(t, newClient(t)))
		})
	}
}

type contractCase struct {
	name string
	run  func(t *testing.T, s *scope)
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
	{"list order", contractListOrder},
	{"assignee queries", contractAssigneeQueries},
	{"comments", contractComments},
	{"dependencies and ready", contractDependencies},
	{"ready filter", contractReadyFilter},
	{"children", contractChildren},
	{"release", contractRelease},
}

// scope is one case's view of a possibly shared database: the issues it
// created, and names (assignees, labels) no other case uses.
type scope struct {
	beads.Client
	tag     string
	created map[string]bool
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func newScope(t *testing.T, c beads.Client) *scope {
	name := t.Name()
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return &scope{Client: c, tag: nonAlnum.ReplaceAllString(strings.ToLower(name), ""), created: map[string]bool{}}
}

// who is an assignee name unique to the case.
func (s *scope) who(name string) string { return name + "-" + s.tag }

// label is a label unique to the case.
func (s *scope) label(name string) string { return "ct-" + s.tag + "-" + name }

// mine keeps the issues this case created.
func (s *scope) mine(issues []*beads.Issue) []*beads.Issue {
	var out []*beads.Issue
	for _, is := range issues {
		if s.created[is.ID] {
			out = append(out, is)
		}
	}
	return out
}

// mustCreate creates an issue or fails the test.
func (s *scope) mustCreate(t *testing.T, opts beads.CreateOptions) *beads.Issue {
	t.Helper()
	is, err := s.Create(opts)
	if err != nil {
		t.Fatalf("Create(%q): %v", opts.Title, err)
	}
	if is == nil || is.ID == "" {
		t.Fatalf("Create(%q) returned no ID: %+v", opts.Title, is)
	}
	s.created[is.ID] = true
	return is
}

func (s *scope) mustShow(t *testing.T, id string) *beads.Issue {
	t.Helper()
	is, err := s.Show(id)
	if err != nil {
		t.Fatalf("Show(%s): %v", id, err)
	}
	return is
}

// want checks that, of the issues this case created, got holds exactly want.
func (s *scope) want(t *testing.T, what string, got []*beads.Issue, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if g, w := ids(s.mine(got)), sorted(want...); !reflect.DeepEqual(g, w) {
		t.Errorf("%s = %v (of this case's issues), want %v", what, g, w)
	}
}

func mustDo(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// refused checks that err is a refusal: an error, and not a missing issue.
func refused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Errorf("%s succeeded, want it refused", what)
	} else if errors.Is(err, beads.ErrNotFound) {
		t.Errorf("%s refusal wraps ErrNotFound: %v", what, err)
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

func labels(is *beads.Issue) []string {
	return sorted(is.Labels...)
}

func contractCreateShow(t *testing.T, s *scope) {
	desc := "first line\nsecond line"
	created := s.mustCreate(t, beads.CreateOptions{
		Title: "alpha", Description: desc, Labels: []string{"x", "gt:task"}, Priority: 1,
	})
	got := s.mustShow(t, created.ID)
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

	dflt := s.mustCreate(t, beads.CreateOptions{Title: "default priority", Priority: -1})
	if p := s.mustShow(t, dflt.ID).Priority; p != 2 {
		t.Errorf("priority -1 created priority %d, want bd's default 2", p)
	}
	actor := s.mustCreate(t, beads.CreateOptions{Title: "by someone", Priority: -1, Actor: "contract-actor"})
	if by := s.mustShow(t, actor.ID).CreatedBy; by != "contract-actor" {
		t.Errorf("CreatedBy = %q, want contract-actor", by)
	}
	wisp := s.mustCreate(t, beads.CreateOptions{Title: "a wisp", Priority: -1, Ephemeral: true})
	if !s.mustShow(t, wisp.ID).Ephemeral {
		t.Error("ephemeral create is not ephemeral")
	}
}

func contractMissing(t *testing.T, s *scope) {
	const id = "gt-nosuch"
	if _, err := s.Show(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Show(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Update(id, beads.UpdateOptions{Status: ptr("in_progress")}); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Update(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Close(id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Close(missing) = %v, want ErrNotFound", err)
	}
	if err := s.AddComment(id, "x"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("AddComment(missing) = %v, want ErrNotFound", err)
	}
	if _, err := s.Comments(id); err == nil {
		t.Error("Comments(missing) succeeded")
	}
	real := s.mustCreate(t, beads.CreateOptions{Title: "exists", Priority: -1})
	if err := s.AddDependency(real.ID, id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("AddDependency(on missing) = %v, want ErrNotFound", err)
	}
	// A batch close naming a missing issue fails as not found and closes
	// nothing.
	if err := s.Close(real.ID, id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Close(real, missing) = %v, want ErrNotFound", err)
	}
	if st := s.mustShow(t, real.ID).Status; st != "open" {
		t.Errorf("Close(real, missing) left %s %q, want it untouched", real.ID, st)
	}
}

func contractFlagTitle(t *testing.T, s *scope) {
	if _, err := s.Create(beads.CreateOptions{Title: "--help", Priority: -1}); !errors.Is(err, beads.ErrFlagTitle) {
		t.Errorf("Create(--help) = %v, want ErrFlagTitle", err)
	}
}

func contractShowMultiple(t *testing.T, s *scope) {
	a := s.mustCreate(t, beads.CreateOptions{Title: "a", Priority: -1})
	b := s.mustCreate(t, beads.CreateOptions{Title: "b", Priority: -1})
	got, err := s.ShowMultiple([]string{a.ID, "gt-nosuch", b.ID})
	if err != nil {
		t.Fatalf("ShowMultiple: %v", err)
	}
	if len(got) != 2 || got[a.ID] == nil || got[b.ID] == nil || got[a.ID].Title != "a" {
		t.Errorf("ShowMultiple = %v, want exactly %s and %s", got, a.ID, b.ID)
	}
	empty, err := s.ShowMultiple(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("ShowMultiple(nil) = %v, %v", empty, err)
	}
}

func contractUpdateFields(t *testing.T, s *scope) {
	is := s.mustCreate(t, beads.CreateOptions{Title: "before", Description: "old", Priority: 3})
	desc := "new body\nwith two lines"
	mustDo(t, "Update", s.Update(is.ID, beads.UpdateOptions{
		Title: ptr("after"), Priority: ptr(0), Description: &desc,
		Assignee: ptr(s.who("alice")), Status: ptr("in_progress"),
	}))
	got := s.mustShow(t, is.ID)
	if got.Title != "after" || got.Priority != 0 || got.Description != desc || got.Assignee != s.who("alice") || got.Status != "in_progress" {
		t.Errorf("after Update: title %q priority %d description %q assignee %q status %q",
			got.Title, got.Priority, got.Description, got.Assignee, got.Status)
	}
	mustDo(t, "clear description", s.Update(is.ID, beads.UpdateOptions{Description: ptr("")}))
	if d := s.mustShow(t, is.ID).Description; d != "" {
		t.Errorf("description after clearing = %q", d)
	}
	mustDo(t, "hooked", s.Update(is.ID, beads.UpdateOptions{Status: ptr(beads.StatusHooked)}))
	if st := s.mustShow(t, is.ID).Status; st != beads.StatusHooked {
		t.Errorf("status = %q, want hooked", st)
	}
	// A hooked issue is not an in_progress claim: reassigning it is allowed.
	mustDo(t, "reassign hooked", s.Update(is.ID, beads.UpdateOptions{Assignee: ptr(s.who("bob"))}))
	if a := s.mustShow(t, is.ID).Assignee; a != s.who("bob") {
		t.Errorf("hooked reassign left assignee %q", a)
	}
}

func contractLabels(t *testing.T, s *scope) {
	is := s.mustCreate(t, beads.CreateOptions{Title: "labels", Labels: []string{"a", "b"}, Priority: -1})
	mustDo(t, "add/remove", s.Update(is.ID, beads.UpdateOptions{AddLabels: []string{"c"}, RemoveLabels: []string{"a"}}))
	if got := labels(s.mustShow(t, is.ID)); !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Errorf("after add c, remove a: %v", got)
	}
	mustDo(t, "set", s.Update(is.ID, beads.UpdateOptions{SetLabels: []string{"z", "y"}}))
	if got := labels(s.mustShow(t, is.ID)); !reflect.DeepEqual(got, []string{"y", "z"}) {
		t.Errorf("after set y,z: %v", got)
	}
	// An empty SetLabels is no label change at all, not "clear the labels".
	mustDo(t, "set empty", s.Update(is.ID, beads.UpdateOptions{SetLabels: []string{}, Title: ptr("labels 2")}))
	if got := labels(s.mustShow(t, is.ID)); !reflect.DeepEqual(got, []string{"y", "z"}) {
		t.Errorf("after an empty SetLabels: %v, want y,z kept", got)
	}
	single := s.mustCreate(t, beads.CreateOptions{Title: "single", Label: "gt:one", Priority: -1})
	if got := labels(s.mustShow(t, single.ID)); !reflect.DeepEqual(got, []string{"gt:one"}) {
		t.Errorf("Label create: %v", got)
	}
	typed := s.mustCreate(t, beads.CreateOptions{Title: "typed", Type: "bug", Priority: -1})
	got := s.mustShow(t, typed.ID)
	if !reflect.DeepEqual(labels(got), []string{"gt:bug"}) || got.Type != "task" {
		t.Errorf("Type create: labels %v type %q, want gt:bug on a task", got.Labels, got.Type)
	}
}

func contractClaimFence(t *testing.T, s *scope) {
	alice, bob := s.who("alice"), s.who("bob")
	is := s.mustCreate(t, beads.CreateOptions{Title: "claimed", Priority: -1})
	mustDo(t, "claim", s.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(alice)}))
	refused(t, "reassigning alice's in_progress claim to bob", s.Update(is.ID, beads.UpdateOptions{Assignee: ptr(bob)}))
	refused(t, "releasing alice's claim by update (status open, no assignee)",
		s.Update(is.ID, beads.UpdateOptions{Status: ptr("open"), Assignee: ptr("")}))
	if got := s.mustShow(t, is.ID); got.Assignee != alice || got.Status != "in_progress" {
		t.Errorf("refused updates changed the claim to %q/%q", got.Assignee, got.Status)
	}
	mustDo(t, "same holder", s.Update(is.ID, beads.UpdateOptions{Assignee: ptr(alice), Priority: ptr(1)}))
	mustDo(t, "forced reassign", s.Update(is.ID, beads.UpdateOptions{Assignee: ptr(bob), Force: true}))
	if a := s.mustShow(t, is.ID).Assignee; a != bob {
		t.Errorf("forced reassign left assignee %q", a)
	}
	open := s.mustCreate(t, beads.CreateOptions{Title: "open, assigned", Priority: -1})
	mustDo(t, "assign open", s.Update(open.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	mustDo(t, "reassign open", s.Update(open.ID, beads.UpdateOptions{Assignee: ptr(bob)}))
}

func contractCloseReopen(t *testing.T, s *scope) {
	a := s.mustCreate(t, beads.CreateOptions{Title: "a", Priority: -1})
	b := s.mustCreate(t, beads.CreateOptions{Title: "b", Priority: -1})
	mustDo(t, "Close", s.Close(a.ID))
	got := s.mustShow(t, a.ID)
	if got.Status != "closed" || got.CloseReason != "Closed" || got.ClosedAt == "" {
		t.Errorf("Close: status %q reason %q closed_at %q, want closed \"Closed\" set", got.Status, got.CloseReason, got.ClosedAt)
	}
	mustDo(t, "Close an already closed issue", s.Close(a.ID))
	mustDo(t, "CloseWithReason", s.CloseWithReason("finished", b.ID))
	if r := s.mustShow(t, b.ID).CloseReason; r != "finished" {
		t.Errorf("CloseWithReason reason = %q", r)
	}
	mustDo(t, "reopen", s.Update(a.ID, beads.UpdateOptions{Status: ptr("open")}))
	got = s.mustShow(t, a.ID)
	if got.Status != "open" || got.ClosedAt != "" || got.CloseReason != "" {
		t.Errorf("reopen: status %q closed_at %q reason %q", got.Status, got.ClosedAt, got.CloseReason)
	}
	// Closing by update records no reason.
	u := s.mustCreate(t, beads.CreateOptions{Title: "closed by update", Priority: -1})
	mustDo(t, "Update(status=closed)", s.Update(u.ID, beads.UpdateOptions{Status: ptr("closed")}))
	if got := s.mustShow(t, u.ID); got.Status != "closed" || got.CloseReason != "" {
		t.Errorf("Update(status=closed): status %q reason %q, want closed with no reason", got.Status, got.CloseReason)
	}
	x := s.mustCreate(t, beads.CreateOptions{Title: "x", Priority: -1})
	y := s.mustCreate(t, beads.CreateOptions{Title: "y", Priority: -1})
	mustDo(t, "close two", s.CloseWithReason("batch", x.ID, y.ID))
	for _, id := range []string{x.ID, y.ID} {
		if st := s.mustShow(t, id).Status; st != "closed" {
			t.Errorf("%s status %q after batch close", id, st)
		}
	}
	mustDo(t, "Close()", s.Close())
}

func contractCloseFence(t *testing.T, s *scope) {
	parent := s.mustCreate(t, beads.CreateOptions{Title: "parent", Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "child", Parent: parent.ID, Priority: -1})
	refused(t, "closing a parent with an open child", s.CloseWithReason("r", parent.ID))
	refused(t, "Update(status=closed) of a parent with an open child", s.Update(parent.ID, beads.UpdateOptions{Status: ptr("closed")}))
	if st := s.mustShow(t, parent.ID).Status; st != "open" {
		t.Errorf("refused close left status %q", st)
	}
	mustDo(t, "ForceCloseWithReason", s.ForceCloseWithReason("forced", parent.ID))
	if got := s.mustShow(t, parent.ID); got.Status != "closed" || got.CloseReason != "forced" {
		t.Errorf("forced close: %q %q", got.Status, got.CloseReason)
	}
	other := s.mustCreate(t, beads.CreateOptions{Title: "other parent", Priority: -1})
	kid := s.mustCreate(t, beads.CreateOptions{Title: "closed kid", Parent: other.ID, Priority: -1})
	mustDo(t, "close kid", s.Close(kid.ID))
	mustDo(t, "close parent of closed child", s.Close(other.ID))

	// bd refuses to close an issue an open issue blocks, by close or by
	// update.
	blocker := s.mustCreate(t, beads.CreateOptions{Title: "blocker", Priority: -1})
	blocked := s.mustCreate(t, beads.CreateOptions{Title: "blocked", Priority: -1})
	mustDo(t, "AddDependency", s.AddDependency(blocked.ID, blocker.ID))
	refused(t, "closing an issue with an open blocker", s.Close(blocked.ID))
	refused(t, "Update(status=closed) of an issue with an open blocker", s.Update(blocked.ID, beads.UpdateOptions{Status: ptr("closed")}))
	mustDo(t, "force close blocked", s.ForceCloseWithReason("forced", blocked.ID))
	mustDo(t, "close the blocker itself", s.Close(blocker.ID))

	// bd refuses to close an issue assigned to someone other than the
	// actor. No case's assignee is ever the actor.
	theirs := s.mustCreate(t, beads.CreateOptions{Title: "alice's", Priority: -1})
	mustDo(t, "assign", s.Update(theirs.ID, beads.UpdateOptions{Assignee: ptr(s.who("alice"))}))
	refused(t, "closing an issue assigned to another actor", s.Close(theirs.ID))
	if st := s.mustShow(t, theirs.ID).Status; st != "open" {
		t.Errorf("refused close left status %q", st)
	}
	mustDo(t, "ForceCloseWithReason theirs", s.ForceCloseWithReason("done", theirs.ID))
	if st := s.mustShow(t, theirs.ID).Status; st != "closed" {
		t.Errorf("forced close of alice's issue left status %q", st)
	}
	// The assignee fence is close's alone: an update to status closed
	// passes it.
	also := s.mustCreate(t, beads.CreateOptions{Title: "alice's too", Priority: -1})
	mustDo(t, "assign also", s.Update(also.ID, beads.UpdateOptions{Assignee: ptr(s.who("alice"))}))
	mustDo(t, "Update(status=closed) of alice's issue", s.Update(also.ID, beads.UpdateOptions{Status: ptr("closed")}))
	if st := s.mustShow(t, also.ID).Status; st != "closed" {
		t.Errorf("Update(status=closed) left status %q", st)
	}
}

func contractListFilters(t *testing.T, s *scope) {
	l := s.label("L")
	a := s.mustCreate(t, beads.CreateOptions{Title: "a", Labels: []string{"gt:task", l}, Priority: 1})
	mustDo(t, "assign a", s.Update(a.ID, beads.UpdateOptions{Assignee: ptr(s.who("alice"))}))
	b := s.mustCreate(t, beads.CreateOptions{Title: "b", Labels: []string{"gt:bug"}, Priority: 3})
	cl := s.mustCreate(t, beads.CreateOptions{Title: "closed", Priority: 3})
	mustDo(t, "close", s.Close(cl.ID))
	w := s.mustCreate(t, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})

	got, err := s.List(beads.ListOptions{Priority: -1})
	s.want(t, "List{}", got, err, a.ID, b.ID)
	got, err = s.List(beads.ListOptions{Status: "all", Priority: -1})
	s.want(t, "List{Status:all}", got, err, a.ID, b.ID, cl.ID)
	got, err = s.List(beads.ListOptions{Status: "closed", Priority: -1})
	s.want(t, "List{Status:closed}", got, err, cl.ID)
	got, err = s.List(beads.ListOptions{Status: "open", Priority: -1})
	s.want(t, "List{Status:open}", got, err, a.ID, b.ID)
	got, err = s.List(beads.ListOptions{Label: l, Priority: -1})
	s.want(t, "List{Label}", got, err, a.ID)
	got, err = s.List(beads.ListOptions{Type: "bug", Priority: -1})
	s.want(t, "List{Type:bug}", got, err, b.ID)
	got, err = s.List(beads.ListOptions{Assignee: s.who("alice"), Priority: -1})
	s.want(t, "List{Assignee}", got, err, a.ID)
	got, err = s.List(beads.ListOptions{NoAssignee: true, Priority: -1})
	s.want(t, "List{NoAssignee}", got, err, b.ID)
	got, err = s.List(beads.ListOptions{Priority: 3, Status: "all"})
	s.want(t, "List{Priority:3,all}", got, err, b.ID, cl.ID)
	got, err = s.List(beads.ListOptions{Ephemeral: true, Priority: -1})
	s.want(t, "List{Ephemeral}", got, err, w.ID)
	parent := s.mustCreate(t, beads.CreateOptions{Title: "p", Priority: -1})
	kid := s.mustCreate(t, beads.CreateOptions{Title: "kid", Parent: parent.ID, Priority: -1})
	got, err = s.List(beads.ListOptions{Parent: parent.ID, Priority: -1})
	s.want(t, "List{Parent}", got, err, kid.ID)
}

// contractListOrder pins List's order: priority first, 0 highest. (Within a
// priority bd lists newest first, but two creates can land in the same
// second, so that is not pinned.)
func contractListOrder(t *testing.T, s *scope) {
	l := s.label("ord")
	low := s.mustCreate(t, beads.CreateOptions{Title: "low", Labels: []string{l}, Priority: 3})
	high := s.mustCreate(t, beads.CreateOptions{Title: "high", Labels: []string{l}, Priority: 1})
	mid := s.mustCreate(t, beads.CreateOptions{Title: "mid", Labels: []string{l}, Priority: 2})
	got, err := s.List(beads.ListOptions{Label: l, Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, is := range got {
		order = append(order, is.ID)
	}
	if want := []string{high.ID, mid.ID, low.ID}; !reflect.DeepEqual(order, want) {
		t.Errorf("List order = %v, want %v (by priority)", order, want)
	}
	one, err := s.List(beads.ListOptions{Label: l, Limit: 1, Priority: -1})
	if err != nil || len(one) != 1 || one[0].ID != high.ID {
		t.Errorf("List{Limit:1} = %v, %v; want only %s", ids(one), err, high.ID)
	}
}

func contractAssigneeQueries(t *testing.T, s *scope) {
	alice := s.who("alice")
	if is, err := s.GetAssignedIssue(alice); err != nil || is != nil {
		t.Errorf("GetAssignedIssue(nobody's) = %v, %v; want nil, nil", is, err)
	}
	hooked := s.mustCreate(t, beads.CreateOptions{Title: "hooked", Priority: -1})
	mustDo(t, "hook", s.Update(hooked.ID, beads.UpdateOptions{Status: ptr(beads.StatusHooked), Assignee: ptr(alice)}))
	if is, err := s.GetAssignedIssue(alice); err != nil || is == nil || is.ID != hooked.ID {
		t.Fatalf("GetAssignedIssue with only a hooked issue = %v, %v", is, err)
	}
	working := s.mustCreate(t, beads.CreateOptions{Title: "working", Priority: -1})
	mustDo(t, "claim", s.Update(working.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(alice)}))
	if is, err := s.GetAssignedIssue(alice); err != nil || is == nil || is.ID != working.ID {
		t.Errorf("GetAssignedIssue prefers in_progress over hooked: got %v, %v", is, err)
	}
	open := s.mustCreate(t, beads.CreateOptions{Title: "open", Priority: -1})
	mustDo(t, "assign", s.Update(open.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	if is, err := s.GetAssignedIssue(alice); err != nil || is == nil || is.ID != open.ID {
		t.Errorf("GetAssignedIssue prefers open: got %v, %v", is, err)
	}
	done := s.mustCreate(t, beads.CreateOptions{Title: "done", Priority: -1})
	mustDo(t, "assign done", s.Update(done.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	mustDo(t, "close done", s.ForceCloseWithReason("done", done.ID))
	wisp := s.mustCreate(t, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	mustDo(t, "assign wisp", s.Update(wisp.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	bobs := s.mustCreate(t, beads.CreateOptions{Title: "bob's", Priority: -1})
	mustDo(t, "assign bob", s.Update(bobs.ID, beads.UpdateOptions{Assignee: ptr(s.who("bob"))}))

	got, err := s.ListByAssignee(alice)
	s.want(t, "ListByAssignee(alice)", got, err, hooked.ID, working.ID, open.ID, done.ID)
	if len(got) != 4 {
		t.Errorf("ListByAssignee(alice) returned %d issues, want only alice's 4", len(got))
	}
	got, err = s.ListIssueStatuses(beads.StatusOpen, beads.StatusInProgress)
	s.want(t, "ListIssueStatuses(open,in_progress)", got, err, working.ID, open.ID, bobs.ID)
	got, err = s.ListIssueStatuses()
	if err != nil || len(got) != 0 {
		t.Errorf("ListIssueStatuses() = %v, %v; want nothing", ids(got), err)
	}
	got, err = s.ListAssignedIssueStatuses(alice, beads.StatusOpen, beads.StatusInProgress)
	s.want(t, "ListAssignedIssueStatuses(alice, open, in_progress)", got, err, working.ID, open.ID, wisp.ID)
	got, err = s.ListAssignedIssueStatuses("", beads.StatusOpen)
	if err != nil || len(got) != 0 {
		t.Errorf("ListAssignedIssueStatuses(\"\") = %v, %v; want nothing", ids(got), err)
	}
}

func contractComments(t *testing.T, s *scope) {
	is := s.mustCreate(t, beads.CreateOptions{Title: "talk", Priority: -1})
	if got, err := s.Comments(is.ID); err != nil || len(got) != 0 {
		t.Errorf("Comments(new) = %v, %v", got, err)
	}
	mustDo(t, "first", s.AddComment(is.ID, "first"))
	mustDo(t, "second", s.AddComment(is.ID, "second one"))
	got, err := s.Comments(is.ID)
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

// depOn returns is's dependency on id of relation typ, or nil.
func depOn(is *beads.Issue, id, typ string) *beads.IssueDep {
	for i, d := range is.Dependencies {
		if d.ID == id && d.DependencyType == typ {
			return &is.Dependencies[i]
		}
	}
	return nil
}

func contractDependencies(t *testing.T, s *scope) {
	blocker := s.mustCreate(t, beads.CreateOptions{Title: "blocker", Priority: -1})
	blocked := s.mustCreate(t, beads.CreateOptions{Title: "blocked", Priority: -1})
	mustDo(t, "AddDependency", s.AddDependency(blocked.ID, blocker.ID))
	mustDo(t, "AddDependency again", s.AddDependency(blocked.ID, blocker.ID))
	got := s.mustShow(t, blocked.ID)
	dep := depOn(got, blocker.ID, "blocks")
	if dep == nil || len(got.Dependencies) != 1 {
		t.Fatalf("Show(blocked).Dependencies = %+v, want one blocks on %s", got.Dependencies, blocker.ID)
	}
	if dep.Title != "blocker" || dep.Status != "open" {
		t.Errorf("dependency = title %q status %q, want the blocker's", dep.Title, dep.Status)
	}
	ready, err := s.Ready()
	s.want(t, "Ready while blocked", ready, err, blocker.ID)
	// A blocker in progress still blocks.
	mustDo(t, "start blocker", s.Update(blocker.ID, beads.UpdateOptions{Status: ptr("in_progress")}))
	ready, err = s.Ready()
	s.want(t, "Ready while the blocker is in progress", ready, err)
	mustDo(t, "close blocker", s.Close(blocker.ID))
	ready, err = s.Ready()
	s.want(t, "Ready after blocker closed", ready, err, blocked.ID)
	if dep := depOn(s.mustShow(t, blocked.ID), blocker.ID, "blocks"); dep == nil || dep.Status != "closed" {
		t.Errorf("dependency after the blocker closed = %+v, want status closed", dep)
	}
	mustDo(t, "reopen blocker", s.Update(blocker.ID, beads.UpdateOptions{Status: ptr("open")}))
	mustDo(t, "RemoveDependency", s.RemoveDependency(blocked.ID, blocker.ID))
	if got := s.mustShow(t, blocked.ID); len(got.Dependencies) != 0 {
		t.Errorf("Dependencies after remove = %+v", got.Dependencies)
	}
	ready, err = s.Ready()
	s.want(t, "Ready after RemoveDependency", ready, err, blocker.ID, blocked.ID)
}

func contractReadyFilter(t *testing.T, s *scope) {
	work := s.mustCreate(t, beads.CreateOptions{Title: "work", Labels: []string{"gt:task"}, Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "mail", Labels: []string{"gt:message"}, Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "agent", Labels: []string{"gt:agent"}, Priority: -1})
	busy := s.mustCreate(t, beads.CreateOptions{Title: "busy", Priority: -1})
	mustDo(t, "claim", s.Update(busy.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(s.who("alice"))}))
	done := s.mustCreate(t, beads.CreateOptions{Title: "done", Priority: -1})
	mustDo(t, "close", s.Close(done.ID))
	s.mustCreate(t, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	parent := s.mustCreate(t, beads.CreateOptions{Title: "parent", Priority: -1})
	kid := s.mustCreate(t, beads.CreateOptions{Title: "kid", Parent: parent.ID, Priority: -1})
	ready, err := s.Ready()
	s.want(t, "Ready", ready, err, work.ID, parent.ID, kid.ID)
}

func contractChildren(t *testing.T, s *scope) {
	parent := s.mustCreate(t, beads.CreateOptions{Title: "parent", Priority: -1})
	a := s.mustCreate(t, beads.CreateOptions{Title: "child a", Parent: parent.ID, Priority: -1})
	b := s.mustCreate(t, beads.CreateOptions{Title: "child b", Parent: parent.ID, Priority: -1})
	w := s.mustCreate(t, beads.CreateOptions{Title: "wisp child", Parent: parent.ID, Priority: -1, Ephemeral: true})
	s.mustCreate(t, beads.CreateOptions{Title: "unrelated", Priority: -1})
	for _, kid := range []*beads.Issue{a, w} {
		if !strings.HasPrefix(kid.ID, parent.ID+".") {
			t.Errorf("child ID %q is not under %q", kid.ID, parent.ID)
		}
	}
	got := s.mustShow(t, a.ID)
	if got.Parent != parent.ID || depOn(got, parent.ID, "parent-child") == nil {
		t.Errorf("Show(child): parent %q deps %+v", got.Parent, got.Dependencies)
	}
	if !s.mustShow(t, w.ID).Ephemeral {
		t.Error("ephemeral child is not ephemeral")
	}
	mustDo(t, "close b", s.Close(b.ID))
	kids, err := s.Children(parent.ID)
	s.want(t, "Children", kids, err, a.ID, b.ID, w.ID)
	none, err := s.Children(a.ID)
	s.want(t, "Children(leaf)", none, err)

	// RemoveDependency removes a parent-child link too.
	mustDo(t, "RemoveDependency(child, parent)", s.RemoveDependency(a.ID, parent.ID))
	if got := s.mustShow(t, a.ID); got.Parent != "" || len(got.Dependencies) != 0 {
		t.Errorf("after unlinking: parent %q deps %+v", got.Parent, got.Dependencies)
	}
	kids, err = s.Children(parent.ID)
	s.want(t, "Children after unlinking", kids, err, b.ID, w.ID)
}

func contractRelease(t *testing.T, s *scope) {
	is := s.mustCreate(t, beads.CreateOptions{Title: "stuck", Priority: -1})
	mustDo(t, "claim", s.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(s.who("dead"))}))
	mustDo(t, "ReleaseWithReason", s.ReleaseWithReason(is.ID, "worker died"))
	got := s.mustShow(t, is.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Errorf("after release: status %q assignee %q, want open and none", got.Status, got.Assignee)
	}
	if !strings.Contains(got.Notes, "worker died") {
		t.Errorf("notes = %q, want the release reason", got.Notes)
	}
	other := s.mustCreate(t, beads.CreateOptions{Title: "stuck 2", Priority: -1})
	mustDo(t, "claim 2", s.Update(other.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(s.who("dead"))}))
	mustDo(t, "Release", s.Release(other.ID))
	if got := s.mustShow(t, other.ID); got.Status != "open" || got.Assignee != "" {
		t.Errorf("after Release: status %q assignee %q", got.Status, got.Assignee)
	}
	if err := s.Release("gt-nosuch"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Release(missing) = %v, want ErrNotFound", err)
	}
}
