package daemon

import (
	"context"
	"fmt"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
)

// libraryIssueSource reads the convoy issue surface from the in-process
// beadsdk.Storage the daemon still holds. It is TEMPORARY: it exists so
// internal/convoy can be off the beads library before the daemon's own stores
// are, and the slice that opens beads.Client stores in the daemon
// (gt-7iwy0.3.2) deletes this file with that import.
type libraryIssueSource struct {
	ctx   context.Context
	store beadsdk.Storage
}

var _ convoy.IssueSource = libraryIssueSource{}

// librarySource wraps one store for the convoy reads.
func librarySource(ctx context.Context, store beadsdk.Storage) convoy.IssueSource {
	return libraryIssueSource{ctx: ctx, store: store}
}

// librarySources wraps every store in the daemon's map, skipping empty ones.
func librarySources(ctx context.Context, stores map[string]beadsdk.Storage) map[string]convoy.IssueSource {
	out := make(map[string]convoy.IssueSource, len(stores))
	for name, store := range stores {
		if store != nil {
			out[name] = librarySource(ctx, store)
		}
	}
	return out
}

// dependencyRecordStore reads a bead's raw dependency edges, whatever their
// target. beadsdk.Storage does not declare it; the Dolt store the daemon
// opens does. This mirrors internal/convoy's own read, which the bd-backed
// ClientSource answers from the dependencies table.
type dependencyRecordStore interface {
	GetDependencyRecords(ctx context.Context, issueID string) ([]*beadsdk.Dependency, error)
}

func (s libraryIssueSource) Show(id string) (*beads.Issue, error) {
	issue, err := s.store.GetIssue(s.ctx, id)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("%s: %w", id, beads.ErrNotFound)
	}
	return libraryIssue(issue), nil
}

func (s libraryIssueSource) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	issues, err := s.store.GetIssuesByIDs(s.ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*beads.Issue, len(issues))
	for _, issue := range issues {
		if issue != nil {
			out[issue.ID] = libraryIssue(issue)
		}
	}
	return out, nil
}

func (s libraryIssueSource) Comments(id string) ([]beads.Comment, error) {
	comments, err := s.store.GetIssueComments(s.ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]beads.Comment, 0, len(comments))
	for _, c := range comments {
		if c == nil {
			continue
		}
		out = append(out, beads.Comment{
			ID:        c.ID,
			IssueID:   c.IssueID,
			Author:    c.Author,
			Text:      c.Text,
			CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}

// Deps reads the raw dependency edges: the up direction from the joined
// dependents view, which carries the relation, and the down direction from
// the store's raw records, which keep every cross-rig target the joined view
// drops (gt-j02xy).
func (s libraryIssueSource) Deps(issueID, direction, depType string) ([]string, error) {
	if direction == "up" {
		dependents, err := s.store.GetDependentsWithMetadata(s.ctx, issueID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(dependents))
		for _, d := range dependents {
			if d != nil && (depType == "" || string(d.DependencyType) == depType) {
				ids = append(ids, d.ID)
			}
		}
		return ids, nil
	}

	reader, ok := s.store.(dependencyRecordStore)
	if !ok {
		return nil, fmt.Errorf("store %T cannot read raw dependency records", s.store)
	}
	records, err := reader.GetDependencyRecords(s.ctx, issueID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(records))
	for _, d := range records {
		if d != nil && (depType == "" || string(d.Type) == depType) {
			ids = append(ids, d.DependsOnID)
		}
	}
	return ids, nil
}

func (s libraryIssueSource) TrackedBy(target string) ([]string, error) {
	return s.Deps(target, "up", "tracks")
}

// Close is a no-op: the daemon owns the store and releases it itself.
func (s libraryIssueSource) Close() error { return nil }

// libraryIssue projects a beadsdk.Issue onto the beads.Issue fields the
// convoy reads consume.
func libraryIssue(issue *beadsdk.Issue) *beads.Issue {
	return &beads.Issue{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Design:      issue.Design,
		Notes:       issue.Notes,
		Status:      string(issue.Status),
		Priority:    issue.Priority,
		Type:        string(issue.IssueType),
		Assignee:    issue.Assignee,
		CloseReason: issue.CloseReason,
		Labels:      issue.Labels,
	}
}
