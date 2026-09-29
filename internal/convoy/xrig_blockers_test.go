package convoy

import (
	"context"
	"errors"
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
	// readErr, when set, fails GetIssuesByIDs: a rig whose Dolt went away.
	readErr error
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
	if s.readErr != nil {
		return nil, s.readErr
	}
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

// withHQBlocker re-points gt-work's blocker at hq-blk, a town-level bead.
// Production stores such an edge as the bare ID.
func (x *xrigTown) withHQBlocker(status beadsdk.Status) *xrigTown {
	x.hq.issues["hq-blk"] = &beadsdk.Issue{ID: "hq-blk", Title: "hq-blk", Status: status, IssueType: beadsdk.TypeTask}
	x.gastown.deps = nil
	x.gastown.edge("gt-work", "hq-blk", "blocks")
	return x
}

// closeResolver is the resolver gt close builds: it holds no store up front,
// never holds hq, and opens a rig's store on first use (close.go). opens
// counts open calls per rig; failOpen names rigs whose open fails.
func (x *xrigTown) closeResolver(opens map[string]int, failOpen ...string) *StoreResolver {
	rigs := map[string]beadsdk.Storage{"gastown": x.gastown, "oag": x.oag}
	return NewOpeningStoreResolver(x.townRoot, func(name string) (beadsdk.Storage, error) {
		if opens != nil {
			opens[name]++
		}
		for _, f := range failOpen {
			if f == name {
				return nil, errors.New("dial tcp 127.0.0.1:3307: connection refused")
			}
		}
		return rigs[name], nil
	})
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
// blocker whose rig store is not open counts as blocking, and the log names it
// as unreadable, not as missing.
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
	if !loggedLine(logged, "gt-work", "oag-x", "unreadable") {
		t.Errorf("expected a log naming the unreadable blocker oag-x of gt-work, got %v", logged)
	}
}

// TestFeedNextReadyIssue_DanglingBlockerHolds: the owning rig answers and has
// no such bead (a hard-deleted blocker leaves its external edge behind). The
// bead is held and the log says the blocker is unresolved.
func TestFeedNextReadyIssue_DanglingBlockerHolds(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)
	delete(x.oag.issues, "oag-x")

	slung, logged := x.feed(t, x.resolver(true))

	if strings.Contains(slung, "gt-work") {
		t.Errorf("gt-work's blocker oag-x is gone; it must not be slung, got %q", slung)
	}
	if !loggedLine(logged, "gt-work", "oag-x", "unresolved in any rig") {
		t.Errorf("expected a log naming the unresolved blocker oag-x of gt-work, got %v", logged)
	}
}

// TestBlockReason_OwningStoreFailureIsUnreadable (review minor 3): an open or
// read failure in the blocker's rig is reported as unreadable with the error,
// not as a blocker no rig has. The bead is held either way.
func TestBlockReason_OwningStoreFailureIsUnreadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	x := newXrigTown(t, beadsdk.StatusClosed)
	reason := blockReason(ctx, x.hq, "gt-work", x.closeResolver(nil, "oag"))
	if !strings.Contains(reason, "oag-x") || !strings.Contains(reason, "unreadable") || !strings.Contains(reason, "connection refused") {
		t.Errorf("open failure: reason = %q, want oag-x unreadable with the open error", reason)
	}

	y := newXrigTown(t, beadsdk.StatusClosed)
	y.oag.readErr = errors.New("dolt: query timeout")
	reason = blockReason(ctx, y.hq, "gt-work", y.resolver(true))
	if !strings.Contains(reason, "oag-x") || !strings.Contains(reason, "unreadable") || !strings.Contains(reason, "query timeout") {
		t.Errorf("read failure: reason = %q, want oag-x unreadable with the read error", reason)
	}
}

// TestBlockReason_HomeRigStoreUnavailableBlocks (review minor 4): with the
// bead's own rig store missing, the town store has none of its edges, and
// reading it there would say "not blocked". It blocks, naming the rig.
func TestBlockReason_HomeRigStoreUnavailableBlocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	x := newXrigTown(t, beadsdk.StatusClosed)

	daemonNoGastown := NewStoreResolver(x.townRoot, map[string]beadsdk.Storage{"hq": x.hq, "oag": x.oag})
	if reason := blockReason(ctx, x.hq, "gt-sib", daemonNoGastown); !strings.Contains(reason, "gastown") {
		t.Errorf("daemon resolver without the gastown store: reason = %q, want a block naming rig gastown", reason)
	}
	if reason := blockReason(ctx, x.hq, "gt-sib", x.closeResolver(nil, "gastown")); !strings.Contains(reason, "gastown") || !strings.Contains(reason, "connection refused") {
		t.Errorf("close resolver failing to open gastown: reason = %q, want a block naming rig gastown and the error", reason)
	}
}

// TestStoreResolver_CachesFailedOpen (review minor 5): a rig that failed to
// open is not re-opened for every lookup within one resolver's life.
func TestStoreResolver_CachesFailedOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	x := newXrigTown(t, beadsdk.StatusClosed)
	opens := map[string]int{}
	r := x.closeResolver(opens, "oag")

	for i := 0; i < 3; i++ {
		if reason := blockReason(ctx, x.hq, "gt-work", r); reason == "" {
			t.Fatalf("lookup %d: gt-work must be blocked while oag cannot open", i)
		}
	}
	if opens["oag"] != 1 {
		t.Errorf("oag opened %d times, want 1 (a failed open is remembered for the resolver's life)", opens["oag"])
	}
}

// TestFeedNextReadyIssue_HQBlocker (review I1) runs an hq-level blocker
// through both production resolvers: the daemon's, which holds every store
// including hq, and gt close's, which holds none and never opens hq (the
// caller's town store answers for hq). Open holds; closed feeds.
func TestFeedNextReadyIssue_HQBlocker(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"daemon", "close"} {
		for _, status := range []beadsdk.Status{beadsdk.StatusOpen, beadsdk.StatusClosed} {
			kind, status := kind, status
			t.Run(kind+"/"+string(status), func(t *testing.T) {
				t.Parallel()
				x := newXrigTown(t, beadsdk.StatusClosed).withHQBlocker(status)
				r := x.resolver(true)
				if kind == "close" {
					r = x.closeResolver(nil)
				}

				slung, logged := x.feed(t, r)

				if status == beadsdk.StatusOpen {
					if strings.Contains(slung, "gt-work") {
						t.Errorf("gt-work is blocked by open hq-blk; must not be slung, got %q", slung)
					}
					if !loggedLine(logged, "gt-work", "hq-blk (open)") {
						t.Errorf("expected a log naming gt-work blocked by open hq-blk, got %v", logged)
					}
					return
				}
				if !strings.Contains(slung, "sling gt-work gastown") {
					t.Errorf("hq-blk is closed; expected gt-work slung, got %q (log %v)", slung, logged)
				}
			})
		}
	}
}

// TestFeedNextReadyIssue_OpeningResolverReachesBlockerRig: gt close holds only
// the town store and opens a rig's store on first use (gt-tq6l). The blocker's
// rig has to be opened too, or every cross-rig blocker reads as unresolved and
// the bead is held forever even after its blocker closes.
func TestFeedNextReadyIssue_OpeningResolverReachesBlockerRig(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, beadsdk.StatusClosed)

	slung, logged := x.feed(t, x.closeResolver(nil))

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
