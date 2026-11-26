package config

import (
	"strings"
	"testing"
)

// TestReconcilerValidate: ValidateDaemonPatrolConfig refuses a present
// reconciler block whose interval or grace the patrol's parsers refuse, naming
// the settings key the operator edits, and accepts an absent block, unset keys
// and the bounds (gt-tf650y). The refusal rows leave Enabled unset: a present
// block is refused whether or not the patrol is on.
func TestReconcilerValidate(t *testing.T) {
	t.Parallel()

	const intervalKey = "daemon.patrols.reconciler.interval"
	const graceKey = "daemon.patrols.reconciler.grace"

	intervalCases := []struct{ what, value, want string }{
		{"refuses 59s, below the 1m floor", "59s", intervalKey},
		{"refuses 61m, above the 1h ceiling", "61m", intervalKey},
		{"refuses soon, not a duration", "soon", intervalKey},
		{"accepts 1m, the floor", "1m", ""},
		{"accepts 60m, the ceiling", "60m", ""},
	}
	for _, tc := range intervalCases {
		t.Run("interval "+tc.what, func(t *testing.T) {
			t.Parallel()
			c := patrolDoc(func(p *PatrolsConfig) {
				p.Reconciler = &ReconcilerConfig{Interval: tc.value}
			})
			checkPatrolConfig(t, c, "interval "+tc.value, tc.want)
		})
	}

	graceCases := []struct{ what, value, want string }{
		{"refuses 59s, below the 1m floor", "59s", graceKey},
		{"refuses 169h, above the 7d ceiling", "169h", graceKey},
		{"refuses soon, not a duration", "soon", graceKey},
		{"accepts 1m, the floor", "1m", ""},
		{"accepts 168h, the ceiling", "168h", ""},
	}
	for _, tc := range graceCases {
		t.Run("grace "+tc.what, func(t *testing.T) {
			t.Parallel()
			c := patrolDoc(func(p *PatrolsConfig) {
				p.Reconciler = &ReconcilerConfig{Grace: tc.value}
			})
			checkPatrolConfig(t, c, "grace "+tc.value, tc.want)
		})
	}

	t.Run("accepts an absent block", func(t *testing.T) {
		t.Parallel()
		checkPatrolConfig(t, patrolDoc(func(*PatrolsConfig) {}), "no reconciler block", "")
	})

	t.Run("accepts a block with its keys unset", func(t *testing.T) {
		t.Parallel()
		c := patrolDoc(func(p *PatrolsConfig) {
			p.Reconciler = &ReconcilerConfig{Enabled: true}
		})
		checkPatrolConfig(t, c, "reconciler with unset keys", "")
	})

	t.Run("names the interval when both are out of range", func(t *testing.T) {
		t.Parallel()
		c := patrolDoc(func(p *PatrolsConfig) {
			p.Reconciler = &ReconcilerConfig{Interval: "soon", Grace: "soon"}
		})
		err := ValidateDaemonPatrolConfig(c)
		if err == nil || !strings.Contains(err.Error(), intervalKey) || strings.Contains(err.Error(), graceKey) {
			t.Fatalf("ValidateDaemonPatrolConfig(interval and grace both soon) = %v, want it refused naming %s only", err, intervalKey)
		}
	})
}

// TestReconcilerValidateLimits: ValidateDaemonPatrolConfig refuses a present
// reconciler block whose limit for any of the four duties the patrol grades is
// out of range, naming that duty's settings key, and accepts the defaults
// (gt-vh4noj). The refusal rows leave Enabled unset: a present block is refused
// whether or not the patrol is on.
func TestReconcilerValidateLimits(t *testing.T) {
	t.Parallel()

	const (
		backupKey    = "daemon.patrols.reconciler.backup_limit"
		archiveKey   = "daemon.patrols.reconciler.archive_limit"
		gcKey        = "daemon.patrols.reconciler.gc_limit"
		townStateKey = "daemon.patrols.reconciler.town_state_limit"
	)

	cases := []struct {
		what string
		fill func(*ReconcilerConfig)
		want string
	}{
		{"refuses the backup limit below the 1h floor", func(r *ReconcilerConfig) { r.BackupLimit = "59m" }, backupKey},
		{"refuses the backup limit above the 30d ceiling", func(r *ReconcilerConfig) { r.BackupLimit = "721h" }, backupKey},
		{"refuses the archive limit below the 1h floor", func(r *ReconcilerConfig) { r.ArchiveLimit = "59m" }, archiveKey},
		{"refuses the archive limit above the 30d ceiling", func(r *ReconcilerConfig) { r.ArchiveLimit = "721h" }, archiveKey},
		{"refuses the gc limit below the 1h floor", func(r *ReconcilerConfig) { r.GCLimit = "59m" }, gcKey},
		{"refuses the gc limit above the 30d ceiling", func(r *ReconcilerConfig) { r.GCLimit = "721h" }, gcKey},
		{"refuses the town_state limit below the 1h floor", func(r *ReconcilerConfig) { r.TownStateLimit = "59m" }, townStateKey},
		{"refuses the town_state limit above the 30d ceiling", func(r *ReconcilerConfig) { r.TownStateLimit = "721h" }, townStateKey},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()
			c := patrolDoc(func(p *PatrolsConfig) {
				r := &ReconcilerConfig{}
				tc.fill(r)
				p.Reconciler = r
			})
			checkPatrolConfig(t, c, tc.what, tc.want)
		})
	}

	t.Run("accepts the limits unset", func(t *testing.T) {
		t.Parallel()
		c := patrolDoc(func(p *PatrolsConfig) {
			p.Reconciler = &ReconcilerConfig{Enabled: true}
		})
		checkPatrolConfig(t, c, "reconciler with unset limits", "")
	})

	t.Run("accepts each limit at its bounds", func(t *testing.T) {
		t.Parallel()
		c := patrolDoc(func(p *PatrolsConfig) {
			p.Reconciler = &ReconcilerConfig{
				BackupLimit:    "1h",
				ArchiveLimit:   "720h",
				GCLimit:        "1h",
				TownStateLimit: "720h",
			}
		})
		checkPatrolConfig(t, c, "reconciler limits at their bounds", "")
	})
}
