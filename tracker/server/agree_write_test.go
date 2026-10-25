package server

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// Unit tier: the real client against the real routes with a stub store.
func TestClientWritesAgreeWithTheRoutes(t *testing.T) {
	t.Parallel()
	var nextErr error
	st := writeStub{
		create: func(_ context.Context, p api.CreateParams) (api.Issue, error) {
			return api.Issue{ID: "gt-new", Title: p.Title, CreatedBy: p.Actor}, nextErr
		},
		update: func(context.Context, string, api.UpdatePatch, api.Guard) error { return nextErr },
		close:  func(context.Context, []string, string, bool) error { return nextErr },
		del:    func(context.Context, []string) error { return nextErr },
	}
	ts := writeServer(t, st)
	c, err := client.New(client.Config{BaseURL: ts.URL, Token: "good-token", SchemaLevel: 3})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	is, err := c.Create(ctx, api.CreateParams{Title: "t", Actor: "mallory"})
	if err != nil || is.ID != "gt-new" || is.CreatedBy != tokenActor {
		t.Errorf("Create = %+v, %v; want the issue created by the token's actor", is, err)
	}
	title := "x"
	for name, call := range map[string]func() error{
		"Update": func() error { return c.Update(ctx, "gt-a", api.UpdatePatch{Title: &title}, api.Guard{}) },
		"Close":  func() error { return c.Close(ctx, []string{"gt-a"}, "r", false) },
		"Delete": func() error { return c.Delete(ctx, []string{"gt-a"}) },
	} {
		nextErr = nil
		if err := call(); err != nil {
			t.Errorf("%s success = %v, want nil on 204", name, err)
		}
		for sentinel, e := range map[*api.Error]error{
			api.ErrGuardFailed: api.GuardFailed("held"), api.ErrNotFound: api.NotFound("gt-a"),
			api.ErrConflict: api.Conflict("fence"), api.ErrInvalid: api.Invalid("bad"),
		} {
			nextErr = e
			if err := call(); !errors.Is(err, sentinel) {
				t.Errorf("%s with %v = %v, want errors.Is %v", name, e, err, sentinel.Kind)
			}
		}
	}
	nextErr = api.Conflict("dup")
	if _, err := c.Create(ctx, api.CreateParams{Title: "t"}); !errors.Is(err, api.ErrConflict) {
		t.Errorf("Create conflict = %v, want api.ErrConflict", err)
	}
}
