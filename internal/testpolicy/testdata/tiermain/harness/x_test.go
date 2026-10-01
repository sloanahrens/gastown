package harness

import (
	"os"
	"testing"

	tu "github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(tu.HermeticMain(m))
}
