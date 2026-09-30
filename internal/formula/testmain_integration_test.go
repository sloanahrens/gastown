//go:build integration

package formula

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which runs formula shell with real
// bash and git, under the hermetic harness.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
