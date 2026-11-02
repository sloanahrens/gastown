package contract

import (
	"errors"
	"maps"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "comments come back oldest first with their author and text", Run: commentsRoundTrip})
	register(Case{Name: "a comment needs an author, text and an existing issue", Run: commentsRefusals})
	register(Case{Name: "a comment counts as activity and shows in the list count", Run: commentsActivityAndCount})
	register(Case{Name: "comment counts for many issues in one read, refusing an id the store does not hold", Fake: true, Run: commentCountsBatch})
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

func commentCountsBatch(t *testing.T, s store.Store) {
	busy := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "busy"})
	quiet := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "quiet"})
	other := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "other"})
	for _, text := range []string{"one", "two", "three"} {
		if err := s.AddComment(ctx(), busy.ID, "me", text); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddComment(ctx(), other.ID, "me", "not named"); err != nil {
		t.Fatal(err)
	}
	got, err := s.CommentCounts(ctx(), []string{busy.ID, quiet.ID})
	if err != nil {
		t.Fatalf("CommentCounts: %v", err)
	}
	if want := map[string]int{busy.ID: 3, quiet.ID: 0}; !maps.Equal(got, want) {
		t.Errorf("CommentCounts = %v, want %v", got, want)
	}
	if err := s.AddComment(ctx(), busy.ID, "me", "four"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.CommentCounts(ctx(), []string{busy.ID}); err != nil || got[busy.ID] != 4 {
		t.Errorf("CommentCounts after a new comment = %v, %v; want %s at 4", got, err, busy.ID)
	}
	if got, err := s.CommentCounts(ctx(), nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("CommentCounts(nil) = %#v, %v; want an empty non-nil map", got, err)
	}
	if got, err := s.CommentCounts(ctx(), []string{"", ""}); err != nil || len(got) != 0 {
		t.Errorf("CommentCounts of empty ids = %v, %v; want an empty map", got, err)
	}
	if _, err := s.CommentCounts(ctx(), []string{"gt-nope"}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("CommentCounts of a missing id = %v, want api.ErrNotFound", err)
	}
	if _, err := s.CommentCounts(ctx(), []string{busy.ID, "gt-nope"}); !errors.Is(err, api.ErrNotFound) {
		t.Errorf("CommentCounts with one missing id = %v, want api.ErrNotFound", err)
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
