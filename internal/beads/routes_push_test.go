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
// failed push is returned rather than dropped. The test names its own push, so
// it moves no package state.
func TestWriteRoutesPushesTheWorkspace(t *testing.T) {
	t.Parallel()
	var pushed []string
	push := func(_ context.Context, townRoot string) (string, error) {
		pushed = append(pushed, townRoot)
		return "manifest", nil
	}
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := writeRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}}, push); err != nil {
		t.Fatalf("writeRoutes: %v", err)
	}
	if want := filepath.Dir(beadsDir); len(pushed) != 1 || pushed[0] != want {
		t.Errorf("pushed %v, want the town root %s once", pushed, want)
	}

	failed := func(context.Context, string) (string, error) {
		return "", errors.New("the gateway at gw:7440 did not answer")
	}
	err := writeRoutes(beadsDir, []Route{{Prefix: "hq-", Path: "."}}, failed)
	if err == nil {
		t.Fatal("writeRoutes succeeded with a failed push")
	}
	if !strings.Contains(err.Error(), "bd gateway's workspace was not updated") {
		t.Errorf("error = %v, want it to name the stale workspace", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error = %v, want the push's own failure", err)
	}
}

// TestWriteRoutesDoesNotPushAFailedWrite: a file that cannot be published is the
// rename's failure, and nothing is pushed: the gateway must not be told a route
// table that never landed.
func TestWriteRoutesDoesNotPushAFailedWrite(t *testing.T) {
	t.Parallel()
	pushed := false
	push := func(context.Context, string) (string, error) {
		pushed = true
		return "manifest", nil
	}
	blocked := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(filepath.Join(blocked, RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeRoutes(blocked, []Route{{Prefix: "hq-", Path: "."}}, push); err == nil {
		t.Fatal("writeRoutes published over a directory at routes.jsonl")
	}
	if pushed {
		t.Error("pushed after a failed routes write, want no push")
	}
}
