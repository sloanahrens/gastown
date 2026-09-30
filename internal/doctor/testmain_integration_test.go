//go:build integration

package doctor

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which starts real git, tmux and bd,
// under the hermetic harness.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
