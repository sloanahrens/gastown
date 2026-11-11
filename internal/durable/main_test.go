package durable_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain is in the external test package because unittier imports durable: the
// package's own tests cannot import it.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
