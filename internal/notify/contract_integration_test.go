//go:build integration

package notify_test

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/testutil"
)

// buildGT links a gt binary from this checkout, so the contract runs against
// the code under test rather than whatever gt is installed on PATH.
func buildGT(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the source tree")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	bin := filepath.Join(t.TempDir(), "gt")
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1", "-o", bin, "./cmd/gt")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building gt: %v\n%s", err, out)
	}
	return bin
}

// TestIntegrationNotifierContract runs the contract against notify.CLI and a
// gt built from this tree, in a scratch town: no GT_*/BD_* identity or socket
// from the caller's session reaches gt, the tmux socket derives from the
// scratch town's path, and the Dolt port is poisoned, so nothing it does can
// reach a live town.
func TestIntegrationNotifierContract(t *testing.T) {
	gt := buildGT(t)
	town := testutil.ScratchTown(t)
	env := func() []string {
		return testutil.CleanGTEnv(
			"GT_TOWN_ROOT="+town,
			"GT_TEST_HERMETIC=1",
			"GT_DOLT_PORT=1",
			"BEADS_DOLT_PORT=1",
		)
	}
	notifyfake.RunNotifierContract(t, func(t *testing.T) notify.Notifier {
		return &notify.CLI{Bin: gt, Dir: town, Env: env}
	})
}
