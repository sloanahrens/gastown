//go:build !integration

package crewfresh

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier. This file is built only without the integration
// tag; with it, gitexec_integration_test.go supplies the one TestMain (the
// hermetic harness, and the stand-in git's role), so a package never has two.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
