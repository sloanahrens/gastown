// Package storetest holds the in-memory store.Store for the unit tier. It
// models the behavior the contract pins, nothing more: methods whose beads
// have not landed return store.ErrUnimplemented, like Postgres.
package storetest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

// Fake is an in-memory store.Store.
type Fake struct {
	mu     sync.Mutex
	now    func() time.Time
	issues map[string]api.Issue
	next   int
}

var _ store.Store = (*Fake)(nil)

// NewFake returns an empty Fake using the real clock.
func NewFake() *Fake { return NewFakeWithClock(time.Now) }

// NewFakeWithClock returns an empty Fake whose timestamps come from now.
func NewFakeWithClock(now func() time.Time) *Fake {
	return &Fake{now: now, issues: map[string]api.Issue{}}
}

// rigOf is the rig prefix of an id: the text before its first dash.
func rigOf(id string) string {
	rig, _, ok := strings.Cut(id, "-")
	if !ok {
		return ""
	}
	return rig
}

func (f *Fake) CreateIssue(_ context.Context, p api.CreateParams) (api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.TrimSpace(p.Title) == "" {
		return api.Issue{}, api.Invalid("title is required")
	}
	id := p.ID
	switch {
	case id != "":
		rig := rigOf(id)
		if rig == "" {
			return api.Issue{}, api.Invalid("id %q has no rig prefix", id)
		}
		if p.Rig != "" && p.Rig != rig {
			return api.Issue{}, api.Invalid("id %q does not belong to rig %q", id, p.Rig)
		}
	case p.Rig != "":
		f.next++
		id = fmt.Sprintf("%s-%05d", p.Rig, f.next)
	default:
		return api.Issue{}, api.Invalid("a rig or an id is required")
	}
	if _, dup := f.issues[id]; dup {
		return api.Issue{}, api.Conflict("issue %s already exists", id)
	}
	typ := p.Type
	if typ == "" {
		typ = "task"
	}
	labels := slices.Clone(p.Labels)
	slices.Sort(labels)
	now := f.now()
	is := api.Issue{
		ID: id, Title: p.Title, Description: p.Description, AcceptanceCriteria: p.Acceptance,
		Status: "open", Priority: p.Priority, Type: typ, Assignee: p.Assignee, CreatedBy: p.Actor,
		CreatedAt: now, UpdatedAt: now, Parent: p.Parent, Labels: labels, Ephemeral: p.Ephemeral,
		EventKind: p.EventKind, Payload: p.EventPayload,
	}
	f.issues[id] = is
	return is, nil
}

func (f *Fake) GetIssues(_ context.Context, ids []string) (map[string]api.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]api.Issue{}
	for _, id := range ids {
		if is, ok := f.issues[id]; ok {
			out[id] = is
		}
	}
	return out, nil
}

func unimplemented(method string) error {
	return fmt.Errorf("%w: Fake.%s", store.ErrUnimplemented, method)
}

func (*Fake) ListIssues(context.Context, api.ListFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("ListIssues")
}
func (*Fake) Assigned(context.Context, api.AssignedRequest) (api.AssignedPage, error) {
	return api.AssignedPage{}, unimplemented("Assigned")
}
func (*Fake) UpdateIssue(context.Context, string, api.UpdatePatch, api.Guard) error {
	return unimplemented("UpdateIssue")
}
func (*Fake) CloseIssues(context.Context, []string, string, bool) error {
	return unimplemented("CloseIssues")
}
func (*Fake) DeleteIssues(context.Context, []string) error { return unimplemented("DeleteIssues") }
func (*Fake) AddComment(context.Context, string, string, string) error {
	return unimplemented("AddComment")
}
func (*Fake) Comments(context.Context, string) ([]api.Comment, error) {
	return nil, unimplemented("Comments")
}
func (*Fake) AddDependency(context.Context, string, string, string) error {
	return unimplemented("AddDependency")
}
func (*Fake) RemoveDependency(context.Context, string, string) error {
	return unimplemented("RemoveDependency")
}
func (*Fake) Dependencies(context.Context, string, string) ([]api.IssueDep, error) {
	return nil, unimplemented("Dependencies")
}
func (*Fake) Ready(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Ready")
}
func (*Fake) Blocked(context.Context, api.ReadyFilter) (api.IssuePage, error) {
	return api.IssuePage{}, unimplemented("Blocked")
}
func (*Fake) ChildrenOf(context.Context, []string) (map[string][]api.Issue, error) {
	return nil, unimplemented("ChildrenOf")
}
func (*Fake) CreateGraph(context.Context, api.GraphRequest) (api.GraphResult, error) {
	return api.GraphResult{}, unimplemented("CreateGraph")
}
func (*Fake) DispatchBoard(context.Context, api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
	return api.DispatchBoardResponse{}, unimplemented("DispatchBoard")
}
func (*Fake) Stats(context.Context, string) (api.Stats, error) {
	return api.Stats{}, unimplemented("Stats")
}
func (*Fake) ConfigGet(context.Context, string) (string, bool, error) {
	return "", false, unimplemented("ConfigGet")
}
func (*Fake) ConfigSet(context.Context, string, string) error { return unimplemented("ConfigSet") }
func (*Fake) EventsAfter(context.Context, int64, int) (api.EventsPage, error) {
	return api.EventsPage{}, unimplemented("EventsAfter")
}
