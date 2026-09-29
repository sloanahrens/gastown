package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMaintainCommand_Registered(t *testing.T) {
	t.Parallel()
	var found bool
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "maintain" {
			found = true

			if f := cmd.Flags().Lookup("force"); f == nil {
				t.Error("expected --force flag")
			}
			if f := cmd.Flags().Lookup("dry-run"); f == nil {
				t.Error("expected --dry-run flag")
			}
			if f := cmd.Flags().Lookup("threshold"); f == nil {
				t.Error("expected --threshold flag")
			} else if f.DefValue != "100" {
				t.Errorf("expected threshold default 100, got %s", f.DefValue)
			}
			// The remote-divergence pre-flight is gone with the Dolt remotes
			// (ADR 0002). --force-diverged still parses, so old scripts keep
			// working, but it is hidden, deprecated and does nothing.
			if f := cmd.Flags().Lookup("force-diverged"); f == nil {
				t.Error("expected --force-diverged to still parse")
			} else if !f.Hidden || f.Deprecated == "" {
				t.Errorf("expected --force-diverged hidden and deprecated, got hidden=%v deprecated=%q", f.Hidden, f.Deprecated)
			}

			if cmd.GroupID != GroupServices {
				t.Errorf("expected GroupServices, got %s", cmd.GroupID)
			}
			break
		}
	}
	if !found {
		t.Fatal("maintain command not registered on rootCmd")
	}
}

func TestMaintainNeedsFlatten(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		commitCount int
		countKnown  bool
		threshold   int
		flatten     bool
	}{
		{"quiet below threshold", 0, true, 100, false},
		{"near threshold", 99, true, 100, false},
		{"at threshold", 100, true, 100, true},
		{"over threshold", 200, true, 100, true},
		{"well over threshold", 1000, true, 100, true},
		{"at custom threshold", 5, true, 5, true},
		{"below custom threshold", 4, true, 5, false},
		// gt-racu: an unknown count is not a zero. A failed measurement must
		// not become "below threshold" — that is the silent skip.
		{"unknown count flattens rather than skips", 0, false, 100, true},
		{"unknown count ignores a stale value", 7, false, 100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := maintainDBInfo{name: "om", commitCount: tt.commitCount, countKnown: tt.countKnown}
			if got := db.needsFlatten(tt.threshold); got != tt.flatten {
				t.Errorf("needsFlatten(%d) with countKnown=%v commitCount=%d: got %v, want %v",
					tt.threshold, tt.countKnown, tt.commitCount, got, tt.flatten)
			}
		})
	}
}

func TestMaintainCountRendering(t *testing.T) {
	t.Parallel()
	known := maintainDBInfo{name: "om", commitCount: 608, countKnown: true}
	zero := maintainDBInfo{name: "om", commitCount: 0, countKnown: true}
	unknown := maintainDBInfo{name: "om", countErr: errors.New("connect: connection refused")}

	if got, want := known.countText(), "608 commits"; got != want {
		t.Errorf("known countText: got %q, want %q", got, want)
	}
	if got, want := known.countLabel(), "608"; got != want {
		t.Errorf("known countLabel: got %q, want %q", got, want)
	}
	if got, want := zero.countText(), "0 commits"; got != want {
		t.Errorf("zero countText: got %q, want %q", got, want)
	}

	wantUnknown := "commits unknown (connect: connection refused)"
	if got := unknown.countText(); got != wantUnknown {
		t.Errorf("unknown countText: got %q, want %q", got, wantUnknown)
	}
	if got, want := unknown.countLabel(), "unknown"; got != want {
		t.Errorf("unknown countLabel: got %q, want %q", got, want)
	}

	// The defect: a failed count and a genuinely quiet database were
	// indistinguishable in the plan. Neither the error text nor the bare
	// label may collapse the two.
	if zero.countText() == unknown.countText() {
		t.Errorf("a failed count and a genuine zero render identically: %q", zero.countText())
	}
	if zero.countLabel() == unknown.countLabel() {
		t.Errorf("a failed count and a genuine zero label identically: %q", zero.countLabel())
	}
}

func TestMaintainDBInfo(t *testing.T) {
	t.Parallel()
	// Verify the struct can hold expected values.
	info := maintainDBInfo{
		name:        "gastown",
		commitCount: 500,
		countKnown:  true,
		hasBackup:   true,
	}
	if info.name != "gastown" {
		t.Errorf("expected name gastown, got %s", info.name)
	}
	if info.commitCount != 500 {
		t.Errorf("expected 500 commits, got %d", info.commitCount)
	}
	if !info.hasBackup {
		t.Error("expected hasBackup true")
	}
}

