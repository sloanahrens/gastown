//go:build integration

package mail

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness. Tests that
// need a Dolt server start the shared container lazily via
// RequireDoltContainer; Finish terminates it. The prefix registry is the
// unit tier's, so the untagged tests compiled in beside these resolve the
// same session names.
func TestMain(m *testing.M) {
	session.SetDefaultRegistry(testPrefixRegistry())
	os.Exit(testutil.HermeticMain(m))
}
