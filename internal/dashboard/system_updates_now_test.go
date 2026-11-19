package dashboard

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Now block names the roll and the sweep running at the poll, from the update
// section and the tier sweep the hub holds. A roll counts only while it runs, and
// the block is nil when neither runs, so the page draws no banner for an idle town.
func TestSystemUpdatesNowNamesTheRollAndSweepRunning(t *testing.T) {
	t.Parallel()

	running := &UpdateStatus{Running: true, RunID: "run-7", Current: "install", ElapsedSec: 480, StartedAt: sysTestClock}
	finished := &UpdateStatus{RunID: "run-6", Outcome: "ok", Current: "verify", ElapsedSec: 300, StartedAt: sysTestClock}
	sweeping := &TierSweep{Running: &TierSweepRun{Rig: "gastown", Tier: "integration", ElapsedSec: 42}}
	idle := &TierSweep{Sweeps: []TierSweepRow{{At: sysTestClock, Rig: "gastown", SHA: sysTestSHA(3)[:8]}}}

	cases := []struct {
		name   string
		update *UpdateStatus
		sweep  *TierSweep
		want   *SystemUpdatesNow
	}{
		{
			name:   "a roll running",
			update: running,
			sweep:  idle,
			want:   &SystemUpdatesNow{Roll: &NowRoll{RunID: "run-7", Step: "install", ElapsedSec: 480}},
		},
		{
			name:   "a sweep running",
			update: finished,
			sweep:  sweeping,
			want:   &SystemUpdatesNow{Sweep: &NowSweep{Rig: "gastown", Tier: "integration", ElapsedSec: 42}},
		},
		{
			name:   "a roll and a sweep running",
			update: running,
			sweep:  sweeping,
			want: &SystemUpdatesNow{
				Roll:  &NowRoll{RunID: "run-7", Step: "install", ElapsedSec: 480},
				Sweep: &NowSweep{Rig: "gastown", Tier: "integration", ElapsedSec: 42},
			},
		},
		{
			name:   "only finished runs",
			update: finished,
			sweep:  idle,
			want:   nil,
		},
		{
			name:   "nothing read",
			update: nil,
			sweep:  nil,
			want:   nil,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewHub(Config{
				Update:        func() *UpdateStatus { return tt.update },
				TierSweep:     func() *TierSweep { return tt.sweep },
				SystemUpdates: func() *SystemUpdates { return NewSystemUpdates(nil, nil, nil, false) },
			})
			h.pollUpdate()
			h.pollTierSweep()
			h.pollSystemUpdates()

			got := h.State().SystemUpdates
			require.NotNil(t, got, "the section is in the feed once its reader reports")
			assert.Equal(t, tt.want, got.Now)
		})
	}
}

// The section reaches the page's state feed as system_updates: its rows, and the
// Now block beside them.
func TestSystemUpdatesNowSectionReachesTheHubFeed(t *testing.T) {
	t.Parallel()

	sweep := []TierSweepRow{{At: sysTestClock, Rig: "gastown", SHA: sysTestSHA(3)[:8]}}
	h := NewHub(Config{
		Update: func() *UpdateStatus {
			return &UpdateStatus{Running: true, RunID: "run-9", Current: "install", ElapsedSec: 90}
		},
		SystemUpdates: func() *SystemUpdates { return NewSystemUpdates(sweep, nil, nil, false) },
	})
	h.pollUpdate()
	h.pollSystemUpdates()

	b, err := json.Marshal(h.State())
	require.NoError(t, err)
	var got struct {
		SystemUpdates struct {
			Now struct {
				Roll struct {
					RunID      string `json:"run_id"`
					Step       string `json:"step"`
					ElapsedSec int    `json:"elapsed_sec"`
				} `json:"roll"`
			} `json:"now"`
			Rows []struct {
				SHA string `json:"sha"`
			} `json:"rows"`
		} `json:"system_updates"`
	}
	require.NoError(t, json.Unmarshal(b, &got), "the state is not the feed's JSON")
	assert.Equal(t, "run-9", got.SystemUpdates.Now.Roll.RunID)
	assert.Equal(t, "install", got.SystemUpdates.Now.Roll.Step)
	assert.Equal(t, 90, got.SystemUpdates.Now.Roll.ElapsedSec)
	require.Len(t, got.SystemUpdates.Rows, 1)
	assert.Equal(t, sysTestSHA(3)[:8], got.SystemUpdates.Rows[0].SHA)
}

// Run starts the section's poller when the reader is wired: with a page watching
// and a one-nanosecond interval, the poller's first tick reads the section, and
// the hub's feed holds it once Run returns. The read signals the test, so the
// test never waits on the clock.
func TestSystemUpdatesNowRunStartsThePoller(t *testing.T) {
	t.Parallel()

	read := make(chan struct{}, 1)
	h := NewHub(Config{
		SystemUpdates: func() *SystemUpdates {
			// The one-nanosecond tick reads again and again: only the first read
			// is awaited, so the rest must not block on a full channel.
			select {
			case read <- struct{}{}:
			default:
			}
			return NewSystemUpdates(nil, nil, nil, false)
		},
		SystemUpdatesEvery: time.Nanosecond,
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()
	wantSignal(t, read, "the section's poller never read the section")
	stop()
	wantSignal(t, done, "the hub did not stop")

	assert.NotNil(t, h.State().SystemUpdates, "the poller's read is not in the feed")
}
