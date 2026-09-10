package cmd

import (
	"context"
	"fmt"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

type priorFindingsStore struct {
	beadsdk.Storage
	issues map[string]*beadsdk.Issue
}

func (s *priorFindingsStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return issue, nil
}

func (s *priorFindingsStore) GetLabels(_ context.Context, id string) ([]string, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return append([]string(nil), issue.Labels...), nil
}

func (s *priorFindingsStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	if _, ok := s.issues[id]; !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return nil, nil
}

func TestBuildPriorFindings_ParsesIDSevPathLineTitleLines(t *testing.T) {
	notes := `Findings from the last rejection:
- id:abc123456789 sev:major internal/foo.go:42 — leaky abstraction
- id:def987654321 sev:minor internal/bar.go:7 — missing test
not a finding line
`
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{
		"gt-source": {ID: "gt-source", Notes: notes, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}
	bd := beads.NewWithStore(t.TempDir(), store)

	got := buildPriorFindings(bd, "gt-source", 3)

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0].ID != "abc123456789" || got[0].Severity != "major" || got[0].Path != "internal/foo.go" || got[0].Line != 42 || got[0].Title != "leaky abstraction" {
		t.Errorf("finding[0] = %+v, unexpected", got[0])
	}
	if got[1].ID != "def987654321" || got[1].Severity != "minor" {
		t.Errorf("finding[1] = %+v, unexpected", got[1])
	}
	for _, f := range got {
		if f.Attempt != 3 {
			t.Errorf("finding %+v: Attempt = %d, want 3", f, f.Attempt)
		}
	}
}

func TestBuildPriorFindings_NoSourceIssueReturnsNil(t *testing.T) {
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{}}
	bd := beads.NewWithStore(t.TempDir(), store)

	if got := buildPriorFindings(bd, "", 1); got != nil {
		t.Errorf("expected nil for empty sourceIssue, got %+v", got)
	}
	if got := buildPriorFindings(bd, "gt-missing", 1); got != nil {
		t.Errorf("expected nil for a source issue that can't be shown, got %+v", got)
	}
}
