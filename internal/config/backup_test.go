package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A backup is the previous copy with the file's own mode, and a path that is not
// there yet has nothing to keep.
func TestBackupFileKeepsThePreviousCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if got, err := BackupFile(path, "20261025T120000Z"); err != nil || got != "" {
		t.Fatalf("BackupFile of a missing path = %q, %v; want nothing to keep", got, err)
	}

	body := []byte(`{"role_agents":{"crew":"claude-sonnet"}}`)
	if err := os.WriteFile(path, body, 0o640); err != nil {
		t.Fatal(err)
	}
	dst, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if want := path + ".bak-20261025T120000Z"; dst != want {
		t.Errorf("backup = %q, want %q", dst, want)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != string(body) {
		t.Errorf("backup content = %q, %v; want the file's", got, err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("backup mode = %v, want the file's 0640", fi.Mode().Perm())
	}
}

// Two writes inside one stamp's resolution leave two copies: the older file is
// the one an operator restores, so a backup never replaces one (gt-jaagc).
func TestBackupFileNeverOverwritesAnEarlierCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if err := os.WriteFile(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := BackupFile(path, "20261025T120000Z")
	if err != nil {
		t.Fatalf("the second BackupFile: %v", err)
	}
	if second == first {
		t.Fatalf("the second backup was written over the first at %q", first)
	}
	if !strings.HasPrefix(second, first+"-") {
		t.Errorf("second backup = %q, want the first's name with a suffix", second)
	}
	got, err := os.ReadFile(first)
	if err != nil || string(got) != "first\n" {
		t.Errorf("the first copy = %q, %v; want the file as it was before the second write", got, err)
	}
}

// The suffix search is bounded: a directory that has collected BackupNameAttempts
// copies is reported, not searched forever (gt-jaagc).
func TestBackupFileStopsAfterBackupNameAttempts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := path + ".bak-20261025T120000Z"
	for i := 0; i < BackupNameAttempts; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i+1)
		}
		if err := os.WriteFile(name, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, err := BackupFile(path, "20261025T120000Z")
	if err == nil {
		t.Fatalf("BackupFile with every name taken = nil, want the taken names reported")
	}
	if !strings.Contains(err.Error(), "already exist") {
		t.Errorf("BackupFile = %v, want it to say the names are taken", err)
	}
}

// retentionNow is the fixed clock the retention tests run at, so a copy aged by
// eight days is past the window whatever the host's clock says.
var retentionNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

