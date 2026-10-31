package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// trackerRouteRig lays out a town whose one route puts rig gastown on the
// tracker backend — a mayor/town.json so the wrapper finds the town, the route
// table under the town's .beads, and a rig whose own .beads redirects into its
// mayor clone the way a real rig's does. It returns the rig directory, which is
// what `gt polecat list` opens its wrapper on.
func trackerRouteRig(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		filepath.Join(townRoot, ".beads"),
		filepath.Join(townRoot, "gastown", ".beads"),
		filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("lay out the town: %v", err)
		}
	}
	writes := map[string]string{
		filepath.Join(townRoot, "mayor", "town.json"):            "{}\n",
		filepath.Join(townRoot, ".beads", beads.RoutesFileName):  `{"prefix":"gt-","path":"gastown/mayor/rig","backend":"tracker"}` + "\n",
		filepath.Join(townRoot, "gastown", ".beads", "redirect"): "mayor/rig/.beads\n",
	}
	for path, content := range writes {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return filepath.Join(townRoot, "gastown")
}

// TestPolecatListTrackerRigWarnsNoPreload is gt-p85pw.108: on a rig the route
// table puts on the tracker, `gt polecat list`'s seat build used to run a bd sql
// preload that failed and warned once per rig, because there is no bd on a
// tracker rig. The preload now reads nothing there, so the build sends no sql
// read to bd and warns nothing.
//
// It drives buildRigSeatsOn, the per-rig seat build `gt polecat list` runs
// through buildRigSeats, with a beads wrapper whose runner records and refuses
// every bd call. The build writes its warnings to os.Stderr, so the claim is
// read off the calls: a preload that warns has first made a bd sql read. The
// Dolt-rig half — that the preload still reads exactly as before — is pinned in
// internal/beads.
func TestPolecatListTrackerRigWarnsNoPreload(t *testing.T) {
	t.Parallel()
	rigPath := trackerRouteRig(t)
	// One polecat directory, so the build does not return before its reads.
	if err := os.MkdirAll(filepath.Join(rigPath, "polecats", "nux"), 0o755); err != nil {
		t.Fatalf("lay out a polecat: %v", err)
	}

	var mu sync.Mutex
	var calls [][]string
	bd := beads.New(rigPath, beads.WithDirRunner(func(_ context.Context, _ string, args []string) ([]byte, []byte, error) {
		mu.Lock()
		calls = append(calls, args)
		mu.Unlock()
		return nil, nil, errors.New("bd must not run on a tracker rig")
	}))

	buildRigSeatsOn(bd, &rig.Rig{Name: "gastown", Path: rigPath}, nil, time.Minute, time.Now(), "")

	for _, args := range calls {
		if slices.Contains(args, "sql") {
			t.Fatalf("gt polecat list read bd sql on a tracker rig: bd %v", args)
		}
	}
}
