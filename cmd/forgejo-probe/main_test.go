package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the package's tests under the unit tier. The command's behaviour
// is tested through forgejoprobe.Run, where its seams are, so this package has
// no test of its own beyond the wiring that compiles.
func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }
