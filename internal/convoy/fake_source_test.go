package convoy

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// depEdge is one dependency edge a test seeds: from depends on to by typ.
type depEdge struct{ from, to, typ string }

// depSpec is a dependency edge as a test seeds it: the issue, the target it
// depends on, and the relation.
type depSpec struct {
	IssueID     string
	DependsOnID string
	Type        string
	CreatedAt   string
	CreatedBy   string
}

// fakeStore is the convoy tests' store: a beadsfake database read through
// ClientSource, plus the writes a test seeds it with. The raw dependency
// statements ClientSource sends are answered from the edges the test records,
// because the fake's SQLCSV is scripted, not a real database.
type fakeStore struct {
	db      *beadsfake.Fake
	source  IssueSource
	edges   []depEdge
	hidden  map[string]bool // issues the store forgot, per forget
	readErr error           // when set, every read fails: a store whose Dolt went away
}

// newFakeStore is an empty fake town database (prefix hq) behind
// ClientSource.
func newFakeStore() *fakeStore {
	s := &fakeStore{db: beadsfake.New(beadsfake.WithPrefix("hq"))}
	s.db.OnSQL(func(query string) ([][]string, error) { return answerRawDeps(s.edges, query) })
	s.source = ClientSource(s.db)
	return s
}

// newFakeRigStore is a fake store holding issues.
func newFakeRigStore(issues ...*beads.Issue) *fakeStore {
	s := newFakeStore()
	for _, iss := range issues {
		s.db.Seed(*iss)
	}
	return s
}

// setupTestStore is an empty in-memory store, the shape the convoy tests
// seed. The returned cleanup is a no-op kept for the callers.
func setupTestStore(t *testing.T) (*fakeStore, func()) {
	t.Helper()
	return newFakeStore(), func() {}
}

// holdStore is a fake store seeded with the records the hold rule reads.
func holdStore(issues ...*beads.Issue) *fakeStore {
	s := newFakeStore()
	for _, iss := range issues {
		s.seed(*iss)
	}
	return s
}

func (s *fakeStore) Show(id string) (*beads.Issue, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.hidden[id] {
		return nil, fmt.Errorf("%s: %w", id, beads.ErrNotFound)
	}
	return s.source.Show(id)
}

func (s *fakeStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	issues, err := s.source.ShowMultiple(ids)
	if err != nil {
		return nil, err
	}
	for id := range s.hidden {
		delete(issues, id)
	}
	return issues, nil
}

// forget hides a seeded issue: the store stops answering for it, the way a
// hard-deleted bead leaves its dangling edges behind.
func (s *fakeStore) forget(id string) {
	if s.hidden == nil {
		s.hidden = map[string]bool{}
	}
	s.hidden[id] = true
}

func (s *fakeStore) Comments(id string) ([]beads.Comment, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.source.Comments(id)
}

func (s *fakeStore) Deps(issueID, direction, depType string) ([]string, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.source.Deps(issueID, direction, depType)
}

func (s *fakeStore) TrackedBy(target string) ([]string, error) {
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.source.TrackedBy(target)
}

// Close releases nothing: the fake is in memory.
func (s *fakeStore) Close() error { return nil }

// seed stores issues exactly as given: the test's starting state.
func (s *fakeStore) seed(issues ...beads.Issue) {
	s.db.Seed(issues...)
}

// link records a dependency edge, both for the fake's issue reads and for the
// raw dep statements the source answers.
func (s *fakeStore) link(from, to, depType string) {
	s.edges = append(s.edges, depEdge{from: from, to: to, typ: depType})
	_ = s.db.AddTypedDependency(from, to, depType)
}

// edge is link, the name the cross-rig tests gave it.
func (s *fakeStore) edge(from, to, depType string) { s.link(from, to, depType) }

// clearEdges drops every recorded edge, for a test that re-points a bead's
// dependencies.
func (s *fakeStore) clearEdges() {
	for _, e := range s.edges {
		_ = s.db.RemoveDependency(e.from, e.to)
	}
	s.edges = nil
}

// label adds a label to a seeded issue.
func (s *fakeStore) label(id, label string) {
	_ = s.db.Update(id, beads.UpdateOptions{AddLabels: []string{label}})
}

