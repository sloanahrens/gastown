package forgejotest

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain fails the run when a goroutine outlives the tests (docs/testing.md, "The rules").
func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }
