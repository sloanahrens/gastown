package convoy

import (
	"context"
	"fmt"
	"testing"

	beadsdk "github.com/steveyegge/beads"
)

// The writes below let the tests build a store the way they would build a
// real one; the reads the convoy code makes are in xrig_blockers_test.go.
// TestIntegrationFakeStoreMatchesDolt pins the joins against a real store.

// setupTestStore is an empty in-memory store, written through the same calls
// a Dolt store takes. The returned cleanup is a no-op kept for the callers.
func setupTestStore(t *testing.T) (*fakeRigStore, func()) {
	t.Helper()
	return newFakeRigStore(), func() {}
}

func (s *fakeRigStore) CreateIssue(_ context.Context, issue *beadsdk.Issue, _ string) error {
	if _, ok := s.issues[issue.ID]; ok {
		return fmt.Errorf("issue %s already exists", issue.ID)
	}
	cp := *issue
	s.issues[issue.ID] = &cp
	return nil
}

func (s *fakeRigStore) AddDependency(_ context.Context, dep *beadsdk.Dependency, _ string) error {
	cp := *dep
	s.deps = append(s.deps, &cp)
	return nil
}

// UpdateIssue applies the one update the tests make: a status change.
func (s *fakeRigStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	iss, ok := s.issues[id]
	if !ok {
		return fmt.Errorf("issue %s not found", id)
	}
	for k, v := range updates {
		if k != "status" {
			return fmt.Errorf("fake store: unsupported update %q", k)
		}
		iss.Status = beadsdk.Status(fmt.Sprint(v))
	}
	return nil
}

func (s *fakeRigStore) AddLabel(_ context.Context, issueID, label, _ string) error {
	iss, ok := s.issues[issueID]
	if !ok {
		return fmt.Errorf("issue %s not found", issueID)
	}
	iss.Labels = append(iss.Labels, label)
	return nil
}

// GetDependentsWithMetadata returns the issues with an edge to issueID. The
// target is matched exactly, so an "external:<prefix>:<id>" edge is found
// only under that form, as in the Dolt store.
func (s *fakeRigStore) GetDependentsWithMetadata(_ context.Context, issueID string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	var out []*beadsdk.IssueWithDependencyMetadata
	for _, d := range s.deps {
		if d.DependsOnID != issueID {
			continue
		}
		if iss, ok := s.issues[d.IssueID]; ok {
			out = append(out, &beadsdk.IssueWithDependencyMetadata{Issue: *iss, DependencyType: d.Type})
		}
	}
	return out, nil
}
