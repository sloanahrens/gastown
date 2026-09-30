//go:build !integration

package util_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness with a refusing
// git on PATH (internal/testpolicy/gitfree.txt). The unit tier reads canned
// process tables and disk numbers; the real ps, lsof, tmux, diskutil and
// process groups are the integration tier's (testmain_integration_test.go).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
