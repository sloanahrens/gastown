//go:build !integration

package crewfresh

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier; the integration tier's is in
// gitexec_integration_test.go, whose tag compiles both.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
