package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The birds' reader is built over the town's runtime directory, and the command
// says what its Birds panel reads.
func TestDashboardBirdsWiring(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if path, want := dashboardRuntimeDir(root), filepath.Join(root, ".runtime"); path != want {
		t.Errorf("the birds are read from %q, want %q", path, want)
	}

	long := dashboardCmd.Long
	for _, want := range []string{"Birds panel", "daily.json", "26 hours"} {
		if !strings.Contains(long, want) {
			t.Errorf("gt dashboard's help does not name %q", want)
		}
	}
}

// The reader the command builds over the town's runtime directory returns the
// row for a bird that wrote its daily report there. Pointing the helper at the
// town root instead of its runtime directory leaves no row (gt-7f7nn).
func TestDashboardBirdsReaderReadsTheRuntimeDir(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	// The town holds the report under its own .runtime; the reader is built by
	// the helper the command uses, so a helper naming another directory leaves
	// no row.
	runtimeDir := filepath.Join(root, ".runtime")
	writeBirdReport(t, runtimeDir, "dunlin", 2*time.Hour)
	page := filepath.Join(runtimeDir, "dunlin", "reports", "daily.html")
	if err := os.WriteFile(page, []byte("<html>all quiet</html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows := dashboard.NewBirdReportsReader(dashboardRuntimeDir(root)).Read(birdsNow)

	if len(rows) != 1 {
		t.Fatalf("the reader wired to the town's runtime dir returned %d rows, want 1: %+v", len(rows), rows)
	}
	if got := rows[0]; got.Bird != "dunlin" || got.State != dashboard.BirdReportFresh || !got.HasPage {
		t.Errorf("row for dunlin reads %+v, want a fresh row with its page", got)
	}
}
