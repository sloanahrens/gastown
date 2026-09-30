package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// memStore is an in-memory beadsdk.Storage for the convoy manager's unit
// tests: the event log, issues and dependency edges the manager and
// internal/convoy read, with the write paths the tests use to shape them.
// Only that surface is implemented; any other method panics through the nil
// embedded Storage, so a test that strays past it fails loudly.
//
// It copies the Dolt store's observable behavior where the manager depends
// on it (TestMemStoreMatchesBeadsStore pins each point against a
// real store):
//   - CreateIssue records "created", CloseIssue "closed", and UpdateIssue with
//     a status "closed" / "reopened" (from closed) / "status_changed", as
//     issueops.DetermineEventType decides.
//   - Events are dated to the second, like the events table's DATETIME, and
//     GetAllEventsSince returns those strictly after since, oldest second
//     first; within one second, in reverse write order (Dolt promises none).
//   - Dependency reads resolve a target through the issues it holds, so an
//     edge to an issue in another store (external) lists no local issue.
type memStore struct {
	beadsdk.Storage

	mu       sync.Mutex
	now      func() time.Time
	issues   map[string]*beadsdk.Issue
	deps     []*beadsdk.Dependency
	events   []*beadsdk.Event
	config   map[string]string
	comments map[string][]*beadsdk.Comment
	nextID   int
	closed   bool

	eventsErr error // returned by GetAllEventsSince and EventsTail when set

	// journal is the store's events journal (bd events tail), one record
	// per recorded event. journalFloor, when set, is the oldest seq still
	// retained: a read from below floor-1 is refused as pruned past.
	journal      []beads.EventRecord
	journalFloor int64
	tails        int
	// truncateAlways, while truncateOnce is set, is returned by the next
	// EventsTail, which then clears truncateOnce.
	truncateAlways *beads.EventsTruncatedError
	truncateOnce   bool
	// failAfterTruncate is returned by every EventsTail after the scripted
	// truncation has been served.
	failAfterTruncate error
}

var _ beadsdk.Storage = (*memStore)(nil)

// newMemStore returns an empty memStore and a cleanup that closes it, the
// shape setupTestStore returns.
func newMemStore(t *testing.T) (*memStore, func()) {
	t.Helper()
	s := &memStore{
		now:      time.Now,
		issues:   map[string]*beadsdk.Issue{},
		config:   map[string]string{"issue_prefix": "test"},
		comments: map[string][]*beadsdk.Comment{},
	}
	return s, func() { _ = s.Close() }
}

func (s *memStore) stamp() time.Time { return s.now().UTC().Truncate(time.Second) }

func copyIssue(i *beadsdk.Issue) *beadsdk.Issue {
	c := *i
	c.Labels = append([]string(nil), i.Labels...)
	return &c
}

// record appends an event; the caller holds s.mu.
func (s *memStore) record(issueID string, typ beadsdk.EventType, actor, oldValue, newValue string) {
	e := &beadsdk.Event{
		ID:        newEventUUID(),
		IssueID:   issueID,
		EventType: typ,
		Actor:     actor,
		CreatedAt: s.stamp(),
	}
	if oldValue != "" {
		e.OldValue = &oldValue
	}
	if newValue != "" {
		e.NewValue = &newValue
	}
	s.events = append(s.events, e)

	op := "update"
	switch typ {
	case beadsdk.EventCreated:
		op = "create"
	case beadsdk.EventClosed:
		op = "close"
	}
	rec := beads.EventRecord{Seq: int64(len(s.journal) + 1), Op: op, IssueID: issueID, Actor: actor}
	if i, ok := s.issues[issueID]; ok {
		rec.Status = string(i.Status)
	}
	s.journal = append(s.journal, rec)
}

// EventsTail serves the journal as bd events tail does: records after since,
// at most limit, and a pruned-past since refused with the retained window.
func (s *memStore) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tails++
	if s.eventsErr != nil {
		return nil, s.eventsErr
	}
	if !s.truncateOnce && s.truncateAlways != nil && s.failAfterTruncate != nil {
		return nil, s.failAfterTruncate
	}
	if s.truncateOnce && s.truncateAlways != nil {
		s.truncateOnce = false
		t := *s.truncateAlways
		t.Since = since
		return nil, &t
	}
	if s.journalFloor > 0 && since < s.journalFloor-1 {
		return nil, &beads.EventsTruncatedError{Since: since, Floor: s.journalFloor, Head: int64(len(s.journal))}
	}
	page := &beads.EventsPage{NextSince: since}
	for _, r := range s.journal {
		if r.Seq <= since {
			continue
		}
		if limit > 0 && len(page.Records) == limit {
			page.More = true
			break
		}
		page.Records = append(page.Records, r)
		page.NextSince = r.Seq
	}
	return page, nil
}

