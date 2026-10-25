package server

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: the real client against the real routes with a stub store.
func TestClientLinkMethodsAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	var removed, wroteAuthor string
	st := okLinks()
	st.addComment = func(_ context.Context, _, author, _ string) error { wroteAuthor = author; return nil }
	st.comments = func(context.Context, string) ([]api.Comment, error) {
		return []api.Comment{{ID: "c1", Author: "me", Text: "hi"}}, nil
	}
	st.removeDep = func(_ context.Context, _, on string) error {
		removed = on
		if on == "gt-gone" {
			return api.NotFound("dependency")
		}
		return nil
	}
	st.deps = func(_ context.Context, _ string, typ string) ([]api.IssueDep, error) {
		return []api.IssueDep{{ID: "gt-b", Status: "open", DependencyType: typ}}, nil
	}
	ts := linkServer(t, st)
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.AddComment(ctx, "gt-a", "", "hello"); err != nil || wroteAuthor != linkTokenActor {
		t.Errorf("AddComment = %v, author %q; want the token's actor when none is given", err, wroteAuthor)
	}
	if got, err := c.Comments(ctx, "gt-a"); err != nil || len(got) != 1 || got[0].Text != "hi" {
		t.Errorf("Comments = %v, %v", got, err)
	}
	if err := c.AddDependency(ctx, "gt-a", "gt-b", "blocks"); err != nil {
		t.Errorf("AddDependency = %v", err)
	}
	if err := c.RemoveDependency(ctx, "gt-a", "external:om:om-7"); err != nil || removed != "external:om:om-7" {
		t.Errorf("RemoveDependency external = %v, store got %q", err, removed)
	}
	if err := c.RemoveDependency(ctx, "gt-a", "gt-gone"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("RemoveDependency missing = %v, want api.ErrNotFound", err)
	}
	if deps, err := c.Dependencies(ctx, "gt-a", "tracks"); err != nil || len(deps) != 1 || deps[0].DependencyType != "tracks" {
		t.Errorf("Dependencies = %v, %v; want the type delivered", deps, err)
	}
}
