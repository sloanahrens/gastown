//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
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
	// Force sequential test execution to avoid bd file locks on Windows.
	_ = flag.Set("test.parallel", "1")
	flag.Parse()

	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration TestMain: %v\n", err)
		os.Exit(1)
	}
	if testutil.DoltContainerPort() == "" {
		fmt.Fprintln(os.Stderr, "integration TestMain: dolt setup failed — Docker required for integration tests")
		os.Exit(1)
	}

	os.Exit(h.Finish(m.Run()))
}
