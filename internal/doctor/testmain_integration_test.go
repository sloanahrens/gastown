//go:build integration

package doctor

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which starts real git, tmux and bd,
// under the hermetic harness.
//
// The session prefix registry is process state, so it is set here once, to
// the prefixes createTestRig gives gastown and niflheim, instead of swapped
// per test.
func TestMain(m *testing.M) {
	reg := session.NewPrefixRegistry()
	reg.Register("ga", "gastown")
	reg.Register("ni", "niflheim")
	session.SetDefaultRegistry(reg)
	os.Exit(testutil.HermeticMain(m))
}
