package daemon

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTrackerTown plants a town whose route table is routes and whose gastown
// rig names the Dolt database gastown in its metadata.
func writeTrackerTown(t *testing.T, routes string) string {
	t.Helper()
	townRoot := t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{townBeads, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), []byte(`{"dolt_database":"gastown"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// TestWispReaperSkipsTrackerRigs: the daemon's sweep leaves a tracker rig's
// Dolt database out and logs the skip, and keeps the bd database it does not own.
func TestWispReaperSkipsTrackerRigs(t *testing.T) {
	t.Parallel()
	townRoot := writeTrackerTown(t, `{"path":"gastown/mayor/rig","prefix":"gt-","backend":"tracker"}
{"path":"beads/mayor/rig","prefix":"be-"}
`)
	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0), config: &Config{TownRoot: townRoot}}

	kept, tracked, err := d.skipTrackerRigs([]string{"gastown", "beads"})
	if err != nil {
		t.Fatalf("skipTrackerRigs: %v", err)
	}
	if want := []string{"beads"}; !slices.Equal(kept, want) {
		t.Errorf("sweep keeps %v, want %v: the tracker rig's database must not reach the bd sweep", kept, want)
	}
	if tracked != 1 {
		t.Errorf("tracker rigs = %d, want 1", tracked)
	}
	if !strings.Contains(buf.String(), "wisp_reaper: gastown: skipped") {
		t.Errorf("log = %q, want a line that the tracker rig gastown was skipped", buf.String())
	}
}

// TestWispReaperRefusesAnUnreadableRouteTable: a route table that does not
// parse hides the tracker rigs, so the pass refuses instead of sweeping them.
func TestWispReaperRefusesAnUnreadableRouteTable(t *testing.T) {
	t.Parallel()
	townRoot := writeTrackerTown(t, "{not json\n")
	var buf strings.Builder
	d := &Daemon{logger: log.New(&buf, "", 0), config: &Config{TownRoot: townRoot}}

	kept, _, err := d.skipTrackerRigs([]string{"gastown", "beads"})
	if err == nil {
		t.Fatalf("skipTrackerRigs kept %v, want an error for a route table that does not parse", kept)
	}
	if strings.Contains(buf.String(), "skipped") {
		t.Errorf("log = %q, want no skip line when the route table cannot be read", buf.String())
	}
}
