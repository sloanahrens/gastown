package installlock

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeClock is the injected clock: Sleep advances it, so waiting out the lock
// costs no wall time. onSleep, when set, runs on each Sleep.
type fakeClock struct {
	now     time.Time
	slept   []time.Duration
	onSleep func(n int)
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(0, 0)} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.slept))
	}
}

func (c *fakeClock) total() time.Duration {
	var sum time.Duration
	for _, d := range c.slept {
		sum += d
	}
	return sum
}

// execCall is one observed exec.
type execCall struct {
	path string
	argv []string
	env  []string
}

// recorder is the exec seam: it records the call instead of replacing the test
// process, and returns err to simulate an exec that failed. before, when set,
// runs first and sees the state at the moment of the exec.
type recorder struct {
	calls  []execCall
	err    error
	before func()
}

func (r *recorder) exec(path string, argv, env []string) error {
	if r.before != nil {
		r.before()
	}
	r.calls = append(r.calls, execCall{path, argv, env})
	return r.err
}

func newRunner(c *fakeClock, r *recorder) runner {
	return runner{now: c.Now, sleep: c.Sleep, exec: r.exec}
}

// self is a command that exists on every host: the test binary.
func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("test binary path: %v", err)
	}
	return exe
}

// holdLock takes the exclusive flock on path through its own open file, as a
// second install would find it held, and returns the function that lets go.
// Letting go is an explicit LOCK_UN, not a close: a process another test forks
// at that moment holds a copy of the descriptor until it execs, and a close
// would not free the lock until then.
func holdLock(t *testing.T, path string) (release func()) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { f.Close() })
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("hold %s: %v", path, err)
	}
	return func() {
		if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
			t.Errorf("release %s: %v", path, err)
		}
	}
}

// lockIsFree reports whether the flock on path can be taken right now.
func lockIsFree(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("probe %s: %v", path, err)
	}
	return false
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestSecondHolderTimesOutWithExitLockBusy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, marker := filepath.Join(dir, "install-gt.lock"), filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	holdLock(t, lock)

	clock, rec := newFakeClock(), &recorder{}
	var stderr strings.Builder
	got := newRunner(clock, rec).run(Config{
		LockPath: lock, Wait: 2 * time.Second, Marker: marker,
		Command: []string{self(t)}, Stderr: &stderr,
	})

	if got != ExitLockBusy {
		t.Fatalf("a second holder got status %d, want %d (stderr: %s)", got, ExitLockBusy, stderr.String())
	}
	if len(rec.calls) != 0 {
		t.Errorf("the command was exec'd without the lock: %v", rec.calls)
	}
	if clock.total() != 2*time.Second {
		t.Errorf("waited %s over %v, want exactly the 2s wait", clock.total(), clock.slept)
	}
	for _, d := range clock.slept {
		if d > retryInterval {
			t.Errorf("slept %s in one go, want at most %s between tries", d, retryInterval)
		}
	}
	if !fileExists(marker) {
		t.Error("the marker was removed though the command never started")
	}
}

func TestWaitEndsWhenTheHolderLetsGo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "install-gt.lock")
	release := holdLock(t, lock)

	clock, rec := newFakeClock(), &recorder{err: errors.New("stop here")}
	clock.onSleep = func(n int) {
		if n == 2 {
			release() // the other install finishes during the second pause
		}
	}
	got := newRunner(clock, rec).run(Config{
		LockPath: lock, Wait: 10 * time.Second, Command: []string{self(t)}, Stderr: io.Discard,
	})

	if len(rec.calls) != 1 {
		t.Fatalf("the command was exec'd %d times after the holder let go (status %d, slept %v), want 1", len(rec.calls), got, clock.slept)
	}
	if len(clock.slept) != 2 {
		t.Errorf("slept %v, want two pauses and then the lock", clock.slept)
	}
}

