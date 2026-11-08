package dashboard

import (
	"os"
	"path/filepath"
	"testing"
)

// A releases path the reader cannot list is a named ledger failure on every
// row, never an app read as having no release.
func TestAppPowerNamesAnUnlistableLedgerDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	off := filepath.Join(root, "off")
	if err := os.MkdirAll(off, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := "since=2026-11-08T11:00:00Z by=operator env=prod reason=drill\n"
	if err := os.WriteFile(filepath.Join(off, "alpha.off"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	// The releases path exists and cannot be listed: a file where a directory
	// belongs.
	releases := filepath.Join(root, "releases")
	if err := os.WriteFile(releases, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := NewAppPowerReader(off, releases).Read(appNow)
	if p == nil || len(p.Apps) != 1 {
		t.Fatalf("line = %+v, want the marker's row", p)
	}
	if p.Apps[0].ReleaseNote != appLedgerReadFailed {
		t.Errorf("release note = %q, want %q", p.Apps[0].ReleaseNote, appLedgerReadFailed)
	}
}
