package daemon

import (
	"io"
	"log"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// TestHealthTicksReconcilerEnabled: with the reconciler patrol enabled, Ticks
// lists a reconciler tick carrying its configured interval and the last run the
// record holds, so the town health judges a stalled reconciler (gt-v71maj).
func TestHealthTicksReconcilerEnabled(t *testing.T) {
	t.Parallel()

	lastRun := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		interval     string
		recordedAt   time.Time
		wantInterval time.Duration
		wantLastRun  time.Time
	}{
		{"a configured interval is the tick's interval", "10m", lastRun, 10 * time.Minute, lastRun},
		{"an unset interval reads the default", "", lastRun, config.DefaultReconcilerInterval, lastRun},
		{"no run recorded reads a zero last run", "10m", time.Time{}, 10 * time.Minute, time.Time{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			if !tt.recordedAt.IsZero() {
				if err := savePatrolLastRun(root, "reconciler", tt.recordedAt); err != nil {
					t.Fatalf("savePatrolLastRun: %v", err)
				}
			}
			d := &Daemon{
				config: &Config{TownRoot: root},
				logger: log.New(io.Discard, "", 0),
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					Reconciler: &config.ReconcilerConfig{Enabled: true, Interval: tt.interval},
				}},
			}

			ticks, err := (&healthSources{d: d}).Ticks()
			if err != nil {
				t.Fatalf("Ticks: %v", err)
			}
			var got *townhealth.Tick
			for i := range ticks {
				if ticks[i].Name == "reconciler" {
					got = &ticks[i]
				}
			}
			if got == nil {
				t.Fatalf("Ticks = %+v, want a reconciler tick listed while the patrol is enabled", ticks)
			}
			if got.Err != nil {
				t.Errorf("reconciler tick Err = %v, want none", got.Err)
			}
			if got.Interval != tt.wantInterval {
				t.Errorf("reconciler tick Interval = %v, want %v", got.Interval, tt.wantInterval)
			}
			if !got.LastFired.Equal(tt.wantLastRun) {
				t.Errorf("reconciler tick LastFired = %v, want %v", got.LastFired, tt.wantLastRun)
			}
		})
	}
}
