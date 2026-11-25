package daemon

import (
	"testing"
	"time"
)

// TestPatrolScanCheckTick pins the patrol_scan ticker's check cadence
// (gt-21gba2): a quarter of the configured interval, floored at a minute and
// capped at five.
func TestPatrolScanCheckTick(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		interval string
		want     time.Duration
	}{
		{
			name:     "10m interval checks every 2m30s",
			interval: "10m",
			want:     2*time.Minute + 30*time.Second,
		},
		{
			name:     "2m interval is floored at 1m",
			interval: "2m",
			want:     time.Minute,
		},
		{
			name:     "40m interval is capped at 5m",
			interval: "40m",
			want:     5 * time.Minute,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			pc := &DaemonPatrolConfig{Patrols: &PatrolsConfig{
				PatrolScan: &PatrolScanConfig{Enabled: true, IntervalStr: tc.interval},
			}}

			if got := patrolScanCheckTick(pc); got != tc.want {
				t.Errorf("patrolScanCheckTick(interval %s) = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}