// CreateIssue seeds issue, refusing an id already stored as the old test store
// did.
func (s *fakeStore) CreateIssue(_ context.Context, issue *beads.Issue, _ string) error {
	if _, err := s.db.Show(issue.ID); err == nil {
		return fmt.Errorf("issue %s already exists", issue.ID)
	}
	s.db.Seed(*issue)
	return nil
}

// AddDependency records dep's edge.
func (s *fakeStore) AddDependency(_ context.Context, dep *depSpec, _ string) error {
	s.link(dep.IssueID, dep.DependsOnID, dep.Type)
	return nil
}

// AddLabel adds a label to a seeded issue.
func (s *fakeStore) AddLabel(_ context.Context, issueID, label, _ string) error {
	s.label(issueID, label)
	return nil
}

// UpdateIssue applies the one update the tests make: a status change.
func (s *fakeStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	status, ok := updates["status"]
	if !ok {
		return fmt.Errorf("fake store: unsupported update %v", updates)
	}
	value := fmt.Sprint(status)
	return s.db.Update(id, beads.UpdateOptions{Status: &value})
}

// fakeDB exposes the fake for the helpers that seed a town database directly.
func (s *fakeStore) fakeDB() *beadsfake.Fake { return s.db }

// brokenSource is a store that will not answer: a rig whose Dolt went away.
type brokenSource struct{ err error }

func (s brokenSource) Show(string) (*beads.Issue, error)                      { return nil, s.err }
func (s brokenSource) ShowMultiple([]string) (map[string]*beads.Issue, error) { return nil, s.err }
func (s brokenSource) Comments(string) ([]beads.Comment, error)               { return nil, s.err }
func (s brokenSource) Deps(string, string, string) ([]string, error)          { return nil, s.err }
func (s brokenSource) TrackedBy(string) ([]string, error)                     { return nil, s.err }
func (s brokenSource) Close() error                                           { return nil }

// joinedOnlySource serves the issue reads but has no raw dependency read: the
// shape a store left with only bd's joined view has, where cross-rig blockers
// are unknowable and the caller must fail safe.
type joinedOnlySource struct{ IssueSource }

func (s joinedOnlySource) Deps(string, string, string) ([]string, error) {
	return nil, fmt.Errorf("store %T cannot read raw dependency records", s.IssueSource)
}

// answerRawDeps answers one of the two raw dependency statements ClientSource
// sends from edges, the way the dependencies table would.
//
//	up:   SELECT issue_id FROM dependencies WHERE (depends_on_issue_id = 'X'
//	      OR depends_on_wisp_id = 'X' OR depends_on_external LIKE '%:X' ...)
//	      [AND type = 'T']
//	down: SELECT COALESCE(...) AS depends_on_id FROM dependencies
//	      WHERE issue_id = 'X' [AND type = 'T']
func answerRawDeps(edges []depEdge, query string) ([][]string, error) {
	depType := sqlLiteral(query, "AND type = '")
	if strings.Contains(query, "SELECT issue_id FROM") {
		target := sqlLiteral(query, "depends_on_issue_id = '")
		rows := [][]string{{"issue_id"}}
		for _, e := range edges {
			if !depTargets(e.to, target) {
				continue
			}
			if depType != "" && e.typ != depType {
				continue
			}
			rows = append(rows, []string{e.from})
		}
		return rows, nil
	}

	from := sqlLiteral(query, "issue_id = '")
	rows := [][]string{{"depends_on_id"}}
	for _, e := range edges {
		if e.from != from {
			continue
		}
		if depType != "" && e.typ != depType {
			continue
		}
		rows = append(rows, []string{e.to})
	}
	return rows, nil
}

// depTargets reports whether the stored target to is the one the up query
// names: the id itself, or an external:<prefix>:<id> edge to it.
func depTargets(to, id string) bool {
	return to == id || strings.HasSuffix(to, ":"+id)
}

// sqlLiteral returns the single-quoted literal after prefix in query, or "".
func sqlLiteral(query, prefix string) string {
	i := strings.Index(query, prefix)
	if i < 0 {
		return ""
	}
	rest := query[i+len(prefix):]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		return ""
	}
	return strings.ReplaceAll(rest[:end], "''", "'")
}