// fakeDoltOnPath writes a `dolt` stub into a temp dir and puts that dir first
// on PATH for the test.
func fakeDoltOnPath(t *testing.T, script string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write dolt stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestMaintainHasBackupConfigurations drives the probe's two answering cases:
// the backup is configured, and it is genuinely absent. Both leave the error
// nil — the caller is entitled to read false as "no backup configured" only
// when the command actually answered (gt-ij15).
func TestMaintainHasBackupConfigurations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a shell script")
	}

	tests := []struct {
		name string
		// listed is what the stubbed `dolt backup` prints, one name per line.
		listed []string
		want   bool
	}{
		{name: "present", listed: []string{"backup_export", "gastown-backup"}, want: true},
		{name: "absent", listed: []string{"backup_export"}, want: false},
		{name: "empty list", listed: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			script := "#!/bin/sh\n"
			for _, line := range tc.listed {
				script += "echo '" + line + "'\n"
			}
			fakeDoltOnPath(t, script)

			dataDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dataDir, "gastown"), 0o755); err != nil {
				t.Fatalf("mkdir db dir: %v", err)
			}

			got, err := maintainHasBackup(dataDir, "gastown")
			if err != nil {
				t.Fatalf("maintainHasBackup: unexpected error %v", err)
			}
			if got != tc.want {
				t.Errorf("maintainHasBackup = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMaintainHasBackupProbeFailure covers the defect: every failure mode of the
// probe used to return a bare false, which the plan read as "no backup
// configured" and the run read as success.
func TestMaintainHasBackupProbeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a shell script")
	}

	t.Run("command exits non-zero", func(t *testing.T) {
		fakeDoltOnPath(t, "#!/bin/sh\necho 'cannot read data dir' >&2\nexit 1\n")

		dataDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dataDir, "gastown"), 0o755); err != nil {
			t.Fatalf("mkdir db dir: %v", err)
		}

		hasBackup, err := maintainHasBackup(dataDir, "gastown")
		if err == nil {
			t.Fatal("a failed probe reported success — the run would skip this database's backup")
		}
		if !strings.Contains(err.Error(), "cannot read data dir") {
			t.Errorf("error drops the command's output: %v", err)
		}
		if hasBackup {
			t.Error("a failed probe must not report a backup either")
		}
	})

	t.Run("dolt not on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())

		hasBackup, err := maintainHasBackup(t.TempDir(), "gastown")
		if err == nil {
			t.Fatal("a missing dolt binary reported success — the run would skip this database's backup")
		}
		if hasBackup {
			t.Error("a failed probe must not report a backup either")
		}
	})

	t.Run("data dir unreadable", func(t *testing.T) {
		fakeDoltOnPath(t, "#!/bin/sh\nexit 0\n")

		hasBackup, err := maintainHasBackup(t.TempDir(), "gastown")
		if err == nil {
			t.Fatal("an unreadable data dir reported success — the run would skip this database's backup")
		}
		if hasBackup {
			t.Error("a failed probe must not report a backup either")
		}
	})
}

// TestMaintainBackupRendering pins the plan line: a failed probe and a genuinely
// unbacked database must not render identically, because the operator's next
// action differs — fix the probe versus nothing to fix.
func TestMaintainBackupRendering(t *testing.T) {
	t.Parallel()

	backedUp := maintainDBInfo{name: "gastown", hasBackup: true, backupKnown: true}
	unbacked := maintainDBInfo{name: "gastown", backupKnown: true}
	unknown := maintainDBInfo{name: "gastown", backupErr: errors.New("dolt backup: executable file not found in $PATH")}

	if got := backedUp.backupText(); got != "" {
		t.Errorf("backed-up backupText: got %q, want empty", got)
	}
	if got := unbacked.backupText(); got != "" {
		t.Errorf("unbacked backupText: got %q, want empty", got)
	}

	want := "backup unknown (dolt backup: executable file not found in $PATH)"
	if got := unknown.backupText(); got != want {
		t.Errorf("unknown backupText: got %q, want %q", got, want)
	}
	if unknown.backupText() == unbacked.backupText() {
		t.Errorf("a failed probe and a genuine absence render identically: %q", unknown.backupText())
	}
}

