//go:build !integration

package crewfresh

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier, whose git is the fakeGit the tests hold. The
// integration tier's TestMain, which drives real git, is in
// gitexec_integration_test.go: `go test -tags integration` compiles both
// tiers together, so the unit-tier TestMain is the one that carries
// !integration (docs/testing.md; internal/tmux has the same shape). The
// unit tier's run-time rules are off in the integration tier by design
// (internal/testutil/unittier).
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
