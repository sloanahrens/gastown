package convoy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	beadsRouting "github.com/steveyegge/gastown/internal/beads"
)

// fakeRigStore is one rig's beads store: its own issues and the dependency
// edges recorded on them. Its joined dependency view behaves as beads v1.0.5's
// Dolt store does: an edge whose target is not one of this store's issues
// (every "external:<prefix>:<id>" edge) is dropped (gt-j02xy).
type fakeRigStore struct {
	beadsdk.Storage
	issues map[string]*beadsdk.Issue
	deps   []*beadsdk.Dependency
}

func newFakeRigStore(issues ...*beadsdk.Issue) *fakeRigStore {
	s := &fakeRigStore{issues: make(map[string]*beadsdk.Issue)}
	for _, iss := range issues {
		s.issues[iss.ID] = iss
	}
	return s
}

func (s *fakeRigStore) edge(from, to, depType string) {
	s.deps = append(s.deps, &beadsdk.Dependency{IssueID: from, DependsOnID: to, Type: beadsdk.DependencyType(depType)})
}

func (s *fakeRigStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	return s.issues[id], nil
}

func (s *fakeRigStore) GetIssueComments(context.Context, string) ([]*beadsdk.Comment, error) {
	return nil, nil
}

func (s *fakeRigStore) GetIssuesByIDs(_ context.Context, ids []string) ([]*beadsdk.Issue, error) {
	var out []*beadsdk.Issue
	for _, id := range ids {
		if iss, ok := s.issues[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (s *fakeRigStore) GetDependencyRecords(_ context.Context, issueID string) ([]*beadsdk.Dependency, error) {
	var out []*beadsdk.Dependency
	for _, d := range s.deps {
		if d.IssueID == issueID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *fakeRigStore) GetDependenciesWithMetadata(_ context.Context, issueID string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	var out []*beadsdk.IssueWithDependencyMetadata
	for _, d := range s.deps {
		if d.IssueID != issueID {
			continue
		}
		iss, ok := s.issues[d.DependsOnID]
		if !ok {
			continue // the v1.0.5 join drops a target this store does not hold
		}
		out = append(out, &beadsdk.IssueWithDependencyMetadata{Issue: *iss, DependencyType: d.Type})
	}
	return out, nil
}

// xrigTown is a town convoy tracking two gastown beads. gt-work (priority 1,
// fed first) is blocked by oag-x, which lives in a third rig, oag.
type xrigTown struct {
	townRoot string
	hq       *fakeRigStore
	gastown  *fakeRigStore
	oag      *fakeRigStore
}

func newXrigTown(t *testing.T, blockerStatus beadsdk.Status) *xrigTown {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	task := func(id string, priority int, status beadsdk.Status) *beadsdk.Issue {
		return &beadsdk.Issue{ID: id, Title: id, Status: status, Priority: priority, IssueType: beadsdk.TypeTask, CreatedAt: now, UpdatedAt: now}
	}

	x := &xrigTown{
		townRoot: t.TempDir(),
		hq:       newFakeRigStore(task("hq-cv1", 2, beadsdk.StatusOpen)),
		gastown:  newFakeRigStore(task("gt-work", 1, beadsdk.StatusOpen), task("gt-sib", 2, beadsdk.StatusOpen)),
		oag:      newFakeRigStore(task("oag-x", 2, blockerStatus)),
	}
	x.hq.edge("hq-cv1", "external:gt:gt-work", "tracks")
	x.hq.edge("hq-cv1", "external:gt:gt-sib", "tracks")
	x.gastown.edge("gt-work", "external:oag:oag-x", "blocks")

	if err := beadsRouting.WriteRoutes(filepath.Join(x.townRoot, ".beads"), []beadsRouting.Route{
		{Prefix: "hq-", Path: "."},
		{Prefix: "gt-", Path: "gastown/mayor/rig"},
		{Prefix: "oag-", Path: "oag/mayor/rig"},
	}); err != nil {
		t.Fatalf("WriteRoutes: %v", err)
	}
	return x
}

func (x *xrigTown) resolver(withOag bool) *StoreResolver {
	stores := map[string]beadsdk.Storage{"hq": x.hq, "gastown": x.gastown}
	if withOag {
		stores["oag"] = x.oag
	}
	return NewStoreResolver(x.townRoot, stores)
}

// feed runs the continuation feed on the town convoy and returns what was
// slung and what was logged.
func (x *xrigTown) feed(t *testing.T, resolver *StoreResolver) (slung string, logged []string) {
	t.Helper()
	gtPath, logPath := makeGTStub(t, 0)
	logger, msgs := makeLogger()
	feedNextReadyIssue(context.Background(), x.hq, x.townRoot, "hq-cv1", "test", logger, gtPath, func(string) bool { return false }, resolver)
	data, err := os.ReadFile(logPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading gt stub log: %v", err)
	}
	return string(data), *msgs
}

func loggedLine(msgs []string, parts ...string) bool {
	for _, m := range msgs {
		all := true
		for _, p := range parts {
			if !strings.Contains(m, p) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// TestFeedNextReadyIssue_ThirdRigOpenBlockerHolds pins gt-j02xy: a cross-rig
// bead blocked by an open bead in a third rig must not be fed. The joined
// dependency view drops the external blocker, so the feed saw no blocker.
func TestFeedNextReadyIssue_ThirdRigOpenBlockerHolds(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusOpen)

	slung, logged := x.feed(t, x.resolver(true))

	if strings.Contains(slung, "gt-work") {
		t.Errorf("gt-work is blocked by open oag-x and must not be slung, got %q", slung)
	}
	if !strings.Contains(slung, "sling gt-sib gastown") {
		t.Errorf("expected the unblocked sibling gt-sib to be slung, got %q (log %v)", slung, logged)
	}
	if !loggedLine(logged, "gt-work", "blocked", "oag-x") {
		t.Errorf("expected a log naming gt-work blocked by oag-x, got %v", logged)
	}
}

// TestFeedNextReadyIssue_ThirdRigClosedBlockerFeeds: once the third-rig
// blocker is closed in its own rig, the bead feeds.
func TestFeedNextReadyIssue_ThirdRigClosedBlockerFeeds(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)

	slung, logged := x.feed(t, x.resolver(true))

	if !strings.Contains(slung, "sling gt-work gastown") {
		t.Errorf("gt-work's blocker oag-x is closed; expected gt-work slung, got %q (log %v)", slung, logged)
	}
}

// TestFeedNextReadyIssue_UnresolvableBlockerHolds is the fail-safe ruling: a
// blocker no rig can answer for counts as blocking, and the log names it.
func TestFeedNextReadyIssue_UnresolvableBlockerHolds(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)

	slung, logged := x.feed(t, x.resolver(false)) // no oag store reachable

	if strings.Contains(slung, "gt-work") {
		t.Errorf("gt-work's blocker oag-x cannot be resolved; it must not be slung, got %q", slung)
	}
	if !strings.Contains(slung, "sling gt-sib gastown") {
		t.Errorf("expected the unblocked sibling gt-sib to be slung, got %q (log %v)", slung, logged)
	}
	if !loggedLine(logged, "gt-work", "oag-x", "unresolved") {
		t.Errorf("expected a log naming the unresolved blocker oag-x of gt-work, got %v", logged)
	}
}

// TestFeedNextReadyIssue_OpeningResolverReachesBlockerRig: gt close holds only
// the town store and opens a rig's store on first use (gt-tq6l). The blocker's
// rig has to be opened too, or every cross-rig blocker reads as unresolved and
// the bead is held forever even after its blocker closes.
func TestFeedNextReadyIssue_OpeningResolverReachesBlockerRig(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)
	rigs := map[string]beadsdk.Storage{"gastown": x.gastown, "oag": x.oag}
	resolver := NewOpeningStoreResolver(x.townRoot, func(name string) (beadsdk.Storage, error) {
		return rigs[name], nil
	})
	resolver.stores["hq"] = x.hq

	slung, logged := x.feed(t, resolver)

	if !strings.Contains(slung, "sling gt-work gastown") {
		t.Errorf("gt-work's blocker oag-x is closed in oag; expected gt-work slung, got %q (log %v)", slung, logged)
	}
}

// TestIsIssueBlocked_CrossRigBlockerWithoutResolver: with no resolver the
// home store is all there is, and a blocker it does not hold is unresolved,
// so the bead is blocked.
func TestIsIssueBlocked_CrossRigBlockerWithoutResolver(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)

	if !isIssueBlocked(context.Background(), x.gastown, "gt-work", nil) {
		t.Error("without a resolver, gt-work's cross-rig blocker cannot be resolved and must block")
	}
	if isIssueBlocked(context.Background(), x.gastown, "gt-sib", nil) {
		t.Error("gt-sib has no blockers and must not read as blocked")
	}
}

// TestIsIssueBlocked_StoreWithoutRawRecordsBlocks: a store that offers only
// the joined view cannot show cross-rig blockers, so its beads are blocked
// rather than fed on a view that may have dropped one.
func TestIsIssueBlocked_StoreWithoutRawRecordsBlocks(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)
	joinedOnly := &joinedOnlyStore{x.gastown}

	if !isIssueBlocked(context.Background(), joinedOnly, "gt-sib", nil) {
		t.Error("a store without raw dependency records must fail safe (blocked)")
	}
}

// TestTrackedIDs_StoreWithoutRawRecordsErrors pins the reviewer's point on
// 9c53a58: falling back to the joined view silently loses every cross-rig
// tracked bead, so it is an error instead.
func TestTrackedIDs_StoreWithoutRawRecordsErrors(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)

	if _, err := trackedIDs(context.Background(), &joinedOnlyStore{x.hq}, "hq-cv1"); err == nil {
		t.Error("trackedIDs on a store without raw dependency records must return an error")
	}
	ids, err := trackedIDs(context.Background(), x.hq, "hq-cv1")
	if err != nil || len(ids) != 2 {
		t.Errorf("trackedIDs = %v, %v; want gt-work and gt-sib", ids, err)
	}
}

// joinedOnlyStore hides GetDependencyRecords, which beadsdk.Storage does not
// declare, leaving only the joined view.
type joinedOnlyStore struct {
	beadsdk.Storage
}
