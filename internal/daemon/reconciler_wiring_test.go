package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// TestReconcilerWiringTickerUsesTheInterval: newReconcilerTicker asks its seam
// for one ticker at the interval the patrol config names, and returns the
// seam's channel and stop func (gt-n9gpxa).
func TestReconcilerWiringTickerUsesTheInterval(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		patrol *DaemonPatrolConfig
		want   time.Duration
	}{
		{"no config reads the default interval", nil, config.DefaultReconcilerInterval},
		{
			"the configured interval is used",
			&DaemonPatrolConfig{Patrols: &PatrolsConfig{
				Reconciler: &config.ReconcilerConfig{Interval: "10m"},
			}},
			10 * time.Minute,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ticks := make(chan time.Time)
			stops := 0
			var asked []time.Duration
			d := &Daemon{patrolConfig: tt.patrol}
			d.reconcilerTickerFn = func(interval time.Duration) (<-chan time.Time, func()) {
				asked = append(asked, interval)
				return ticks, func() { stops++ }
			}

			channel, stop := d.newReconcilerTicker()

			if channel != ticks {
				t.Errorf("newReconcilerTicker returned %v, want the seam's channel", channel)
			}
			if len(asked) != 1 || asked[0] != tt.want {
				t.Errorf("the seam was asked for %v, want one ticker at %v", asked, tt.want)
			}
			if stop == nil {
				t.Fatal("newReconcilerTicker returned no stop func")
			}
			stop()
			if stops != 1 {
				t.Errorf("the stop func ran %d times, want the ticker stopped once", stops)
			}
		})
	}
}

// TestReconcilerWiringTickerWithoutASeam: with no seam the ticker is a real one,
// so the caller always gets a channel and a stop func to release it (gt-n9gpxa).
func TestReconcilerWiringTickerWithoutASeam(t *testing.T) {
	t.Parallel()

	d := &Daemon{patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		Reconciler: &config.ReconcilerConfig{Enabled: true},
	}}}
	channel, stop := d.newReconcilerTicker()
	if channel == nil || stop == nil {
		t.Fatal("newReconcilerTicker with no seam returned no ticker")
	}
	stop()
}
