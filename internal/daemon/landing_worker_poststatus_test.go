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
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/landworker"
)

// switchMainState is a MainStateStore a test can make fail at reads or writes.
type switchMainState struct {
	inner   landworker.MemoryMainState
	mu      sync.Mutex
	loadErr error
	saveErr error
}

func (s *switchMainState) Load() (landworker.MainState, error) {
	s.mu.Lock()
	err := s.loadErr
	s.mu.Unlock()
	if err != nil {
		return landworker.MainState{}, err
	}
	return s.inner.Load()
}

func (s *switchMainState) Save(st landworker.MainState) error {
	s.mu.Lock()
	err := s.saveErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.inner.Save(st)
}

func (s *switchMainState) failLoad(err error) { s.mu.Lock(); s.loadErr = err; s.mu.Unlock() }
func (s *switchMainState) failSave(err error) { s.mu.Lock(); s.saveErr = err; s.mu.Unlock() }

// writeRigPostLandStatusContext writes merge_queue.post_land_status_context
// into the rig's own settings, the only file the daemon reads it from.
func writeRigPostLandStatusContext(t *testing.T, rigPath, name string) {
	t.Helper()
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), &config.RigSettings{
		Type:       "rig-settings",
		Version:    1,
		MergeQueue: &config.MergeQueueConfig{PostLandStatusContext: name},
	}); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}
}

// TestDaemonPostLandStatusTimeoutStoresOneAttentionItem drives a rig's status
// wiring through newPostLandRunner, the daemon's real closures, and reads the
// attention collector that the daemon's queue reads. The timeout is the
// configured 17 minutes, not the 60 minute default.
func TestDaemonPostLandStatusTimeoutStoresOneAttentionItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	const timeout = 17 * time.Minute
	const name = "ci / post-land (push)"
	pl := landworker.PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	pending := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
		Context: name, Status: forgejo.StatePending,
	}}}
	success := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
		Context: name, Status: forgejo.StateSuccess,
	}}}

	cases := []struct {
		name      string
		script    []*forgejo.CombinedStatus
		wantItems int
		wantState string
	}{
		{name: "pending at the timeout stores one item", script: []*forgejo.CombinedStatus{pending}, wantItems: 1, wantState: "pending"},
		{name: "missing at the timeout stores one item", script: []*forgejo.CombinedStatus{{}}, wantItems: 1, wantState: "missing"},
		{name: "a verdict before the timeout stores none", script: []*forgejo.CombinedStatus{pending, success}, wantItems: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAttentionFixture(t, now)
			clk := clockwork.NewFakeClockAt(now)
			f.d.clock = clk
			rigPath := t.TempDir()
			writeRigPostLandStatusContext(t, rigPath, "ci / post-land (push)")

			reads := 0
			runner := f.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{
				Status: func(context.Context, landworker.PostLand) (*forgejo.CombinedStatus, error) {
					i := min(reads, len(tc.script)-1)
					reads++
					return tc.script[i], nil
				},
				State:   &landworker.MemoryMainState{},
				Timeout: timeout,
			})
			runner.Trigger(ctx, pl)

			// A worker pass each minute: nothing is stored before the timeout.
			for minute := 0; minute < 17; minute++ {
				runner.PollPending(ctx)
				if items := postLandItems(t, f); len(items) != 0 {
					t.Fatalf("%d items at %dm, before the 17 minute timeout", len(items), minute)
				}
				clk.Advance(time.Minute)
			}
			runner.PollPending(ctx)
			items := postLandItems(t, f)
			if len(items) != tc.wantItems {
				t.Fatalf("items at 17m = %d, want %d: %+v", len(items), tc.wantItems, items)
			}
			if tc.wantItems == 1 {
				it := items[0]
				if want := "post-land-unknown:" + attentionRig + ":" + pl.Commit; it.Key != want {
					t.Fatalf("key = %q, want %q", it.Key, want)
				}
				if it.Rig != attentionRig || it.SHA != pl.Commit || it.Kind != attention.KindPostLandUnknown {
					t.Fatalf("item = %+v", it)
				}
				if !strings.Contains(it.Summary, "ci / post-land (push)") || !strings.Contains(it.Summary, tc.wantState) {
					t.Fatalf("summary %q does not name the context and last state %q", it.Summary, tc.wantState)
				}
			}

			clk.Advance(time.Minute)
			runner.PollPending(ctx)
			if got := postLandItems(t, f); len(got) != tc.wantItems {
				t.Fatalf("items on a later pass = %d, want still %d", len(got), tc.wantItems)
			}
		})
	}

	// The item is raised before the pending entry is removed and saved, so a
	// daemon that stops between the two finds the entry again and raises the
	// item again rather than losing it.
	t.Run("a restart between the callback and the removal raises the item again", func(t *testing.T) {
		t.Parallel()
		rigPath := t.TempDir()
		writeRigPostLandStatusContext(t, rigPath, name)
		read := func(context.Context, landworker.PostLand) (*forgejo.CombinedStatus, error) {
			return pending, nil
		}
		store := &switchMainState{}
		if err := store.inner.Save(landworker.MainState{PostLandPending: []landworker.PendingPostLand{{
			Land: pl, Context: name, Since: now,
		}}}); err != nil {
			t.Fatalf("seeding the main state: %v", err)
		}

		f := newAttentionFixture(t, now)
		f.d.clock = clockwork.NewFakeClockAt(now.Add(timeout))
		store.failSave(errors.New("disk full"))
		first := f.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{Status: read, State: store, Timeout: timeout})
		first.PollPending(ctx)
		if items := postLandItems(t, f); len(items) != 1 {
			t.Fatalf("items after the timed-out pass = %d, want one: %+v", len(items), items)
		}
		if st, err := store.inner.Load(); err != nil || len(st.PostLandPending) != 1 {
			t.Fatalf("the failed save left %+v (%v), want the pending entry kept", st.PostLandPending, err)
		}

		// The daemon restarts: a new attention store, the same main state,
		// whose entry the failed save left behind.
		restarted := now.Add(timeout + time.Minute)
		f2 := newAttentionFixture(t, restarted)
		f2.d.clock = clockwork.NewFakeClockAt(restarted)
		store.failSave(nil)
		second := f2.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{Status: read, State: store, Timeout: timeout})
		second.PollPending(ctx)

		items := postLandItems(t, f2)
		if len(items) != 1 {
			t.Fatalf("items after the restart = %d, want the timeout raised again: %+v", len(items), items)
		}
		if want := "post-land-unknown:" + attentionRig + ":" + pl.Commit; items[0].Key != want {
			t.Fatalf("key = %q, want %q", items[0].Key, want)
		}
	})
}

