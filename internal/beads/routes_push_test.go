package beads

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteRoutesPushesTheWorkspace: a routes write tells the bd gateway its
// workspace is stale, naming the town root the routes file belongs to, and a
// failed push is returned rather than dropped.
//
// The test moves the package's one pusher seam, so it must not run in parallel:
// a parallel test writing routes would read the swapped value (docs/testing.md).
func TestWriteRoutesPushesTheWorkspace(t *testing.T) {
	restore := pushWorkspaceFn
	t.Cleanup(func() { pushWorkspaceFn = restore })

	var pushed []string
	pushWorkspaceFn = func(_ context.Context, townRoot string) (string, error) {
		pushed = append(pushed, townRoot)
		return "manifest", nil
	}
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := WriteRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}}); err != nil {
		t.Fatalf("WriteRoutes: %v", err)
	}
	if want := filepath.Dir(beadsDir); len(pushed) != 1 || pushed[0] != want {
		t.Errorf("pushed %v, want the town root %s once", pushed, want)
	}

	pushWorkspaceFn = func(context.Context, string) (string, error) {
		return "", errors.New("the gateway at gw:7440 did not answer")
	}
	err := WriteRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}})
	if err == nil {
		t.Fatal("WriteRoutes succeeded with a failed push")
	}
	if !strings.Contains(err.Error(), "bd gateway's workspace was not updated") {
		t.Errorf("error = %v, want it to name the stale workspace", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error = %v, want the push's own failure", err)
	}

	// A file that cannot be published is the rename's failure, and nothing is
	// pushed: the gateway must not be told a route table that never landed.
	pushed = nil
	blocked := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(filepath.Join(blocked, RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRoutes(blocked, []Route{{Prefix: "hq-", Path: "."}}); err == nil {
		t.Fatal("WriteRoutes published over a directory at routes.jsonl")
	}
	if len(pushed) != 0 {
		t.Errorf("pushed %v after a failed routes write, want no push", pushed)
	}
}
