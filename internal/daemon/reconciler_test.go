package daemon

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// TestReconcilerPatrolInterval covers the interval the reconciler patrol runs
// at: the parsed Reconciler interval when the block sets a good one, and the
// five-minute default when there is no config, no block, or an interval that
// ParseInterval refuses.
func TestReconcilerPatrolInterval(t *testing.T) {
	t.Parallel()

	withBlock := func(r *config.ReconcilerConfig) *config.DaemonPatrolConfig {
		return &config.DaemonPatrolConfig{Patrols: &config.PatrolsConfig{Reconciler: r}}
	}

	tests := []struct {
		name string
		pc   *config.DaemonPatrolConfig
		want time.Duration
	}{
		{"a nil config reads the default", nil, 5 * time.Minute},
		{"a config with no patrols reads the default", &config.DaemonPatrolConfig{}, 5 * time.Minute},
		{"an absent reconciler block reads the default", withBlock(nil), 5 * time.Minute},
		{"a block with an unset interval reads the default", withBlock(&config.ReconcilerConfig{Enabled: true}), 5 * time.Minute},
		{"an interval that does not parse reads the default", withBlock(&config.ReconcilerConfig{Interval: "soon"}), 5 * time.Minute},
		{"an interval under the minimum reads the default", withBlock(&config.ReconcilerConfig{Interval: "30s"}), 5 * time.Minute},
		{"a good interval is returned", withBlock(&config.ReconcilerConfig{Interval: "10m"}), 10 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reconcilerInterval(tt.pc); got != tt.want {
				t.Fatalf("reconcilerInterval() = %v, want %v", got, tt.want)
			}
		})
	}
}
