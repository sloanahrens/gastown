package land

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDayClock is a clock a test moves by hand, so no test sleeps to cross a day.
type fakeDayClock struct{ at time.Time }

func (c *fakeDayClock) now() time.Time { return c.at }

// TestLocalRoundBoundTwoPerDay: a bead gets two local rounds a day, the third is
// refused with ErrLocalRoundBound, another bead is unaffected, and the next
// local day allows two again.
func TestLocalRoundBoundTwoPerDay(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	b := Bound{Dir: t.TempDir(), Now: clock.now}

	for want := 1; want <= 2; want++ {
		n, err := b.Allow("gt-aaa")
		if err != nil || n != want {
			t.Fatalf("Allow(gt-aaa) call %d = (%d, %v), want (%d, nil)", want, n, err, want)
		}
	}
	if _, err := b.Allow("gt-aaa"); !errors.Is(err, ErrLocalRoundBound) {
		t.Fatalf("third Allow(gt-aaa) err = %v, want ErrLocalRoundBound", err)
	}
	if n, err := b.Allow("gt-bbb"); err != nil || n != 1 {
		t.Fatalf("Allow(gt-bbb) = (%d, %v), want (1, nil): another bead must be unaffected", n, err)
	}

	clock.at = time.Date(2026, 11, 14, 0, 30, 0, 0, time.Local)
	for want := 1; want <= 2; want++ {
		n, err := b.Allow("gt-aaa")
		if err != nil || n != want {
			t.Fatalf("next-day Allow(gt-aaa) call %d = (%d, %v), want (%d, nil)", want, n, err, want)
		}
	}
	if _, err := b.Allow("gt-aaa"); !errors.Is(err, ErrLocalRoundBound) {
		t.Fatalf("next-day third Allow(gt-aaa) err = %v, want ErrLocalRoundBound", err)
	}
}

// TestLocalRoundBoundConcurrentCallsGrantTwo: callers racing for one bead get
// exactly the two rounds a day allows, never more.
func TestLocalRoundBoundConcurrentCallsGrantTwo(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	b := Bound{Dir: t.TempDir(), Now: clock.now}

	const callers = 8
	var granted atomic.Int32
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Allow("gt-aaa")
			switch {
			case err == nil:
				granted.Add(1)
			case !errors.Is(err, ErrLocalRoundBound):
				t.Errorf("concurrent Allow err = %v, want nil or ErrLocalRoundBound", err)
			}
		}()
	}
	wg.Wait()
	if got := granted.Load(); got != localRoundsPerDay {
		t.Fatalf("%d concurrent callers were granted %d rounds, want %d", callers, got, localRoundsPerDay)
	}
}

// TestLocalRoundBoundCorruptStateFailsClosed: a state file that does not parse
// makes Allow fail with an error naming the file, and the count is not reset:
// the next call fails the same way and the file is left as it was.
func TestLocalRoundBoundCorruptStateFailsClosed(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	dir := t.TempDir()
	b := Bound{Dir: dir, Now: clock.now}
	path := filepath.Join(dir, "gt-aaa.json")
	garbage := []byte("{not json")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}

	for call := 1; call <= 2; call++ {
		n, err := b.Allow("gt-aaa")
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("call %d: Allow = (%d, %v), want an error naming %s", call, n, err, path)
		}
		if errors.Is(err, ErrLocalRoundBound) {
			t.Fatalf("call %d: a corrupt file must not read as the bound being reached", call)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(garbage) {
		t.Fatalf("corrupt state file was rewritten to %q, want it left as %q", got, garbage)
	}
}

