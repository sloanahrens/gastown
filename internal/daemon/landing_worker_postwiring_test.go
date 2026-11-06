package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/landworker"
)

// statusClientByRef answers a combined-status read from a script keyed by the
// ref asked for, and records every (owner, repo, ref) it was asked.
type statusClientByRef struct {
	mu      sync.Mutex
	byRef   map[string]*forgejo.CombinedStatus
	asked   [][3]string
	missing *forgejo.CombinedStatus
}

func (c *statusClientByRef) CombinedStatus(_ context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, [3]string{owner, repo, ref})
	if cs, ok := c.byRef[ref]; ok {
		return cs, nil
	}
	return c.missing, nil
}

func statusOf(name string, state forgejo.CommitState) *forgejo.CombinedStatus {
	return &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{Context: name, Status: state}}}
}

const wiringContext = "ci / post-land (push)"

// postLandWiringFixture builds the wiring the way newRigLandingWorker does, over
// fakes, and the runner newPostLandRunner makes from it.
type postLandWiringFixture struct {
	f      *attentionFixture
	clk    *clockwork.FakeClock
	client *statusClientByRef
	store  *switchMainState
	runner *landworker.PostLandRunner
	wiring postLandWiring
	older  string
	newer  string
}

func newPostLandWiringFixture(t *testing.T, timeout time.Duration) *postLandWiringFixture {
	t.Helper()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	w := &postLandWiringFixture{
		f:      newAttentionFixture(t, now),
		clk:    clockwork.NewFakeClockAt(now),
		client: &statusClientByRef{byRef: map[string]*forgejo.CombinedStatus{}, missing: &forgejo.CombinedStatus{}},
		store:  &switchMainState{},
		older:  "1111111111111111111111111111111111111111",
		newer:  "2222222222222222222222222222222222222222",
	}
	w.f.d.clock = w.clk
	redMain := &landworker.RedMain{
		Rig:   attentionRig,
		Beads: beadsfake.New(beadsfake.WithPrefix("gt")),
		State: w.store,
		Logf:  func(string, ...any) {},
	}
	isAncestor := func(ancestor, descendant string) (bool, error) {
		return ancestor == w.older && descendant == w.newer, nil
	}
	w.wiring = newPostLandWiring(w.client, "sloan", "gastown", w.store, timeout, nil, redMain.Beads, redMain, isAncestor)
	rigPath := t.TempDir()
	writeRigPostLandStatusContext(t, rigPath, wiringContext)
	w.runner = w.f.d.newPostLandRunner(attentionRig, rigPath, w.wiring)
	return w
}

// TestPostLandWiringReadsTheLandedCommitsStatus: the status a landing's verdict
// comes from is that commit's, asked of the rig's own repository, never the
// status of whatever main is by the time the pass runs (owl gt-3cjvrp).
func TestPostLandWiringReadsTheLandedCommitsStatus(t *testing.T) {
	t.Parallel()
	w := newPostLandWiringFixture(t, time.Hour)
	if _, err := w.wiring.Status(context.Background(), landworker.PostLand{Commit: w.older}); err != nil {
		t.Fatal(err)
	}
	if len(w.client.asked) != 1 || w.client.asked[0] != [3]string{"sloan", "gastown", w.older} {
		t.Fatalf("status asked of %v, want owner sloan, repo gastown, ref %s (the landed commit)", w.client.asked, w.older)
	}
}

// TestPostLandWiringDecidesInLandingOrder drives the wiring end to end: the
// state store is the one given (pending entries and verdicts land in it), the
// ancestry reader orders verdicts, so an older commit's green after a newer one
// leaves LastGreen at the newer commit.
func TestPostLandWiringDecidesInLandingOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newPostLandWiringFixture(t, time.Hour)
	w.client.byRef[w.older] = statusOf(wiringContext, forgejo.StatePending)
	w.client.byRef[w.newer] = statusOf(wiringContext, forgejo.StateSuccess)
	w.runner.Trigger(ctx, landworker.PostLand{BeadID: "gt-old", Commit: w.older, Target: "main"})
	w.runner.Trigger(ctx, landworker.PostLand{BeadID: "gt-new", Commit: w.newer, Target: "main"})

	st, _ := w.store.Load()
	if len(st.PostLandPending) != 2 {
		t.Fatalf("pending in the wired store = %d, want both landings recorded there", len(st.PostLandPending))
	}
	w.runner.PollPending(ctx)
	w.client.byRef[w.older] = statusOf(wiringContext, forgejo.StateSuccess)
	w.clk.Advance(time.Minute)
	w.runner.PollPending(ctx)

	st, _ = w.store.Load()
	if st.LastGreen != w.newer || st.LastRun != w.newer {
		t.Fatalf("LastGreen %q LastRun %q, want both at the newer commit: the ancestry reader must be wired", st.LastGreen, st.LastRun)
	}
}

