package modguard

import (
	"testing"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

// TestEnvArgsGuardOnTree fails for every test in the module that prints an
// environment in a failure message. It runs on the module root the shared
// tree holds, so it moved here from internal/testpolicy (gt-6lh6sr).
func TestEnvArgsGuardOnTree(t *testing.T) {
	t.Parallel()
	vs, err := testpolicy.ScanEnvArgs(tree.Root())
	if err != nil {
		t.Fatalf("ScanEnvArgs: %v", err)
	}
	for _, v := range vs {
		t.Error(v.String())
	}
}
