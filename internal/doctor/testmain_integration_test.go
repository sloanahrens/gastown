//go:build integration

package doctor

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which starts real git, tmux and bd,
// under the hermetic harness. The prefix registry is the unit tier's, so the
// untagged tests compiled in beside these resolve the same session names.
func TestMain(m *testing.M) {
	session.SetDefaultRegistry(testPrefixRegistry())
	os.Exit(testutil.HermeticMain(m))
}