func (s *memStore) tailCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tails
}

// The convoy manager reads a store's journal through bd; in this package's
// tests the stores are memStores, which carry their own journal.
func init() {
	newEventJournal = func(townRoot, name string, store beadsdk.Storage) (eventJournal, error) {
		if j, ok := store.(eventJournal); ok {
			return j, nil
		}
		return nil, fmt.Errorf("test store %s (%T) has no events journal", name, store)
	}
}

func (s *memStore) CreateIssue(_ context.Context, issue *beadsdk.Issue, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if issue.ID == "" {
		return fmt.Errorf("memStore: issue needs an ID")
	}
	if _, dup := s.issues[issue.ID]; dup {
		return fmt.Errorf("issue %s already exists", issue.ID)
	}
	c := copyIssue(issue)
	if c.Status == "" {
		c.Status = beadsdk.StatusOpen
	}
	s.issues[c.ID] = c
	data, _ := json.Marshal(c)
	s.record(c.ID, beadsdk.EventCreated, actor, "", string(data))
	return nil
}

func (s *memStore) CloseIssue(_ context.Context, id, reason, actor, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.issues[id]
	if !ok {
		return fmt.Errorf("issue not found: %s", id)
	}
	now := s.now().UTC()
	i.Status = beadsdk.StatusClosed
	i.ClosedAt = &now
	i.CloseReason = reason
	s.record(id, beadsdk.EventClosed, actor, "", reason)
	return nil
}

func (s *memStore) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.issues[id]
	if !ok {
		return fmt.Errorf("failed to get issue for update: issue not found: %s", id)
	}
	old := copyIssue(i)
	typ := beadsdk.EventUpdated
	for key, v := range updates {
		str := fmt.Sprint(v)
		switch key {
		case "status":
			switch {
			case str == string(beadsdk.StatusClosed):
				typ = beadsdk.EventClosed
				now := s.now().UTC()
				i.ClosedAt = &now
			case old.Status == beadsdk.StatusClosed:
				typ = beadsdk.EventReopened
				i.ClosedAt = nil
				i.CloseReason = ""
			default:
				typ = beadsdk.EventStatusChanged
			}
			i.Status = beadsdk.Status(str)
		case "assignee":
			i.Assignee = str
		case "title":
			i.Title = str
		case "description":
			i.Description = str
		case "notes":
			i.Notes = str
		default:
			return fmt.Errorf("memStore: update of %q is not modeled", key)
		}
	}
	oldData, _ := json.Marshal(old)
	newData, _ := json.Marshal(updates)
	s.record(id, typ, actor, string(oldData), string(newData))
	return nil
}

func (s *memStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue not found: %s", id)
	}
	return copyIssue(i), nil
}

