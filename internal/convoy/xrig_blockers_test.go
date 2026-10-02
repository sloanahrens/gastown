package convoy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// xrigTown is a town convoy tracking two gastown beads. gt-work (priority 1,
// fed first) is blocked by oag-x, which lives in a third rig, oag.
type xrigTown struct {
	townRoot string
	hq       *fakeStore
	gastown  *fakeStore
	oag      *fakeStore
}

func newXrigTown(t *testing.T, blockerStatus string) *xrigTown {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	task := func(id string, priority int, status string) *beads.Issue {
		return &beads.Issue{ID: id, Title: id, Status: status, Priority: priority, Type: "task", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339)}
	}

	x := &xrigTown{
		townRoot: t.TempDir(),
		hq:       newFakeRigStore(task("hq-cv1", 2, "open")),
		gastown:  newFakeRigStore(task("gt-work", 1, "open"), task("gt-sib", 2, "open")),
		oag:      newFakeRigStore(task("oag-x", 2, blockerStatus)),
	}
	x.hq.edge("hq-cv1", "external:gt:gt-work", "tracks")
	x.hq.edge("hq-cv1", "external:gt:gt-sib", "tracks")
	x.gastown.edge("gt-work", "external:oag:oag-x", "blocks")

	if err := beads.WriteRoutes(filepath.Join(x.townRoot, ".beads"), []beads.Route{
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
func (x *xrigTown) withHQBlocker(status string) *xrigTown {
	x.hq.seed(beads.Issue{ID: "hq-blk", Title: "hq-blk", Status: status, Type: "task"})
	x.gastown.clearEdges()
	x.gastown.edge("gt-work", "hq-blk", "blocks")
	return x
}

// closeResolver is the resolver gt close builds: it holds no store up front,
// never holds hq, and opens a rig's store on first use (close.go). opens
// counts open calls per rig; failOpen names rigs whose open fails.
func (x *xrigTown) closeResolver(opens map[string]int, failOpen ...string) *StoreResolver {
	rigs := map[string]IssueSource{"gastown": x.gastown, "oag": x.oag}
	return NewOpeningStoreResolver(x.townRoot, func(name string) (IssueSource, error) {
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
	stores := map[string]IssueSource{"hq": x.hq, "gastown": x.gastown}
	if withOag {
		stores["oag"] = x.oag
	}
	return NewStoreResolver(x.townRoot, stores)
}

// feed runs the continuation feed on the town convoy and returns what was
// slung and what was logged.
func (x *xrigTown) feed(t *testing.T, resolver *StoreResolver) (slung string, logged []string) {
	t.Helper()
	gt := &slingLog{}
	logger, msgs := makeLogger()
	feedNextReadyIssue(context.Background(), x.hq, x.townRoot, "hq-cv1", "test", logger, gt.sling, func(string) bool { return false }, resolver)
	data, err := gt.read()
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
	x := newXrigTown(t, "open")

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
	x := newXrigTown(t, "closed")

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
	x := newXrigTown(t, "closed")

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
	x := newXrigTown(t, "closed")
	x.oag.forget("oag-x")

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

	x := newXrigTown(t, "closed")
	reason := BlockReason(ctx, x.hq, "gt-work", x.closeResolver(nil, "oag"))
	if !strings.Contains(reason, "oag-x") || !strings.Contains(reason, "unreadable") || !strings.Contains(reason, "connection refused") {
		t.Errorf("open failure: reason = %q, want oag-x unreadable with the open error", reason)
	}

	y := newXrigTown(t, "closed")
	y.oag.readErr = errors.New("dolt: query timeout")
	reason = BlockReason(ctx, y.hq, "gt-work", y.resolver(true))
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
	x := newXrigTown(t, "closed")

	daemonNoGastown := NewStoreResolver(x.townRoot, map[string]IssueSource{"hq": x.hq, "oag": x.oag})
	if reason := BlockReason(ctx, x.hq, "gt-sib", daemonNoGastown); !strings.Contains(reason, "gastown") {
		t.Errorf("daemon resolver without the gastown store: reason = %q, want a block naming rig gastown", reason)
	}
	if reason := BlockReason(ctx, x.hq, "gt-sib", x.closeResolver(nil, "gastown")); !strings.Contains(reason, "gastown") || !strings.Contains(reason, "connection refused") {
		t.Errorf("close resolver failing to open gastown: reason = %q, want a block naming rig gastown and the error", reason)
	}
}

// TestBlockOf_NamesTheFailSafeHold (gt-gg7w9): the hold a caller escalates is
// told apart from an open blocker, and from each other, by Cause and BlockerID,
// not by matching the reason text.
func TestBlockOf_NamesTheFailSafeHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	open := newXrigTown(t, "open")
	if b := BlockOf(ctx, open.hq, "gt-work", open.resolver(true)); b.Reason == "" || b.Held() || b.Cause != BlockOpen {
		t.Errorf("open blocker: got %+v, want a reason that is not a fail-safe hold", b)
	}

	closed := newXrigTown(t, "closed")
	if b := BlockOf(ctx, closed.hq, "gt-work", closed.resolver(true)); b != (Block{}) {
		t.Errorf("closed blocker: got %+v, want no block", b)
	}

	dangling := newXrigTown(t, "closed")
	dangling.oag.forget("oag-x")
	if b := BlockOf(ctx, dangling.hq, "gt-work", dangling.resolver(true)); b.Cause != BlockUnresolved || b.BlockerID != "oag-x" || !b.Held() {
		t.Errorf("dangling edge: got %+v, want unresolved oag-x", b)
	}

	down := newXrigTown(t, "closed")
	down.oag.readErr = errors.New("dolt: query timeout")
	if b := BlockOf(ctx, down.hq, "gt-work", down.resolver(true)); b.Cause != BlockUnreadable || b.BlockerID != "oag-x" || !b.Held() {
		t.Errorf("rig store down: got %+v, want unreadable oag-x", b)
	}

	// The bead's own store failing names no blocker: there is none to name yet.
	noHome := newXrigTown(t, "closed")
	gone := NewStoreResolver(noHome.townRoot, map[string]IssueSource{"hq": noHome.hq, "oag": noHome.oag})
	if b := BlockOf(ctx, noHome.hq, "gt-sib", gone); b.Cause != BlockUnreadable || b.BlockerID != "" {
		t.Errorf("home store unavailable: got %+v, want unreadable with no blocker id", b)
	}
}

// TestStoreResolver_CachesFailedOpen (review minor 5): a rig that failed to
// open is not re-opened for every lookup within one resolver's life.
func TestStoreResolver_CachesFailedOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	x := newXrigTown(t, "closed")
	opens := map[string]int{}
	r := x.closeResolver(opens, "oag")

	for i := 0; i < 3; i++ {
		if reason := BlockReason(ctx, x.hq, "gt-work", r); reason == "" {
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
		for _, status := range []string{"open", "closed"} {
			kind, status := kind, status
			t.Run(kind+"/"+status, func(t *testing.T) {
				t.Parallel()
				x := newXrigTown(t, "closed").withHQBlocker(status)
				r := x.resolver(true)
				if kind == "close" {
					r = x.closeResolver(nil)
				}

				slung, logged := x.feed(t, r)

				if status == "open" {
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
	x := newXrigTown(t, "closed")

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
	x := newXrigTown(t, "closed")

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
	x := newXrigTown(t, "closed")
	joinedOnly := joinedOnlySource{x.gastown}

	if !isIssueBlocked(context.Background(), joinedOnly, "gt-sib", nil) {
		t.Error("a store without raw dependency records must fail safe (blocked)")
	}
}

// TestTrackedIDs_StoreWithoutRawRecordsErrors pins the reviewer's point on
// 9c53a58: falling back to the joined view silently loses every cross-rig
// tracked bead, so it is an error instead.
func TestTrackedIDs_StoreWithoutRawRecordsErrors(t *testing.T) {
	t.Parallel()
	x := newXrigTown(t, "closed")

	if _, err := trackedIDs(joinedOnlySource{x.hq}, "hq-cv1"); err == nil {
		t.Error("trackedIDs on a store without raw dependency records must return an error")
	}
	ids, err := trackedIDs(x.hq, "hq-cv1")
	if err != nil || len(ids) != 2 {
		t.Errorf("trackedIDs = %v, %v; want gt-work and gt-sib", ids, err)
	}
}
