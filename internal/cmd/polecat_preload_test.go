package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
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
// table puts on the tracker, `gt polecat list`'s preload used to fail and warn
// once per rig, because the read behind it is a bd sql call and there is no bd
// on a tracker rig. The preload now warms nothing there and says nothing.
//
// The warning is the preload's error printed at its call site, so an empty
// buffer is the whole claim: nothing failed. The Dolt-rig half — that the
// preload still reads exactly as before — is pinned in internal/beads, where a
// recorded bd can be watched without a store behind it.
func TestPolecatListTrackerRigWarnsNoPreload(t *testing.T) {
	t.Parallel()
	var warn bytes.Buffer

	preloadRigSeats(&warn, beads.New(trackerRouteRig(t)), "gastown")

	if got := warn.String(); got != "" {
		t.Fatalf("gt polecat list warned on a tracker rig: %q, want no preload warning", got)
	}
}
