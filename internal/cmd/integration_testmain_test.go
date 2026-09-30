//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tests under the hermetic harness (gt-lwi)
// with a mandatory ephemeral Dolt container. Tests like
// TestAgentWorktreesStayClean and TestBeadsRoutingFromTownRoot spawn gt/bd
// subprocesses that create databases (e.g., "tr", "hq"); routing to an
// isolated container (via GT_DOLT_PORT) means those databases are destroyed
// when the container is terminated — preventing orphan accumulation in the
// shared production Dolt data dir.
func TestMain(m *testing.M) {
	flag.Parse()
	_ = flag.Set("test.parallel", strconv.Itoa(integrationParallelism(runtime.GOOS, explicitParallel())))

	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: %v\n", err)
		os.Exit(1)
	}
	if testutil.DoltContainerPort() == "" {
		fmt.Fprintln(os.Stderr, "integration TestMain: dolt setup failed — Docker required for integration tests")
		os.Exit(1)
	}

	sweepStaleGTBinaries()
	code := h.Finish(m.Run())
	removeBuiltGT()
	os.Exit(code)
}

// integrationTestParallel is how many parallel integration tests run at once
// unless -parallel names a number. Each scheduler test spends ~6s in two bd
// inits and runs a dozen bd and gt subprocesses against the one shared Dolt
// container; run one at a time, that family alone took ~450s.
const integrationTestParallel = 4

// integrationParallelism is the -test.parallel the integration binary runs
// with: 1 on Windows, where concurrent bd processes collide on file locks;
// otherwise explicit when -parallel was given, else integrationTestParallel.
func integrationParallelism(goos string, explicit int) int {
	switch {
	case goos == "windows":
		return 1
	case explicit > 0:
		return explicit
	default:
		return integrationTestParallel
	}
}

// explicitParallel returns the -test.parallel given on the command line, or 0.
func explicitParallel() int {
	n := 0
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "test.parallel" {
			n, _ = strconv.Atoi(f.Value.String())
		}
	})
	return n
}
