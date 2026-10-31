//go:build !integration

package crewfresh

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier, whose git is the fakeGit the tests hold. The
// integration tier's TestMain, which drives real git, is in
// gitexec_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
