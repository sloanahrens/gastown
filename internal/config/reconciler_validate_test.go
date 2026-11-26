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
