package contract

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "create then get returns the issue created", Fake: true, Run: createThenGet})
	register(Case{Name: "an explicit id is kept and a duplicate is a conflict", Fake: true, Run: explicitIDAndDuplicate})
	register(Case{Name: "create refuses a missing title, a missing rig and a mismatched id", Fake: true, Run: createRefusals})
	register(Case{Name: "get returns only the ids that exist", Fake: true, Run: getSkipsMissing})
	register(Case{Name: "generated ids are distinct and carry the rig prefix", Fake: true, Run: generatedIDs})
}

func ctx() context.Context { return context.Background() }

func mustCreate(t *testing.T, s store.Store, p api.CreateParams) api.Issue {
	t.Helper()
	is, err := s.CreateIssue(ctx(), p)
	if err != nil {
		t.Fatalf("CreateIssue(%+v): %v", p, err)
	}
	return is
}

func createThenGet(t *testing.T, s store.Store) {
	before := time.Now().Add(-time.Minute)
	made := mustCreate(t, s, api.CreateParams{
		Rig: "gt", Title: "write the thing", Type: "bug", Priority: 1, Description: "details",
		Labels: []string{"b", "a"}, Actor: "town/crew/dipper",
	})
	got, err := s.GetIssues(ctx(), []string{made.ID})
	if err != nil {
		t.Fatal(err)
	}
	is, ok := got[made.ID]
	if !ok {
		t.Fatalf("GetIssues did not return %s that CreateIssue just made", made.ID)
	}
	if is.Title != "write the thing" || is.Type != "bug" || is.Priority != 1 || is.Description != "details" {
		t.Errorf("got %+v, want the fields it was created with", is)
	}
	if is.Status != "open" {
		t.Errorf("Status = %q, want open for a new issue", is.Status)
	}
	if !slices.Equal(is.Labels, []string{"a", "b"}) {
		t.Errorf("Labels = %v, want [a b] (sorted)", is.Labels)
	}
	if is.CreatedBy != "town/crew/dipper" {
		t.Errorf("CreatedBy = %q, want the actor", is.CreatedBy)
	}
	if is.CreatedAt.Before(before) || is.UpdatedAt.Before(is.CreatedAt) {
		t.Errorf("CreatedAt %v / UpdatedAt %v are not sane timestamps", is.CreatedAt, is.UpdatedAt)
	}
	if is.Ephemeral {
		t.Error("a durable issue came back ephemeral")
	}
}

func explicitIDAndDuplicate(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{ID: "gt-abc12", Title: "x"})
	if is.ID != "gt-abc12" {
		t.Errorf("ID = %q, want the explicit gt-abc12", is.ID)
	}
	if is.Type != "task" {
		t.Errorf("Type = %q, want the default task", is.Type)
	}
	_, err := s.CreateIssue(ctx(), api.CreateParams{ID: "gt-abc12", Title: "again"})
	if !errors.Is(err, api.ErrConflict) {
		t.Errorf("duplicate id error = %v, want api.ErrConflict", err)
	}
}

func createRefusals(t *testing.T, s store.Store) {
	for name, p := range map[string]api.CreateParams{
		"no title":        {Rig: "gt"},
		"no rig, no id":   {Title: "x"},
		"id outside rig":  {ID: "hq-1", Rig: "gt", Title: "x"},
		"id without dash": {ID: "gt1", Title: "x"},
	} {
		if _, err := s.CreateIssue(ctx(), p); !errors.Is(err, api.ErrInvalid) {
			t.Errorf("%s: error = %v, want api.ErrInvalid", name, err)
		}
	}
}

func getSkipsMissing(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	got, err := s.GetIssues(ctx(), []string{a.ID, "gt-nope", "hq-nope"})
	if err != nil {
		t.Fatalf("GetIssues with missing ids: %v (a missing id is absent, not an error)", err)
	}
	if len(got) != 1 || got[a.ID].ID != a.ID {
		t.Errorf("GetIssues = %v, want only %s", got, a.ID)
	}
	if got, err := s.GetIssues(ctx(), nil); err != nil || len(got) != 0 {
		t.Errorf("GetIssues(nil) = %v, %v; want an empty map", got, err)
	}
}

func generatedIDs(t *testing.T, s store.Store) {
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		id := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "n"}).ID
		if len(id) < 4 || id[:3] != "hq-" {
			t.Errorf("generated id %q does not start with hq-", id)
		}
		if seen[id] {
			t.Errorf("generated id %q repeated", id)
		}
		seen[id] = true
	}
}
