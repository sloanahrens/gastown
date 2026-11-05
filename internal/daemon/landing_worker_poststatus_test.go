package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/landworker"
)

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
	pl := landworker.PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	pending := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
		Context: "ci / post-land (push)", Status: forgejo.StatePending,
	}}}
	success := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
		Context: "ci / post-land (push)", Status: forgejo.StateSuccess,
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
}

// postLandItems is the attention items the daemon's collector raises now.
func postLandItems(t *testing.T, f *attentionFixture) []attention.Item {
	t.Helper()
	items, err := f.src.collectPostLandUnknown(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return items
}
