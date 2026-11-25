//go:build integration

package done

import (
	"os"
	"slices"
	"testing"
)

// TestIntegrationStaticGateListerListsAPackageWhoseTestMainStartsAContainer: the real go list and
// the parser list the one test of internal/cmd's integration tier, under its tag. No test binary
// starts, so the TestMain that starts a container never runs (gt-8uqh1g).
func TestIntegrationStaticGateListerListsAPackageWhoseTestMainStartsAContainer(t *testing.T) {
	gr := GateRun{
		Pattern:  "TestIntegrationAcceptanceTickRetriesAGuardMissOnRealBd",
		Tags:     "integration",
		Packages: []string{"./internal/cmd"},
	}
	lister := newStaticGateLister(execGateListCommand, os.Environ, os.DirFS)
	got, err := lister("../..", gr)
	if err != nil {
		t.Fatalf("lister(%q) error = %v", gr.Pattern, err)
	}
	want := []string{gr.Pattern}
	if !slices.Equal(got, want) {
		t.Errorf("lister(%q) = %q, want %q", gr.Pattern, got, want)
	}
}
