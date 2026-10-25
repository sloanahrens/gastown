package contract

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "comments come back oldest first with their author and text", Run: commentsRoundTrip})
	register(Case{Name: "a comment needs an author, text and an existing issue", Run: commentsRefusals})
	register(Case{Name: "a comment counts as activity and shows in the list count", Run: commentsActivityAndCount})
}

func commentsRoundTrip(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t"})
	for _, c := range []struct{ author, text string }{{"alice", "first"}, {"bob", "second"}, {"alice", "third"}} {
		if err := s.AddComment(ctx(), is.ID, c.author, c.text); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Comments(ctx(), is.ID)
	if err != nil || len(got) != 3 {
		t.Fatalf("Comments = %v, %v; want 3", got, err)
	}
	for i, want := range []struct{ author, text string }{{"alice", "first"}, {"bob", "second"}, {"alice", "third"}} {
		if got[i].Author != want.author || got[i].Text != want.text || got[i].IssueID != is.ID || got[i].ID == "" || got[i].CreatedAt.IsZero() {
			t.Errorf("comment %d = %+v, want %+v on %s", i, got[i], want, is.ID)
		}
	}
	if got[0].ID == got[1].ID {
		t.Error("two comments share an id")
	}
	empty := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "quiet"})
	if got, err := s.Comments(ctx(), empty.ID); err != nil || got == nil || len(got) != 0 {
		t.Errorf("Comments of an issue with none = %#v, %v; want an empty non-nil slice", got, err)
	}
}

func commentsRefusals(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t"})
	if err := s.AddComment(ctx(), is.ID, "", "text"); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("no author = %v, want api.ErrInvalid", err)
	}
	if err := s.AddComment(ctx(), is.ID, "me", "  "); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("empty text = %v, want api.ErrInvalid", err)
	}
	if err := s.AddComment(ctx(), "gt-nope", "me", "hi"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("missing issue = %v, want api.ErrNotFound", err)
	}
	if _, err := s.Comments(ctx(), "gt-nope"); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("Comments of a missing issue = %v, want api.ErrNotFound", err)
	}
}

func commentsActivityAndCount(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "t"})
	if err := s.AddComment(ctx(), is.ID, "me", "hi"); err != nil {
		t.Fatal(err)
	}
	if got := mustGet(t, s, is.ID); !got.UpdatedAt.After(is.UpdatedAt) {
		t.Errorf("UpdatedAt %v not after %v: a comment is activity", got.UpdatedAt, is.UpdatedAt)
	}
	page := mustList(t, s, api.ListFilter{})
	if len(page.Issues) != 1 || page.Issues[0].CommentCount != 1 {
		t.Errorf("listed %+v, want CommentCount 1", page.Issues)
	}
}
