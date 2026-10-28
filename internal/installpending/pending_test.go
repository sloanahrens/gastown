package installpending

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/procid"
)

// testNow is the instant the marker tests judge from: fixed, so an age in a
// case is exact rather than a reading of the wall clock.
func testNow() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }

// testStore is a marker in a temp dir of the test's own. The tests hold the
// path, so none of them reaches the operator's marker through
// INSTALL_GT_DAEMON_DIR (gt-vc34c).
func testStore(t *testing.T) Store {
	t.Helper()
	return StoreIn(filepath.Join(t.TempDir(), FileName))
}

// writeMarker puts body at the store's path, for the cases that need a marker
// no Write would produce.
func writeMarker(t *testing.T, s Store, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPending_LiveMarkerStands is the marker's live case, and the one the
// other rules are read against: a marker this process wrote minutes ago is an
// install in flight.
func TestPending_LiveMarkerStands(t *testing.T) {
	t.Parallel()
	now := testNow()
	s := testStore(t)
	written := now.Add(-5 * time.Minute)
	if err := s.Write(written, os.Getpid()); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	m, err := s.Pending(now)
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
	m, err := testStore(t).Pending(testNow())
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
		s := testStore(t)
		if err := s.Write(now.Add(-age), os.Getpid()); err != nil {
			t.Fatalf("writing the marker: %v", err)
		}
		m, err := s.Pending(now)
		if err != nil {
			t.Fatalf("age %s: reading the marker: %v", age, err)
		}
		if m != nil {
			t.Errorf("a marker %s old (ceiling %s) still reads as pending: %+v", age, Ceiling, m)
		}
	}
}

// TestPending_AMarkerFarAheadOfTheClockIsNotPending pins the malformed-future
// rule: a marker dated past the reader's clock by more than FutureSkew is not
// evidence of an install, so it cannot stand until the clock catches up and
// for Ceiling after that (gt-vc34c).
func TestPending_AMarkerFarAheadOfTheClockIsNotPending(t *testing.T) {
	t.Parallel()
	now := testNow()
	s := testStore(t)
	if err := s.Write(now.Add(FutureSkew+time.Minute), os.Getpid()); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	m, err := s.Pending(now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m != nil {
		t.Fatalf("a marker dated past the clock reads as pending: %+v", m)
	}
}

// TestPending_AMarkerJustAheadOfTheClockStillStands pins the other side of the
// FutureSkew rule: a reader that captured its clock just before the writer
// wrote must not read the fresh marker as malformed (gt-vc34c).
func TestPending_AMarkerJustAheadOfTheClockStillStands(t *testing.T) {
	t.Parallel()
	now := testNow()
	s := testStore(t)
	if err := s.Write(now.Add(FutureSkew/2), os.Getpid()); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	m, err := s.Pending(now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m == nil {
		t.Fatal("a marker written half a FutureSkew ahead of the reading does not stand")
	}
}

// TestPending_AReusedPIDIsNotTheWriter pins the identity rule: the marker names
// a process, not a number, so a marker carrying a start token no current holder
// of that pid was started with is a dead writer whose pid went to someone else.
func TestPending_AReusedPIDIsNotTheWriter(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := testNow()
	writeMarker(t, s, fmt.Sprintf(`{"at":%q,"pid":%d,"start":"started-before-this-process"}`,
		now.Add(-time.Minute).Format(time.RFC3339), os.Getpid()))

	m, err := s.Pending(now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m != nil {
		t.Fatalf("a marker from a reused pid reads as pending: %+v", m)
	}
}

// TestPending_AMarkerWithoutAStartTokenFallsBackToThePID pins the fallback a
// platform with no start token leaves us: the pid probe decides, and Ceiling
// bounds it. A live writer must still read as pending, or a town that cannot
// take start tokens never waits out an install.
func TestPending_AMarkerWithoutAStartTokenFallsBackToThePID(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	now := testNow()
	writeMarker(t, s, fmt.Sprintf(`{"at":%q,"pid":%d}`, now.Add(-time.Minute).Format(time.RFC3339), os.Getpid()))

	m, err := s.Pending(now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m == nil {
		t.Fatal("a marker from this live process, with no start token, does not stand")
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
			s := testStore(t)
			writeMarker(t, s, tc.body)

			m, err := s.Pending(testNow())
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
	s := testStore(t)
	if err := os.MkdirAll(s.Path(), 0o755); err != nil {
		t.Fatal(err)
	}

	if m, err := s.Pending(testNow()); err == nil {
		t.Fatalf("a directory where the marker belongs read as pending=%+v, want an error", m)
	}
}

// TestWrite_ReplacesAnEarlierMarker pins the refresh: Ceiling measures from
// the last cycle that found the install due, so a later write must move the
// time rather than keep the first one.
func TestWrite_ReplacesAnEarlierMarker(t *testing.T) {
	t.Parallel()
	now := testNow()
	s := testStore(t)
	if err := s.Write(now.Add(-20*time.Minute), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(now.Add(-time.Minute), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	m, err := s.Pending(now)
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

// TestWrite_RecordsTheWritersStartToken pins that a write takes the writer's
// start token when the platform has one: the pid alone is not an identity, and
// the reader's identity rule has nothing to compare without it.
func TestWrite_RecordsTheWritersStartToken(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := s.Write(testNow(), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	m, err := s.Pending(testNow())
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m == nil {
		t.Fatal("the marker this process just wrote does not stand")
	}
	want, _ := procid.StartToken(os.Getpid())
	if m.Start != want {
		t.Errorf("marker start = %q, want the writer's %q", m.Start, want)
	}
}

// TestWrite_RefusesNoWriterPID pins that a marker names the process a reader
// probes; one without a pid could never be read as spent by that rule.
func TestWrite_RefusesNoWriterPID(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := s.Write(testNow(), 0); err == nil {
		t.Fatal("a marker with no writer pid was written")
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("a refused write left a file behind: %v", err)
	}
}

// TestClear_RemovesTheMarkerAndToleratesNone pins both halves of Clear: a
// standing marker stops standing, and a town with none is not an error.
func TestClear_RemovesTheMarkerAndToleratesNone(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := s.Clear(); err != nil {
		t.Fatalf("clearing a marker that was never written: %v", err)
	}
	if err := s.Write(testNow(), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if err := s.Clear(); err != nil {
		t.Fatalf("clearing the marker: %v", err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Errorf("the marker file survived Clear: %v", err)
	}
	if m, err := s.Pending(testNow()); err != nil || m != nil {
		t.Errorf("after Clear the marker reads pending=%+v, err=%v; want none", m, err)
	}
}

// TestClear_ReportsADirectoryItCannotRemove pins that a failed removal is
// reported: the caller logs it, and a silent success would leave a marker the
// next reader trusts.
func TestClear_ReportsADirectoryItCannotRemove(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if err := os.MkdirAll(s.Path(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Path(), "child"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := s.Clear(); err == nil {
		t.Fatal("clearing a non-empty directory at the marker path reported success")
	}
}

// TestDirFor_HonoursTheDaemonDirOverride pins that the marker follows the
// same directory the install lock and scripts/install-gt.sh write to, or the
// writer and the reader disagree about which file is the marker.
func TestDirFor_HonoursTheDaemonDirOverride(t *testing.T) {
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
