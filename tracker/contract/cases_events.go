package contract

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "events record a lifecycle in order and outlive a delete", Run: eventsLifecycle})
	register(Case{Name: "events page by seq and say where the feed starts", Run: eventsPaging})
	register(Case{Name: "events record the actor from the request context", Run: eventsActor})
}

func eventsFor(t *testing.T, s store.Store, id string) []api.Event {
	t.Helper()
	page, err := s.EventsAfter(ctx(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []api.Event
	for _, e := range page.Events {
		if e.IssueID == id {
			out = append(out, e)
		}
	}
	return out
}

func ops(es []api.Event) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Op
	}
	return out
}

func eventsLifecycle(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a", Labels: []string{"x", "y"}, Actor: "alice"})
	b := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "b"})
	mustUpdate(t, s, a.ID, api.UpdatePatch{Status: str("in_progress"), Title: str("a2"), AddLabels: []string{"z"}, Actor: "alice"}, api.Guard{})
	if err := s.AddComment(ctx(), a.ID, "bob", "hi"); err != nil {
		t.Fatal(err)
	}
	mustDep(t, s, a.ID, b.ID, "tracks")
	if err := s.RemoveDependency(ctx(), a.ID, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseIssues(api.WithActor(ctx(), "carol"), []string{a.ID}, "done", false); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, a.ID, api.UpdatePatch{Status: str("open"), Actor: "dave"}, api.Guard{})

	got := eventsFor(t, s, a.ID)
	want := []string{"created", "label_added", "label_added", "updated", "status_changed", "label_added", "commented", "dependency_added", "dependency_removed", "closed", "reopened"}
	if !slices.Equal(ops(got), want) {
		t.Fatalf("ops = %v\nwant  %v", ops(got), want)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq %d is not after %d", got[i].Seq, got[i-1].Seq)
		}
	}
	byOp := func(op string) api.Event {
		for _, e := range got {
			if e.Op == op {
				return e
			}
		}
		t.Fatalf("no %s event", op)
		return api.Event{}
	}
	if e := byOp("status_changed"); e.OldValue == nil || *e.OldValue != "open" || e.NewValue == nil || *e.NewValue != "in_progress" || e.Actor != "alice" {
		t.Errorf("status_changed = %+v, want open -> in_progress by alice", e)
	}
	if e := byOp("closed"); e.Actor != "carol" || e.Comment == nil || *e.Comment != "done" {
		t.Errorf("closed = %+v, want carol with the reason as its comment", e)
	}
	if e := byOp("commented"); e.Actor != "bob" {
		t.Errorf("commented actor = %q, want bob", e.Actor)
	}
	if e := byOp("reopened"); e.Actor != "dave" {
		t.Errorf("reopened actor = %q, want dave", e.Actor)
	}
	if err := s.DeleteIssues(ctx(), []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	after := eventsFor(t, s, a.ID)
	if len(after) != len(want)+1 || after[len(after)-1].Op != "deleted" {
		t.Errorf("after delete: %v, want every earlier event kept and a final deleted", ops(after))
	}
}

func eventsPaging(t *testing.T, s store.Store) {
	if page, err := s.EventsAfter(ctx(), 0, 0); err != nil || len(page.Events) != 0 || page.FirstSeq != 0 || page.NextAfter != 0 || page.Events == nil {
		t.Fatalf("an empty feed = %+v, %v; want no events (non-nil), FirstSeq 0, NextAfter 0", page, err)
	}
	for i := 0; i < 5; i++ {
		mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "n"})
	}
	var all []int64
	after := int64(0)
	for pages := 0; pages < 10; pages++ {
		page, err := s.EventsAfter(ctx(), after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if page.FirstSeq <= 0 {
			t.Errorf("FirstSeq = %d, want the oldest stored seq", page.FirstSeq)
		}
		for _, e := range page.Events {
			all = append(all, e.Seq)
		}
		if len(page.Events) == 0 {
			if page.NextAfter != after {
				t.Errorf("an empty page moved NextAfter from %d to %d", after, page.NextAfter)
			}
			break
		}
		after = page.NextAfter
	}
	if len(all) != 5 || !slices.IsSorted(all) || len(slices.Compact(slices.Clone(all))) != 5 {
		t.Errorf("paged seqs = %v, want 5 distinct ascending", all)
	}
}

func eventsActor(t *testing.T, s store.Store) {
	a := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a"})
	var bg context.Context = api.WithActor(ctx(), "town/crew/x")
	if err := s.DeleteIssues(bg, []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	es := eventsFor(t, s, a.ID)
	if last := es[len(es)-1]; last.Op != "deleted" || last.Actor != "town/crew/x" {
		t.Errorf("last event = %+v, want deleted by the context's actor", last)
	}
}

func init() {
	register(Case{Name: "an event carries the issue's status after the change, and none after a delete", Run: eventsCarryStatus})
}

func eventStatus(t *testing.T, e api.Event) string {
	t.Helper()
	if len(e.Payload) == 0 {
		return ""
	}
	var p struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		t.Fatalf("event %d payload %s is not JSON: %v", e.Seq, e.Payload, err)
	}
	return p.Status
}

func eventsCarryStatus(t *testing.T, s store.Store) {
	is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "journaled"})
	if err := s.CloseIssues(ctx(), []string{is.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	mustUpdate(t, s, is.ID, api.UpdatePatch{Status: str("open")}, api.Guard{})
	mustUpdate(t, s, is.ID, api.UpdatePatch{Title: str("renamed")}, api.Guard{})
	if err := s.DeleteIssues(ctx(), []string{is.ID}); err != nil {
		t.Fatal(err)
	}
	page, err := s.EventsAfter(ctx(), 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range page.Events {
		if e.IssueID == is.ID {
			got = append(got, e.Op+":"+eventStatus(t, e))
		}
	}
	want := []string{"created:open", "closed:closed", "reopened:open", "updated:open", "deleted:"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("events for %s = %v, want %v (status after each change; a delete has none)", is.ID, got, want)
	}
}
