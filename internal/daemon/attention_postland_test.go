package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/landworker"
)

// postLandRunnerOver registers a rig runner over store with the given timeout.
func postLandRunnerOver(f *attentionFixture, store landworker.MainStateStore, timeout time.Duration) *landworker.PostLandRunner {
	r := &landworker.PostLandRunner{Rig: attentionRig, State: store, Timeout: timeout, Logf: func(string, ...any) {}}
	f.d.postLandRunners.set(attentionRig, r)
	return r
}

// TestCollectPostLandUnknown: the collector derives one item per timed-out
// pending entry from the rig's own durable state, naming the context and the
// last state, and none for an entry still inside its timeout.
func TestCollectPostLandUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	const timeout = 30 * time.Minute
	store := &landworker.MemoryMainState{}
	if err := store.Save(landworker.MainState{PostLandPending: []landworker.PendingPostLand{
		{Land: landworker.PostLand{Commit: "abc1234567890"}, Context: "ci / post-land (push)", Since: now.Add(-time.Hour), LastState: "missing"},
		{Land: landworker.PostLand{Commit: "fresh1234567"}, Context: "ci / post-land (push)", Since: now.Add(-time.Minute), LastState: "pending"},
	}}); err != nil {
		t.Fatal(err)
	}
	postLandRunnerOver(f, store, timeout)

	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the timed-out entry only", items)
	}
	it := items[0]
	if it.Key != "post-land-unknown:"+attentionRig+":abc1234567890" || it.Kind != attention.KindPostLandUnknown ||
		it.Severity != attention.SeverityLow || it.Rig != attentionRig || it.SHA != "abc1234567890" {
		t.Errorf("item = %+v", it)
	}
	if !strings.Contains(it.Summary, "ci / post-land (push)") || !strings.Contains(it.Summary, "missing") {
		t.Errorf("summary %q does not name the context and last state", it.Summary)
	}

	// A restarted daemon builds a new collector over the same state and derives
	// the same item: nothing was held only in memory.
	f2 := newAttentionFixture(t, now)
	postLandRunnerOver(f2, store, timeout)
	again, err := f2.src.collectPostLandUnknown(context.Background())
	if err != nil || len(again) != 1 || again[0].Key != it.Key {
		t.Fatalf("items after a restart = %+v (%v), want the same keyed item", again, err)
	}
}

// A timed-out entry stops raising when its alarm window ends.
func TestCollectPostLandUnknownEndsWithTheWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	store := &landworker.MemoryMainState{}
	if err := store.Save(landworker.MainState{PostLandPending: []landworker.PendingPostLand{
		{Land: landworker.PostLand{Commit: "old1234567890"}, Context: "ci", Since: now.Add(-landworker.PostLandAlarmWindow - 2*time.Hour)},
	}}); err != nil {
		t.Fatal(err)
	}
	postLandRunnerOver(f, store, time.Hour)
	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %+v (%v), want none past the window", items, err)
	}
}

type unreadableState struct{ err error }

func (u unreadableState) Load() (landworker.MainState, error) { return landworker.MainState{}, u.err }
func (u unreadableState) Save(landworker.MainState) error     { return u.err }

// An unreadable pending store is an item, never zero alarms.
func TestCollectPostLandUnknownUnreadableStoreIsAnItem(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	postLandRunnerOver(f, unreadableState{err: errors.New("permission denied")}, time.Hour)
	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 || items[0].Key != "post-land-store-failure:"+attentionRig || !strings.Contains(items[0].Summary, "permission denied") {
		t.Fatalf("items = %+v, want the one unreadable-store item naming the failure", items)
	}
}

// Entries the bound dropped are an item of their own.
func TestCollectPostLandUnknownReportsDroppedEntries(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f := newAttentionFixture(t, now)
	store := &landworker.MemoryMainState{}
	if err := store.Save(landworker.MainState{PostLandDropped: &landworker.PostLandDropped{Count: 4, Oldest: "dead1234567890", At: now.Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	postLandRunnerOver(f, store, time.Hour)
	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil || len(items) != 1 || items[0].Key != "post-land-dropped:"+attentionRig || !strings.Contains(items[0].Summary, "4 pending") {
		t.Fatalf("items = %+v (%v), want the one dropped-entries item", items, err)
	}
}
