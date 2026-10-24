package cmd

import (
	"path/filepath"
	"strings"
	"testing"
)

// The help names --mango-dir and its default, and the flag is empty until it is
// given, so the town's own metrics directory is the default.
func TestDashboardMangoDirFlag(t *testing.T) {
	t.Parallel()

	f := dashboardCmd.Flags().Lookup("mango-dir")
	if f == nil {
		t.Fatal("gt dashboard has no --mango-dir flag")
	}
	if f.DefValue != "" {
		t.Errorf("--mango-dir defaults to %q, want the town's directory by default", f.DefValue)
	}
	if !strings.Contains(f.Usage, "<town>/.runtime/metrics/latest") {
		t.Errorf("--mango-dir's help does not name its default: %q", f.Usage)
	}
}

// The panel is read from the flag's directory when one is given, and from the
// town's metrics directory when none is.
func TestDashboardMangoPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if want := filepath.Join(root, ".runtime", "metrics", "latest"); dashboardMangoPath(root, "") != want {
		t.Errorf("the panel is read from %q by default, want %q", dashboardMangoPath(root, ""), want)
	}

	elsewhere := t.TempDir()
	if got := dashboardMangoPath(root, elsewhere); got != elsewhere {
		t.Errorf("--mango-dir %q was read as %q", elsewhere, got)
	}
}
