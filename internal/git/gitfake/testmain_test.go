package gitfake

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the fake's tests under the hermetic harness with a refusing
// git on PATH (internal/testpolicy/gitfree.txt). The contract runs against
// real git from internal/git's integration tier
// (internal/git/contract_integration_test.go).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
