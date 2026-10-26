package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// writeTownRoutes writes a town's routes file in a temporary directory, so the
// check reads nothing outside the test. The path is spelled out here rather
// than taken from the function under test (gt-rtxfy).
func writeTownRoutes(t *testing.T, routes string) string {
	t.Helper()
	town := t.TempDir()
	dir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, beads.RoutesFileName), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// A rig routed to the tracker is reported, and a Dolt rig is not.
func TestTrackerKVCheckReportsTrackerRigs(t *testing.T) {
	t.Parallel()
	town := writeTownRoutes(t, `{"prefix":"tr-","path":"trackerrig","backend":"tracker"}`+"\n"+
		`{"prefix":"dl-","path":"doltrig"}`+"\n")
	res := NewTrackerKVCheck().Run(&CheckContext{TownRoot: town})
	if res.Status != StatusWarning {
		t.Fatalf("status = %v, want a warning for a tracker rig", res.Status)
	}
	if len(res.Details) != 1 || res.Details[0] != "trackerrig" {
		t.Errorf("details = %v, want only the tracker rig", res.Details)
	}
}

// With no tracker rig the check is OK, and a missing routes file is OK too.
func TestTrackerKVCheckQuietWithoutTrackerRigs(t *testing.T) {
	t.Parallel()
	town := writeTownRoutes(t, `{"prefix":"dl-","path":"doltrig"}`+"\n")
	if res := NewTrackerKVCheck().Run(&CheckContext{TownRoot: town}); res.Status != StatusOK {
		t.Errorf("status with only Dolt rigs = %v, want OK", res.Status)
	}
	if res := NewTrackerKVCheck().Run(&CheckContext{TownRoot: t.TempDir()}); res.Status != StatusOK {
		t.Errorf("status with no routes file = %v, want OK", res.Status)
	}
}

// A routes file that cannot be read is a warning, not OK: an OK here would
// hide tracker-routed rigs behind a read failure (gt-rtxfy).
func TestTrackerKVCheckWarnsWhenRoutesCannotBeRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the routes file belongs cannot be read as one.
	if err := os.MkdirAll(filepath.Join(town, ".beads", beads.RoutesFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	res := NewTrackerKVCheck().Run(&CheckContext{TownRoot: town})
	if res.Status != StatusWarning {
		t.Fatalf("status with an unreadable routes file = %v, want a warning", res.Status)
	}
}
