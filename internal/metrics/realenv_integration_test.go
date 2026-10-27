//go:build integration

package metrics

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/workspace"
)

// TestIntegrationRealEnv is the ported scripts/integ_town_metrics_smoke.py: the
// extract runs read-only over the real town, and every ledger line it read is
// accounted for as a row or as a logged drop. It belongs to the integration
// tier, which the hourly sweep runs and the gate does not, and it skips where
// no town is present.
//
// Cost: one git show per landing the diff cache does not hold, one bd list per
// bead directory, and at most 150 capped bd show calls. Every command carries a
// timeout; nothing polls.
func TestIntegrationRealEnv(t *testing.T) {
	ctx := context.Background()
	real, err := RealEnv(ctx, RealEnvOptions{
		// "smoke" writes under <town>/.runtime/metrics/smoke, never into docs/.
		Today: "smoke",
		Town:  smokeTown(t),
		Run:   OSRunnerDir(DefaultAllowEnv),
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	// Extract refuses a ledger line that vanished without a logged drop.
	result, err := Extract(ctx, real.Extract)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := SaveCaches(real.Extract, real.Town); err != nil {
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

// smokeTown is the town the smoke reads: TOWN_METRICS_TOWN when the sweep names
// one, else the live town the hermetic harness recorded. The harness's HOME is
// a sandbox, so the home-level bead directory it lists is the sandbox's, not
// the operator's; the ledgers, usage files and repos all come from the town.
func smokeTown(t *testing.T) string {
	t.Helper()
	for _, town := range []string{os.Getenv(envTown), workspace.ForbiddenTownRoot()} {
		if town == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(town, ".runtime", "landings")); err == nil {
			return town
		}
	}
	t.Skip("the smoke needs a live town: set " + envTown + " to one with .runtime/landings")
	return ""
}
