// Command preflight measures the exec tax in the process tree it runs in and
// warns when that tree pays it, without failing: the gate's verdict is the
// tree's tests, not the host (gt-2ycne.1).
//
//	go run ./internal/exectax/cmd/preflight
//
// `make gate` runs it before lint, so a gate that is about to run several
// times slow says so in its own output, and the landing worker keeps the
// warning in the landing record (internal/land, parseWarnings).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/exectax"
	"github.com/steveyegge/gastown/internal/workspace"
)

func main() {
	// The probe is a measurement, not a check: no exit code carries it.
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "exec-tax preflight: %v\n", err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := exectax.Probe(ctx, exectax.Options{})
	if err != nil {
		return fmt.Errorf("measuring the exec tax: %w", err)
	}
	red := threshold()
	if !res.Taxed(red) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "gate: WARNING exec tax %s/exec in this process tree: a fresh executable here waits on a macOS scan or a throttled process, and the unit tier builds one per package (see gt-2ycne.1)\n", res.Median.Round(time.Millisecond))
	return nil
}

// threshold is the red line the town health report judges the same
// measurement by (settings/config.json operational.health.exec_tax_red), so
// the warning and the status line cannot disagree. A town whose settings are
// absent or broken reads the compiled default: this runs inside a gate, which
// must not create the file it looks for or fail on one it cannot read.
func threshold() time.Duration {
	town, err := workspace.FindFromCwd()
	if err != nil {
		return exectax.DefaultRed
	}
	if _, err := os.Stat(filepath.Join(town, "settings", "config.json")); err != nil {
		return exectax.DefaultRed
	}
	th, _, err := config.LoadOperationalConfig(town).GetHealthSettings().Resolve()
	if err != nil {
		return exectax.DefaultRed
	}
	return th.ExecTax.Red
}
