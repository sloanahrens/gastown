package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// archiveRow lists a town whose scheduled_maintenance archive_dir is dir and
// returns its dolt_archive row.
func archiveRow(t *testing.T, dir string) Job {
	t.Helper()
	root := t.TempDir()
	writeMayorDaemon(t, root, fmt.Sprintf(`{"scheduled_maintenance":{"archive_dir":%q}}`, dir))
	rows, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row, ok := findJob(rows, doltArchiveName)
	if !ok {
		t.Fatalf("no %q row in %d rows", doltArchiveName, len(rows))
	}
	return row
}

// writeArchive writes one archive file in dir, modified at mod.
func writeArchive(t *testing.T, dir, name string, mod time.Time) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestJobsDoltArchiveRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) string // the archive directory
		want  State
		run   *time.Time // the row's last run; nil wants none
	}{
		{
			name: "newest archive file two hours old is ok",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeArchive(t, dir, "dolt-log-hq-2026-11-18.csv.gz", testNow.Add(-3*time.Hour))
				writeArchive(t, dir, "dolt-log-gt-2026-11-19.csv.gz", testNow.Add(-2*time.Hour))
				// Another step's file, newer than either archive above.
				writeArchive(t, dir, "events-gt-2026-11-19.csv.gz", testNow.Add(-time.Hour))
				return dir
			},
			want: StateOK,
			run:  ptrTime(testNow.Add(-2 * time.Hour)),
		},
		{
			name: "archive file past the day and its grace is overdue",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeArchive(t, dir, "dolt-log-gt-2026-11-17.csv.gz", testNow.Add(-(30*time.Hour + time.Minute)))
				return dir
			},
			want: StateOverdue,
			run:  ptrTime(testNow.Add(-(30*time.Hour + time.Minute))),
		},
		{
			name:  "directory with no archive file is never_run",
			setup: func(t *testing.T) string { return t.TempDir() },
			want:  StateNeverRun,
		},
		{
			name: "missing directory is never_run",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "not-yet-written")
			},
			want: StateNeverRun,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row := archiveRow(t, c.setup(t))
			if row.State != c.want {
				t.Errorf("State = %q, want %q", row.State, c.want)
			}
			if row.Kind != config.DaemonStepPatrol || row.Cadence != nightlyCadence {
				t.Errorf("Kind = %q, Cadence = %v, want %q and %v", row.Kind, row.Cadence, config.DaemonStepPatrol, nightlyCadence)
			}
			if c.run == nil {
				if row.LastRun != nil || row.Outcome != "" {
					t.Errorf("LastRun = %v, Outcome = %q, want neither", row.LastRun, row.Outcome)
				}
				return
			}
			if row.LastRun == nil || !row.LastRun.Equal(*c.run) {
				t.Errorf("LastRun = %v, want %v", row.LastRun, *c.run)
			}
			if row.Outcome != OutcomeOK {
				t.Errorf("Outcome = %q, want ok for an archive file", row.Outcome)
			}
		})
	}
}
