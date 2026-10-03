package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/convoy"
)

// memStore is the convoy manager's unit-test store: a beadsfake database — the
// in-memory fake internal/beads pins to bd's Client contract — carrying the
// parts of the events journal the manager's tests script. beadsfake journals
// like bd but offers no way to fail, prune or truncate the tail, and the poll
// path's recovery behavior is exactly what those tests exercise, so this
// wrapper owns EventsTail and the tail counter.
//
// The small write helpers the convoy tests use (CreateIssue, CloseIssue,
// UpdateIssue) keep the call sites readable: they spell out beads.CreateOptions
// and beads.UpdateOptions once instead of at every write.
type memStore struct {
	*beadsfake.Fake

	// deps are the raw edges the store holds, whatever their target: the
	// shape internal/convoy's raw dependency read answers from. beadsfake
	// resolves a dependency's target through its own issues, so a cross-rig
	// external:<prefix>:<id> edge has to be recorded here instead.
	deps []memDependency

	// tails counts every EventsTail call, so a test can see that a poll paged
	// through the journal.
	tails int
	// eventsErr, when set, is returned by every EventsTail.
	eventsErr error
	// journalFloor, when set, is the oldest seq still retained: a read from
	// below floor-1 is refused as pruned past.
	journalFloor int64
	// truncateAlways, while truncateOnce is set, is returned by the next
	// EventsTail, which then clears truncateOnce.
	truncateAlways *beads.EventsTruncatedError
	truncateOnce   bool
	// failAfterTruncate is returned by every EventsTail after the scripted
	// truncation has been served.
	failAfterTruncate error

	mu sync.Mutex
}

var _ convoy.Store = (*memStore)(nil)
var _ eventJournal = (*memStore)(nil)

// newMemStore returns an empty memStore and a cleanup, the shape the convoy
// tests use.
func newMemStore(t *testing.T) (*memStore, func()) {
	t.Helper()
	return newMemStoreAt(clockwork.NewFakeClockAt(testEpoch)), func() {}
}

// newMemStoreAt returns an empty memStore whose clock is clk, for tests that
// space lifecycle transitions in time.
func newMemStoreAt(clk clockwork.Clock) *memStore {
	return &memStore{Fake: beadsfake.New(beadsfake.WithPrefix("gt"), beadsfake.WithClock(clk))}
}

// EventsTail serves the journal as bd events tail does: records after since,
// at most limit, with the scripted failure, truncation or floor applied first.
func (s *memStore) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	s.mu.Lock()
	s.tails++
	err := s.eventsErr
	truncateOnce := s.truncateOnce
	truncateAlways := s.truncateAlways
	failAfter := s.failAfterTruncate
	floor := s.journalFloor
	if truncateOnce {
		s.truncateOnce = false
	}
	s.mu.Unlock()

	switch {
	case err != nil:
		return nil, err
	case !truncateOnce && truncateAlways != nil && failAfter != nil:
		return nil, failAfter
	case truncateOnce && truncateAlways != nil:
		t := *truncateAlways
		t.Since = since
		return nil, &t
	case floor > 0 && since < floor-1:
		head, err := s.journalHead()
		if err != nil {
			return nil, err
		}
		return nil, &beads.EventsTruncatedError{Since: since, Floor: floor, Head: head}
	}
	return s.Fake.EventsTail(since, limit)
}

// journalHead is the journal's newest seq.
func (s *memStore) journalHead() (int64, error) {
	page, err := s.Fake.EventsTail(0, 0)
	if err != nil {
		return 0, err
	}
	return page.NextSince, nil
}

func (s *memStore) tailCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tails
}

