package scrub

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain applies the unit tier's run-level checks (docs/testing.md, "The
// rules").
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
