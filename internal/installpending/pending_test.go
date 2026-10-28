package installpending

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testNow is the instant the marker tests judge from: fixed, so an age in a
// case is exact rather than a reading of the wall clock.
func testNow() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }

// TestPending_LiveMarkerStands is the marker's live case, and the one the
// other rules are read against: a marker this process wrote minutes ago is an
// install in flight.
func TestPending_LiveMarkerStands(t *testing.T) {
	t.Parallel()
	now := testNow()
	town := t.TempDir()
	written := now.Add(-5 * time.Minute)
	if err := Write(town, written, os.Getpid()); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	m, err := Pending(town, now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m == nil {
		t.Fatal("a marker written five minutes ago by this process does not stand")
	}
	if m.PID != os.Getpid() {
		t.Errorf("marker pid = %d, want the writer's %d", m.PID, os.Getpid())
	}
	if !m.At.Equal(written) {
		t.Errorf("marker at = %s, want %s", m.At, written)
	}
}

// TestPending_NoMarkerIsNotPending pins the absent case, so the rules below
// are read as exceptions to a live marker rather than as the only way to nil.
func TestPending_NoMarkerIsNotPending(t *testing.T) {
	t.Parallel()
	m, err := Pending(t.TempDir(), testNow())
	if err != nil {
		t.Fatalf("reading a marker that was never written: %v", err)
	}
	if m != nil {
		t.Fatalf("a town with no marker reports a pending install: %+v", m)
	}
}

// TestPending_MarkerOlderThanTheCeilingIsNotPending pins the age rule: a
// marker no cycle refreshed is spent, whatever its writer is doing now.
func TestPending_MarkerOlderThanTheCeilingIsNotPending(t *testing.T) {
	t.Parallel()
	for _, age := range []time.Duration{Ceiling, Ceiling + time.Hour} {
		now := testNow()
		town := t.TempDir()
		if err := Write(town, now.Add(-age), os.Getpid()); err != nil {
			t.Fatalf("writing the marker: %v", err)
		}
		m, err := Pending(town, now)
		if err != nil {
			t.Fatalf("age %s: reading the marker: %v", age, err)
		}
		if m != nil {
			t.Errorf("a marker %s old (ceiling %s) still reads as pending: %+v", age, Ceiling, m)
		}
	}
}

// TestPending_BrokenMarkerIsAnError pins that a marker file which cannot be
// read or parsed is never reported as no install: a caller that refuses work
// while an install stands must not read a broken marker as permission.
func TestPending_BrokenMarkerIsAnError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not json", "pending: yes\n"},
		{"no fields", "{}"},
		{"no writer pid", `{"at":"2026-10-28T11:59:00Z"}`},
		{"no time", `{"pid":4242}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if err := os.MkdirAll(Dir(town), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(Path(town), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}

			m, err := Pending(town, testNow())
			if err == nil {
				t.Fatalf("a %s marker parsed as pending=%+v, want an error", tc.name, m)
			}
			if m != nil {
				t.Errorf("a broken marker returned a value beside its error: %+v", m)
			}
		})
	}
}

// TestPending_UnreadableMarkerIsAnError covers the read failure as distinct
// from the absence above: a path that answers EISDIR is a marker the reader
// could not judge, and judging it as none would clear the way for a sling.
func TestPending_UnreadableMarkerIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(Path(town), 0o755); err != nil {
		t.Fatal(err)
	}

	if m, err := Pending(town, testNow()); err == nil {
		t.Fatalf("a directory where the marker belongs read as pending=%+v, want an error", m)
	}
}

// TestWrite_ReplacesAnEarlierMarker pins the refresh: Ceiling measures from
// the last cycle that found the install due, so a later write must move the
// time rather than keep the first one.
func TestWrite_ReplacesAnEarlierMarker(t *testing.T) {
	t.Parallel()
	now := testNow()
	town := t.TempDir()
	if err := Write(town, now.Add(-20*time.Minute), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := Write(town, now.Add(-time.Minute), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	m, err := Pending(town, now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m == nil {
		t.Fatal("the refreshed marker does not stand")
	}
	if want := now.Add(-time.Minute); !m.At.Equal(want) {
		t.Errorf("marker at = %s, want the later write's %s", m.At, want)
	}
}

// TestWrite_RefusesNoWriterPID pins that a marker names the process a reader
// probes; one without a pid could never be read as spent by that rule.
func TestWrite_RefusesNoWriterPID(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := Write(town, testNow(), 0); err == nil {
		t.Fatal("a marker with no writer pid was written")
	}
	if _, err := os.Stat(Path(town)); !os.IsNotExist(err) {
		t.Errorf("a refused write left a file behind: %v", err)
	}
}

// TestClear_RemovesTheMarkerAndToleratesNone pins both halves of Clear: a
// standing marker stops standing, and a town with none is not an error.
func TestClear_RemovesTheMarkerAndToleratesNone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := Clear(town); err != nil {
		t.Fatalf("clearing a marker that was never written: %v", err)
	}
	if err := Write(town, testNow(), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if err := Clear(town); err != nil {
		t.Fatalf("clearing the marker: %v", err)
	}
	if _, err := os.Stat(Path(town)); !os.IsNotExist(err) {
		t.Errorf("the marker file survived Clear: %v", err)
	}
	if m, err := Pending(town, testNow()); err != nil || m != nil {
		t.Errorf("after Clear the marker reads pending=%+v, err=%v; want none", m, err)
	}
}

// TestClear_ReportsADirectoryItCannotRemove pins that a failed removal is
// reported: the caller logs it, and a silent success would leave a marker the
// next reader trusts.
func TestClear_ReportsADirectoryItCannotRemove(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(Path(town), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(Path(town), "child"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Clear(town); err == nil {
		t.Fatal("clearing a non-empty directory at the marker path reported success")
	}
}

// TestDir_HonoursTheDaemonDirOverride pins that the marker follows the same
// directory the install lock and scripts/install-gt.sh write to, or the writer
// and the reader disagree about which file is the marker.
func TestDir_HonoursTheDaemonDirOverride(t *testing.T) {
	t.Parallel()
	town := "/town"
	if got, want := dirFor(town, func(string) string { return "" }), filepath.Join(town, "daemon"); got != want {
		t.Errorf("dir with no override = %q, want %q", got, want)
	}
	overridden := func(key string) string {
		if key == "INSTALL_GT_DAEMON_DIR" {
			return "/elsewhere/daemon"
		}
		return ""
	}
	if got, want := dirFor(town, overridden), "/elsewhere/daemon"; got != want {
		t.Errorf("dir with INSTALL_GT_DAEMON_DIR set = %q, want %q", got, want)
	}
}