// TestMaintainBackupRefusal pins the fail-closed gate: one unanswered probe
// stops the run, and the error names the database and carries the probe's own
// error. Every answering plan proceeds untouched.
func TestMaintainBackupRefusal(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("dolt backup: signal: killed")

	if err := maintainBackupRefusal([]maintainDBInfo{
		{name: "be", hasBackup: true, backupKnown: true},
		{name: "gastown", backupKnown: true},
	}); err != nil {
		t.Fatalf("an answered plan was refused: %v", err)
	}

	err := maintainBackupRefusal([]maintainDBInfo{
		{name: "be", hasBackup: true, backupKnown: true},
		{name: "gastown", backupErr: probeErr},
	})
	if err == nil {
		t.Fatal("an unanswered backup probe did not refuse the run")
	}
	if !strings.Contains(err.Error(), "gastown") {
		t.Errorf("refusal does not name the database whose probe failed: %v", err)
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("refusal drops the probe's error: %v", err)
	}
}

func TestMaintainConstants(t *testing.T) {
	t.Parallel()
	if defaultMaintainThreshold != 100 {
		t.Errorf("expected default threshold 100, got %d", defaultMaintainThreshold)
	}
}

// TestMaintainClassifyForPlan drives the plan's counting rules without a live
// Dolt server: a refused database must never be counted as flattening (the
// count that feeds "Will flatten: N" and the exit-code decision), and an
// unknown commit count must be counted at most once, only for a database that
// will actually flatten (gt-aku6).
func TestMaintainClassifyForPlan(t *testing.T) {
	t.Parallel()

	const threshold = 100

	t.Run("below threshold does not flatten", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 5, countKnown: true}
		row := maintainClassifyForPlan(db, threshold)
		if row.willFlatten {
			t.Errorf("row = %+v, want no flatten for a database under threshold", row)
		}
	})

	t.Run("over threshold flattens", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 500, countKnown: true}
		row := maintainClassifyForPlan(db, threshold)
		if !row.willFlatten {
			t.Error("willFlatten = false, want true for an over-threshold database")
		}
		if row.countUnknown {
			t.Error("countUnknown = true for a database with a known count")
		}
	})

	t.Run("unknown count flattens and is counted exactly once", func(t *testing.T) {
		db := maintainDBInfo{name: "om", countErr: errors.New("connect: connection refused")}
		row := maintainClassifyForPlan(db, threshold)
		if !row.willFlatten {
			t.Fatal("willFlatten = false for an unknown-count database, want it flattened rather than silently skipped (gt-racu)")
		}
		if !row.countUnknown {
			t.Error("countUnknown = false for a database whose count measurement failed")
		}
	})

	t.Run("backup state is classified independently of flatten", func(t *testing.T) {
		backedUp := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, hasBackup: true, backupKnown: true},
			threshold)
		if !backedUp.hasBackup || backedUp.backupUnknown {
			t.Errorf("backedUp row = %+v, want hasBackup only", backedUp)
		}

		unbacked := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, backupKnown: true},
			threshold)
		if unbacked.hasBackup || unbacked.backupUnknown {
			t.Errorf("unbacked row = %+v, want neither hasBackup nor backupUnknown", unbacked)
		}

		unknownBackup := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, backupErr: errors.New("boom")},
			threshold)
		if !unknownBackup.backupUnknown || unknownBackup.hasBackup {
			t.Errorf("unknownBackup row = %+v, want backupUnknown only", unknownBackup)
		}
	})
}

// TestMaintainPlanTally exercises the aggregate counts the same way
// runMaintain's plan-display loop derives them from maintainClassifyForPlan:
// an unknown count flattens (gt-racu) and is tallied once as unknown.
func TestMaintainPlanTally(t *testing.T) {
	t.Parallel()

	const threshold = 100
	dbInfos := []maintainDBInfo{
		{name: "busy", commitCount: 500, countKnown: true},
		{name: "unknown-count", countErr: errors.New("connect: connection refused")},
		{name: "quiet", commitCount: 1, countKnown: true},
	}

	var flattenCount, unknownCount int
	for _, db := range dbInfos {
		row := maintainClassifyForPlan(db, threshold)
		if row.willFlatten {
			flattenCount++
			if row.countUnknown {
				unknownCount++
			}
		}
	}

	if flattenCount != 2 {
		t.Errorf("flattenCount = %d, want 2 (busy, unknown-count)", flattenCount)
	}
	if unknownCount != 1 {
		t.Errorf("unknownCount = %d, want 1 (unknown-count)", unknownCount)
	}
}
