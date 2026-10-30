// Package wiringtest checks that every Jev battery passes its scrub_check
// setting to the client it asks through. Building the batteries reaches beads
// and the Dolt server, so the package runs under the hermetic harness.
package wiringtest

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