// CreateIssue creates one issue with the given ID, in the status the literal
// names (beadsfake creates every issue open, so any other status is an update).
func (s *memStore) CreateIssue(_ context.Context, issue *beads.Issue, actor string) error {
	if _, err := s.Create(beads.CreateOptions{
		ID:          issue.ID,
		Title:       issue.Title,
		Description: issue.Description,
		Priority:    issue.Priority,
		Assignee:    issue.Assignee,
		Labels:      issue.Labels,
		Ephemeral:   issue.Ephemeral,
		Actor:       actor,
	}); err != nil {
		return err
	}
	// beads.CreateOptions carries no notes, and the merge-rejection marker a
	// test sets lives there, so they are appended after the create.
	if issue.Notes != "" {
		if err := s.AppendNotes(issue.ID, issue.Notes); err != nil {
			return err
		}
	}
	if issue.Status == "" || issue.Status == string(beads.StatusOpen) {
		return nil
	}
	status := issue.Status
	return s.Update(issue.ID, beads.UpdateOptions{Status: &status})
}

// CloseIssue closes an issue, recording reason.
func (s *memStore) CloseIssue(_ context.Context, id, reason, _, _ string) error {
	return s.CloseWithReason(reason, id)
}

// UpdateIssue applies the fields the tests set through a map: beadsfake's own
// Update takes typed options, and notes are append-only there (AppendNotes),
// which is what the manager's journal reads care about.
func (s *memStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	var opts beads.UpdateOptions
	hasOpts, hasNotes := false, false
	var notes string
	for key, v := range updates {
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("memStore: update of %q with %T is not modeled", key, v)
		}
		switch key {
		case "status":
			opts.Status = &str
			hasOpts = true
		case "assignee":
			opts.Assignee = &str
			hasOpts = true
		case "title":
			opts.Title = &str
			hasOpts = true
		case "description":
			opts.Description = &str
			hasOpts = true
		case "notes":
			notes, hasNotes = str, true
		default:
			return fmt.Errorf("memStore: update of %q is not modeled", key)
		}
	}
	if hasOpts {
		if err := s.Update(id, opts); err != nil {
			return err
		}
	}
	if hasNotes {
		return s.AppendNotes(id, notes)
	}
	return nil
}

// memDependency is one raw edge the store holds, whatever its target.
type memDependency struct {
	IssueID     string
	DependsOnID string
	Type        string
}

// addRawDep records a typed edge out of issueID. Unlike beadsfake's
// AddDependency the target need not exist locally: the convoy tests' cross-rig
// edges name external:<prefix>:<id>.
func (s *memStore) addRawDep(issueID, dependsOnID, depType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps = append(s.deps, memDependency{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
}

// SQLCSV answers the raw dependency statements internal/convoy makes through
// ClientSource.Deps, in bd sql --csv's shape (the header row first). Any other
// statement is unscripted.
func (s *memStore) SQLCSV(query beadsql.Query) ([][]string, error) {
	stmt := query.String()
	s.mu.Lock()
	deps := append([]memDependency(nil), s.deps...)
	s.mu.Unlock()

	depType := sqlArgAfter(stmt, "AND type = ")
	switch {
	case strings.HasPrefix(stmt, "SELECT COALESCE("):
		id := sqlArgAfter(stmt, "issue_id = ")
		rows := [][]string{{"depends_on_id"}}
		for _, d := range deps {
			if d.IssueID == id && (depType == "" || d.Type == depType) {
				rows = append(rows, []string{d.DependsOnID})
			}
		}
		return rows, nil
	case strings.HasPrefix(stmt, "SELECT issue_id FROM dependencies"):
		id := sqlArgAfter(stmt, "depends_on_issue_id = ")
		rows := [][]string{{"issue_id"}}
		for _, d := range deps {
			if (d.DependsOnID == id || strings.HasSuffix(d.DependsOnID, ":"+id)) && (depType == "" || d.Type == depType) {
				rows = append(rows, []string{d.IssueID})
			}
		}
		return rows, nil
	}
	return nil, fmt.Errorf("memStore: unscripted SQL: %s", stmt)
}

// sqlArgAfter returns the single-quoted literal that follows marker in an
// inlined beadsql statement, or "".
func sqlArgAfter(stmt, marker string) string {
	i := strings.Index(stmt, marker)
	if i < 0 {
		return ""
	}
	rest := stmt[i+len(marker):]
	if !strings.HasPrefix(rest, "'") {
		return ""
	}
	end := strings.Index(rest[1:], "'")
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}