// TestDaemonPostLandStatusStoreFailureStoresAndClearsOneItem drives the
// runner's store-failure record through the daemon's own wiring: three failed
// passes raise one item keyed by rig, a fourth raises none, and the pass that
// reads the store again clears it.
func TestDaemonPostLandStatusStoreFailureStoresAndClearsOneItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	const name = "ci / post-land (push)"
	f := newAttentionFixture(t, now)
	clk := clockwork.NewFakeClockAt(now)
	f.d.clock = clk
	rigPath := t.TempDir()
	writeRigPostLandStatusContext(t, rigPath, name)
	store := &switchMainState{}
	if err := store.inner.Save(landworker.MainState{PostLandPending: []landworker.PendingPostLand{{
		Land: landworker.PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}, Context: name, Since: now,
	}}}); err != nil {
		t.Fatalf("seeding the main state: %v", err)
	}
	store.failLoad(errors.New("permission denied"))
	runner := f.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{
		Status: func(context.Context, landworker.PostLand) (*forgejo.CombinedStatus, error) {
			return &forgejo.CombinedStatus{}, nil
		},
		State:   store,
		Timeout: 17 * time.Minute,
	})

	// A store that cannot be READ is an item at once (never zero alarms), one
	// keyed by rig however many passes fail.
	for pass := 1; pass <= landworker.PostLandStatusStoreFailureAfter; pass++ {
		runner.PollPending(ctx)
		clk.Advance(time.Minute)
		if items := postLandItems(t, f); len(items) != 1 {
			t.Fatalf("items on failed pass %d = %d, want the one store-failure item", pass, len(items))
		}
	}
	it := postLandItems(t, f)[0]
	if want := "post-land-store-failure:" + attentionRig; it.Key != want {
		t.Fatalf("key = %q, want %q", it.Key, want)
	}
	if it.Kind != attention.KindPostLandUnknown || it.Rig != attentionRig || it.SHA != "" {
		t.Fatalf("item = %+v", it)
	}
	if !strings.Contains(it.Summary, attentionRig) || !strings.Contains(it.Summary, "permission denied") {
		t.Fatalf("summary %q does not name the rig and the failure", it.Summary)
	}
	runner.PollPending(ctx)
	clk.Advance(time.Minute)
	if got := postLandItems(t, f); len(got) != 1 {
		t.Fatalf("items on a later failed pass = %d, want still one", len(got))
	}

	store.failLoad(nil)
	runner.PollPending(ctx)
	if got := postLandItems(t, f); len(got) != 0 {
		t.Fatalf("items after the good pass = %d, want the failure cleared: %+v", len(got), got)
	}
}

// postLandItems is the attention items the daemon's collector raises now.
func postLandItems(t *testing.T, f *attentionFixture) []attention.Item {
	t.Helper()
	f.src.now = f.d.clk().Now() // the collector reads the daemon's clock, as a tick does
	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return items
}

// TestDaemonPostLandUnreadableRigSettingsSaysStatusModeIsOff: a rig settings
// file that exists but cannot be read turns status mode off, and the daemon says
// so once per reason, not on every trigger and pass; a missing file says nothing.
func TestDaemonPostLandUnreadableRigSettingsSaysStatusModeIsOff(t *testing.T) {
	t.Parallel()
	f := newAttentionFixture(t, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC))
	rigPath := t.TempDir()
	runner := f.d.newPostLandRunner(attentionRig, rigPath, postLandWiring{})
	for range 3 {
		runner.Trigger(context.Background(), landworker.PostLand{Commit: "abc1234567890", Target: "main"})
	}
	if strings.Contains(f.logs.String(), "cannot be read") {
		t.Fatalf("a missing settings file was reported as unreadable:\n%s", f.logs.String())
	}
	if err := os.MkdirAll(filepath.Dir(config.RigSettingsPath(rigPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.RigSettingsPath(rigPath), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runner.Trigger(context.Background(), landworker.PostLand{Commit: "abc1234567890", Target: "main"})
	}
	if n := strings.Count(f.logs.String(), "status mode is off"); n != 1 {
		t.Fatalf("the unreadable-settings line was logged %d times over three triggers, want once:\n%s", n, f.logs.String())
	}
}
