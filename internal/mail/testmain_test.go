//go:build !integration

package mail

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness (gt-lwi) with a
// refusing git on PATH (internal/testpolicy/gitfree.txt). bd answers through
// the bdScript runner, so no bd process or Dolt container is involved; the
// real bd and Dolt paths are the integration tier's
// (testmain_integration_test.go).
func TestMain(m *testing.M) {
	// Every test sees the same prefix registry, set once here rather than
	// swapped per test, so the tests can run in parallel.
	session.SetDefaultRegistry(testPrefixRegistry())
	os.Exit(testutil.HermeticMain(m, testutil.WithoutGit()))
}
