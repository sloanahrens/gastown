package shelltier

import (
	"os"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// trackedShellTests is `git ls-files '*_test.sh'` on the rig when the manifest
// was written: the scripts, in git's order, with no testdata fixtures.
var trackedShellTests = []string{
	".githooks/pre-push_test.sh",
	"deploy/k8s/kind/kind_test.sh",
	"scripts/check-install-path_test.sh",
	"scripts/deep-review_test.sh",
	"scripts/flake-sweep_test.sh",
	"scripts/forgejo-cutover_test.sh",
	"scripts/forgejo-probe_test.sh",
	"scripts/forgejo-provision_test.sh",
	"scripts/forgejo-resync_test.sh",
	"scripts/gate-vs-load_test.sh",
	"scripts/install-local_test.sh",
	"scripts/makefile-gate_test.sh",
	"scripts/polecat-go-floor_test.sh",
	"scripts/post-land-shell_test.sh",
	"scripts/resource-sampler_test.sh",
	"scripts/town_metrics_test.sh",
}

// trackedLister is the fake git: it lists the tracked scripts and nothing else.
func trackedLister() ([]string, error) {
	return slices.Clone(trackedShellTests), nil
}

// onDisk is the fake filesystem: a path exists when it is a tracked script.
func onDisk(path string) bool {
	return slices.Contains(trackedShellTests, path)
}
