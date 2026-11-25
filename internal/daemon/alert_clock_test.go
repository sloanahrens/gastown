package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAlertClockLast covers both reads the clock serves: an item stamped
// through this clock, or through a second clock on the same directory, reads
// back its instant; a missing file or a file that does not hold one RFC3339
// line reads as never raised.
func TestAlertClockLast(t *testing.T) {
	t.Parallel()

	const key = "squash-deferral"
	raised := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	stamp := func(t *testing.T, c alertClock) {
		t.Helper()
		if err := c.stamp(key, raised); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	tests := []struct {
		name   string
		setup  func(t *testing.T, c alertClock)
		want   time.Time
		wantOK bool
	}{
		{"a stamp reads back the same instant", stamp, raised, true},
		{"a second clock on the same directory reads the stamp", func(t *testing.T, c alertClock) {
			stamp(t, alertClock{dir: c.dir})
		}, raised, true},
		{"a missing file is never raised", func(t *testing.T, c alertClock) {}, time.Time{}, false},
		{"a file holding garbage is never raised", func(t *testing.T, c alertClock) {
			if err := os.MkdirAll(c.dir, 0o700); err != nil {
				t.Fatalf("creating the clock directory: %v", err)
			}
			if err := os.WriteFile(filepath.Join(c.dir, key), []byte("not a timestamp\n"), 0o600); err != nil {
				t.Fatalf("writing the garbage file: %v", err)
			}
		}, time.Time{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := alertClock{dir: filepath.Join(t.TempDir(), "alert-clock")}
			tc.setup(t, c)

			got, ok := c.last(key)
			if ok != tc.wantOK {
				t.Fatalf("last(%q) ok = %v (read %v), want %v", key, ok, got, tc.wantOK)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("last(%q) = %v, want %v", key, got, tc.want)
			}
		})
	}
}

// TestAlertClockStampFailures drives both refusals stamp has: a directory it
// cannot create and an entry it cannot replace. Each error reaches the caller,
// so a rule cannot believe it recorded a raise the write did not make.
func TestAlertClockStampFailures(t *testing.T) {
	t.Parallel()

	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "alert-clock")
	if err := os.MkdirAll(filepath.Join(dir, "key"), 0o700); err != nil {
		t.Fatalf("creating the blocking directory: %v", err)
	}

	tests := []struct {
		name string
		dir  string
	}{
		{"a clock under a regular file cannot be created", filepath.Join(blocker, "alert-clock")},
		{"an entry that is a directory cannot be replaced", dir},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := (alertClock{dir: tc.dir}).stamp("key", time.Time{}); err == nil {
				t.Fatalf("stamp %s: got nil, want an error", tc.name)
			}
		})
	}
}

// TestAlertClockStampPermissions pins the modes stamp promises: the clock
// directory is created at 0700 on first use, and the entry is written at 0600,
// so the last-raised state is not readable by another account on the host.
func TestAlertClockStampPermissions(t *testing.T) {
	t.Parallel()

	const key = "backpressure"
	c := alertClock{dir: filepath.Join(t.TempDir(), "alert-clock")}
	if err := c.stamp(key, time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	checks := []struct {
		name string
		path string
		want os.FileMode
	}{
		{"the directory is created 0700", c.dir, 0o700},
		{"the file is written 0600", filepath.Join(c.dir, key), 0o600},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info, err := os.Stat(tc.path)
			if err != nil {
				t.Fatalf("stat %s: %v", tc.path, err)
			}
			if got := info.Mode().Perm(); got != tc.want {
				t.Errorf("%s mode = %o, want %o", tc.name, got, tc.want)
			}
		})
	}
}
