package ring

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain fails the run on a goroutine that outlives its test, the unit
// tier's rule (docs/testing.md, "The rules").
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
