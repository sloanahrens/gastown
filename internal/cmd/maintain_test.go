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
			// The divergence pre-flight is opt-OUT: flatten refuses by default
			// and --force-diverged turns the guard off.
			if f := cmd.Flags().Lookup("force-diverged"); f == nil {
				t.Error("expected --force-diverged flag")
			} else if f.DefValue != "false" {
				t.Errorf("expected --force-diverged default false, got %s", f.DefValue)
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

// TestMaintainPreflightRefusal drives every branch of the flatten guard. The
// guard exists because flatten rewrites the commit graph: squashing a database
// whose remote has moved on makes the two histories disagree, and the
// force-push that follows a flatten then deletes the remote-only commits.
func TestMaintainPreflightRefusal(t *testing.T) {
	t.Parallel()
	fetchErr := errors.New("DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused")

	tests := []struct {
		name          string
		preflight     maintainPreflight
		forceDiverged bool
		wantRefusal   string
	}{
		{
			name:      "no remote clears the guard",
			preflight: maintainPreflight{},
		},
		{
			name:      "remote verified as not diverged clears the guard",
			preflight: maintainPreflight{Remote: "origin"},
		},
		{
			name:        "diverged refuses with the required message",
			preflight:   maintainPreflight{Remote: "origin", Diverged: true},
			wantRefusal: "diverged from origin; pass --force-diverged",
		},
		{
			// The load-bearing case: a pre-flight that could not run is not a
			// pass. Only a completed check licenses the destructive write.
			name:        "failed check refuses instead of passing",
			preflight:   maintainPreflight{Remote: "origin", Err: fetchErr},
			wantRefusal: "cannot verify remote (DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			name:        "unreachable remote with no name known still refuses",
			preflight:   maintainPreflight{Err: errors.New("list remotes: connection refused")},
			wantRefusal: "cannot verify remote (list remotes: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			// A failed check on a database that also looked diverged reports
			// the uncertainty, not the divergence: the divergence was read from
			// a check that did not finish.
			name:        "failure outranks a partial divergence reading",
			preflight:   maintainPreflight{Remote: "origin", Diverged: true, Err: fetchErr},
			wantRefusal: "cannot verify remote (DOLT_FETCH origin: dial tcp 127.0.0.1:443: connect: connection refused) — pass --force-diverged to flatten anyway",
		},
		{
			name:          "--force-diverged overrides a detected divergence",
			preflight:     maintainPreflight{Remote: "origin", Diverged: true},
			forceDiverged: true,
		},
		{
			name:          "--force-diverged overrides a failed check",
			preflight:     maintainPreflight{Remote: "origin", Err: fetchErr},
			forceDiverged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.preflight.refusal(tt.forceDiverged); got != tt.wantRefusal {
				t.Errorf("refusal(forceDiverged=%v) = %q, want %q", tt.forceDiverged, got, tt.wantRefusal)
			}
		})
	}
}

