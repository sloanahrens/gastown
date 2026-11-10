package config

import (
	"slices"
	"testing"
)

// TestLandingWorkerRowNeedsOnlyOm: the landing worker's verdict is main's Forgejo
// status, so the container execs no toolchain for it. A rig with post_land_command
// set would need make, go and golangci-lint, and the image does not carry them.
func TestLandingWorkerRowNeedsOnlyOm(t *testing.T) {
	t.Parallel()
	var needs []string
	found := false
	for _, s := range DaemonSteps() {
		if s.Name == "landing_worker" {
			needs, found = s.Needs, true
		}
	}
	if !found {
		t.Fatal("the audit table has no landing_worker row")
	}
	if !slices.Contains(needs, "om") {
		t.Errorf("landing_worker Needs %q does not name om", needs)
	}
	for _, toolchain := range []string{"make", "go", "golangci-lint"} {
		if slices.Contains(needs, toolchain) {
			t.Errorf("landing_worker Needs %q names %s; the image carries no toolchain", needs, toolchain)
		}
	}
}
