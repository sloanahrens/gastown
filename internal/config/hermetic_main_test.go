//go:build !integration

package config_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's unit tier under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, and
// workspace resolution refusing the live town. Tests resolve agents against a
// fakeHost (host_test.go) instead of stubbing binaries onto PATH. WithoutGit
// puts a refusing git first on PATH (internal/testpolicy/gitfree.txt). It
// lives in the external test package because testutil imports config. The
// integration tier's TestMain is in testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