func TestExecGetsTheCommandAndTheLockedEnvironment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "install-gt.lock")
	rec := &recorder{err: errors.New("stop here")}
	cmd := []string{self(t), "--sha", "abc"}

	newRunner(newFakeClock(), rec).run(Config{
		LockPath: lock, Command: cmd, Stderr: io.Discard,
		Env: []string{"PATH=/bin", "INSTALL_GT_LOCKED=0", "HOME=/h"},
	})

	if len(rec.calls) != 1 {
		t.Fatalf("exec called %d times, want 1", len(rec.calls))
	}
	call := rec.calls[0]
	if call.path != cmd[0] {
		t.Errorf("exec path %q, want %q", call.path, cmd[0])
	}
	if strings.Join(call.argv, " ") != strings.Join(cmd, " ") {
		t.Errorf("exec argv %v, want %v", call.argv, cmd)
	}
	var locked []string
	for _, kv := range call.env {
		if strings.HasPrefix(kv, "INSTALL_GT_LOCKED=") {
			locked = append(locked, kv)
		}
	}
	if len(locked) != 1 || locked[0] != "INSTALL_GT_LOCKED=1" {
		t.Errorf("INSTALL_GT_LOCKED in the exec environment = %v, want exactly INSTALL_GT_LOCKED=1", locked)
	}
	if !contains(call.env, "PATH=/bin") || !contains(call.env, "HOME=/h") {
		t.Errorf("the rest of the environment was dropped: %v", call.env)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// At the moment of the exec the lock must be held through an fd the command
// will inherit: open, not close-on-exec, and the file the lock is on.
func TestLockFdIsHeldAndInheritableAtExec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "install-gt.lock")
	var lockStat unix.Stat_t
	rec := &recorder{err: errors.New("stop here")}
	rec.before = func() {
		if err := unix.Stat(lock, &lockStat); err != nil {
			t.Errorf("stat lock at exec: %v", err)
			return
		}
		inherited, held := 0, !lockIsFree(t, lock)
		for fd := 3; fd < 256; fd++ {
			var st unix.Stat_t
			if unix.Fstat(fd, &st) != nil || st.Dev != lockStat.Dev || st.Ino != lockStat.Ino {
				continue
			}
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err == nil && flags&unix.FD_CLOEXEC == 0 {
				inherited++
			}
		}
		if inherited != 1 {
			t.Errorf("%d inheritable descriptors on the lock file at exec, want exactly 1", inherited)
		}
		if !held {
			t.Error("the lock was not held at exec")
		}
	}

	newRunner(newFakeClock(), rec).run(Config{LockPath: lock, Command: []string{self(t)}, Stderr: io.Discard})

	if len(rec.calls) != 1 {
		t.Fatalf("exec called %d times, want 1", len(rec.calls))
	}
}

func TestMarkerGoesBeforeExecAndComesBackWhenExecFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock, marker := filepath.Join(dir, "install-gt.lock"), filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{err: errors.New("exec format error")}
	var markerAtExec bool
	rec.before = func() { markerAtExec = fileExists(marker) }
	var stderr strings.Builder

	got := newRunner(newFakeClock(), rec).run(Config{
		LockPath: lock, Marker: marker, Command: []string{self(t)}, Stderr: &stderr,
	})

	if markerAtExec {
		t.Error("the marker still existed when exec ran: a command that started would be reported as one that never did")
	}
	if got != 1 {
		t.Errorf("status %d after a failed exec, want 1", got)
	}
	if !fileExists(marker) {
		t.Error("the marker was not put back after the exec failed, so the caller would not print its fallback result")
	}
	if !strings.Contains(stderr.String(), "exec format error") {
		t.Errorf("stderr %q does not name the exec failure", stderr.String())
	}
}

