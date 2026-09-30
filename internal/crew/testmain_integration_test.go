//go:build integration

package crew

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with real
// git on PATH.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