// A copy is pruned only when it is outside the newest backupRetentionCount and
// older than backupRetentionAge: the copy just written and every file whose
// name is not <path>.bak-* stay (gt-9tx8h).
func TestBackupFilePrunesOldCopies(t *testing.T) {
	t.Parallel()

	// copiesByAge names n copies, newest first: copy i is age plus i minutes
	// before the clock, so each has its own place in the mtime order.
	copiesByAge := func(prefix string, n int, age time.Duration) (map[string]time.Duration, []string) {
		copies := map[string]time.Duration{}
		var order []string
		for i := range n {
			name := fmt.Sprintf("config.json.bak-%s-%02d", prefix, i)
			copies[name] = age + time.Duration(i)*time.Minute
			order = append(order, name)
		}
		return copies, order
	}
	oldThirtyFive, oldOrder := copiesByAge("old", 35, 8*24*time.Hour)
	youngForty, _ := copiesByAge("young", 40, time.Hour)
	mixedYoung, _ := copiesByAge("myoung", 35, time.Hour)
	mixedOld, mixedOldOrder := copiesByAge("mold", 3, 8*24*time.Hour)

	cases := []struct {
		name string
		// copies are written before the backup, each aged before the clock.
		copies map[string]time.Duration
		// unrelated files are written too and must survive.
		unrelated []string
		// newAge is how old the copy this call writes is, so a case can put the
		// just-written copy outside the window.
		newAge   time.Duration
		wantGone []string
	}{
		{
			name:     "keeps the newest thirty once every copy is past the age window",
			copies:   oldThirtyFive,
			wantGone: oldOrder[29:],
		},
		{
			name:     "keeps a copy younger than the age window beyond the thirty",
			copies:   youngForty,
			wantGone: nil,
		},
		{
			name:     "drops an old copy while the young ones stay",
			copies:   merge(mixedYoung, mixedOld),
			wantGone: mixedOldOrder,
		},
		{
			name:      "leaves a file whose name is not a backup",
			copies:    oldThirtyFive,
			unrelated: []string{"config.json.orig", "notes.txt", "backup-config.json", "config.json.bak"},
			wantGone:  oldOrder[29:],
		},
		{
			name:     "keeps the copy just written when it is the oldest",
			copies:   oldThirtyFive,
			newAge:   30 * 24 * time.Hour,
			wantGone: oldOrder[30:],
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "config.json")
			if err := os.WriteFile(path, []byte(`{"live":true}`), 0o644); err != nil {
				t.Fatal(err)
			}
			for name, age := range c.copies {
				copyPath := filepath.Join(dir, name)
				if err := os.WriteFile(copyPath, []byte("old\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				setMtime(t, copyPath, retentionNow.Add(-age))
			}
			for _, name := range c.unrelated {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("not a backup\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			// Every case sets the written copy's mtime itself, so the result never
			// depends on what the host's clock reads.
			write := func(dst string, data []byte, mode os.FileMode) error {
				if err := writeBackupCopy(dst, data, mode); err != nil {
					return err
				}
				setMtime(t, dst, retentionNow.Add(-c.newAge))
				return nil
			}
			got, err := backupFile(path, "20261027T120000Z", backupRun{
				now:   func() time.Time { return retentionNow },
				write: write,
				prune: pruneBackups,
			})
			if err != nil {
				t.Fatalf("BackupFile: %v", err)
			}
			if want := filepath.Join(dir, "config.json.bak-20261027T120000Z"); got != want {
				t.Fatalf("backup = %q, want %q", got, want)
			}

			present := dirNames(t, dir)
			for _, name := range c.wantGone {
				if present[name] {
					t.Errorf("%s survived past the retention window", name)
				}
			}
			for name := range c.copies {
				if slices.Contains(c.wantGone, name) {
					continue
				}
				if !present[name] {
					t.Errorf("%s was removed, want it kept", name)
				}
			}
			for _, name := range c.unrelated {
				if !present[name] {
					t.Errorf("%s was removed, want an unrelated file left alone", name)
				}
			}
			if !present[filepath.Base(got)] {
				t.Errorf("the copy just written, %s, was pruned", filepath.Base(got))
			}
		})
	}
}

// The exported call prunes as well, on the wall clock the writers use.
func TestBackupFilePrunesOnTheWallClock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"live":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 35 {
		copyPath := filepath.Join(dir, fmt.Sprintf("config.json.bak-20260101T000000Z-%02d", i))
		if err := os.WriteFile(copyPath, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		setMtime(t, copyPath, time.Now().Add(-8*24*time.Hour-time.Duration(i)*time.Minute))
	}

	if _, err := BackupFile(path, "20261027T120000Z"); err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if got := len(dirNames(t, dir)); got != backupRetentionCount+1 {
		t.Errorf("files left = %d, want the newest %d copies", got, backupRetentionCount)
	}
}

// A copy cut short is an error and is not left behind: an operator who restores
// it would get a truncated config (gt-9tx8h).
func TestBackupFileFailsOnAShortCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"role_agents":{"crew":"claude-sonnet"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	short := func(dst string, data []byte, mode os.FileMode) error {
		return os.WriteFile(dst, data[:len(data)/2], mode)
	}

	got, err := backupFile(path, "20261027T120000Z", backupRun{
		now:   time.Now,
		write: short,
		prune: pruneBackups,
	})
	if err == nil {
		t.Fatalf("BackupFile with a short copy = %q, nil; want the short copy reported", got)
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("BackupFile = %v, want it to name the copy's length", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.json.bak-20261027T120000Z")); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the short copy is still there: %v, want it removed", statErr)
	}
	if live, readErr := os.ReadFile(path); readErr != nil || string(live) != body {
		t.Errorf("the live file = %q, %v; want it untouched", live, readErr)
	}
}

// A directory that cannot be pruned still gets its backup: the prune's error is
// logged and the copy is kept (gt-9tx8h).
func TestBackupFileKeepsTheBackupWhenThePruneFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := `{"role_agents":{"crew":"claude-sonnet"}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	failing := func(string, string, time.Time) error { return errors.New("the directory cannot be read") }

	got, err := backupFile(path, "20261027T120000Z", backupRun{
		now:   func() time.Time { return retentionNow },
		write: writeBackupCopy,
		prune: failing,
	})
	if err != nil {
		t.Fatalf("BackupFile with a failing prune = %v, want the backup kept", err)
	}
	if content, readErr := os.ReadFile(got); readErr != nil || string(content) != body {
		t.Errorf("the copy = %q, %v; want the file's content", content, readErr)
	}
}

// A directory that happens to carry a backup's name is neither written through
// nor removed: the copy takes the next free name (gt-9tx8h).
func TestBackupFileLeavesADirectoryAtTheNameAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taken := path + ".bak-20261027T120000Z"
	if err := os.Mkdir(taken, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := BackupFile(path, "20261027T120000Z")
	if err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if want := taken + "-2"; got != want {
		t.Errorf("backup = %q, want %q", got, want)
	}
	if fi, statErr := os.Stat(taken); statErr != nil || !fi.IsDir() {
		t.Errorf("the directory at %s = %v, %v; want it left alone", taken, fi, statErr)
	}
}

// setMtime pins a file's modification time, which the retention window reads.
func setMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// dirNames is the set of names in dir.
func dirNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// merge returns both copy sets as one.
func merge(a, b map[string]time.Duration) map[string]time.Duration {
	out := map[string]time.Duration{}
	for name, age := range a {
		out[name] = age
	}
	for name, age := range b {
		out[name] = age
	}
	return out
}
