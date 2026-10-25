//go:build integration

package cmd

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// childNoContainerEnv marks a test binary this package spawns to probe what
// happens before any bd call, so the child works on a host without Docker
// (gt-wtzzc).
const childNoContainerEnv = "GT_TEST_INTEGRATION_NO_CONTAINER"

// childTestName is the one test the child runs, and childRunPattern the
// -test.run value that restricts a process to it.
const childTestName = "TestIntegrationHelperInitWithoutPortChild"

const childRunPattern = "^" + childTestName + "$"

// TestMain runs the integration tests under the hermetic harness (gt-lwi)
// with a mandatory ephemeral Dolt container. Tests like
// TestIntegrationAgentWorktreesStayClean and TestIntegrationBeadsRoutingFromTownRoot spawn gt/bd
// subprocesses that create databases (e.g., "tr", "hq"); routing to an
// isolated container (via GT_DOLT_PORT) means those databases are destroyed
// when the container is terminated — preventing orphan accumulation in the
// shared production Dolt data dir.
func TestMain(m *testing.M) {
	flag.Parse()
	if runsOnlyChild() {
		// The child proves a bd init is refused before any bd or git call, so
		// it needs neither the command tree fixture nor the container. It
		// keeps its spawning test's environment, which puts a fake bd first
		// on PATH rather than a real one.
		os.Exit(m.Run())
	}
	prepareTestCommandTree() // tests then only read the tree
	if runtime.GOOS == "windows" {
		// Concurrent bd processes collide on file locks on Windows.
		_ = flag.Set("test.parallel", "1")
	}

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

// runsOnlyChild reports whether this process is the no-container child. The
// narrowing to childRunPattern keeps an inherited childNoContainerEnv from
// dropping the harness from a whole run, which would put every test in the
// package outside its sandbox (gt-wtzzc).
func runsOnlyChild() bool {
	if os.Getenv(childNoContainerEnv) != "1" {
		return false
	}
	f := flag.Lookup("test.run")
	return f != nil && f.Value.String() == childRunPattern
}
