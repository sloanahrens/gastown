package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// writeTownRoutes writes a town's routes file in a temporary directory, so the
// check reads nothing outside the test (gt-rtxfy).
func writeTownRoutes(t *testing.T, routes string) string {
	t.Helper()
	town := t.TempDir()
	dir := beads.GetTownBeadsPath(town)
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
