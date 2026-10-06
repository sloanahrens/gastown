package beadsfake

import (
	"errors"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

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

// RunActorContract checks that a client made to act as an actor (the fake's
// WithActor, *beads.Beads' ActingAs) records that actor as the creator of
// issues and the author of comments, whatever the process environment says
// (gt-0wkug). newClient returns a client acting as actor.
func RunActorContract(t *testing.T, newClient func(t *testing.T, actor string) beads.Client) {
	t.Helper()
	t.Run("actor", func(t *testing.T) {
		t.Parallel()
		actor := "contract-actor"
		s := newScope(t, newClient(t, actor))
		is := s.mustCreate(t, beads.CreateOptions{Title: "acted", Priority: -1})
		if by := s.mustShow(t, is.ID).CreatedBy; by != actor {
			t.Errorf("CreatedBy = %q, want %s", by, actor)
		}
		mustDo(t, "comment", s.AddComment(is.ID, "hello"))
		got, err := s.Comments(is.ID)
		if err != nil || len(got) != 1 || got[0].Author != actor {
			t.Errorf("Comments = %+v, %v; want one by %s", got, err, actor)
		}
	})
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
	{"events", contractEvents},
	{"assignee queries", contractAssigneeQueries},
	{"comments", contractComments},
	{"dependencies and ready", contractDependencies},
	{"typed dependencies", contractTypedDependencies},
	{"ready filter", contractReadyFilter},
	{"children", contractChildren},
	{"delete issues", contractDeleteIssues},
	{"release", contractRelease},
	{"batch close refusal", contractBatchCloseRefusal},
	{"batch close of a blocks chain in reverse order", contractBatchCloseChain},
	{"append notes", contractAppendNotes},
	{"guarded transfer", contractGuardedTransfer},
	{"guard and note edges", contractGuardEdges},
	{"merge request blockers", contractMergeRequestBlockers},
	{"merge requests", contractMergeRequests},
	{"agent active_mr", contractAgentActiveMR},
	{"merge slot", contractMergeSlot},
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
	assigned := s.mustCreate(t, beads.CreateOptions{Title: "assigned", Priority: -1, Assignee: s.who("holder")})
	if who := s.mustShow(t, assigned.ID).Assignee; who != s.who("holder") {
		t.Errorf("Assignee = %q, want %q", who, s.who("holder"))
	}
	// A database can outlive a run, so the chosen ID is unique to this one.
	id := "gt-" + s.tag + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	named := s.mustCreate(t, beads.CreateOptions{ID: id, Title: "named", Priority: -1})
	if named.ID != id || s.mustShow(t, id).Title != "named" {
		t.Errorf("create with ID %s made %q", id, named.ID)
	}
	wisp := s.mustCreate(t, beads.CreateOptions{Title: "a wisp", Priority: -1, Ephemeral: true})
	if !s.mustShow(t, wisp.ID).Ephemeral {
		t.Error("ephemeral create is not ephemeral")
	}
	criteria := "- [ ] create sets the field\n- [ ] the block keeps its newlines"
	criteriaBead := s.mustCreate(t, beads.CreateOptions{Title: "with criteria", Priority: -1, Acceptance: criteria})
	if got := s.mustShow(t, criteriaBead.ID).AcceptanceCriteria; got != criteria {
		t.Errorf("acceptance criteria at create = %q, want %q", got, criteria)
	}
	plain := s.mustCreate(t, beads.CreateOptions{Title: "no criteria", Priority: -1})
	if got := s.mustShow(t, plain.ID).AcceptanceCriteria; got != "" {
		t.Errorf("a create without --acceptance left criteria %q", got)
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
	// A batch close naming a missing issue is not found for it, and closes
	// the ids that resolve: one unresolvable argument must not discard the
	// rest of the batch (bd since be-sut).
	if err := s.Close(real.ID, id); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Close(real, missing) = %v, want ErrNotFound", err)
	}
	if st := s.mustShow(t, real.ID).Status; st != "closed" {
		t.Errorf("Close(real, missing) left %s %q, want it closed", real.ID, st)
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
	criteria := "- [x] the refusal names the verb\n- [ ] make lint && make gate pass"
	mustDo(t, "acceptance", s.Update(is.ID, beads.UpdateOptions{Acceptance: &criteria}))
	if got := s.mustShow(t, is.ID).AcceptanceCriteria; got != criteria {
		t.Errorf("acceptance criteria = %q, want %q", got, criteria)
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

	// Persistent promotes a wisp: after it the issue is a durable one.
	wisp := s.mustCreate(t, beads.CreateOptions{Title: "wisp", Priority: -1, Ephemeral: true})
	if !s.mustShow(t, wisp.ID).Ephemeral {
		t.Fatalf("%s was not created as a wisp", wisp.ID)
	}
	mustDo(t, "Update Persistent", s.Update(wisp.ID, beads.UpdateOptions{Persistent: true}))
	if s.mustShow(t, wisp.ID).Ephemeral {
		t.Error("a persistent update left the issue a wisp")
	}
	durable, err := s.List(beads.ListOptions{Priority: -1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !holds(s.mine(durable), wisp.ID) {
		t.Errorf("List without Ephemeral after Persistent does not hold %s", wisp.ID)
	}
	wisps, err := s.List(beads.ListOptions{Priority: -1, Ephemeral: true})
	if err != nil {
		t.Fatalf("List{Ephemeral}: %v", err)
	}
	if holds(s.mine(wisps), wisp.ID) {
		t.Errorf("List{Ephemeral} after Persistent still holds %s", wisp.ID)
	}
}

// holds reports whether issues include id.
func holds(issues []*beads.Issue, id string) bool {
	for _, is := range issues {
		if is.ID == id {
			return true
		}
	}
	return false
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

	// The pin is a close fence too, and like the assignee fence it is
	// close's alone: an update to status closed passes it, a release or a
	// forced close clears it.
	pinned := s.mustCreate(t, beads.CreateOptions{Title: "pinned", Priority: -1})
	mustDo(t, "pin", s.Update(pinned.ID, beads.UpdateOptions{Status: ptr("pinned")}))
	refused(t, "closing a pinned issue", s.Close(pinned.ID))
	if st := s.mustShow(t, pinned.ID).Status; st != "pinned" {
		t.Errorf("refused close left status %q", st)
	}
	mustDo(t, "Release", s.Release(pinned.ID))
	mustDo(t, "close released", s.Close(pinned.ID))
	still := s.mustCreate(t, beads.CreateOptions{Title: "pinned again", Priority: -1})
	mustDo(t, "pin again", s.Update(still.ID, beads.UpdateOptions{Status: ptr("pinned")}))
	mustDo(t, "Update(status=closed) of a pinned issue", s.Update(still.ID, beads.UpdateOptions{Status: ptr("closed")}))
	last := s.mustCreate(t, beads.CreateOptions{Title: "pinned last", Priority: -1})
	mustDo(t, "pin last", s.Update(last.ID, beads.UpdateOptions{Status: ptr("pinned")}))
	mustDo(t, "ForceCloseWithReason of a pinned issue", s.ForceCloseWithReason("forced", last.ID))
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
	got, err = s.List(beads.ListOptions{Label: l, Priority: -1, IncludeInfra: true})
	s.want(t, "List{Label,IncludeInfra}", got, err, a.ID)
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
	got, err = s.List(beads.ListOptions{Status: "open,closed", Priority: 3})
	s.want(t, "List{Status:open,closed}", got, err, b.ID, cl.ID)
	closedAt, err := time.Parse(time.RFC3339, s.mustShow(t, cl.ID).ClosedAt)
	if err != nil {
		t.Fatalf("closed_at of %s: %v", cl.ID, err)
	}
	got, err = s.List(beads.ListOptions{Status: "all", Priority: 3, ClosedAfter: closedAt.Add(-time.Hour)})
	s.want(t, "List{ClosedAfter:before close}", got, err, cl.ID)
	got, err = s.List(beads.ListOptions{Status: "all", Priority: 3, ClosedAfter: closedAt.Add(time.Hour)})
	s.want(t, "List{ClosedAfter:after close}", got, err)

	// Labels is an AND over every one of them, on its own or alongside the
	// single Label.
	m := s.label("M")
	both := s.mustCreate(t, beads.CreateOptions{Title: "both", Labels: []string{l, m}, Priority: -1})
	got, err = s.List(beads.ListOptions{Labels: []string{l, m}, Priority: -1})
	s.want(t, "List{Labels}", got, err, both.ID)
	got, err = s.List(beads.ListOptions{Label: l, Labels: []string{m}, Priority: -1})
	s.want(t, "List{Label,Labels}", got, err, both.ID)
	got, err = s.List(beads.ListOptions{Labels: []string{l, s.label("none")}, Priority: -1})
	s.want(t, "List{Labels:one absent}", got, err)

	// CreatedAfter keeps only what was created at or after the bound. (Two
	// creates can land in the same second, so, as for ClosedAfter, the bound
	// sits an hour off the issue's own created_at rather than on it.)
	ca := s.mustCreate(t, beads.CreateOptions{Title: "created first", Priority: 4})
	cb := s.mustCreate(t, beads.CreateOptions{Title: "created second", Priority: 4})
	createdAt, err := time.Parse(time.RFC3339, ca.CreatedAt)
	if err != nil {
		t.Fatalf("created_at of %s: %v", ca.ID, err)
	}
	got, err = s.List(beads.ListOptions{Priority: 4, CreatedAfter: createdAt.Add(-time.Hour)})
	s.want(t, "List{CreatedAfter:before create}", got, err, ca.ID, cb.ID)
	got, err = s.List(beads.ListOptions{Priority: 4, CreatedAfter: createdAt.Add(time.Hour)})
	s.want(t, "List{CreatedAfter:after create}", got, err)
}

// contractEvents pins event issues: EventKind makes an issue_type "event"
// carrying its kind and payload, and IssueType filters on issue_type.
func contractEvents(t *testing.T, s *scope) {
	payload := `{"date":"2026-01-02","n":3}`
	ev := s.mustCreate(t, beads.CreateOptions{Title: "audit " + s.tag, Priority: -1, EventKind: "contract.audit", EventPayload: payload})
	task := s.mustCreate(t, beads.CreateOptions{Title: "work " + s.tag, Priority: -1})
	got := s.mustShow(t, ev.ID)
	if got.Type != "event" || got.EventKind != "contract.audit" || got.Payload != payload {
		t.Errorf("Show = type %q kind %q payload %q, want event contract.audit %s", got.Type, got.EventKind, got.Payload, payload)
	}
	mustDo(t, "close event", s.CloseWithReason("recorded", ev.ID))
	events, err := s.List(beads.ListOptions{IssueType: "event", Status: "all", Priority: -1})
	s.want(t, "List{IssueType:event}", events, err, ev.ID)
	for _, is := range s.mine(events) {
		if is.Payload != payload {
			t.Errorf("List payload = %q, want %s", is.Payload, payload)
		}
	}
	tasks, err := s.List(beads.ListOptions{IssueType: "task", Status: "all", Priority: -1})
	s.want(t, "List{IssueType:task}", tasks, err, task.ID)
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
	author := s.who("patrol")
	mustDo(t, "comment as", s.AddCommentAs(is.ID, author, "third"))
	got, err = s.Comments(is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := got[len(got)-1]; last.Text != "third" || last.Author != author {
		t.Errorf("AddCommentAs comment = %+v, want text third by %s", last, author)
	}
	if err := s.AddCommentAs(s.tag+"-nosuch", author, "x"); err == nil {
		t.Error("AddCommentAs on a missing issue succeeded")
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

// contractTypedDependencies pins AddTypedDependency the way a tracks edge uses
// it: a tracks edge to a local issue shows on Show and does not block, a tracks
// edge to an external:<rig>:<id> target is accepted although no such issue
// exists here (bd's show omits it), and RemoveDependency drops either.
func contractTypedDependencies(t *testing.T, s *scope) {
	tracker := s.mustCreate(t, beads.CreateOptions{Title: "tracker", Priority: -1})
	local := s.mustCreate(t, beads.CreateOptions{Title: "tracked", Priority: -1})
	external := "external:" + s.tag + ":" + s.tag + "-abc"
	mustDo(t, "AddTypedDependency(local)", s.AddTypedDependency(tracker.ID, local.ID, "tracks"))
	mustDo(t, "AddTypedDependency(external)", s.AddTypedDependency(tracker.ID, external, "tracks"))
	got := s.mustShow(t, tracker.ID)
	if depOn(got, local.ID, "tracks") == nil {
		t.Fatalf("Show(tracker).Dependencies = %+v, want tracks on %s", got.Dependencies, local.ID)
	}
	for _, d := range got.Dependencies {
		if d.ID == external {
			t.Errorf("Show(tracker) listed the external edge %+v; bd's show omits external targets", d)
		}
	}
	ready, err := s.Ready()
	s.want(t, "Ready with a tracks edge", ready, err, tracker.ID, local.ID)
	blocker := s.mustCreate(t, beads.CreateOptions{Title: "blocker", Priority: -1})
	mustDo(t, "AddDependency(blocker)", s.AddDependency(tracker.ID, blocker.ID))
	if deps, err := s.DepList(tracker.ID, "tracks"); err != nil || len(deps) != 1 || deps[0].ID != local.ID || deps[0].DependencyType != "tracks" {
		t.Errorf("DepList(tracker, tracks) = %+v, %v; want only %s (tracks), no external edge", deps, err, local.ID)
	}
	if deps, err := s.DepList(tracker.ID, ""); err != nil || len(deps) != 2 || depOn(&beads.Issue{Dependencies: deps}, blocker.ID, "blocks") == nil {
		t.Errorf("DepList(tracker, any) = %+v, %v; want %s (tracks) and %s (blocks)", deps, err, local.ID, blocker.ID)
	}
	if deps, err := s.DepList(local.ID, "tracks"); err != nil || len(deps) != 0 {
		t.Errorf("DepList(tracked) = %+v, %v; want none (edges point down only)", deps, err)
	}
	if _, err := s.DepList(s.tag+"-nosuch", "tracks"); err == nil {
		t.Error("DepList of a missing issue succeeded")
	}
	mustDo(t, "RemoveDependency(blocker)", s.RemoveDependency(tracker.ID, blocker.ID))
	mustDo(t, "RemoveDependency(local)", s.RemoveDependency(tracker.ID, local.ID))
	mustDo(t, "RemoveDependency(external)", s.RemoveDependency(tracker.ID, external))
	if got := s.mustShow(t, tracker.ID); len(got.Dependencies) != 0 {
		t.Errorf("Dependencies after remove = %+v", got.Dependencies)
	}
	if err := s.AddTypedDependency(s.tag+"-nosuch", local.ID, "tracks"); err == nil {
		t.Error("AddTypedDependency from a missing issue succeeded")
	}
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
	all, err := s.ReadyAll()
	s.want(t, "ReadyAll", all, err, work.ID, parent.ID, kid.ID)
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
	grand := s.mustCreate(t, beads.CreateOptions{Title: "grandchild", Parent: w.ID, Priority: -1, Ephemeral: true})
	byParent, err := s.ChildrenOf(parent.ID, a.ID, w.ID)
	if err != nil || len(byParent) != 2 {
		t.Errorf("ChildrenOf: %d parents, err %v; want the parent and the wisp child (a leaf is absent)", len(byParent), err)
	}
	s.want(t, "ChildrenOf[parent]", byParent[parent.ID], nil, a.ID, b.ID, w.ID)
	s.want(t, "ChildrenOf[wisp child]", byParent[w.ID], nil, grand.ID)

	// RemoveDependency removes a parent-child link too.
	mustDo(t, "RemoveDependency(child, parent)", s.RemoveDependency(a.ID, parent.ID))
	if got := s.mustShow(t, a.ID); got.Parent != "" || len(got.Dependencies) != 0 {
		t.Errorf("after unlinking: parent %q deps %+v", got.Parent, got.Dependencies)
	}
	kids, err = s.Children(parent.ID)
	s.want(t, "Children after unlinking", kids, err, b.ID, w.ID)
}

// contractRelease pins that Release does not force. A claim another actor
// still holds survives it, with an error, because bd refuses to reassign
// their live in_progress claim without --force (gt-v8ujv); the store's own
// claim is released, reason and all.
func contractRelease(t *testing.T, s *scope) {
	held := s.mustCreate(t, beads.CreateOptions{Title: "held", Priority: -1})
	holder := s.who("dead")
	mustDo(t, "claim", s.Update(held.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(holder)}))
	refused(t, "releasing another actor's live claim", s.ReleaseWithReason(held.ID, "worker died"))
	if got := s.mustShow(t, held.ID); got.Status != "in_progress" || got.Assignee != holder {
		t.Errorf("a refused release changed the claim to %q held by %q, want in_progress by %s", got.Status, got.Assignee, holder)
	}

	// The store's own claim is releasable. Its actor is the creator bd
	// recorded: no case's assignee is ever the actor, so this is the only
	// name the contract can claim as.
	is := s.mustCreate(t, beads.CreateOptions{Title: "stuck", Priority: -1})
	self := is.CreatedBy
	if self == "" {
		t.Fatal("the store recorded no creator, so its own claim cannot be made")
	}
	mustDo(t, "claim as the store's actor", s.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(self)}))
	mustDo(t, "ReleaseWithReason", s.ReleaseWithReason(is.ID, "worker died"))
	got := s.mustShow(t, is.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Errorf("after release: status %q assignee %q, want open and none", got.Status, got.Assignee)
	}
	if !strings.Contains(got.Notes, "worker died") {
		t.Errorf("notes = %q, want the release reason", got.Notes)
	}
	other := s.mustCreate(t, beads.CreateOptions{Title: "stuck 2", Priority: -1})
	mustDo(t, "claim 2", s.Update(other.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(self)}))
	mustDo(t, "Release", s.Release(other.ID))
	if got := s.mustShow(t, other.ID); got.Status != "open" || got.Assignee != "" {
		t.Errorf("after Release: status %q assignee %q", got.Status, got.Assignee)
	}
	if err := s.Release("gt-nosuch"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("Release(missing) = %v, want ErrNotFound", err)
	}
}

// contractDeleteIssues pins the batch delete: every id is gone afterwards, a
// live issue is left with no edge onto a deleted one, and an empty batch is a
// no-op.
func contractDeleteIssues(t *testing.T, s *scope) {
	mustDo(t, "DeleteIssues()", s.DeleteIssues())

	a := s.mustCreate(t, beads.CreateOptions{Title: "doomed a", Priority: -1})
	b := s.mustCreate(t, beads.CreateOptions{Title: "doomed b", Priority: -1})
	mustDo(t, "comment", s.AddComment(a.ID, "goes with it"))
	mustDo(t, "dependency", s.AddDependency(a.ID, b.ID))

	// A survivor pointing at a deleted issue: deleting both ends of the only
	// edge above cannot show that the edge went with them.
	survivor := s.mustCreate(t, beads.CreateOptions{Title: "survivor", Priority: -1})
	mustDo(t, "survivor dependency", s.AddDependency(survivor.ID, a.ID))

	mustDo(t, "DeleteIssues", s.DeleteIssues(a.ID, b.ID))
	for _, id := range []string{a.ID, b.ID} {
		if _, err := s.Show(id); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("Show(%s) after DeleteIssues = %v, want ErrNotFound", id, err)
		}
	}
	if got := s.mustShow(t, survivor.ID); depOn(got, a.ID, "blocks") != nil {
		t.Errorf("%s still depends on deleted %s: %+v", survivor.ID, a.ID, got.Dependencies)
	}
	if got, err := s.List(beads.ListOptions{Status: "all", Priority: -1}); err != nil {
		t.Errorf("List after DeleteIssues: %v", err)
	} else if mine := s.mine(got); len(mine) != 1 || mine[0].ID != survivor.ID {
		t.Errorf("List after DeleteIssues holds %v, want only %s", ids(mine), survivor.ID)
	}
}

// contractBatchCloseRefusal pins what a batch close in which some issues are
// refused returns. bd 1.2 closes the rest and exits 0; the Client re-reads
// the batch and reports the issues left open as a *beads.PartialCloseError
// wrapping beads.ErrCloseRefused, so a caller never counts a refused issue
// as closed. A batch in which every issue is refused fails as bd fails it.
func contractBatchCloseRefusal(t *testing.T, s *scope) {
	first := s.mustCreate(t, beads.CreateOptions{Title: "closable", Priority: -1})
	parent := s.mustCreate(t, beads.CreateOptions{Title: "parent", Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "open child", Parent: parent.ID, Priority: -1})
	partial(t, "a batch close whose later issue is refused",
		s.CloseWithReason("batch", first.ID, parent.ID), []string{first.ID}, []string{parent.ID})
	if got := s.mustShow(t, first.ID); got.Status != "closed" || got.CloseReason != "batch" {
		t.Errorf("the closable issue: status %q reason %q, want closed \"batch\"", got.Status, got.CloseReason)
	}
	if st := s.mustShow(t, parent.ID).Status; st != "open" {
		t.Errorf("the refused issue's status = %q, want open", st)
	}

	other := s.mustCreate(t, beads.CreateOptions{Title: "parent 2", Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "open child 2", Parent: other.ID, Priority: -1})
	last := s.mustCreate(t, beads.CreateOptions{Title: "closable last", Priority: -1})
	partial(t, "a batch close whose first issue is refused",
		s.Close(other.ID, last.ID), []string{last.ID}, []string{other.ID})
	if st := s.mustShow(t, last.ID).Status; st != "closed" {
		t.Errorf("the issue after the refused one: status %q, want closed", st)
	}

	// The assignee and blocker refusals are skipped the same way, and an
	// issue the batch finds already closed counts as closed.
	theirs := s.mustCreate(t, beads.CreateOptions{Title: "alice's", Priority: -1})
	mustDo(t, "assign", s.Update(theirs.ID, beads.UpdateOptions{Assignee: ptr(s.who("alice"))}))
	blocker := s.mustCreate(t, beads.CreateOptions{Title: "blocker", Priority: -1})
	blocked := s.mustCreate(t, beads.CreateOptions{Title: "blocked", Priority: -1})
	mustDo(t, "AddDependency", s.AddDependency(blocked.ID, blocker.ID))
	mine := s.mustCreate(t, beads.CreateOptions{Title: "mine", Priority: -1})
	partial(t, "a batch close with an assignee and a blocker refusal",
		s.Close(theirs.ID, first.ID, blocked.ID, mine.ID), []string{first.ID, mine.ID}, []string{theirs.ID, blocked.ID})

	refused(t, "a batch close in which every issue is refused", s.CloseWithReason("batch", other.ID, parent.ID))
	for _, id := range []string{other.ID, parent.ID} {
		if st := s.mustShow(t, id).Status; st != "open" {
			t.Errorf("%s status %q after an all-refused batch, want open", id, st)
		}
	}

	// Forced, nothing is refused.
	mustDo(t, "a forced batch close", s.ForceCloseWithReason("forced", other.ID, theirs.ID))
}

// contractBatchCloseChain pins that bd closes a batch in argument order,
// refusing an issue whose blocker is still open at its turn even when the
// blocker comes later in the same batch. A molecule's steps form such a
// chain, and Children lists them in ID order, not dependency order.
func contractBatchCloseChain(t *testing.T, s *scope) {
	// s1 <- s2 <- s3: s2 depends on s1, s3 on s2.
	var chain []*beads.Issue
	for i := 0; i < 3; i++ {
		chain = append(chain, s.mustCreate(t, beads.CreateOptions{Title: "step", Priority: -1}))
		if i > 0 {
			mustDo(t, "AddDependency", s.AddDependency(chain[i].ID, chain[i-1].ID))
		}
	}
	s1, s2, s3 := chain[0].ID, chain[1].ID, chain[2].ID
	partial(t, "closing the chain last-first", s.Close(s3, s2, s1), []string{s1}, []string{s2, s3})
	partial(t, "closing the rest last-first", s.Close(s3, s2), []string{s2}, []string{s3})
	mustDo(t, "closing the last step", s.Close(s3))

	// In dependency order the same batch closes whole.
	var fwd []string
	for i := 0; i < 3; i++ {
		is := s.mustCreate(t, beads.CreateOptions{Title: "step", Priority: -1})
		if i > 0 {
			mustDo(t, "AddDependency", s.AddDependency(is.ID, fwd[i-1]))
		}
		fwd = append(fwd, is.ID)
	}
	mustDo(t, "closing a chain first-first", s.Close(fwd...))
}

// partial checks that err reports a partial close of exactly closed and
// notClosed.
func partial(t *testing.T, what string, err error, closed, notClosed []string) {
	t.Helper()
	var pe *beads.PartialCloseError
	if !errors.As(err, &pe) {
		t.Errorf("%s = %v, want a *beads.PartialCloseError", what, err)
		return
	}
	if !errors.Is(err, beads.ErrCloseRefused) || errors.Is(err, beads.ErrNotFound) {
		t.Errorf("%s = %v, want it to wrap ErrCloseRefused and not ErrNotFound", what, err)
	}
	if g, w := sorted(pe.Closed...), sorted(closed...); !reflect.DeepEqual(g, w) {
		t.Errorf("%s: Closed = %v, want %v", what, g, w)
	}
	if g, w := sorted(pe.NotClosed...), sorted(notClosed...); !reflect.DeepEqual(g, w) {
		t.Errorf("%s: NotClosed = %v, want %v", what, g, w)
	}
	all := append(append([]string{}, closed...), notClosed...)
	if g, w := sorted(beads.ClosedIDs(all, err)...), sorted(closed...); !reflect.DeepEqual(g, w) {
		t.Errorf("%s: ClosedIDs = %v, want %v", what, g, w)
	}
}

func contractAppendNotes(t *testing.T, s *scope) {
	is := s.mustCreate(t, beads.CreateOptions{Title: "notes", Priority: -1})
	mustDo(t, "first append", s.AppendNotes(is.ID, "first line"))
	if n := s.mustShow(t, is.ID).Notes; n != "first line" {
		t.Errorf("notes after one append = %q, want the note alone", n)
	}
	mustDo(t, "second append", s.AppendNotes(is.ID, "second\nwith two lines"))
	if n := s.mustShow(t, is.ID).Notes; n != "first line\nsecond\nwith two lines" {
		t.Errorf("notes after two appends = %q, want them joined by a newline", n)
	}
	if err := s.AppendNotes("gt-nosuch", "x"); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("AppendNotes(missing) = %v, want ErrNotFound", err)
	}
}

func contractGuardedTransfer(t *testing.T, s *scope) {
	alice, bob := s.who("alice"), s.who("bob")
	is := s.mustCreate(t, beads.CreateOptions{Title: "hooked work", Priority: -1})
	mustDo(t, "claim", s.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress"), Assignee: ptr(alice)}))

	// The guard names the holder, so it moves an in_progress claim that a
	// plain update may not.
	ok, err := s.TransferIfAssignee(is.ID, alice, beads.StatusHooked, bob)
	if err != nil || !ok {
		t.Fatalf("TransferIfAssignee(alice -> bob) = %v, %v; want true, nil", ok, err)
	}
	if got := s.mustShow(t, is.ID); got.Status != beads.StatusHooked || got.Assignee != bob {
		t.Errorf("after the transfer: status %q assignee %q, want hooked %s", got.Status, got.Assignee, bob)
	}

	// A guard that no longer holds writes nothing and is not an error.
	ok, err = s.TransferIfAssignee(is.ID, alice, "open", alice)
	if err != nil || ok {
		t.Errorf("TransferIfAssignee with a stale guard = %v, %v; want false, nil", ok, err)
	}
	ok, err = s.ReleaseIfAssignee(is.ID, alice)
	if err != nil || ok {
		t.Errorf("ReleaseIfAssignee with a stale guard = %v, %v; want false, nil", ok, err)
	}
	if got := s.mustShow(t, is.ID); got.Status != beads.StatusHooked || got.Assignee != bob {
		t.Errorf("stale guards changed the issue to %q/%q", got.Status, got.Assignee)
	}

	mustDo(t, "bob starts", s.Update(is.ID, beads.UpdateOptions{Status: ptr("in_progress")}))
	ok, err = s.ReleaseIfAssignee(is.ID, bob)
	if err != nil || !ok {
		t.Fatalf("ReleaseIfAssignee(bob) = %v, %v; want true, nil", ok, err)
	}
	if got := s.mustShow(t, is.ID); got.Status != "open" || got.Assignee != "" {
		t.Errorf("after the release: status %q assignee %q, want open and none", got.Status, got.Assignee)
	}

	// An empty expected guards on "unassigned".
	ok, err = s.TransferIfAssignee(is.ID, "", beads.StatusHooked, alice)
	if err != nil || !ok {
		t.Errorf("TransferIfAssignee(unassigned -> alice) = %v, %v; want true, nil", ok, err)
	}
	if _, err := s.ReleaseIfAssignee("gt-nosuch", alice); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("ReleaseIfAssignee(missing) = %v, want ErrNotFound", err)
	}
}

// contractGuardEdges pins the guarded writes' and AppendNotes' edges.
func contractGuardEdges(t *testing.T, s *scope) {
	alice, bob := s.who("alice"), s.who("bob")

	// An empty guard means unassigned: it does not match an assigned issue.
	held := s.mustCreate(t, beads.CreateOptions{Title: "held", Priority: -1})
	mustDo(t, "assign", s.Update(held.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	if ok, err := s.TransferIfAssignee(held.ID, "", beads.StatusHooked, bob); err != nil || ok {
		t.Errorf("TransferIfAssignee(assigned, guard \"\") = %v, %v; want false, nil", ok, err)
	}
	if ok, err := s.ReleaseIfAssignee(held.ID, ""); err != nil || ok {
		t.Errorf("ReleaseIfAssignee(assigned, guard \"\") = %v, %v; want false, nil", ok, err)
	}
	if got := s.mustShow(t, held.ID); got.Assignee != alice || got.Status != "open" {
		t.Errorf("a failed empty guard changed the issue to %q/%q", got.Status, got.Assignee)
	}
	if _, err := s.TransferIfAssignee("gt-nosuch", alice, beads.StatusHooked, bob); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("TransferIfAssignee(missing) = %v, want ErrNotFound", err)
	}

	// A transfer to closed passes the close fence an update does: a parent
	// with an open child stays open.
	parent := s.mustCreate(t, beads.CreateOptions{Title: "parent", Priority: -1})
	s.mustCreate(t, beads.CreateOptions{Title: "open child", Parent: parent.ID, Priority: -1})
	mustDo(t, "assign parent", s.Update(parent.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	ok, err := s.TransferIfAssignee(parent.ID, alice, string(beads.StatusClosed), alice)
	if ok {
		t.Errorf("TransferIfAssignee(parent with an open child -> closed) = true, want it refused")
	}
	refused(t, "TransferIfAssignee(parent with an open child -> closed)", err)
	if st := s.mustShow(t, parent.ID).Status; st != "open" {
		t.Errorf("refused transfer left the parent %q", st)
	}
	// Close's assignee fence is not an update's: the holder's issue closes.
	done := s.mustCreate(t, beads.CreateOptions{Title: "alice's done", Priority: -1})
	mustDo(t, "assign done", s.Update(done.ID, beads.UpdateOptions{Assignee: ptr(alice)}))
	if ok, err := s.TransferIfAssignee(done.ID, alice, string(beads.StatusClosed), alice); err != nil || !ok {
		t.Errorf("TransferIfAssignee(alice's issue -> closed) = %v, %v; want true, nil", ok, err)
	}
	if st := s.mustShow(t, done.ID).Status; st != "closed" {
		t.Errorf("transfer to closed left status %q", st)
	}

	// Release reopens a closed issue.
	closed := s.mustCreate(t, beads.CreateOptions{Title: "closed", Priority: -1})
	mustDo(t, "close", s.Close(closed.ID))
	mustDo(t, "Release(closed)", s.Release(closed.ID))
	if got := s.mustShow(t, closed.ID); got.Status != "open" || got.ClosedAt != "" || got.Assignee != "" {
		t.Errorf("Release(closed): status %q closed_at %q assignee %q, want open", got.Status, got.ClosedAt, got.Assignee)
	}

	// An empty note appends nothing.
	noted := s.mustCreate(t, beads.CreateOptions{Title: "noted", Priority: -1})
	mustDo(t, "AppendNotes(\"\") with no notes", s.AppendNotes(noted.ID, ""))
	if n := s.mustShow(t, noted.ID).Notes; n != "" {
		t.Errorf("notes after an empty append = %q, want none", n)
	}
	mustDo(t, "append", s.AppendNotes(noted.ID, "one"))
	mustDo(t, "AppendNotes(\"\") with notes", s.AppendNotes(noted.ID, ""))
	if n := s.mustShow(t, noted.ID).Notes; n != "one" {
		t.Errorf("notes after an empty append = %q, want \"one\"", n)
	}
}

// contractMergeRequestBlockers pins ListMergeRequests' dependency
// hydration: an MR's open blockers come back in BlockedBy, and a closed one
// no longer blocks.
func contractMergeRequestBlockers(t *testing.T, s *scope) {
	const mrLabel = "gt:merge-request"
	desc := "branch: polecat/nux/" + s.tag + "\ntarget: main\nsource_issue: gt-src"
	mr := s.mustCreate(t, beads.CreateOptions{Title: "blocked MR", Labels: []string{mrLabel}, Description: desc, Priority: -1})
	free := s.mustCreate(t, beads.CreateOptions{Title: "free MR", Labels: []string{mrLabel}, Description: desc, Priority: -1})
	blocker := s.mustCreate(t, beads.CreateOptions{Title: "blocker", Priority: -1})
	mustDo(t, "AddDependency", s.AddDependency(mr.ID, blocker.ID))

	blockers := func() map[string][]string {
		t.Helper()
		got, err := beads.ListMergeRequests(s.Client, beads.ListOptions{Label: mrLabel, Status: "open", Priority: -1})
		s.want(t, "ListMergeRequests", got, err, mr.ID, free.ID)
		out := map[string][]string{}
		for _, is := range s.mine(got) {
			out[is.ID] = is.BlockedBy
			if is.BlockedByCount != len(is.BlockedBy) {
				t.Errorf("%s: BlockedByCount %d, BlockedBy %v", is.ID, is.BlockedByCount, is.BlockedBy)
			}
		}
		return out
	}
	got := blockers()
	if !reflect.DeepEqual(got[mr.ID], []string{blocker.ID}) || len(got[free.ID]) != 0 {
		t.Errorf("BlockedBy = %v, want %s blocked by %s and %s by nothing", got, mr.ID, blocker.ID, free.ID)
	}
	mustDo(t, "close the blocker", s.Close(blocker.ID))
	if got := blockers(); len(got[mr.ID]) != 0 {
		t.Errorf("BlockedBy after the blocker closed = %v, want none", got[mr.ID])
	}
}

// contractMergeRequests pins beads.ListMergeRequests: on *beads.Beads it
// reads the wisps table by SQL, on any other Client through List, and the
// two must agree.
func contractMergeRequests(t *testing.T, s *scope) {
	const mrLabel = "gt:merge-request"
	desc := func(rig string) string {
		d := "branch: polecat/nux/" + s.tag + "\ntarget: main\nsource_issue: gt-src"
		if rig != "" {
			d += "\nrig: " + rig
		}
		return d
	}
	durable := s.mustCreate(t, beads.CreateOptions{Title: "durable MR", Labels: []string{mrLabel}, Description: desc(""), Priority: -1})
	wisp := s.mustCreate(t, beads.CreateOptions{Title: "wisp MR", Labels: []string{mrLabel}, Description: desc("testrig"), Priority: -1, Ephemeral: true})
	elsewhere := s.mustCreate(t, beads.CreateOptions{Title: "other rig's MR", Labels: []string{mrLabel}, Description: desc("otherrig"), Priority: -1, Ephemeral: true})
	done := s.mustCreate(t, beads.CreateOptions{Title: "merged MR", Labels: []string{mrLabel}, Description: desc(""), Priority: -1, Ephemeral: true})
	mustDo(t, "close the merged MR", s.CloseWithReason("merged", done.ID))
	s.mustCreate(t, beads.CreateOptions{Title: "not an MR", Labels: []string{"gt:task"}, Priority: -1, Ephemeral: true})

	got, err := beads.ListMergeRequests(s.Client, beads.ListOptions{Label: mrLabel, Status: "open", Priority: -1})
	s.want(t, "ListMergeRequests(open)", got, err, durable.ID, wisp.ID, elsewhere.ID)
	for _, is := range s.mine(got) {
		if !strings.Contains(is.Description, "branch: polecat/nux/") || !beads.HasLabel(is, mrLabel) {
			t.Errorf("%s came back without its description or labels: %+v", is.ID, is)
		}
		if is.ID != durable.ID && !is.Ephemeral {
			t.Errorf("wisp %s came back not ephemeral", is.ID)
		}
	}
	got, err = beads.ListMergeRequests(s.Client, beads.ListOptions{Label: mrLabel, Status: "open", Priority: -1, Rig: "testrig"})
	s.want(t, "ListMergeRequests(open, rig testrig)", got, err, durable.ID, wisp.ID)
	got, err = beads.ListMergeRequests(s.Client, beads.ListOptions{Label: mrLabel, Status: "all", Priority: -1})
	s.want(t, "ListMergeRequests(all)", got, err, durable.ID, wisp.ID, elsewhere.ID, done.ID)
	got, err = beads.ListMergeRequests(s.Client, beads.ListOptions{Label: mrLabel, Status: "closed", Priority: -1})
	s.want(t, "ListMergeRequests(closed)", got, err, done.ID)
}

// contractAgentActiveMR pins the agent-bead helpers over a Client.
func contractAgentActiveMR(t *testing.T, s *scope) {
	agent := s.mustCreate(t, beads.CreateOptions{
		Title: "Polecat nux", Labels: []string{"gt:agent"}, Priority: -1,
		Description: "role_type: polecat\nrig: testrig\nagent_state: working\nactive_mr: gt-mr-" + s.tag,
	})
	issue, fields, err := beads.GetAgentBead(s.Client, agent.ID)
	if err != nil || issue == nil || fields == nil {
		t.Fatalf("GetAgentBead = %v, %v, %v", issue, fields, err)
	}
	if fields.ActiveMR != "gt-mr-"+s.tag || fields.RoleType != "polecat" || fields.AgentState != "working" {
		t.Errorf("agent fields = %+v", fields)
	}
	if issue, fields, err := beads.GetAgentBead(s.Client, "gt-nosuch"); issue != nil || fields != nil || err != nil {
		t.Errorf("GetAgentBead(missing) = %v, %v, %v; want nil, nil, nil", issue, fields, err)
	}
	task := s.mustCreate(t, beads.CreateOptions{Title: "not an agent", Labels: []string{"gt:task"}, Priority: -1})
	if _, _, err := beads.GetAgentBead(s.Client, task.ID); err == nil {
		t.Error("GetAgentBead(non-agent) succeeded")
	}

	cleared, err := beads.ClearAgentActiveMRIfMatches(s.Client, agent.ID, "gt-mr-newer")
	if err != nil || cleared {
		t.Errorf("clear with another MR = %v, %v; want false, nil", cleared, err)
	}
	if _, f, _ := beads.GetAgentBead(s.Client, agent.ID); f == nil || f.ActiveMR != "gt-mr-"+s.tag {
		t.Errorf("a non-matching clear changed active_mr to %+v", f)
	}
	cleared, err = beads.ClearAgentActiveMRIfMatches(s.Client, agent.ID, " gt-mr-"+s.tag+" ")
	if err != nil || !cleared {
		t.Fatalf("clear with the active MR = %v, %v; want true, nil", cleared, err)
	}
	if _, f, _ := beads.GetAgentBead(s.Client, agent.ID); f == nil || f.ActiveMR != "" || f.RoleType != "polecat" {
		t.Errorf("after the clear: %+v, want active_mr empty and the rest kept", f)
	}
	if cleared, err := beads.ClearAgentActiveMRIfMatches(s.Client, "gt-nosuch", "gt-mr-x"); err != nil || cleared {
		t.Errorf("clear on a missing agent = %v, %v; want false, nil", cleared, err)
	}
	if _, err := beads.ClearAgentActiveMRIfMatches(s.Client, task.ID, "gt-mr-x"); err == nil {
		t.Error("clear on a non-agent succeeded")
	}
	if beads.ForAgentBead(s.Client) == nil {
		t.Error("ForAgentBead returned nil")
	}
}

// contractMergeSlot pins the merge slot helpers over a Client. The slot is
// one bead per database, so only this case may create it.
func contractMergeSlot(t *testing.T, s *scope) {
	alice, bob := s.who("alice"), s.who("bob")
	st, err := beads.MergeSlotCheck(s.Client)
	if err != nil || st.Error != "not found" {
		t.Fatalf("MergeSlotCheck before any slot = %+v, %v; want Error \"not found\"", st, err)
	}
	if _, err := beads.MergeSlotAcquire(s.Client, alice, false); err == nil {
		t.Error("MergeSlotAcquire with no slot succeeded")
	}
	if err := beads.MergeSlotRelease(s.Client, alice); err != nil {
		t.Errorf("MergeSlotRelease with no slot = %v, want nil", err)
	}
	id, err := beads.MergeSlotEnsureExists(s.Client)
	if err != nil || id == "" {
		t.Fatalf("MergeSlotEnsureExists = %q, %v", id, err)
	}
	s.created[id] = true
	if again, err := beads.MergeSlotEnsureExists(s.Client); err != nil || again != id {
		t.Errorf("MergeSlotEnsureExists again = %q, %v; want %q", again, err, id)
	}
	if st, err := beads.MergeSlotCheck(s.Client); err != nil || !st.Available || st.ID != id {
		t.Errorf("fresh slot = %+v, %v; want available %s", st, err, id)
	}

	st, err = beads.MergeSlotAcquire(s.Client, alice, false)
	if err != nil || st.Holder != alice || st.Available {
		t.Fatalf("alice acquires = %+v, %v", st, err)
	}
	st, err = beads.MergeSlotAcquire(s.Client, bob, true)
	if err != nil || st.Holder != alice || !reflect.DeepEqual(st.Waiters, []string{bob}) {
		t.Errorf("bob tries while alice holds = %+v, %v; want holder alice, waiters [bob]", st, err)
	}
	if err := beads.MergeSlotRelease(s.Client, bob); !errors.Is(err, beads.ErrMergeSlotNotHolder) {
		t.Errorf("bob releases alice's slot = %v, want ErrMergeSlotNotHolder", err)
	}
	mustDo(t, "alice releases", beads.MergeSlotRelease(s.Client, alice))
	if st, err := beads.MergeSlotCheck(s.Client); err != nil || st.Holder != bob || len(st.Waiters) != 0 {
		t.Errorf("after alice releases = %+v, %v; want bob promoted", st, err)
	}
	mustDo(t, "bob releases", beads.MergeSlotRelease(s.Client, bob))
	if st, err := beads.MergeSlotCheck(s.Client); err != nil || !st.Available {
		t.Errorf("after bob releases = %+v, %v; want available", st, err)
	}
}
