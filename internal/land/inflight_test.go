package land

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestWorkInFlightCountsEveryRunDirectory: a landing, post-landing or revert
// directory under the work root is a run in flight; a root that does not exist
// holds none; a plain file or another name is not a run.
func TestWorkInFlightCountsEveryRunDirectory(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, root string)
		want  int
	}{
		{name: "no root", setup: func(*testing.T, string) {}, want: 0},
		{name: "an empty root", setup: func(t *testing.T, root string) {
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: 0},
		{name: "one of each run", setup: func(t *testing.T, root string) {
			mkdirs(t, root, "land-1", "post-2", "revert-3")
		}, want: 3},
		{name: "a plain file and another directory", setup: func(t *testing.T, root string) {
			mkdirs(t, root, "wt", "scratch")
			if err := os.WriteFile(filepath.Join(root, "land-file"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := filepath.Join(t.TempDir(), "rig")
			tc.setup(t, root)
			got, err := WorkInFlight(root)
			if err != nil || got != tc.want {
				t.Fatalf("WorkInFlight = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// TestWorkInFlightRefusesAnUnreadableRoot: a root that cannot be read is an error,
// never a count of zero, so the cut-over cannot read a busy root as idle.
func TestWorkInFlightRefusesAnUnreadableRoot(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := WorkInFlight(file); err == nil {
		t.Fatalf("WorkInFlight of a file = %d, nil; want an error", got)
	}
}

// TestWorkRunsNamesEachRunAndWhenItLastChanged: each run directory is listed by
// name with its modification time, so a run that never removed its directory is
// told from a live one by its age; a file or another directory is not listed.
func TestWorkRunsNamesEachRunAndWhenItLastChanged(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "rig")
	mkdirs(t, root, "land-old", "wt")
	old := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "land-old"), old, old); err != nil {
		t.Fatal(err)
	}
	runs, err := WorkRuns(root)
	if err != nil || len(runs) != 1 || runs[0].Name != "land-old" {
		t.Fatalf("WorkRuns = %v, %v; want the one run land-old", runs, err)
	}
	if d := runs[0].ModTime.Sub(old); d < -time.Second || d > time.Second {
		t.Fatalf("WorkRuns modification time = %v; want about %v", runs[0].ModTime, old)
	}
}

// TestWorkRunsRefusesAWorkRootItCannotRead: a work root the walker cannot read
// is an error, never a listing with no runs. A root that is not a directory
// fails the read for every user, root included, so the refusal is asserted the
// same way under every user.
func TestWorkRunsRefusesAWorkRootItCannotRead(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "rig")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, err := WorkRuns(root)
	if err == nil || len(runs) != 0 {
		t.Fatalf("WorkRuns = %v, %v; want the refusal of the unreadable root", runs, err)
	}
}

// mkdirs creates each name directly under root.
func mkdirs(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(root, n), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
