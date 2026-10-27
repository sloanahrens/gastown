package polecatimage

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleLock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		age     time.Duration
		missing bool
		want    bool
	}{
		{"fresh", 19 * time.Minute, false, false},
		{"exactly the threshold", 20 * time.Minute, false, false},
		{"a second past it", 20*time.Minute + time.Second, false, true},
		{"a day old", 24 * time.Hour, false, true},
		{"no directory", 0, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			if !tc.missing {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-tc.age))
			}
			if got := tr.StaleLock(tr.Cfg.LockDir); got != tc.want {
				t.Errorf("StaleLock() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClaimLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	if err := os.MkdirAll(filepath.Dir(tr.Cfg.LockDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := tr.ClaimLock(); err != nil {
		t.Fatalf("ClaimLock() failed: %v", err)
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "4242" {
		t.Errorf("lock records pid %q, want 4242", got)
	}
}

func TestClaimLockRefusesAHeldLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow)
	if err := tr.ClaimLock(); err == nil {
		t.Fatal("ClaimLock() took a lock another run holds")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("the held lock now records pid %q, want 9999", got)
	}
}

// TestTakeLock is a second build's answer to the lock: it takes a free or a
// stale one, and defers to a fresh one without touching it.
func TestTakeLock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// prepare plants the lock the run finds; pid "" plants no pid file.
		prepare func(t *testing.T, tr *testRun)
		took    bool
		// wantPID is the pid the lock records after the call, "" when it
		// records none.
		wantPID string
	}{
		{
			name:    "free",
			prepare: func(*testing.T, *testRun) {},
			took:    true,
			wantPID: "4242",
		},
		{
			name: "a fresh lock another run holds",
			prepare: func(t *testing.T, tr *testRun) {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow)
			},
			took:    false,
			wantPID: "9999",
		},
		{
			name: "a fresh lock with no pid file",
			prepare: func(t *testing.T, tr *testRun) {
				makeLock(t, tr.Cfg.LockDir, "", testNow)
			},
			took:    false,
			wantPID: "",
		},
		{
			name: "a stale lock",
			prepare: func(t *testing.T, tr *testRun) {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
			},
			took:    true,
			wantPID: "4242",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			t.Cleanup(tr.Cleanup)
			tc.prepare(t, tr)
			got, err := tr.TakeLock()
			if err != nil {
				t.Fatalf("TakeLock() error = %v, want none", err)
			}
			if got != tc.took {
				t.Fatalf("TakeLock() = %v, want %v", got, tc.took)
			}
			if tr.HaveLock != tc.took {
				t.Errorf("HaveLock = %v, want %v", tr.HaveLock, tc.took)
			}
			if !exists(tr.Cfg.LockDir) {
				t.Fatal("no lock directory after the call")
			}
			if got := lockPID(t, tr.Cfg.LockDir); got != tc.wantPID {
				t.Errorf("lock records pid %q, want %q", got, tc.wantPID)
			}
			if left := asideDirs(t, tr.Cfg.LockDir); len(left) != 0 {
				t.Errorf("takeover left %q behind", left)
			}
		})
	}
}

func TestTakeLockLogsTheTakeover(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	t.Cleanup(tr.Cleanup)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	if ok, err := tr.TakeLock(); !ok || err != nil {
		t.Fatalf("TakeLock() = %v, %v; want the stale lock taken over", ok, err)
	}
	tr.wantLogged(t, "taking over a stale lock ("+tr.Cfg.LockDir+")")
}

