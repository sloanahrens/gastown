package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDirSizeHuman(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Empty directory
	got := dirSizeHuman(dir)
	if got != "0 B" {
		t.Errorf("empty dir: got %q, want %q", got, "0 B")
	}

	// Write a 1024-byte file
	data := make([]byte, 1024)
	if err := os.WriteFile(filepath.Join(dir, "file.dat"), data, 0644); err != nil {
		t.Fatal(err)
	}
	got = dirSizeHuman(dir)
	if got != "1.0 KB" {
		t.Errorf("1KB file: got %q, want %q", got, "1.0 KB")
	}

	// Add a subdirectory with another file
	subDir := filepath.Join(dir, "sub")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	data2 := make([]byte, 512)
	if err := os.WriteFile(filepath.Join(subDir, "nested.dat"), data2, 0644); err != nil {
		t.Fatal(err)
	}
	got = dirSizeHuman(dir)
	if got != "1.5 KB" {
		t.Errorf("1.5KB total: got %q, want %q", got, "1.5 KB")
	}
}

func TestDirSizeHuman_NonexistentDir(t *testing.T) {
	t.Parallel()
	got := dirSizeHuman("/nonexistent/path/that/does/not/exist")
	if got != "0 B" {
		t.Errorf("nonexistent dir: got %q, want %q", got, "0 B")
	}
}

// ADR 0002: gastown no longer pushes or pulls Dolt remotes, so neither verb
// exists.
func TestDoltCmd_HasNoRemoteSyncVerbs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"sync", "pull"} {
		for _, c := range doltCmd.Commands() {
			if c.Name() == name {
				t.Errorf("gt dolt %s is still registered", name)
			}
		}
	}
}

// The too-many-orphans hint must not tell the operator to rm -rf a test-database
// glob: the glob also matches databases an open hold bead parks, and only the
// hold-aware gt dolt cleanup listing says which are safe to remove.
func TestDoltCleanupHintNamesNoBlanketRm(t *testing.T) {
	t.Parallel()
	hint := tooManyOrphansHint()
	if strings.Contains(hint, "rm -rf") {
		t.Errorf("hint suggests a blanket rm -rf:\n%s", hint)
	}
	if !strings.Contains(hint, "gt dolt cleanup") {
		t.Errorf("hint does not point at gt dolt cleanup:\n%s", hint)
	}
	// The hold lookup queries the server, so the listing must come before the
	// stop; listed after it, no orphan would be marked HELD.
	list, stop := strings.Index(hint, "gt dolt cleanup --dry-run"), strings.Index(hint, "gt dolt stop")
	if list < 0 || stop < 0 || list > stop {
		t.Errorf("hint must list orphans (gt dolt cleanup --dry-run) before gt dolt stop:\n%s", hint)
	}
}

// A dry run changes nothing, so a running daemon or Dolt server must not stop
// it from previewing; a live migration is still refused (gt-f0gp1).
func TestDoltMigrateDryRunWhileServerRunning(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                string
		dry, daemon, server bool
		wantErr             string
	}{
		{"dry run, server running", true, false, true, ""},
		{"dry run, daemon running", true, true, false, ""},
		{"dry run, both running", true, true, true, ""},
		{"live, daemon running", false, true, false, "daemon is running"},
		{"live, server running", false, false, true, "Dolt server is running"},
		{"live, nothing running", false, false, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := migrateRunningGuard(tt.dry, tt.daemon, tt.server)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("migrateRunningGuard = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("migrateRunningGuard = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
