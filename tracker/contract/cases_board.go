package contract

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "the dispatch board answers ready, held and children per rig in one call", Run: boardPerRig})
	register(Case{Name: "the dispatch board applies the exclude lists and carries full rows", Run: boardExcludesAndFullRows})
	register(Case{Name: "the dispatch board reports an unknown rig and still answers the rest", Run: boardUnknownRig})
	register(Case{Name: "the dispatch board probes statuses and marks a missing id not found", Run: boardProbes})
	register(Case{Name: "the dispatch board refuses a request with no rig", Run: boardNoRigs})
	register(Case{Name: "the dispatch board returns every ready issue past one page", Run: boardPastOnePage})
}

func mustBoard(t *testing.T, s store.Store, req api.DispatchBoardRequest) api.DispatchBoardResponse {
	t.Helper()
	resp, err := s.DispatchBoard(ctx(), req)
	if err != nil {
		t.Fatalf("DispatchBoard(%+v): %v", req, err)
	}
	return resp
}

func boardIDs(is []api.Issue) []string {
	out := make([]string, len(is))
	for i, x := range is {
		out[i] = x.ID
	}
	return out
}

func boardPerRig(t *testing.T, s store.Store) {
	epic := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "container", Type: "epic"})
	kidOpen := mustCreate(t, s, api.CreateParams{Parent: epic.ID, Title: "open child"})
	kidDone := mustCreate(t, s, api.CreateParams{Parent: epic.ID, Title: "done child"})
	wisp := mustCreate(t, s, api.CreateParams{Parent: epic.ID, Title: "wisp child", Ephemeral: true})
	if err := s.CloseIssues(ctx(), []string{kidDone.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	held := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "held", Description: "touches a.go", Assignee: "polecat-a"})
	mustUpdate(t, s, held.ID, api.UpdatePatch{Status: str("in_progress"), Actor: "polecat-a"}, api.Guard{})
	inProgressNoOwner := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "in progress, nobody"})
	mustUpdate(t, s, inProgressNoOwner.ID, api.UpdatePatch{Status: str("in_progress")}, api.Guard{})
	other := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq ready"})
	mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq assigned", Assignee: "x"})

	resp := mustBoard(t, s, api.DispatchBoardRequest{Rigs: []string{"gt", "hq"}, HeldStatuses: []string{"hooked", "in_progress"}})
	gt, hq := resp.Rigs["gt"], resp.Rigs["hq"]
	sameSet(t, "gt ready (the in-progress ones are not ready)", boardIDs(gt.Ready), epic.ID, kidOpen.ID)
	sameSet(t, "hq ready (unassigned only)", boardIDs(hq.Ready), other.ID)
	if len(gt.Held) != 1 || gt.Held[0].ID != held.ID || gt.Held[0].Assignee != "polecat-a" || gt.Held[0].Description != "touches a.go" {
		t.Errorf("gt held = %+v, want only the assigned in-progress issue with its description", gt.Held)
	}
	kids := gt.Children[epic.ID]
	if len(kids) != 3 {
		t.Fatalf("children of the container = %+v, want its three children (open, closed and wisp)", kids)
	}
	statuses := map[string]string{}
	for _, k := range kids {
		statuses[k.ID] = k.Status
	}
	if statuses[kidOpen.ID] != "open" || statuses[kidDone.ID] != "closed" || statuses[wisp.ID] == "" {
		t.Errorf("child statuses = %v", statuses)
	}
	if len(gt.Children) != 1 {
		t.Errorf("children map = %v, want only the ready issue that has children", gt.Children)
	}
	if hq.Held == nil || hq.Children == nil || len(hq.Held) != 0 || len(hq.Children) != 0 {
		t.Errorf("hq held/children = %v / %v, want empty and non-nil", hq.Held, hq.Children)
	}
	if resp.Probes == nil || resp.Errors == nil || len(resp.Errors) != 0 {
		t.Errorf("probes/errors = %v / %v, want empty non-nil maps", resp.Probes, resp.Errors)
	}
}

func boardExcludesAndFullRows(t *testing.T, s store.Store) {
	plain := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "plain", Description: "## Goal\nbody", Labels: []string{"keep"}})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "labeled", Labels: []string{"gt:agent"}})
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "a bug", Type: "bug"})
	resp := mustBoard(t, s, api.DispatchBoardRequest{Rigs: []string{"gt"}, ExcludeLabels: []string{"gt:agent"}, ExcludeTypes: []string{"bug"}})
	ready := resp.Rigs["gt"].Ready
	if len(ready) != 1 || ready[0].ID != plain.ID {
		t.Fatalf("ready = %v, want only the plain issue after both exclude lists", boardIDs(ready))
	}
	if ready[0].Description != "## Goal\nbody" || len(ready[0].Labels) != 1 || ready[0].Labels[0] != "keep" || ready[0].Title != "plain" || ready[0].CreatedAt.IsZero() {
		t.Errorf("row = %+v, want the full description, labels, title and creation time", ready[0])
	}
}

func boardUnknownRig(t *testing.T, s store.Store) {
	mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "one"})
	resp := mustBoard(t, s, api.DispatchBoardRequest{Rigs: []string{"gt", "nope"}})
	if len(resp.Rigs["gt"].Ready) != 1 {
		t.Errorf("gt = %+v, want its ready issue despite the unknown rig", resp.Rigs["gt"])
	}
	if _, in := resp.Rigs["nope"]; in || resp.Errors["nope"] == "" {
		t.Errorf("rigs = %v errors = %v, want nope reported in Errors and not in Rigs", resp.Rigs, resp.Errors)
	}
}

func boardProbes(t *testing.T, s store.Store) {
	open := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "open"})
	done := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "done"})
	if err := s.CloseIssues(ctx(), []string{done.ID}, "r", false); err != nil {
		t.Fatal(err)
	}
	resp := mustBoard(t, s, api.DispatchBoardRequest{Rigs: []string{"gt"}, ProbeIDs: []string{open.ID, done.ID, "gt-missing", open.ID}})
	if p := resp.Probes[open.ID]; !p.Found || p.Status != "open" {
		t.Errorf("probe open = %+v", p)
	}
	if p := resp.Probes[done.ID]; !p.Found || p.Status != "closed" {
		t.Errorf("probe closed = %+v", p)
	}
	if p, ok := resp.Probes["gt-missing"]; !ok || p.Found {
		t.Errorf("probe missing = %+v, in map %v; want present with Found=false", p, ok)
	}
}

func boardNoRigs(t *testing.T, s store.Store) {
	if _, err := s.DispatchBoard(ctx(), api.DispatchBoardRequest{}); !errors.Is(err, api.ErrInvalid) {
		t.Errorf("DispatchBoard with no rig = %v, want api.ErrInvalid", err)
	}
}

func boardPastOnePage(t *testing.T, s store.Store) {
	const n = 1003 // more than one page of 1000
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		is := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: fmt.Sprintf("bulk %d", i)})
		want[is.ID] = true
	}
	got := mustBoard(t, s, api.DispatchBoardRequest{Rigs: []string{"gt"}}).Rigs["gt"].Ready
	if len(got) != n {
		t.Fatalf("ready = %d issues, want all %d (the board must not stop at a page)", len(got), n)
	}
	for _, is := range got {
		if !want[is.ID] {
			t.Fatalf("unexpected issue %s", is.ID)
		}
		delete(want, is.ID)
	}
}
