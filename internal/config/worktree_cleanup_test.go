package config

import (
	"encoding/json"
	"testing"
	"time"
)

// decodeWorktreeCleanup decodes body into a PatrolScanConfig and returns the
// worktree_cleanup block, failing if the block is absent.
func decodeWorktreeCleanup(t *testing.T, body string) *WorktreeCleanupConfig {
	t.Helper()
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if cfg.Patrols == nil || cfg.Patrols.PatrolScan == nil {
		t.Fatalf("decode %s: patrol_scan absent", body)
	}
	return cfg.Patrols.PatrolScan.WorktreeCleanup
}

// The whole fail-safe default set in one table: an enabled block with no other
// keys is dry-run, gastown-only, grace 30m, parked grace 24h, cap 2, threshold
// 5. A nil block (patrol_scan present, no worktree_cleanup key) is off.
func TestWorktreeCleanupDefaults(t *testing.T) {
	t.Parallel()
	c := decodeWorktreeCleanup(t, `{"patrols":{"patrol_scan":{"enabled":true,"worktree_cleanup":{"enabled":true}}}}`)
	if !c.IsEnabled() {
		t.Error("IsEnabled = false, want true")
	}
	if !c.IsDryRun() {
		t.Error("IsDryRun = false, want true (dry_run unset)")
	}
	if !c.CoversRig("gastown") || c.CoversRig("beads") {
		t.Errorf("CoversRig: gastown=%v beads=%v, want true/false", c.CoversRig("gastown"), c.CoversRig("beads"))
	}
	if got := c.Grace(); got != 30*time.Minute {
		t.Errorf("Grace = %v, want 30m", got)
	}
	if got := c.ParkedGrace(); got != 24*time.Hour {
		t.Errorf("ParkedGrace = %v, want 24h", got)
	}
	if got := c.Cap(); got != 2 {
		t.Errorf("Cap = %d, want 2", got)
	}
	if got := c.AlertThreshold(); got != 5 {
		t.Errorf("AlertThreshold = %d, want 5", got)
	}

	// A patrol_scan block without worktree_cleanup leaves the field nil, and
	// every accessor stays fail-safe on a nil receiver.
	var absent DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"patrol_scan":{"enabled":true}}}`), &absent); err != nil {
		t.Fatal(err)
	}
	nilBlock := absent.Patrols.PatrolScan.WorktreeCleanup
	if nilBlock != nil {
		t.Fatalf("worktree_cleanup = %+v, want nil", nilBlock)
	}
	if nilBlock.IsEnabled() {
		t.Error("nil block IsEnabled = true, want false")
	}
	if !nilBlock.IsDryRun() {
		t.Error("nil block IsDryRun = false, want true")
	}
	if got := nilBlock.Grace(); got != 30*time.Minute {
		t.Errorf("nil block Grace = %v, want 30m", got)
	}
	if got := nilBlock.ParkedGrace(); got != 24*time.Hour {
		t.Errorf("nil block ParkedGrace = %v, want 24h", got)
	}
	if got := nilBlock.Cap(); got != 2 {
		t.Errorf("nil block Cap = %d, want 2", got)
	}
	if got := nilBlock.AlertThreshold(); got != 5 {
		t.Errorf("nil block AlertThreshold = %d, want 5", got)
	}
}

// Only an explicit dry_run:false turns the pass live; unset and true stay
// dry-run, so a typo cannot remove a worktree.
func TestWorktreeCleanupIsDryRunOnlyFalseReaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true}}}}`, true},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"dry_run":true}}}}`, true},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"dry_run":false}}}}`, false},
	} {
		c := decodeWorktreeCleanup(t, tc.body)
		if got := c.IsDryRun(); got != tc.want {
			t.Errorf("%s: IsDryRun = %v, want %v", tc.body, got, tc.want)
		}
	}
}

// A zero or negative max_per_tick means the default cap, never unlimited; a
// positive value is honored, including 1.
func TestWorktreeCleanupCapNeverUnlimited(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true}}}}`, 2},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"max_per_tick":0}}}}`, 2},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"max_per_tick":-3}}}}`, 2},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"max_per_tick":1}}}}`, 1},
		{`{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"max_per_tick":7}}}}`, 7},
	} {
		if got := decodeWorktreeCleanup(t, tc.body).Cap(); got != tc.want {
			t.Errorf("%s: Cap = %d, want %d", tc.body, got, tc.want)
		}
	}
}

// A malformed duration falls back to its default and never panics; a valid one
// is parsed.
func TestWorktreeCleanupMalformedDurationFallsBack(t *testing.T) {
	t.Parallel()
	c := decodeWorktreeCleanup(t, `{"patrols":{"patrol_scan":{"worktree_cleanup":`+
		`{"enabled":true,"grace":"banana","parked_grace":"3 parsecs"}}}}`)
	if got := c.Grace(); got != 30*time.Minute {
		t.Errorf("malformed grace = %v, want 30m", got)
	}
	if got := c.ParkedGrace(); got != 24*time.Hour {
		t.Errorf("malformed parked_grace = %v, want 24h", got)
	}

	c = decodeWorktreeCleanup(t, `{"patrols":{"patrol_scan":{"worktree_cleanup":`+
		`{"enabled":true,"grace":"45m","parked_grace":"48h"}}}}`)
	if got := c.Grace(); got != 45*time.Minute {
		t.Errorf("grace = %v, want 45m", got)
	}
	if got := c.ParkedGrace(); got != 48*time.Hour {
		t.Errorf("parked_grace = %v, want 48h", got)
	}

	// A non-positive grace would make every seat instantly eligible; it falls
	// back to the default like a malformed one.
	c = decodeWorktreeCleanup(t, `{"patrols":{"patrol_scan":{"worktree_cleanup":`+
		`{"enabled":true,"grace":"0s","parked_grace":"-1h"}}}}`)
	if got := c.Grace(); got != 30*time.Minute {
		t.Errorf("zero grace = %v, want 30m", got)
	}
	if got := c.ParkedGrace(); got != 24*time.Hour {
		t.Errorf("negative parked_grace = %v, want 24h", got)
	}
}

// An explicit rigs list replaces the gastown default; it does not extend it.
func TestWorktreeCleanupCoversRigExplicitList(t *testing.T) {
	t.Parallel()
	c := decodeWorktreeCleanup(t, `{"patrols":{"patrol_scan":{"worktree_cleanup":{"enabled":true,"rigs":["beads","hm"]}}}}`)
	if c.CoversRig("gastown") {
		t.Error("CoversRig(gastown) = true, want false for an explicit list")
	}
	if !c.CoversRig("beads") || !c.CoversRig("hm") {
		t.Errorf("CoversRig: beads=%v hm=%v, want true/true", c.CoversRig("beads"), c.CoversRig("hm"))
	}
}

// The block survives a strict decode/encode round trip, so a daemon rewrite
// does not drop it.
func TestWorktreeCleanupRoundTripsStrictly(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"patrol_scan":{"enabled":true,"worktree_cleanup":` +
		`{"enabled":true,"dry_run":false,"rigs":["gastown"],"grace":"15m",` +
		`"parked_grace":"36h","max_per_tick":3,"blocked_alert_threshold":9}}}}`
	c := decodeWorktreeCleanup(t, body)
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var back WorktreeCleanupConfig
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if back.IsDryRun() || back.Grace() != 15*time.Minute || back.ParkedGrace() != 36*time.Hour ||
		back.Cap() != 3 || back.AlertThreshold() != 9 || !back.CoversRig("gastown") {
		t.Errorf("round trip changed the block: %s", out)
	}
}
