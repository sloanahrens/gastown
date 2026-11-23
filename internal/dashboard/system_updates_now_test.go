package dashboard

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Now block names the roll and the sweep running at the poll, read from the
// update and tier sweep readers. A roll counts only while it runs, and the block
// is nil when neither runs, so the page draws no banner for an idle town.
func TestSystemUpdatesNowNamesTheRollAndSweepRunning(t *testing.T) {
	t.Parallel()

	running := &UpdateStatus{Running: true, RunID: "run-7", Current: "install", ElapsedSec: 480, StartedAt: sysTestClock}
	finished := &UpdateStatus{RunID: "run-6", Outcome: "ok", Current: "verify", ElapsedSec: 300, StartedAt: sysTestClock}
	sweeping := &TierSweep{Running: &TierSweepRun{Rig: "gastown", Tier: "integration", ElapsedSec: 42}}
	idle := &TierSweep{Sweeps: []TierSweepRow{{At: sysTestClock, Rig: "gastown", SHA: sysTestSHA(3)[:8]}}}

	cases := []struct {
		name             string
		update           *UpdateStatus
		sweep            *TierSweep
		want             *SystemUpdatesNow
		wantSweepUnknown bool
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
			name:             "nothing read",
			update:           nil,
			sweep:            nil,
			want:             nil,
			wantSweepUnknown: true,
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
			h.pollSystemUpdates()

			got := h.State().SystemUpdates
			require.NotNil(t, got, "the section is in the feed once its reader reports")
			assert.Equal(t, tt.want, got.Now)
			assert.Equal(t, tt.wantSweepUnknown, got.SweepUnknown, "the sweep source's readability was misread")
		})
	}
}

// The Now block is built from the readers at the section's own poll, not from
// what an older poll left in the feed: a roll that ended before the section poll
// is not named as running.
func TestSystemUpdatesNowReadsItsSourcesAtThePoll(t *testing.T) {
	t.Parallel()

	running := &UpdateStatus{Running: true, RunID: "run-7", Current: "install", ElapsedSec: 480}
	h := NewHub(Config{
		Update:        func() *UpdateStatus { return running },
		SystemUpdates: func() *SystemUpdates { return NewSystemUpdates(nil, nil, nil, false) },
	})
	h.pollUpdate()
	require.NotNil(t, h.State().Update, "the feed's update poll did not read the running roll")

	running = &UpdateStatus{RunID: "run-7", Outcome: "ok", Current: "verify"}
	h.pollSystemUpdates()

	got := h.State().SystemUpdates
	require.NotNil(t, got, "the section is in the feed once its reader reports")
	assert.Nil(t, got.Now, "the section named the roll an older poll left in the feed")
}

// A sweep source that cannot be read is unknown, not idle: the section says so
// rather than letting a missing Now.Sweep read as a town with no sweep running.
func TestSystemUpdatesNowSaysWhatItCannotRead(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		sweep func() *TierSweep
	}{
		{name: "no sweep reader"},
		{name: "a sweep reader that answers nothing", sweep: func() *TierSweep { return nil }},
		{name: "an unreadable sweep log", sweep: func() *TierSweep { return &TierSweep{Unavailable: true} }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := NewHub(Config{
				TierSweep:     tt.sweep,
				SystemUpdates: func() *SystemUpdates { return NewSystemUpdates(nil, nil, nil, false) },
			})
			h.pollSystemUpdates()

			got := h.State().SystemUpdates
			require.NotNil(t, got, "the section is in the feed once its reader reports")
			assert.True(t, got.SweepUnknown, "a sweep source that could not be read reads as idle")
			assert.Nil(t, got.Now, "no sweep runs that this poll can name")
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
		SystemUpdates: func() *SystemUpdates {
			su := NewSystemUpdates(sweep, nil, nil, false)
			su.RollUnknown = true // what the supply sets when the update record cannot be read
			return su
		},
	})
	h.pollUpdate()
	h.pollSystemUpdates()

	b, err := json.Marshal(h.State())
	require.NoError(t, err)
	var got struct {
		SystemUpdates struct {
			RollUnknown  bool `json:"roll_unknown"`
			SweepUnknown bool `json:"sweep_unknown"`
			Now          struct {
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
	assert.True(t, got.SystemUpdates.RollUnknown, "the unreadable-roll flag did not reach the page")
	assert.True(t, got.SystemUpdates.SweepUnknown, "no tier sweep reader, so the sweep half is unknown")
}

// Run starts the section's poller when the reader is wired. The interval is an
// hour, so the ticker cannot be what reads: the page's connect wakes the parked
// worker, and the test wakes the hub again until that one read lands. The read
// signals the test, so nothing here waits on the clock.
func TestSystemUpdatesNowRunStartsThePoller(t *testing.T) {
	t.Parallel()

	read := make(chan struct{}, 1)
	h := NewHub(Config{
		SystemUpdates: func() *SystemUpdates {
			select {
			case read <- struct{}{}:
			default:
			}
			return NewSystemUpdates(nil, nil, nil, false)
		},
		SystemUpdatesEvery: time.Hour,
	})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx)
	}()

	// Run takes each worker's wake before starting it, so a wake from here can
	// land before the section's worker parks. Wake again until the read lands;
	// the loop is bounded, and the guard below names a poller that never ran.
	var polled bool
	for i := 0; i < 1000 && !polled; i++ {
		select {
		case <-read:
			polled = true
		default:
			h.wakeAll()
			runtime.Gosched()
		}
	}
	require.True(t, polled, "the section's poller never read the section")
	stop()
	wantSignal(t, done, "the hub did not stop")

	assert.NotNil(t, h.State().SystemUpdates, "the poller's read is not in the feed")
}
