package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// listArchive returns the dolt_archive row of a town whose mayor/daemon.json
// patrols object is patrols.
func listArchive(t *testing.T, patrols string) (Job, bool) {
	t.Helper()
	root := t.TempDir()
	writeMayorDaemon(t, root, patrols)
	rows, err := List(root, Options{}, testNow)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return findJob(rows, doltArchiveName)
}

// archiveRow lists a town whose scheduled_maintenance archive_dir is dir and
// returns its dolt_archive row.
func archiveRow(t *testing.T, dir string) Job {
	t.Helper()
	row, ok := listArchive(t, fmt.Sprintf(`{"scheduled_maintenance":{"archive_dir":%q}}`, dir))
	if !ok {
		t.Fatalf("no %q row", doltArchiveName)
	}
	return row
}

// writeArchiveAt writes one archive file in dir aged by age.
func writeArchiveAt(t *testing.T, dir, name string, age time.Duration) {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, "archive")
	at := testNow.Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// archiveDirWith returns a directory holding one archive file aged by age.
func archiveDirWith(t *testing.T, name string, age time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	writeArchiveAt(t, dir, name, age)
	return dir
}

func TestJobsDoltArchiveRow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T) string
		want  State
		run   *time.Time // the row's last run; nil wants none
	}{
		{
			name: "newest archive file two hours old is ok",
			setup: func(t *testing.T) string {
				dir := archiveDirWith(t, "dolt-log-gt-2026-11-19.csv.gz", 2*time.Hour)
				writeArchiveAt(t, dir, "dolt-log-hq-2026-11-18.csv.gz", 3*time.Hour)
				return dir
			},
			want: StateOK,
			run:  ptrTime(testNow.Add(-2 * time.Hour)),
		},
		{
			name:  "archive file past the day and its grace is overdue",
			setup: func(t *testing.T) string { return archiveDirWith(t, "dolt-log-gt.csv.gz", 30*time.Hour+time.Minute) },
			want:  StateOverdue,
			run:   ptrTime(testNow.Add(-(30*time.Hour + time.Minute))),
		},
		{
			name:  "directory with no archive file is never_run",
			setup: func(t *testing.T) string { return t.TempDir() },
			want:  StateNeverRun,
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

func TestJobsDoltArchiveRowUnreadable(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "archive")
	writeFile(t, file, "archive")
	cases := []struct{ name, patrols string }{
		{"archive_dir is not a string", `{"scheduled_maintenance":{"archive_dir":42}}`},
		{"archive_dir is a relative path", `{"scheduled_maintenance":{"archive_dir":"relative/archive"}}`},
		{"archive directory cannot be read", fmt.Sprintf(`{"scheduled_maintenance":{"archive_dir":%q}}`, file)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			row, ok := listArchive(t, c.patrols)
			if !ok || row.State != StateUnknown {
				t.Errorf("row = %+v, present = %v; want one unknown %s row", row, ok, doltArchiveName)
			}
		})
	}
	// List reads the same file first and returns its error, so the patrol
	// config's own read failures are driven through the row's reader directly.
	for _, body := range []string{`{"patrols":`, `{"patrols":42}`} {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "mayor", "daemon.json"), body)
		if rows := doltArchiveRows(root, testNow); len(rows) != 1 || rows[0].State != StateUnknown {
			t.Errorf("config %q: rows = %v, want one unknown row", body, rows)
		}
	}
}
