//go:build integration

package procutil

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which starts real processes, under the
// hermetic harness.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
