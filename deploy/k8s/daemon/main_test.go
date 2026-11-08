package daemon

import (
	"flag"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	// -update rewrites testdata/ instead of comparing it. Registering it here
	// keeps the value in the flag set rather than in a package variable a test
	// could change under another.
	flag.Bool("update", false, "rewrite testdata/ with the renderer's current output")
	os.Exit(unittier.Main(m))
}