// TestLocalRoundBoundInvalidStateFailsClosed: a state file that parses but names
// another bead, carries a negative count or has an unparseable day is refused
// with an error naming the file, and the file is left as it was.
func TestLocalRoundBoundInvalidStateFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		state string
	}{
		{name: "other bead", state: `{"bead":"gt-bbb","day":"2026-11-13","count":1}`},
		{name: "negative count", state: `{"bead":"gt-aaa","day":"2026-11-13","count":-1}`},
		{name: "bad day", state: `{"bead":"gt-aaa","day":"13/11/2026","count":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
			dir := t.TempDir()
			b := Bound{Dir: dir, Now: clock.now}
			path := filepath.Join(dir, "gt-aaa.json")
			if err := os.WriteFile(path, []byte(tc.state), 0o600); err != nil {
				t.Fatal(err)
			}

			n, err := b.Allow("gt-aaa")
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("Allow = (%d, %v), want an error naming %s", n, err, path)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.state {
				t.Fatalf("invalid state file was rewritten to %q, want it left as %q", got, tc.state)
			}
		})
	}
}

// TestLocalRoundBoundTouchesOnlyItsDir: after calls the only thing created under
// the caller's directory is Dir, and Dir holds only the bead's state file.
func TestLocalRoundBoundTouchesOnlyItsDir(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	parent := t.TempDir()
	dir := filepath.Join(parent, "local-round")
	b := Bound{Dir: dir, Now: clock.now}

	// The third gt-aaa call is refused by design; only the files it leaves matter here.
	for _, bead := range []string{"gt-aaa", "gt-aaa", "gt-aaa", "gt-bbb"} {
		_, _ = b.Allow(bead)
	}

	top, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0].Name() != "local-round" {
		t.Fatalf("parent holds %v, want only local-round", entryNames(top))
	}
	inside, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := entryNames(inside), []string{"gt-aaa.json", "gt-bbb.json"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Dir holds %v, want %v (no temp files left behind)", got, want)
	}
}

// TestLocalRoundBoundRefusesBadBead: a bead id that could name a path outside
// Dir is refused before any file is touched.
func TestLocalRoundBoundRefusesBadBead(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	parent := t.TempDir()
	dir := filepath.Join(parent, "local-round")
	b := Bound{Dir: dir, Now: clock.now}

	for _, bead := range []string{"", ".", "..", "../gt-aaa", "a/b", `a\b`} {
		if n, err := b.Allow(bead); err == nil {
			t.Fatalf("Allow(%q) = (%d, nil), want a refusal", bead, n)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("refused beads still created %s (stat err %v)", dir, err)
	}
}

// TestLocalRoundBoundUnreadableDirFailsClosed: a Dir that is a file, not a
// directory, makes Allow return an error and no count, so the round is not
// granted unrecorded.
func TestLocalRoundBoundUnreadableDirFailsClosed(t *testing.T) {
	t.Parallel()
	clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b := Bound{Dir: blocker, Now: clock.now}

	n, err := b.Allow("gt-aaa")
	if err == nil {
		t.Fatalf("Allow = (%d, nil), want an error for an unusable Dir", n)
	}
	if n != 0 {
		t.Fatalf("Allow returned count %d alongside the error, want 0", n)
	}
}

// injectedFailFS is the host filesystem with one operation made to fail, so a test
// reaches the write and rename branches without changing file permissions.
type injectedFailFS struct {
	osLedgerFS
	failMkdir  bool
	failWrite  bool
	failRename bool
}

func (f injectedFailFS) MkdirAll(path string, perm os.FileMode) error {
	if f.failMkdir {
		return errors.New("injected mkdir failure")
	}
	return f.osLedgerFS.MkdirAll(path, perm)
}

func (f injectedFailFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if f.failWrite {
		// A real failed write can leave part of the data on disk; leave half of it.
		if err := f.osLedgerFS.WriteFile(name, data[:len(data)/2], perm); err != nil {
			return err
		}
		return errors.New("injected write failure")
	}
	return f.osLedgerFS.WriteFile(name, data, perm)
}

func (f injectedFailFS) Rename(oldpath, newpath string) error {
	if f.failRename {
		return errors.New("injected rename failure")
	}
	return f.osLedgerFS.Rename(oldpath, newpath)
}

// TestLocalRoundBoundWriteFailureGrantsNothing: when the state directory, the
// state file or its rename cannot be written, Allow returns an error and no
// count, and leaves no state or temp file behind, so the round is not granted
// unrecorded.
func TestLocalRoundBoundWriteFailureGrantsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		fs   injectedFailFS
	}{
		{name: "mkdir", fs: injectedFailFS{failMkdir: true}},
		{name: "write", fs: injectedFailFS{failWrite: true}},
		{name: "rename", fs: injectedFailFS{failRename: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeDayClock{at: time.Date(2026, 11, 13, 9, 0, 0, 0, time.Local)}
			dir := filepath.Join(t.TempDir(), "local-round")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			b := Bound{Dir: dir, Now: clock.now, FS: tc.fs}

			n, err := b.Allow("gt-aaa")
			if err == nil {
				t.Fatalf("Allow = (%d, nil), want an error when the %s fails", n, tc.name)
			}
			if n != 0 {
				t.Fatalf("Allow returned count %d alongside the error, want 0", n)
			}
			inside, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(inside) != 0 {
				t.Fatalf("failed %s left %v in Dir, want nothing", tc.name, entryNames(inside))
			}
		})
	}
}

// entryNames lists the names of dir entries, for failure messages.
func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