// TestTakeLockHoldsTheLockForTheRun is what the flock adds: a run that has
// taken the lock keeps it when its directory ages past StaleAfter, as a long
// build's does, and the next run takes over only once Cleanup has released it.
func TestTakeLockHoldsTheLockForTheRun(t *testing.T) {
	t.Parallel()
	held := newTestRun(t)
	makeLock(t, held.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	if ok, err := held.TakeLock(); !ok || err != nil {
		t.Fatalf("TakeLock() = %v, %v; want the stale lock taken over", ok, err)
	}
	t.Cleanup(held.Cleanup)
	if err := os.Chtimes(held.Cfg.LockDir, testNow.Add(-time.Hour), testNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	next := newTestRun(t)
	next.Cfg.LockDir = held.Cfg.LockDir
	next.Cfg.PID = 5151
	t.Cleanup(next.Cleanup)
	if ok, err := next.TakeLock(); ok || err != nil {
		t.Fatalf("TakeLock() = %v, %v; want a refusal while the first run holds the lock", ok, err)
	}
	if got := lockPID(t, held.Cfg.LockDir); got != "4242" {
		t.Errorf("lock records pid %q, want the first run's 4242", got)
	}
	held.Cleanup()
	if ok, err := next.TakeLock(); !ok || err != nil {
		t.Fatalf("TakeLock() after Cleanup = %v, %v; want the lock", ok, err)
	}
}

// TestTakeLockReportsAPathItCannotMake keeps a directory this run cannot make
// apart from a lock another run holds: it fails the run instead of reporting
// the lock as taken by another build.
func TestTakeLockReportsAPathItCannotMake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	blocked := filepath.Join(tr.root, "blocked")
	if err := os.WriteFile(blocked, []byte("a file where a directory belongs"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr.Cfg.LockDir = filepath.Join(blocked, "polecat-image.lock")
	line, err := tr.BuildLock()
	if err == nil {
		t.Fatal("BuildLock() error = nil, want the lock path it cannot make")
	}
	if line != "" {
		t.Errorf("BuildLock() = %q with an error, want no defer line", line)
	}
	tr.wantLogged(t, "ERROR: cannot take the lock "+tr.Cfg.LockDir)
}

// TestTakeLockClassifiesAClaimFailure is the claim's error told apart from
// another run's lock: only a directory already there is a run that holds it, so
// only that one is the script's "already running" line.
func TestTakeLockClassifiesAClaimFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		deferred bool
	}{
		{"a directory another run holds", &fs.PathError{Op: "mkdir", Path: "lock", Err: fs.ErrExist}, true},
		{"a directory it cannot make", os.ErrPermission, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.Run.claim = func() error { return tc.err }
			line, err := tr.BuildLock()
			if tc.deferred {
				if err != nil || line != "already running (lock "+tr.Cfg.LockDir+")" {
					t.Fatalf("BuildLock() = %q, %v; want the defer line for a held lock", line, err)
				}
				return
			}
			if err == nil || line != "" {
				t.Fatalf("BuildLock() = %q, %v; want the claim's error and no defer line", line, err)
			}
			tr.wantLogged(t, "ERROR: cannot take the lock "+tr.Cfg.LockDir)
		})
	}
}

// TestTakeLockYieldsToARunThatTookTheFreedPath covers the takeover's last race:
// a run that takes no flock — the script, until gt-tpg0p.7 — claimed the path
// between this run's rename and its claim. The other run holds the lock, so
// this one defers rather than failing.
func TestTakeLockYieldsToARunThatTookTheFreedPath(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	tr.Run.rename = func(oldpath, newpath string) error {
		if err := os.Rename(oldpath, newpath); err != nil {
			return err
		}
		makeLock(t, oldpath, "7777", testNow)
		return nil
	}
	ok, err := tr.TakeLock()
	if ok || err != nil {
		t.Fatalf("TakeLock() = %v, %v; want a refusal with no error", ok, err)
	}
	if tr.HaveLock {
		t.Error("HaveLock = true after the other run took the freed path")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "7777" {
		t.Errorf("lock records pid %q, want the other run's 7777", got)
	}
	if left := asideDirs(t, tr.Cfg.LockDir); len(left) != 0 {
		t.Errorf("takeover left the stale lock aside at %q", left)
	}
}

// TestBuildLock is a second build's answer at the level the script reports it:
// the run that takes the lock builds, and the one that defers names the holder
// in the script's "already running" line.
func TestBuildLock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, tr *testRun)
		refused bool
	}{
		{"free", func(*testing.T, *testRun) {}, false},
		{
			name: "a stale lock",
			prepare: func(t *testing.T, tr *testRun) {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
			},
		},
		{
			name: "a fresh lock another run holds",
			prepare: func(t *testing.T, tr *testRun) {
				makeLock(t, tr.Cfg.LockDir, "9999", testNow)
			},
			refused: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			t.Cleanup(tr.Cleanup)
			tc.prepare(t, tr)
			got, err := tr.BuildLock()
			if err != nil {
				t.Fatalf("BuildLock() error = %v, want none", err)
			}
			if !tc.refused {
				if got != "" {
					t.Fatalf("BuildLock() = %q, want empty: the run may build", got)
				}
				if !tr.HaveLock {
					t.Error("HaveLock = false after the run took the lock")
				}
				return
			}
			want := "already running (lock " + tr.Cfg.LockDir + ")"
			if got != want {
				t.Errorf("BuildLock() = %q, want %q", got, want)
			}
			if tr.HaveLock {
				t.Error("HaveLock = true after the run deferred")
			}
			if pid := lockPID(t, tr.Cfg.LockDir); pid != "9999" {
				t.Errorf("the held lock records pid %q, want the other run's 9999", pid)
			}
		})
	}
}

// TestTakeLockReportsARenameItCannotMake also checks the refusal releases the
// flock: the next run must not be blocked by a run that took nothing.
func TestTakeLockReportsARenameItCannotMake(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	tr.Run.rename = func(string, string) error { return os.ErrPermission }
	if ok, err := tr.TakeLock(); ok || err == nil {
		t.Fatalf("TakeLock() = %v, %v; want the rename's error", ok, err)
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("lock records pid %q, want the untouched 9999", got)
	}
	next := newTestRun(t)
	next.Cfg.LockDir = tr.Cfg.LockDir
	next.Cfg.PID = 5151
	t.Cleanup(next.Cleanup)
	if ok, err := next.TakeLock(); !ok || err != nil {
		t.Fatalf("TakeLock() by the next run = %v, %v; want the lock a refused run released", ok, err)
	}
}
