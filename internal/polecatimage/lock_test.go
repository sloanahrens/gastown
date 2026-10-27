package polecatimage

import (
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
			tc.prepare(t, tr)
			if got := tr.TakeLock(); got != tc.took {
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
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	if !tr.TakeLock() {
		t.Fatal("TakeLock() did not take over a stale lock")
	}
	tr.wantLogged(t, "taking over a stale lock ("+tr.Cfg.LockDir+")")
}

// TestTakeLockNeverDeletesAnotherRunsFreshLock drives the takeover's re-check:
// the directory the rename moved turns out to be a lock another run made fresh
// after this run read the old one as stale, and a third run then took the path,
// so the fresh lock stays aside for its owner rather than being removed.
func TestTakeLockNeverDeletesAnotherRunsFreshLock(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	tr.Run.rename = func(oldpath, newpath string) error {
		// The other run replaced the stale lock with its own between this run's
		// stale read and its rename, so the rename moves that run's lock.
		if err := os.RemoveAll(oldpath); err != nil {
			return err
		}
		makeLock(t, newpath, "9999", testNow)
		// A third run claimed the real path while the other run's lock was
		// aside.
		makeLock(t, oldpath, "7777", testNow)
		return nil
	}
	if tr.TakeLock() {
		t.Fatal("TakeLock() took a lock that turned out fresh")
	}
	if tr.HaveLock {
		t.Error("HaveLock = true after a refused takeover")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "7777" {
		t.Errorf("lock records pid %q, want the third run's 7777", got)
	}
	left := asideDirs(t, tr.Cfg.LockDir)
	if len(left) != 1 {
		t.Fatalf("takeover left %q aside, want the other run's fresh lock kept in one", left)
	}
	if got := lockPID(t, left[0]); got != "9999" {
		t.Errorf("the lock left aside records pid %q, want the other run's 9999", got)
	}
	tr.wantLogged(t, "left in place")
}

// TestTakeLockKeepsAFreshLockItCannotPutBack is the same re-check when the real
// path is free but the rename back fails: the fresh lock stays aside, because
// removing it would release a lock its owner still believes it holds.
func TestTakeLockKeepsAFreshLockItCannotPutBack(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	calls := 0
	tr.Run.rename = func(oldpath, newpath string) error {
		calls++
		if calls > 1 {
			return os.ErrPermission
		}
		if err := os.RemoveAll(oldpath); err != nil {
			return err
		}
		makeLock(t, newpath, "9999", testNow)
		return nil
	}
	if tr.TakeLock() {
		t.Fatal("TakeLock() took a lock that turned out fresh")
	}
	left := asideDirs(t, tr.Cfg.LockDir)
	if len(left) != 1 {
		t.Fatalf("takeover left %q aside, want the fresh lock kept in one", left)
	}
	if got := lockPID(t, left[0]); got != "9999" {
		t.Errorf("the lock left aside records pid %q, want 9999", got)
	}
	tr.wantLogged(t, "could not be put back")
}

// TestTakeLockPutsAFreshLockBack drives the other half of the re-check: the
// directory moved aside was fresh and no lock replaced it, so it goes back.
func TestTakeLockPutsAFreshLockBack(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	tr.Run.rename = func(oldpath, newpath string) error {
		if err := os.RemoveAll(oldpath); err != nil {
			return err
		}
		makeLock(t, newpath, "9999", testNow)
		return nil
	}
	if tr.TakeLock() {
		t.Fatal("TakeLock() took a lock that turned out fresh")
	}
	if !exists(tr.Cfg.LockDir) {
		t.Fatal("the fresh lock was not put back")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("lock records pid %q, want 9999", got)
	}
	if left := asideDirs(t, tr.Cfg.LockDir); len(left) != 0 {
		t.Errorf("takeover left %q behind", left)
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
			tc.prepare(t, tr)
			got := tr.BuildLock()
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

func TestTakeLockRefusesWhenTheRenameFails(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	makeLock(t, tr.Cfg.LockDir, "9999", testNow.Add(-21*time.Minute))
	tr.Run.rename = func(string, string) error { return os.ErrPermission }
	if tr.TakeLock() {
		t.Fatal("TakeLock() took a lock it could not rename aside")
	}
	if got := lockPID(t, tr.Cfg.LockDir); got != "9999" {
		t.Errorf("lock records pid %q, want the untouched 9999", got)
	}
}
