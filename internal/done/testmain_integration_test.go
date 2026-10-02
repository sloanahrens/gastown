//go:build integration

package done

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness; the unit
// tier's TestMain (hermetic_main_test.go) is the non-integration build. These
// tests drive real git repositories and the real gt binary, not bd.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
