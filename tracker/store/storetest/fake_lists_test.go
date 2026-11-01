package storetest

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// listFixture is a store whose beads between them cover every rule a list
// applies: a closed one, an infrastructure type, a label, a child and a
// parent.
func listFixture(t *testing.T) (*Fake, context.Context) {
	t.Helper()
	f := NewFakeWithClock(func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	ctx := api.WithIdentity(context.Background(), api.Identity{Actor: "tester", Scope: api.ScopeAdmin})
	for _, p := range []api.CreateParams{
		{ID: "fx-1", Title: "one", Labels: []string{"wanted"}, Actor: "tester"},
		{ID: "fx-2", Title: "two", Actor: "tester"},
		{ID: "fx-agent", Title: "agent", Type: "agent", Actor: "tester"},
		{ID: "fx-closed", Title: "closed", Actor: "tester"},
		{ID: "fx-parent", Title: "parent", Type: "epic", Actor: "tester"},
		{ID: "fx-child", Title: "child", Parent: "fx-parent", Actor: "tester"},
	} {
		if _, err := f.CreateIssue(ctx, p); err != nil {
			t.Fatalf("creating %s: %v", p.ID, err)
		}
	}
	if err := f.CloseIssues(ctx, []string{"fx-closed"}, "done", false); err != nil {
		t.Fatalf("closing fx-closed: %v", err)
	}
	return f, ctx
}

func listedIDs(page api.IssuePage) []string {
	ids := make([]string, len(page.Issues))
	for i, is := range page.Issues {
		ids[i] = is.ID
	}
	return ids
}

// TestFakeListIssuesAppliesTheListRules walks the filters the fake models: the
// closed and infrastructure rows it leaves out by default, and the status,
// label, parent, type and id filters.
func TestFakeListIssuesAppliesTheListRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flt  api.ListFilter
		want []string
	}{
		{name: "no filter leaves out closed and infrastructure rows", want: []string{"fx-1", "fx-2", "fx-child", "fx-parent"}},
		{name: "--all keeps the closed rows", flt: api.ListFilter{Statuses: []string{"all"}}, want: []string{"fx-1", "fx-2", "fx-child", "fx-closed", "fx-parent"}},
		{name: "a status", flt: api.ListFilter{Statuses: []string{"closed"}}, want: []string{"fx-closed"}},
		{name: "include infra keeps the agent", flt: api.ListFilter{IncludeInfra: true}, want: []string{"fx-1", "fx-2", "fx-agent", "fx-child", "fx-parent"}},
		{name: "a label", flt: api.ListFilter{Labels: []string{"wanted"}}, want: []string{"fx-1"}},
		{name: "a parent", flt: api.ListFilter{Parent: "fx-parent"}, want: []string{"fx-child"}},
		{name: "an issue type", flt: api.ListFilter{IssueType: "epic"}, want: []string{"fx-parent"}},
		{name: "ids", flt: api.ListFilter{IDs: []string{"fx-2", "fx-9"}}, want: []string{"fx-2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ctx := listFixture(t)
			page, err := f.ListIssues(ctx, tc.flt)
			if err != nil {
				t.Fatalf("ListIssues: %v", err)
			}
			if got := listedIDs(page); !slices.Equal(got, tc.want) {
				t.Errorf("ListIssues(%+v) = %v, want %v", tc.flt, got, tc.want)
			}
		})
	}
}

// TestFakeListIssuesPagesAndCounts: one page holds the limit and names where
// the next begins, and a row carries its comment and link counts.
func TestFakeListIssuesPagesAndCounts(t *testing.T) {
	t.Parallel()
	f, ctx := listFixture(t)
	if err := f.AddComment(ctx, "fx-2", "tester", "a note"); err != nil {
		t.Fatalf("adding a comment: %v", err)
	}
	if err := f.AddDependency(ctx, "fx-2", "fx-1", "blocks"); err != nil {
		t.Fatalf("adding a dependency: %v", err)
	}
	page, err := f.ListIssues(ctx, api.ListFilter{IDs: []string{"fx-1", "fx-2"}, Limit: 1})
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(page.Issues) != 1 || page.NextCursor == "" {
		t.Fatalf("ListIssues with limit 1 = %v, next %q; want one row and a cursor", listedIDs(page), page.NextCursor)
	}
	next, err := f.ListIssues(ctx, api.ListFilter{IDs: []string{"fx-1", "fx-2"}, Limit: 1, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("ListIssues page 2: %v", err)
	}
	if got, want := listedIDs(next), []string{"fx-2"}; !slices.Equal(got, want) {
		t.Fatalf("ListIssues page 2 = %v, want %v", got, want)
	}
	if next.Issues[0].CommentCount != 1 || next.Issues[0].DependencyCount != 1 {
		t.Errorf("fx-2 counts = comment %d, dependency %d; want 1 and 1", next.Issues[0].CommentCount, next.Issues[0].DependencyCount)
	}
}

// TestFakeListIssuesRefusesWhatItDoesNotModel: a filter or cursor the fake
// cannot express is an error, never a wider answer than the caller asked for.
func TestFakeListIssuesRefusesWhatItDoesNotModel(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flt  api.ListFilter
		want error
	}{
		{name: "an assignee filter", flt: api.ListFilter{Assignee: "someone"}, want: store.ErrUnimplemented},
		{name: "a priority filter", flt: api.ListFilter{Priority: new(int)}, want: store.ErrUnimplemented},
		{name: "a cursor that is not one", flt: api.ListFilter{Cursor: "not-a-cursor"}, want: api.ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, ctx := listFixture(t)
			if _, err := f.ListIssues(ctx, tc.flt); !errors.Is(err, tc.want) {
				t.Errorf("ListIssues(%+v) = %v, want %v", tc.flt, err, tc.want)
			}
		})
	}
}