// TestPostLandWiringHonoursTheConfiguredTimeout: the timeout the wiring carries
// is the one the timeout item fires at, not the 60 minute default.
func TestPostLandWiringHonoursTheConfiguredTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const timeout = 17 * time.Minute
	w := newPostLandWiringFixture(t, timeout)
	w.client.byRef[w.older] = statusOf(wiringContext, forgejo.StatePending)
	w.runner.Trigger(ctx, landworker.PostLand{BeadID: "gt-old", Commit: w.older, Target: "main"})
	w.runner.PollPending(ctx)
	w.clk.Advance(timeout - time.Second)
	if items := postLandItems(t, w.f); len(items) != 0 {
		t.Fatalf("%d items one second before the 17 minute timeout", len(items))
	}
	w.clk.Advance(time.Second)
	if items := postLandItems(t, w.f); len(items) != 1 {
		t.Fatalf("%d items at the 17 minute timeout, want one", len(items))
	}
}

// TestAttentionCollectorsIncludeThePostLandKind: the post-land alarms reach the
// queue only through the collector list a tick runs. A function nobody lists is
// no alarm.
func TestAttentionCollectorsIncludeThePostLandKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newPostLandWiringFixture(t, time.Hour)
	w.store.failLoad(errors.New("permission denied"))
	w.f.src.now = w.clk.Now()

	var found bool
	for _, c := range w.f.src.collectors() {
		if c.kind != attention.KindPostLandUnknown {
			continue
		}
		found = true
		items, err := c.collect(ctx)
		if err != nil {
			t.Fatalf("collect: %v", err)
		}
		if len(items) != 1 || items[0].Key != "post-land-store-failure:"+attentionRig {
			t.Fatalf("the listed collector returned %+v, want the unreadable store as an item", items)
		}
	}
	if !found {
		t.Fatal("the post-land collector is not in collectors(): its alarms would never reach the queue")
	}
}

// TestPostLandStoreFailureItemForAStoreThatReadsButCannotBeWritten: a pass that
// cannot save what it learned counts toward the blind-safety-net item even
// though every read works (the unreadable store has its own branch).
func TestPostLandStoreFailureItemForAStoreThatReadsButCannotBeWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newPostLandWiringFixture(t, time.Hour)
	w.client.byRef[w.older] = statusOf(wiringContext, forgejo.StatePending)
	w.runner.Trigger(ctx, landworker.PostLand{BeadID: "gt-old", Commit: w.older, Target: "main"})
	w.store.failSave(errors.New("disk full"))
	for range landworker.PostLandStatusStoreFailureAfter {
		w.clk.Advance(time.Minute)
		w.runner.PollPending(ctx)
	}
	items := postLandItems(t, w.f)
	if len(items) != 1 || items[0].Key != "post-land-store-failure:"+attentionRig || !strings.Contains(items[0].Summary, "disk full") {
		t.Fatalf("items = %+v, want the store-failure item naming the write error", items)
	}
}

// TestPostLandUnreadableRigSettingsRaisesAnAttentionItem: a settings file that
// cannot be read turns status mode off for the rig, so landings record no
// pending verdict and none is ever reached. That is an item at once, cleared
// when the file reads again; a missing file is no setting and no item.
func TestPostLandUnreadableRigSettingsRaisesAnAttentionItem(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	rigPath := t.TempDir()
	f.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{State: &landworker.MemoryMainState{}})

	if items := postLandItems(t, f); len(items) != 0 {
		t.Fatalf("items %+v for a rig with no settings file, want none", items)
	}
	path := config.RigSettingsPath(rigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json \x1b[31m"), 0o600); err != nil {
		t.Fatal(err)
	}
	items := postLandItems(t, f)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want the one settings item", items)
	}
	it := items[0]
	if it.Key != "post-land-settings-unreadable:"+attentionRig || it.Kind != attention.KindPostLandUnknown ||
		it.Severity != attention.SeverityLow || it.Rig != attentionRig {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(it.Summary, "status mode is off") || strings.ContainsAny(it.Summary, "\x1b\n") {
		t.Fatalf("summary %q: want it to say status mode is off, as one clean line", it.Summary)
	}

	if err := os.Remove(path); err != nil { // the town refuses to rewrite a file it cannot parse
		t.Fatal(err)
	}
	writeRigPostLandStatusContext(t, rigPath, wiringContext)
	if items := postLandItems(t, f); len(items) != 0 {
		t.Fatalf("items %+v after the settings read again, want the item cleared", items)
	}
}
