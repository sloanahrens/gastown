//go:build !integration

package land

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs land's unit tier under the hermetic harness (gt-lwi), like
// every package whose tests could reach bd: they use beadsfake and gitfake
// only. WithoutGit puts a refusing git first on PATH
// (internal/testpolicy/gitfree.txt). The integration tier's TestMain is in
// testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