// TestFakeChildrenOfAndComments: children come back under their parent, and a
// named issue's comments come back oldest first. A missing issue's comments are
// not found, not an empty answer.
func TestFakeChildrenOfAndComments(t *testing.T) {
	t.Parallel()
	f, ctx := listFixture(t)
	if err := f.AddComment(ctx, "fx-1", "tester", "first"); err != nil {
		t.Fatalf("adding a comment: %v", err)
	}
	if err := f.AddComment(ctx, "fx-1", "tester", "second"); err != nil {
		t.Fatalf("adding a comment: %v", err)
	}
	kids, err := f.ChildrenOf(ctx, []string{"fx-parent", "fx-1"})
	if err != nil {
		t.Fatalf("ChildrenOf: %v", err)
	}
	if got, want := listedIDs(api.IssuePage{Issues: kids["fx-parent"]}), []string{"fx-child"}; !slices.Equal(got, want) {
		t.Errorf("children of fx-parent = %v, want %v", got, want)
	}
	if _, ok := kids["fx-1"]; ok {
		t.Errorf("ChildrenOf answered fx-1, which has no children")
	}
	comments, err := f.Comments(ctx, "fx-1")
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 2 || comments[0].Text != "first" || comments[1].Text != "second" {
		t.Errorf("comments on fx-1 = %+v, want first then second", comments)
	}
	if _, err := f.Comments(ctx, "fx-missing"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Comments on a missing issue = %v, want not found", err)
	}
}

// TestFakeGetIssuesFillsDependencies: a named read answers what the issue
// depends on, with the link's own record and the target's fields.
func TestFakeGetIssuesFillsDependencies(t *testing.T) {
	t.Parallel()
	f, ctx := listFixture(t)
	if err := f.AddDependency(ctx, "fx-1", "fx-2", "blocks"); err != nil {
		t.Fatalf("adding a dependency: %v", err)
	}
	got, err := f.GetIssues(ctx, []string{"fx-1"})
	if err != nil {
		t.Fatalf("GetIssues: %v", err)
	}
	deps := got["fx-1"].Dependencies
	if len(deps) != 1 {
		t.Fatalf("fx-1 dependencies = %+v, want one", deps)
	}
	dep := deps[0]
	if dep.ID != "fx-2" || dep.Title != "two" || dep.DependencyType != "blocks" {
		t.Errorf("dependency = %+v, want fx-2 (two) as blocks", dep)
	}
	if dep.CreatedBy != "tester" || dep.CreatedAt.IsZero() {
		t.Errorf("dependency link = created_by %q, created_at %v; want tester and a time", dep.CreatedBy, dep.CreatedAt)
	}
}

// TestFakeAddDependencyRecordsAParent: a parent-child link sets the child's
// parent on the issue row, as the real store does, so a list's parent field and
// ChildrenOf read it.
func TestFakeAddDependencyRecordsAParent(t *testing.T) {
	t.Parallel()
	f, ctx := listFixture(t)
	if err := f.AddDependency(ctx, "fx-2", "fx-parent", "parent-child"); err != nil {
		t.Fatalf("adding a parent-child link: %v", err)
	}
	got, err := f.GetIssues(ctx, []string{"fx-2"})
	if err != nil {
		t.Fatalf("GetIssues: %v", err)
	}
	if got["fx-2"].Parent != "fx-parent" {
		t.Errorf("fx-2 parent = %q, want fx-parent", got["fx-2"].Parent)
	}
}
