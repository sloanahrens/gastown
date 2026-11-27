package daemon

import (
	"bytes"
	"log"
	"strings"
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

// TestReconcilerPatrolPass covers what one pass does: a disabled patrol logs
// nothing and records no run, and an active one logs its mode (shadow unless
// the block sets shadow false) and records the run it completed.
func TestReconcilerPatrolPass(t *testing.T) {
	t.Parallel()

	live := false

	tests := []struct {
		name     string
		disabled bool
		shadow   *bool
		wantLog  string
		wantRun  bool
	}{
		{"a disabled patrol logs nothing and records no run", true, nil, "", false},
		{"an active shadow pass logs shadow and records its run", false, nil, "reconciler: pass (shadow)", true},
		{"an active live pass logs live and records its run", false, &live, "reconciler: pass (live)", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logs := &bytes.Buffer{}
			d := &Daemon{
				config: &Config{TownRoot: t.TempDir()},
				logger: log.New(logs, "", 0),
				patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
					Reconciler: &config.ReconcilerConfig{Enabled: true, Shadow: tt.shadow},
				}},
				// The active rows observe, so the live listing and backup root
				// must not reach a real Dolt server or the host's home dir.
				maint: maintenanceSeams{
					serverDatabases: func(*Daemon) ([]string, error) { return nil, nil },
					backupRoot:      func() (string, error) { return t.TempDir(), nil },
				},
			}
			if tt.disabled {
				d.disabledPatrols = map[string]bool{"reconciler": true}
			}

			before := time.Now()
			d.runReconciler()

			if got := strings.TrimSpace(logs.String()); got != tt.wantLog {
				t.Errorf("log = %q, want %q", got, tt.wantLog)
			}
			last := d.reconcilerRun.last()
			if tt.wantRun && last.Before(before) {
				t.Errorf("last run = %v, want a run recorded at or after %v", last, before)
			}
			if !tt.wantRun && !last.IsZero() {
				t.Errorf("last run = %v, want none recorded", last)
			}
		})
	}
}
