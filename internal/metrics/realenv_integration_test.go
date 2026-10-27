//go:build integration

package metrics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRealEnv is the ported scripts/integ_town_metrics_smoke.py: the
// extract runs over the real town's ledgers, and every ledger line it read is
// accounted for as a row or as a logged drop. It reads the live town and writes
// nothing into it — the extract's files and both caches go to temporary
// directories — so a sweep never changes the town it measures. It belongs to
// the integration tier, which the hourly sweep runs and the gate does not, and
// it skips where the sweep names no town.
//
// Cost: one git show per landing the diff cache does not hold, one bd list per
// bead directory, and at most 150 capped bd show calls. Every command carries a
// timeout; nothing polls.
func TestIntegrationRealEnv(t *testing.T) {
	ctx := context.Background()
	real, err := RealEnv(ctx, RealEnvOptions{
		Today: "smoke",
		Town:  smokeTown(t),
		Run:   OSRunnerDir(DefaultAllowEnv),
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	// The wiring only reads the town; the extract's output and the caches the
	// smoke saves go to temporary directories.
	real.Extract.OutDir = t.TempDir()
	// Extract refuses a ledger line that vanished without a logged drop.
	result, err := Extract(ctx, real.Extract)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := SaveCaches(real.Extract, t.TempDir()); err != nil {
		t.Fatalf("SaveCaches: %v", err)
	}
	file, err := os.Open(filepath.Join(real.Extract.OutDir, landingsName))
	if err != nil {
		t.Fatalf("open %s: %v", landingsName, err)
	}
	defer func() { _ = file.Close() }()
	rows, err := ReadCSV(file)
	if err != nil {
		t.Fatalf("read %s: %v", landingsName, err)
	}
	if len(rows) < 1000 {
		t.Errorf("%s suspiciously small: %d rows", landingsName, len(rows))
	}
	if len(rows) != len(result.Landings) {
		t.Errorf("%s holds %d rows, the extract returned %d", landingsName, len(rows), len(result.Landings))
	}
	ledgerDrops := 0
	for _, drop := range result.Footer.RowsDropped {
		if strings.HasPrefix(drop[0], "ledger:") {
			ledgerDrops++
		}
	}
	if got, want := len(rows)+ledgerDrops, result.Footer.RowsRead["ledger"]; got != want {
		t.Errorf("%d rows + %d ledger drops != %d ledger lines read", len(rows), ledgerDrops, want)
	}
}

// smokeTown is the town the smoke reads: the one TOWN_METRICS_TOWN names, which
// the sweep sets. The hermetic harness's forbidden town is deliberately not a
// fallback: that variable names the town a test must not touch, so reading it
// here would be the one thing the harness forbids.
func smokeTown(t *testing.T) string {
	t.Helper()
	town := os.Getenv(envTown)
	if town == "" {
		t.Skip("the smoke needs a live town: set " + envTown + " to one with .runtime/landings")
	}
	if _, err := os.Stat(filepath.Join(town, ".runtime", "landings")); err != nil {
		t.Skipf("%s names no live town: %v", envTown, err)
	}
	return town
}
