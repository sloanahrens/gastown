//go:build !integration

package formula

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness with a refusing
// git on PATH (internal/testpolicy/gitfree.txt): the formula tests read
// templates and never run git or bash. The integration tier's TestMain, in
// testmain_integration_test.go, keeps the real tools for the tests that run
// formula shell.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
