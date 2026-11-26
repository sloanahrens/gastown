package daemon

import (
	"errors"
	"io"
	"log"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// TestReconcilerCycleWanted covers which due duties a maintenance cycle may
// start for: a live patrol names them, while a shadow patrol, a standby reason
// and a set running flag each answer nothing, and the town_state duty is left
// out when its root cannot be read.
func TestReconcilerCycleWanted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	live := false
	overdueBackup := []reconcile.Observation{
		{Duty: config.ReconcilerDutyBackup, Verdict: reconcile.VerdictOverdue},
	}
	overdueTownState := []reconcile.Observation{
		{Duty: config.ReconcilerDutyTownState, Verdict: reconcile.VerdictOverdue},
	}

	// liveDaemon builds a daemon on a fresh town whose reconciler block runs
	// live (shadow false), which a row changes when it needs another mode.
	liveDaemon := func(t *testing.T) *Daemon {
		t.Helper()
		return &Daemon{
			config: &Config{TownRoot: t.TempDir()},
			logger: log.New(io.Discard, "", 0),
			patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
				Reconciler: &config.ReconcilerConfig{Enabled: true, Shadow: &live},
			}},
		}
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, d *Daemon)
		obs   []reconcile.Observation
		want  []string
	}{
		{"a live patrol with an overdue backup names it", nil, overdueBackup, []string{config.ReconcilerDutyBackup}},
		{"a shadow patrol names nothing", func(t *testing.T, d *Daemon) {
			// An unset shadow key is the shipped default, and reads shadow.
			d.patrolConfig.Patrols.Reconciler = &config.ReconcilerConfig{Enabled: true}
		}, overdueBackup, nil},
		{"a town ESTOP names nothing", func(t *testing.T, d *Daemon) {
			if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
				t.Fatal(err)
			}
		}, overdueBackup, nil},
		{"a set running flag names nothing", func(t *testing.T, d *Daemon) {
			d.maintenanceGCRunning.Store(true)
		}, overdueBackup, nil},
		{"a live town_state with a readable root is named", func(t *testing.T, d *Daemon) {
			root := filepath.Join(t.TempDir(), "town-state")
			d.patrolConfig.Patrols.ScheduledMaintenance = &ScheduledMaintenanceConfig{
				TownStateBackupRoot: &root,
			}
		}, overdueTownState, []string{config.ReconcilerDutyTownState}},
		{"a town_state with no root is left out", nil, overdueTownState, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := liveDaemon(t)
			if tt.setup != nil {
				tt.setup(t, d)
			}
			if got := d.reconcilerCycleWanted(now, tt.obs); !slices.Equal(got, tt.want) {
				t.Errorf("reconcilerCycleWanted() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReconcilerDueDuties(t *testing.T) {
	t.Parallel()
	obs := func(duty string, verdict reconcile.Verdict) reconcile.Observation {
		return reconcile.Observation{Duty: duty, Verdict: verdict}
	}
	rootErr := errors.New("the town state root is unset")
	tests := []struct {
		name             string
		obs              []reconcile.Observation
		townStateRootErr error
		want             []string
	}{
		{name: "a due backup is due", obs: []reconcile.Observation{obs("backup", reconcile.VerdictDue)}, want: []string{"backup"}},
		{name: "an overdue backup is due", obs: []reconcile.Observation{obs("backup", reconcile.VerdictOverdue)}, want: []string{"backup"}},
		{name: "a due archive is due", obs: []reconcile.Observation{obs("archive", reconcile.VerdictDue)}, want: []string{"archive"}},
		{name: "an overdue archive is due", obs: []reconcile.Observation{obs("archive", reconcile.VerdictOverdue)}, want: []string{"archive"}},
		{name: "a due gc names its database", obs: []reconcile.Observation{obs("gc:hq", reconcile.VerdictDue)}, want: []string{"gc:hq"}},
		{name: "an overdue gc names its database", obs: []reconcile.Observation{obs("gc:gastown", reconcile.VerdictOverdue)}, want: []string{"gc:gastown"}},
		{name: "a due town_state with a root is due", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictDue)}, want: []string{"town_state"}},
		{name: "an overdue town_state with a root is due", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictOverdue)}, want: []string{"town_state"}},
		{name: "an ok backup is left out", obs: []reconcile.Observation{obs("backup", reconcile.VerdictOK)}},
		{name: "an unknown archive is left out", obs: []reconcile.Observation{obs("archive", reconcile.VerdictUnknown)}},
		{name: "an unrelated duty is left out", obs: []reconcile.Observation{obs("dolt_backup", reconcile.VerdictOverdue)}},
		{name: "a bare gc is left out", obs: []reconcile.Observation{obs("gc", reconcile.VerdictDue)}},
		{name: "a gc with no database is left out", obs: []reconcile.Observation{obs("gc:", reconcile.VerdictOverdue)}},
		{name: "a town_state with no root is left out", obs: []reconcile.Observation{obs("town_state", reconcile.VerdictOverdue)}, townStateRootErr: rootErr},
		{name: "input order is kept", obs: []reconcile.Observation{
			obs("gc:zeta", reconcile.VerdictOverdue),
			obs("backup", reconcile.VerdictOK),
			obs("backup", reconcile.VerdictDue),
			obs("town_state", reconcile.VerdictDue),
		}, want: []string{"gc:zeta", "backup", "town_state"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reconcilerDueDuties(tt.obs, tt.townStateRootErr); !slices.Equal(got, tt.want) {
				t.Errorf("reconcilerDueDuties() = %q, want %q", got, tt.want)
			}
		})
	}
}
