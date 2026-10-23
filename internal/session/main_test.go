//go:build !integration

package session

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's unit tier under the hermetic harness (gt-lwi).
// Git on PATH refuses to run (internal/testpolicy/gitfree.txt): nothing here
// needs it. The integration tier's TestMain is in testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