func TestMarkerThatCannotBeRemovedStopsTheInstallBeforeExec(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "install-gt.lock")
	// A directory with something in it cannot be removed as a file.
	marker := filepath.Join(dir, "marker")
	if err := os.MkdirAll(filepath.Join(marker, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	var stderr strings.Builder

	got := newRunner(newFakeClock(), rec).run(Config{
		LockPath: lock, Marker: marker, Command: []string{self(t)}, Stderr: &stderr,
	})

	if got != 1 {
		t.Errorf("status %d, want 1", got)
	}
	if len(rec.calls) != 0 {
		t.Errorf("the install was started with a marker it could not clear: %v", rec.calls)
	}
	if !strings.Contains(stderr.String(), "cannot remove the marker") {
		t.Errorf("stderr %q does not say the marker could not be removed", stderr.String())
	}
}

// A marker the caller already cleared, or one another run removed, must not be
// read as a removal that failed: the install still starts (gt-trrf6).
func TestMarkerThatIsAlreadyGoneLetsTheInstallStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker") // never created
	rec := &recorder{err: errors.New("stop here")}
	var markerAtExec bool
	rec.before = func() { markerAtExec = fileExists(marker) }
	var stderr strings.Builder

	got := newRunner(newFakeClock(), rec).run(Config{
		LockPath: filepath.Join(dir, "install-gt.lock"), Marker: marker,
		Command: []string{self(t)}, Stderr: &stderr,
	})

	if len(rec.calls) != 1 {
		t.Fatalf("the install was not started for a marker that was already gone (status %d, stderr: %s)", got, stderr.String())
	}
	if strings.Contains(stderr.String(), "cannot remove the marker") {
		t.Errorf("stderr %q reports a marker removal that failed only because the marker was gone", stderr.String())
	}
	if markerAtExec {
		t.Error("a marker appeared at exec that the run did not create")
	}
}

func TestMarkerThatCannotBePutBackAfterAFailedExecIsReported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	markerDir := filepath.Join(dir, "markers")
	if err := os.Mkdir(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(markerDir, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{err: errors.New("exec format error")}
	// The directory goes between the removal and the exec failing, so the
	// marker has nowhere to go back to.
	rec.before = func() {
		if err := os.RemoveAll(markerDir); err != nil {
			t.Errorf("remove %s: %v", markerDir, err)
		}
	}
	var stderr strings.Builder

	got := newRunner(newFakeClock(), rec).run(Config{
		LockPath: filepath.Join(dir, "install-gt.lock"), Marker: marker, Command: []string{self(t)}, Stderr: &stderr,
	})

	if got != 1 {
		t.Errorf("status %d, want 1", got)
	}
	if !strings.Contains(stderr.String(), "cannot put the marker") {
		t.Errorf("stderr %q does not say the marker could not be put back", stderr.String())
	}
}

func TestUnusableInputsRunNothingAndLeaveTheMarker(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no command", Config{LockPath: filepath.Join(dir, "a.lock"), Marker: marker}, "no command"},
		{"missing command", Config{LockPath: filepath.Join(dir, "b.lock"), Marker: marker, Command: []string{"no-such-install-command"}}, "cannot find"},
		{"unopenable lock", Config{LockPath: filepath.Join(dir, "missing-dir", "c.lock"), Marker: marker, Command: []string{self(t)}}, "cannot open the lock file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			var stderr strings.Builder
			tc.cfg.Stderr = &stderr

			got := newRunner(newFakeClock(), rec).run(tc.cfg)

			if got != 1 {
				t.Errorf("status %d, want 1", got)
			}
			if len(rec.calls) != 0 {
				t.Errorf("exec called: %v", rec.calls)
			}
			if !fileExists(marker) {
				t.Error("the marker was removed though nothing started")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tc.want)
			}
		})
	}
}

// A command that cannot be found must not leave a lock file behind or take the
// lock first.
func TestMissingCommandIsFoundBeforeTheLockIsTaken(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "install-gt.lock")

	newRunner(newFakeClock(), &recorder{}).run(Config{LockPath: lock, Command: []string{"no-such-install-command"}, Stderr: io.Discard})

	if fileExists(lock) {
		t.Error("the lock file was created for a command that cannot run")
	}
}
