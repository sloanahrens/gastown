package doctor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// writeTownRoutes writes a town's routes file in a temporary directory, so the
// checks that read it read nothing outside the test. The path is spelled out
// here rather than taken from the function under test (gt-rtxfy).
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