// GetIssuesByIDs returns the issues it holds among ids, in ID order.
func (s *memStore) GetIssuesByIDs(_ context.Context, ids []string) ([]*beadsdk.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beadsdk.Issue
	for _, id := range ids {
		if i, ok := s.issues[id]; ok {
			out = append(out, copyIssue(i))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (s *memStore) AddDependency(_ context.Context, dep *beadsdk.Dependency, actor string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.issues[dep.IssueID]; !ok {
		return fmt.Errorf("issue %s not found", dep.IssueID)
	}
	// Like the Dolt store, a same-prefix local target must exist; a
	// cross-prefix or external: target is stored unvalidated.
	crossPrefix := idPrefix(dep.IssueID) != idPrefix(dep.DependsOnID)
	if !crossPrefix && !strings.HasPrefix(dep.DependsOnID, "external:") {
		if _, ok := s.issues[dep.DependsOnID]; !ok {
			return fmt.Errorf("issue %s not found", dep.DependsOnID)
		}
	}
	for _, d := range s.deps {
		if d.IssueID == dep.IssueID && d.DependsOnID == dep.DependsOnID {
			if d.Type == dep.Type {
				d.Metadata = dep.Metadata
				return nil
			}
			return fmt.Errorf("dependency %s -> %s already exists with type %q (requested %q); remove it first with 'bd dep remove' then re-add",
				dep.IssueID, dep.DependsOnID, d.Type, dep.Type)
		}
	}
	c := *dep
	if c.CreatedAt.IsZero() {
		c.CreatedAt = s.stamp()
	}
	if c.CreatedBy == "" {
		c.CreatedBy = actor
	}
	s.deps = append(s.deps, &c)
	return nil
}

// GetDependencyRecords returns the raw edges out of issueID, whatever their
// target: the reader internal/convoy prefers when a store offers it, as the
// Dolt store does.
func (s *memStore) GetDependencyRecords(_ context.Context, issueID string) ([]*beadsdk.Dependency, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beadsdk.Dependency
	for _, d := range s.deps {
		if d.IssueID == issueID {
			c := *d
			out = append(out, &c)
		}
	}
	return out, nil
}

func (s *memStore) GetDependenciesWithMetadata(_ context.Context, issueID string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beadsdk.IssueWithDependencyMetadata
	for _, d := range s.deps {
		if d.IssueID != issueID {
			continue
		}
		if i, ok := s.issues[d.DependsOnID]; ok {
			out = append(out, &beadsdk.IssueWithDependencyMetadata{Issue: *copyIssue(i), DependencyType: d.Type})
		}
	}
	return out, nil
}

func (s *memStore) GetDependentsWithMetadata(_ context.Context, issueID string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beadsdk.IssueWithDependencyMetadata
	for _, d := range s.deps {
		if d.DependsOnID != issueID {
			continue
		}
		if i, ok := s.issues[d.IssueID]; ok {
			out = append(out, &beadsdk.IssueWithDependencyMetadata{Issue: *copyIssue(i), DependencyType: d.Type})
		}
	}
	return out, nil
}

func (s *memStore) GetAllEventsSince(ctx context.Context, since time.Time) ([]*beadsdk.Event, error) {
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("get events since %v: %w", since, ctx.Err())
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.eventsErr != nil {
		return nil, s.eventsErr
	}
	// Oldest second first, like the Dolt store's ORDER BY created_at. Within
	// one second that order is undefined, so memStore returns those events
	// newest first: a test that relies on write order inside a second fails
	// here instead of passing on luck against Dolt.
	var out []*beadsdk.Event
	for i := len(s.events) - 1; i >= 0; i-- {
		if e := s.events[i]; e.CreatedAt.After(since) {
			c := *e
			out = append(out, &c)
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out, nil
}

func (s *memStore) AddIssueComment(_ context.Context, issueID, author, text string) (*beadsdk.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.issues[issueID]; !ok {
		return nil, fmt.Errorf("issue %s not found", issueID)
	}
	s.nextID++
	c := &beadsdk.Comment{ID: "c-" + strconv.Itoa(s.nextID), IssueID: issueID, Author: author, Text: text, CreatedAt: s.stamp()}
	s.comments[issueID] = append(s.comments[issueID], c)
	cc := *c
	return &cc, nil
}

// GetIssueComments returns issueID's comments oldest first; an issue with
// none has an empty history, not an error.
func (s *memStore) GetIssueComments(_ context.Context, issueID string) ([]*beadsdk.Comment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*beadsdk.Comment
	for _, c := range s.comments[issueID] {
		cc := *c
		out = append(out, &cc)
	}
	return out, nil
}

func (s *memStore) SetConfig(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config[key] = value
	return nil
}

func (s *memStore) GetConfig(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config[key], nil
}

func (s *memStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func (s *memStore) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// eventSeq numbers memStore events across every store in the process, the
// way the events table's UUID() keys are unique across databases: the convoy
// manager dedups lifecycle events by ID across all the stores it polls.
var eventSeq atomic.Uint64

// newEventUUID returns a UUID-shaped ID unique in the process.
func newEventUUID() string {
	n := eventSeq.Add(1)
	return fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
}

// idPrefix is beads' types.ExtractPrefix: the ID through its first dash.
func idPrefix(id string) string {
	if i := strings.Index(id, "-"); i >= 0 {
		return id[:i+1]
	}
	return ""
}

// eventSummary renders events as "<issue>:<type>" in order, the shape the
// contract test compares across stores.
func eventSummary(events []*beadsdk.Event) string {
	parts := make([]string, 0, len(events))
	for _, e := range events {
		parts = append(parts, e.IssueID+":"+string(e.EventType))
	}
	return strings.Join(parts, " ")
}

// storeObservation is what observeStore saw a store do: the surface of
// beadsdk.Storage that memStore models, reduced to what the convoy manager
// and internal/convoy depend on.
type storeObservation struct {
	Events          []string // "<issue>:<type>", sorted: rows dated in the same second have no order
	ReopenOldIsJSON bool     // a reopen's old value is the issue as JSON, not "closed"
	SinceIsStrict   bool     // nothing is returned for since = the newest event's date
	SinceIncludes   bool     // a second earlier returns the newest event again
	DependentsOfA   []string // "<id>/<type>"
	DepsOfConvoy    []string // "<id>/<type>", resolved local targets only
	RecordsOfConvoy []string // raw edges: "<target>/<type>"
	StatusA         string
	StatusB         string
	ByIDs           []string
	MissingTarget   bool     // a same-prefix edge to a missing issue is refused
	ConflictingType bool     // re-adding a pair under another type is refused
	EventIDsAreUUID bool     // every event ID is a 36-character UUID
	Comments        []string // gt-a1's comments, "<author>: <text>", oldest first
	NoComments      bool     // an issue without comments reads as empty, not an error
}

// observeStore runs one script against store and reports what it saw. The
// script is the shape the convoy tests build: work beads in a rig prefix, a
// convoy in hq tracking one of them locally and one in another rig.
func observeStore(t *testing.T, store beadsdk.Storage) storeObservation {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	mk := func(id string) {
		t.Helper()
		if err := store.CreateIssue(ctx, &beadsdk.Issue{ID: id, Title: id, Status: beadsdk.StatusOpen, Priority: 2,
			IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}, "test"); err != nil {
			t.Fatalf("CreateIssue %s: %v", id, err)
		}
	}
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	mk("gt-a1")
	mk("gt-b1")
	mk("hq-cv1")
	must("track local", store.AddDependency(ctx, &beadsdk.Dependency{IssueID: "hq-cv1", DependsOnID: "gt-a1", Type: "tracks"}, "test"))
	must("track other rig", store.AddDependency(ctx, &beadsdk.Dependency{IssueID: "hq-cv1", DependsOnID: "external:bd:bd-z9", Type: "tracks"}, "test"))
	must("close a", store.CloseIssue(ctx, "gt-a1", "done", "test", ""))
	must("reopen a", store.UpdateIssue(ctx, "gt-a1", map[string]interface{}{"status": string(beadsdk.StatusOpen)}, "test"))
	must("start b", store.UpdateIssue(ctx, "gt-b1", map[string]interface{}{"status": string(beadsdk.StatusInProgress)}, "test"))
	must("close b by update", store.UpdateIssue(ctx, "gt-b1", map[string]interface{}{"status": string(beadsdk.StatusClosed)}, "test"))

	var obs storeObservation
	obs.MissingTarget = store.AddDependency(ctx, &beadsdk.Dependency{IssueID: "gt-b1", DependsOnID: "gt-nope", Type: "blocks"}, "test") != nil
	obs.ConflictingType = store.AddDependency(ctx, &beadsdk.Dependency{IssueID: "hq-cv1", DependsOnID: "gt-a1", Type: "related"}, "test") != nil

	_, err := store.AddIssueComment(ctx, "gt-a1", "deacon", "first")
	must("comment 1", err)
	_, err = store.AddIssueComment(ctx, "gt-a1", "witness", "second")
	must("comment 2", err)
	comments, err := store.GetIssueComments(ctx, "gt-a1")
	must("comments", err)
	for _, c := range comments {
		obs.Comments = append(obs.Comments, c.Author+": "+c.Text)
	}
	none, err := store.GetIssueComments(ctx, "gt-b1")
	obs.NoComments = err == nil && len(none) == 0

	events, err := store.GetAllEventsSince(ctx, time.Unix(0, 0).UTC())
	must("events", err)
	var newest time.Time
	obs.EventIDsAreUUID = len(events) > 0
	for _, e := range events {
		if len(e.ID) != 36 || strings.Count(e.ID, "-") != 4 {
			obs.EventIDsAreUUID = false
		}
		obs.Events = append(obs.Events, e.IssueID+":"+string(e.EventType))
		if e.CreatedAt.After(newest) {
			newest = e.CreatedAt
		}
		if e.EventType == beadsdk.EventReopened && e.OldValue != nil {
			obs.ReopenOldIsJSON = strings.HasPrefix(*e.OldValue, "{")
		}
	}
	sort.Strings(obs.Events)
	after, err := store.GetAllEventsSince(ctx, newest)
	must("events after newest", err)
	obs.SinceIsStrict = len(after) == 0
	again, err := store.GetAllEventsSince(ctx, newest.Add(-time.Second))
	must("events since a second earlier", err)
	obs.SinceIncludes = len(again) > 0

	dependents, err := store.GetDependentsWithMetadata(ctx, "gt-a1")
	must("dependents", err)
	for _, d := range dependents {
		obs.DependentsOfA = append(obs.DependentsOfA, d.ID+"/"+string(d.DependencyType))
	}
	deps, err := store.GetDependenciesWithMetadata(ctx, "hq-cv1")
	must("dependencies", err)
	for _, d := range deps {
		obs.DepsOfConvoy = append(obs.DepsOfConvoy, d.ID+"/"+string(d.DependencyType))
	}
	reader, ok := store.(interface {
		GetDependencyRecords(ctx context.Context, issueID string) ([]*beadsdk.Dependency, error)
	})
	if !ok {
		t.Fatalf("%T offers no GetDependencyRecords; internal/convoy reads raw edges through it", store)
	}
	records, err := reader.GetDependencyRecords(ctx, "hq-cv1")
	must("dependency records", err)
	for _, d := range records {
		obs.RecordsOfConvoy = append(obs.RecordsOfConvoy, d.DependsOnID+"/"+string(d.Type))
	}
	sort.Strings(obs.RecordsOfConvoy)

	a, err := store.GetIssue(ctx, "gt-a1")
	must("get a", err)
	obs.StatusA = string(a.Status)
	b, err := store.GetIssue(ctx, "gt-b1")
	must("get b", err)
	obs.StatusB = string(b.Status)
	byIDs, err := store.GetIssuesByIDs(ctx, []string{"gt-b1", "gt-missing", "gt-a1"})
	must("get by ids", err)
	for _, i := range byIDs {
		obs.ByIDs = append(obs.ByIDs, i.ID)
	}
	sort.Strings(obs.ByIDs)
	return obs
}

// TestMemStoreContract pins what memStore does, so the convoy tests built on
// it know the store they run against. TestMemStoreMatchesBeadsStore checks
// the same observation against a real beads store.
func TestMemStoreContract(t *testing.T) {
	t.Parallel()
	store, cleanup := newMemStore(t)
	defer cleanup()

	got := observeStore(t, store)
	want := storeObservation{
		Events: []string{
			"gt-a1:closed", "gt-a1:created", "gt-a1:reopened",
			"gt-b1:closed", "gt-b1:created", "gt-b1:status_changed",
			"hq-cv1:created",
		},
		ReopenOldIsJSON: true,
		EventIDsAreUUID: true,
		SinceIsStrict:   true,
		SinceIncludes:   true,
		DependentsOfA:   []string{"hq-cv1/tracks"},
		DepsOfConvoy:    []string{"gt-a1/tracks"},
		RecordsOfConvoy: []string{"external:bd:bd-z9/tracks", "gt-a1/tracks"},
		StatusA:         "open",
		StatusB:         "closed",
		ByIDs:           []string{"gt-a1", "gt-b1"},
		MissingTarget:   true,
		ConflictingType: true,
		Comments:        []string{"deacon: first", "witness: second"},
		NoComments:      true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("memStore observation:\n got  %+v\n want %+v", got, want)
	}

	// Within one second memStore returns events newest first, so no test can
	// lean on a write order the Dolt store does not promise.
	clk := newFixedClock()
	same, sameCleanup := newMemStore(t)
	defer sameCleanup()
	same.now = clk.Now
	ctx := context.Background()
	for _, id := range []string{"gt-s1", "gt-s2", "gt-s3"} {
		if err := same.CreateIssue(ctx, &beadsdk.Issue{ID: id, Title: id}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	clk.Advance(time.Second)
	if err := same.CreateIssue(ctx, &beadsdk.Issue{ID: "gt-s4", Title: "later"}, "test"); err != nil {
		t.Fatal(err)
	}
	events, err := same.GetAllEventsSince(ctx, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := eventSummary(events), "gt-s3:created gt-s2:created gt-s1:created gt-s4:created"; got != want {
		t.Errorf("event order = %q, want %q (same second newest first, seconds oldest first)", got, want)
	}
}

// TestMemStoreMatchesBeadsStore runs observeStore against a real beads store
// on the package's Dolt container and against memStore, and requires the
// same observation: the differential check behind every convoy test that
// runs on memStore instead of Dolt.
func TestMemStoreMatchesBeadsStore(t *testing.T) {
	t.Parallel()
	takeStoreSlot(t)
	real, cleanup := setupTestStore(t)
	defer cleanup()
	mem, memCleanup := newMemStore(t)
	defer memCleanup()

	if got, want := observeStore(t, real), observeStore(t, mem); !reflect.DeepEqual(got, want) {
		t.Errorf("memStore diverges from the beads store:\n beads   %+v\n memStore %+v", got, want)
	}
}