// TestMaintainPreflightRefusalIsNotSilent pins the property the guard would
// lose if someone "simplified" the failed-check branch to a pass: a pre-flight
// error must never be reported as cleared.
func TestMaintainPreflightRefusalIsNotSilent(t *testing.T) {
	t.Parallel()
	failed := maintainPreflight{Remote: "origin", Err: errors.New("boom")}
	if failed.refusal(false) == "" {
		t.Fatal("a failed pre-flight cleared the guard — flatten would proceed unverified")
	}
	unchecked := maintainPreflight{Remote: "origin"}
	if unchecked.refusal(false) != "" {
		t.Fatal("a verified, undiverged remote was refused — the guard would never clear")
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

	t.Run("below threshold neither flattens nor is refused", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 5, countKnown: true}
		row := maintainClassifyForPlan(db, threshold, false)
		if row.willFlatten || row.refused {
			t.Errorf("row = %+v, want neither willFlatten nor refused for a database under threshold", row)
		}
	})

	t.Run("over threshold with a clean pre-flight flattens", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 500, countKnown: true,
			preflight: maintainPreflight{Remote: "origin"}}
		row := maintainClassifyForPlan(db, threshold, false)
		if !row.willFlatten {
			t.Error("willFlatten = false, want true for an over-threshold, undiverged database")
		}
		if row.refused {
			t.Error("refused = true for a database whose pre-flight cleared it")
		}
		if row.countUnknown {
			t.Error("countUnknown = true for a database with a known count")
		}
	})

	t.Run("diverged database is refused, not counted as flattening", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 500, countKnown: true,
			preflight: maintainPreflight{Remote: "origin", Diverged: true}}
		row := maintainClassifyForPlan(db, threshold, false)
		if row.willFlatten {
			t.Error("willFlatten = true for a diverged database — it must be excluded from the flatten count")
		}
		if !row.refused {
			t.Fatal("refused = false for a diverged database")
		}
		if row.refusalReason == "" {
			t.Error("refusalReason is empty for a refused database")
		}
	})

	t.Run("--force-diverged clears the refusal and counts as flattening", func(t *testing.T) {
		db := maintainDBInfo{name: "om", commitCount: 500, countKnown: true,
			preflight: maintainPreflight{Remote: "origin", Diverged: true}}
		row := maintainClassifyForPlan(db, threshold, true)
		if row.refused {
			t.Error("refused = true with --force-diverged, want the refusal skipped")
		}
		if !row.willFlatten {
			t.Error("willFlatten = false with --force-diverged on a database over threshold")
		}
	})

	t.Run("refused database with an unknown count is not double-counted", func(t *testing.T) {
		// The specific gap: a database that is both over threshold with an
		// unknown commit count AND refused by the pre-flight must contribute
		// to refusedCount alone. Reading countUnknown here would let it also
		// increment unknownCount in the caller, even though it was never
		// flattened.
		db := maintainDBInfo{name: "om", countErr: errors.New("connect: connection refused"),
			preflight: maintainPreflight{Remote: "origin", Diverged: true}}
		row := maintainClassifyForPlan(db, threshold, false)
		if !row.refused {
			t.Fatal("refused = false for a diverged database with an unknown count")
		}
		if row.willFlatten {
			t.Error("willFlatten = true for a refused database")
		}
		if row.countUnknown {
			t.Error("countUnknown = true for a refused database — it was never flattened, so it must not feed the unknown-flatten-count tally")
		}
	})

	t.Run("unknown count flattens and is counted exactly once", func(t *testing.T) {
		db := maintainDBInfo{name: "om", countErr: errors.New("connect: connection refused")}
		row := maintainClassifyForPlan(db, threshold, false)
		if row.refused {
			t.Error("refused = true for a database with no configured remote")
		}
		if !row.willFlatten {
			t.Fatal("willFlatten = false for an unknown-count database, want it flattened rather than silently skipped (gt-racu)")
		}
		if !row.countUnknown {
			t.Error("countUnknown = false for a database whose count measurement failed")
		}
	})

	t.Run("backup state is classified independently of flatten/refusal", func(t *testing.T) {
		backedUp := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, hasBackup: true, backupKnown: true},
			threshold, false)
		if !backedUp.hasBackup || backedUp.backupUnknown {
			t.Errorf("backedUp row = %+v, want hasBackup only", backedUp)
		}

		unbacked := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, backupKnown: true},
			threshold, false)
		if unbacked.hasBackup || unbacked.backupUnknown {
			t.Errorf("unbacked row = %+v, want neither hasBackup nor backupUnknown", unbacked)
		}

		unknownBackup := maintainClassifyForPlan(
			maintainDBInfo{name: "be", commitCount: 1, countKnown: true, backupErr: errors.New("boom")},
			threshold, false)
		if !unknownBackup.backupUnknown || unknownBackup.hasBackup {
			t.Errorf("unknownBackup row = %+v, want backupUnknown only", unknownBackup)
		}
	})
}

// TestMaintainPlanTally exercises the aggregate counts the same way
// runMaintain's plan-display loop derives them from maintainClassifyForPlan,
// pinning the two properties gt-aku6 found missing coverage for: a refused
// database (including one with an unknown count) is excluded from
// flattenCount and unknownCount, and refusedCount tracks it instead.
func TestMaintainPlanTally(t *testing.T) {
	t.Parallel()

	const threshold = 100
	dbInfos := []maintainDBInfo{
		{name: "clean", commitCount: 500, countKnown: true,
			preflight: maintainPreflight{Remote: "origin"}},
		{name: "diverged", commitCount: 500, countKnown: true,
			preflight: maintainPreflight{Remote: "origin", Diverged: true}},
		{name: "diverged-unknown-count", countErr: errors.New("connect: connection refused"),
			preflight: maintainPreflight{Remote: "origin", Diverged: true}},
		{name: "unknown-count-clean", countErr: errors.New("connect: connection refused")},
		{name: "quiet", commitCount: 1, countKnown: true},
	}

	var flattenCount, refusedCount, unknownCount int
	for _, db := range dbInfos {
		row := maintainClassifyForPlan(db, threshold, false)
		switch {
		case row.refused:
			refusedCount++
		case row.willFlatten:
			flattenCount++
			if row.countUnknown {
				unknownCount++
			}
		}
	}

	if flattenCount != 2 {
		t.Errorf("flattenCount = %d, want 2 (clean, unknown-count-clean)", flattenCount)
	}
	if refusedCount != 2 {
		t.Errorf("refusedCount = %d, want 2 (diverged, diverged-unknown-count)", refusedCount)
	}
	if unknownCount != 1 {
		t.Errorf("unknownCount = %d, want 1 — only unknown-count-clean flattens with an unknown count; diverged-unknown-count was refused, not flattened", unknownCount)
	}
}
