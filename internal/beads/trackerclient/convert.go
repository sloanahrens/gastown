package trackerclient

import (
	"encoding/json"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

func ts(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func tsPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ts(*t)
}

// issue converts the wire issue to a beads.Issue.
func issue(a api.Issue) *beads.Issue {
	return &beads.Issue{
		Dependencies: deps(a.Dependencies), Dependents: deps(a.Dependents), BlockedByCount: len(a.BlockedBy),
		ID: a.ID, Title: a.Title, Description: a.Description, Design: a.Design, Notes: a.Notes,
		Status: a.Status, Priority: a.Priority, Type: a.Type,
		CreatedAt: ts(a.CreatedAt), CreatedBy: a.CreatedBy, UpdatedAt: ts(a.UpdatedAt), ClosedAt: tsPtr(a.ClosedAt),
		DeferUntil:  tsPtr(a.DeferUntil),
		CloseReason: a.CloseReason, Parent: a.Parent, Assignee: a.Assignee,
		Children: a.Children, DependsOn: a.DependsOn, BlockedBy: a.BlockedBy, Labels: a.Labels,
		Ephemeral: a.Ephemeral, WispType: a.WispType, EventKind: a.EventKind, Payload: compactJSON(a.Payload),
		AcceptanceCriteria: a.AcceptanceCriteria,
		HookBead:           a.HookBead, AgentState: a.AgentState, LastActivity: tsPtr(a.LastActivity),
		CommentCount: a.CommentCount, DependencyCount: a.DependencyCount, DependentCount: a.DependentCount,
	}
}

// compactJSON returns raw as bd does: compact, with object keys sorted. The
// tracker stores a payload as JSONB, which keeps neither the spacing nor the
// order a caller wrote. Text that is not JSON is returned as it is.
func compactJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var v any
	if json.Unmarshal([]byte(raw), &v) != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(b)
}

func issues(in []api.Issue) []*beads.Issue {
	out := make([]*beads.Issue, len(in))
	for i := range in {
		out[i] = issue(in[i])
	}
	return out
}

func issueMap(in map[string]api.Issue) map[string]*beads.Issue {
	out := make(map[string]*beads.Issue, len(in))
	for id, a := range in {
		out[id] = issue(a)
	}
	return out
}

func comments(in []api.Comment) []beads.Comment {
	out := make([]beads.Comment, len(in))
	for i, c := range in {
		out[i] = beads.Comment{ID: c.ID, IssueID: c.IssueID, Author: c.Author, Text: c.Text, CreatedAt: ts(c.CreatedAt)}
	}
	return out
}

// deps converts the wire links to the seam's, keeping every one. A link to a
// target outside this database (external:<rig>:<id>) arrives id-only — the
// reference, the relation and the link's own record, with the target's fields
// empty — so Show and DepList answer the whole dependency set. bd's own reads
// leave such a link out; a consumer must not read the empty fields as a target.
func deps(in []api.IssueDep) []beads.IssueDep {
	out := make([]beads.IssueDep, len(in))
	for i, d := range in {
		out[i] = beads.IssueDep{
			ID: d.ID, Title: d.Title, Status: d.Status, Priority: d.Priority, Type: d.Type,
			DependencyType: d.DependencyType, CloseReason: d.CloseReason,
			CreatedAt: ts(d.CreatedAt), CreatedBy: d.CreatedBy, Metadata: compactJSON(string(d.Metadata)),
		}
	}
	return out
}
