//go:build integration

package convoy

import (
	"fmt"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with an
// ephemeral Dolt container. Container tests are opt-in (GT_TEST_DOCKER=1, as
// make test-integration sets); without it the store test skips.
//
// BEADS_TEST_MODE=1 makes the beads SDK name its database testdb_<hash> of
// the store path, which is how a pooled path finds its database. It is set
// once here rather than per test so the store test stays eligible for
// t.Parallel: t.Setenv panics inside a parallel test.
func TestMain(m *testing.M) {
	h, err := testutil.StartHermetic(testutil.WithDolt())
	if err != nil {
		fmt.Fprintf(os.Stderr, "convoy TestMain: %v\n", err)
		os.Exit(1)
	}
	_ = os.Setenv("BEADS_TEST_MODE", "1")
	os.Exit(h.Finish(m.Run()))
}
