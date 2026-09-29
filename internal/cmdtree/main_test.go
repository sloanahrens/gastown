package cmdtree

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
