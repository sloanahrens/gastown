package daemon

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"

	"github.com/steveyegge/gastown/internal/deacon"
)

// writeDeaconHeartbeatWithCycle writes a heartbeat with a controlled age and
// cycle, so a test can present a fresh timestamp alongside a stale cycle — the
// shape the background poller produces when the Deacon makes no progress.
func writeDeaconHeartbeatWithCycle(t *testing.T, townRoot string, age time.Duration, cycle int64) {
	t.Helper()
	hb := &deacon.Heartbeat{
		Timestamp: time.Now().Add(-age),
		Cycle:     cycle,
	}
	if err := deacon.WriteHeartbeat(townRoot, hb); err != nil {
		t.Fatalf("writeDeaconHeartbeatWithCycle: %v", err)
	}
}

// seedDeaconCycle primes the daemon's in-memory cycle baseline, as if earlier
// heartbeat ticks had already observed the cycle.
func seedDeaconCycle(d *Daemon, cycle int64, changedAgo time.Duration, stalledTicks int) {
	d.deaconCycle = deaconCycleTracker{
		cycle:        cycle,
		changedAt:    time.Now().Add(-changedAgo),
		known:        true,
		stalledTicks: stalledTicks,
	}
}

// TestCheckDeaconHeartbeat_CycleStall covers the effective-age rule: a
// heartbeat whose timestamp is fresh but whose cycle has stopped advancing
// counts as stale, at the same nudge (5m) and restart (20m) tiers the
// timestamp path uses (gt-t3cw). The stall log line is deliberately distinct
// from "heartbeat is stale" so the two conditions stay tellable apart in
// daemon.log.
func TestCheckDeaconHeartbeat_CycleStall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	activeWork := map[string]beadsdk.Storage{
		"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
			"in_progress": {{ID: "sc-abc"}},
		}},
	}

	tests := []struct {
		name             string
		heartbeatAge     time.Duration
		cycle            int64
		seedCycle        int64
		seedChangedAgo   time.Duration
		seedStalledTicks int
		stores           map[string]beadsdk.Storage
		wantLogs         []string
		wantNoLogs       []string
	}{
		{
			name:             "fresh timestamp, cycle static 6m — nudge path",
			heartbeatAge:     30 * time.Second,
			cycle:            42,
			seedCycle:        42,
			seedChangedAgo:   6 * time.Minute,
			seedStalledTicks: 1,
			stores:           activeWork,
			wantLogs: []string{
				"Deacon cycle stalled (6m0s at cycle 42)",
				"cycle 42 stalled",
				"nudging session",
			},
			wantNoLogs: []string{"heartbeat is stale"},
		},
		{
			name:             "fresh timestamp, cycle static 21m — restart path",
			heartbeatAge:     30 * time.Second,
			cycle:            42,
			seedCycle:        42,
			seedChangedAgo:   21 * time.Minute,
			seedStalledTicks: 1,
			stores:           activeWork,
			wantLogs: []string{
				"Deacon cycle stalled (21m0s at cycle 42)",
				"STUCK DEACON: cycle stalled for 21m0s at cycle 42",
			},
			wantNoLogs: []string{"nudging session", "heartbeat is stale"},
		},
		{
			name:           "fresh timestamp, advancing cycle — healthy",
			heartbeatAge:   30 * time.Second,
			cycle:          43,
			seedCycle:      42,
			seedChangedAgo: 6 * time.Minute,
			stores:         activeWork,
			wantNoLogs: []string{
				"Deacon cycle stalled",
				"heartbeat is stale",
				"nudging session",
				"STUCK DEACON",
			},
		},
		{
			name:             "fresh timestamp, stalled cycle on its first sample — waits",
			heartbeatAge:     30 * time.Second,
			cycle:            42,
			seedCycle:        42,
			seedChangedAgo:   6 * time.Minute,
			seedStalledTicks: 0,
			stores:           activeWork,
			wantLogs: []string{
				"Deacon cycle stalled (6m0s at cycle 42) on its first sample, waiting for confirmation",
			},
			wantNoLogs: []string{"nudging session", "STUCK DEACON"},
		},
		{
			name:           "stale timestamp, advancing cycle — timestamp path unchanged",
			heartbeatAge:   10 * time.Minute,
			cycle:          43,
			seedCycle:      42,
			seedChangedAgo: 6 * time.Minute,
			stores:         activeWork,
			wantLogs: []string{
				"Deacon heartbeat is stale (10m0s old)",
				"nudging session",
			},
			wantNoLogs: []string{"Deacon cycle stalled"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			fakeBinDir := t.TempDir()
			tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
			if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
				t.Fatalf("create tmux log: %v", err)
			}

			writeFakeTmuxWithSession(t, fakeBinDir)
			t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMUX_LOG", tmuxLog)

			writeDeaconHeartbeatWithCycle(t, townRoot, tc.heartbeatAge, tc.cycle)

			d := newTestDaemonWithStores(t, townRoot, tc.stores)
			seedDeaconCycle(d, tc.seedCycle, tc.seedChangedAgo, tc.seedStalledTicks)

			logBuf := &strings.Builder{}
			d.logger = log.New(logBuf, "", 0)

			d.checkDeaconHeartbeat()

			logOutput := logBuf.String()
			for _, want := range tc.wantLogs {
				if !strings.Contains(logOutput, want) {
					t.Errorf("log missing %q\nlog:\n%s", want, logOutput)
				}
			}
			for _, unwanted := range tc.wantNoLogs {
				if strings.Contains(logOutput, unwanted) {
					t.Errorf("log contains %q, want it absent\nlog:\n%s", unwanted, logOutput)
				}
			}
		})
	}
}

// TestCheckDeaconHeartbeat_CycleStallNeedsTwoTicks verifies the debounce: the
// first tick that sees a stalled cycle only records it. Acting on one sample
// would restart a Deacon over a single missed heartbeat tick.
func TestCheckDeaconHeartbeat_CycleStallNeedsTwoTicks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	writeFakeTmuxWithSession(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)

	// Timestamp keeps being refreshed; the cycle does not move.
	writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, 42)

	d := newTestDaemonWithStores(t, townRoot, map[string]beadsdk.Storage{
		"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
			"in_progress": {{ID: "sc-abc"}},
		}},
	})
	// Baseline the cycle seven minutes ago, as an earlier daemon tick would
	// have, so this tick already sees a stall past the 5m threshold.
	seedDeaconCycle(d, 42, 7*time.Minute, 0)

	logBuf := &strings.Builder{}
	d.logger = log.New(logBuf, "", 0)

	d.checkDeaconHeartbeat()
	first := logBuf.String()
	if !strings.Contains(first, "on its first sample, waiting for confirmation") {
		t.Errorf("first tick should only record the stall\nlog:\n%s", first)
	}
	if strings.Contains(first, "nudging session") {
		t.Errorf("first tick must not nudge\nlog:\n%s", first)
	}

	logBuf.Reset()
	d.checkDeaconHeartbeat()
	second := logBuf.String()
	if !strings.Contains(second, "nudging session") {
		t.Errorf("second consecutive tick should nudge\nlog:\n%s", second)
	}
	if strings.Contains(second, "waiting for confirmation") {
		t.Errorf("second tick should not still be waiting\nlog:\n%s", second)
	}
}

// TestCheckDeaconHeartbeat_CycleBaselineReset verifies that a gap in
// observations does not read as a stall. With no heartbeat file there is
// nothing to have a baseline against, so the next heartbeat seen must start
// the clock over rather than inherit the pre-gap timestamp.
func TestCheckDeaconHeartbeat_CycleBaselineReset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	writeFakeTmuxWithSession(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)

	d := newTestDaemonWithStores(t, townRoot, nil)
	logBuf := &strings.Builder{}
	d.logger = log.New(logBuf, "", 0)

	// No heartbeat file at all: the baseline from before the gap must go.
	seedDeaconCycle(d, 42, 30*time.Minute, 3)
	d.checkDeaconHeartbeat()
	if d.deaconCycle.known {
		t.Errorf("cycle baseline survived a tick with no heartbeat file")
	}

	// The next heartbeat is fresh and its cycle is new; nothing to act on.
	writeDeaconHeartbeatWithCycle(t, townRoot, 30*time.Second, 43)
	logBuf.Reset()
	seedDeaconCycle(d, 42, 30*time.Minute, 3)
	d.checkDeaconHeartbeat()
	if logOutput := logBuf.String(); logOutput != "" {
		t.Errorf("expected no action for a fresh, advancing heartbeat\nlog:\n%s", logOutput)
	}
}
