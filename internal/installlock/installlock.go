// Package installlock is the install lock scripts/install-gt.sh takes: an
// exclusive flock that lives exactly as long as the install (gt-1jgci).
//
// Run takes the lock and then replaces the calling process with the install
// command (syscall.Exec), with the lock fd inherited. The process holding the
// flock is therefore the install itself, and the kernel drops the lock when
// that process exits or dies. There is no helper left beside the install that
// could outlive it, or die while the install runs on without the lock.
//
// Because the fd is inherited, every descendant of the install inherits it too,
// not only the install itself. A descendant that outlives the install would
// hold the lock past it. scripts/install-gt.sh closes that where it can happen:
// git runs with auto maintenance off, and the image-build supervisor closes
// every inherited fd, so a detached build never holds the lock.
//
// Because Run replaces the process, the install's own exit status is the
// process's. The status Run itself returns that the caller treats specially is
// ExitLockBusy, and only when the wait ran out. Which statuses the install can
// exit with is stated in the header of scripts/install-gt.sh.
//
// Acquire takes the same lock for a caller that keeps running: the Go installer
// holds it from the binary swap to the end of the run (gt-tpg0p.13).
package installlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExitLockBusy is the status Run returns when the lock did not free within the
// wait. scripts/install-gt.sh maps it to its busy exit 3.
const ExitLockBusy = 75

// ErrBusy reports that the lock did not free within the wait.
var ErrBusy = errors.New("install lock stayed locked")

// retryInterval is how often a waiting acquisition retries the lock.
const retryInterval = 500 * time.Millisecond

// Acquire takes the exclusive lock on path, waiting up to wait, and returns the
// function that releases it. It returns ErrBusy when the wait ran out, and
// another error when the lock file cannot be opened or locked. The lock is held
// until the release runs or the process exits, so a caller that keeps running
// holds it across the work it guards rather than for the process's whole life.
func Acquire(path string, wait time.Duration) (release func(), err error) {
	return acquire(path, wait, time.Now, time.Sleep)
}

// acquire is Acquire with the wait's clock and sleep injected, so a unit test
// runs out a wait without spending it.
func acquire(path string, wait time.Duration, now func() time.Time, sleep func(time.Duration)) (func(), error) {
	f, err := acquireFile(path, wait, now, sleep)
	if err != nil {
		return nil, err
	}
	return func() { unlockAndClose(f) }, nil
}

// acquireFile takes the flock on path and returns the open lock file.
func acquireFile(path string, wait time.Duration, now func() time.Time, sleep func(time.Duration)) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// The wait is the runner's, bound to the same clock and sleep, so Acquire
	// and Run cannot drift into two different waits.
	locked, err := runner{now: now, sleep: sleep}.lock(f, wait)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !locked {
		_ = f.Close()
		return nil, ErrBusy
	}
	return f, nil
}

// unlockAndClose releases the flock before closing the file. flock belongs to
// the open file description, not to this descriptor, so a child another
// goroutine forked while the lock was held keeps the description, and the
// lock, alive until it execs; closing alone would leave the lock held after
// the release (gt-ubj9e, the same class as gt-0ph7q). LOCK_UN drops it for
// every holder of the description.
func unlockAndClose(f *os.File) {
	_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
	_ = f.Close()
}

// Config is one acquisition of the install lock and the command that replaces
// the process once it is held.
type Config struct {
	// LockPath is the file to flock; it is created when missing.
	LockPath string
	// Wait is how long to wait for the lock before returning ExitLockBusy.
	Wait time.Duration
	// Marker, when set, is the file Run removes just before it execs Command.
	// The caller creates it first and reads its presence after Run's process
	// ends to tell "Command never started" from "Command ran". If the exec
	// itself fails, Run puts the marker back.
	Marker string
	// Command is the argv that replaces the process while the lock is held.
	Command []string
	// Env is the environment Command starts with; nil means the process's.
	// INSTALL_GT_LOCKED=1 is always set, so a re-exec'd scripts/install-gt.sh
	// knows the lock is held and does not wait on itself.
	Env []string
	// Stderr receives Run's own messages; nil means os.Stderr.
	Stderr io.Writer
}

// Run takes the lock and execs cfg.Command. On success it does not return: the
// process is the command. It returns only the status the caller exits with when
// it could not get that far: ExitLockBusy when the wait ran out, or 1 when the
// command cannot be found or exec'd, or the lock cannot be opened or taken.
func Run(cfg Config) int {
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Env == nil {
		cfg.Env = os.Environ()
	}
	return runner{now: time.Now, sleep: time.Sleep, exec: syscall.Exec}.run(cfg)
}

// runner carries the seams. Unit tests replace now and sleep, so a timeout test
// costs no wall time, and exec, so the exec that ends the process can be
// observed instead of taken.
type runner struct {
	now   func() time.Time
	sleep func(time.Duration)
	exec  func(path string, argv, env []string) error
}

func (r runner) run(cfg Config) int {
	if len(cfg.Command) == 0 {
		fmt.Fprintln(cfg.Stderr, "install-gt: install-lock: no command given")
		return 1
	}
	path, err := exec.LookPath(cfg.Command[0])
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "install-gt: cannot find %s: %v\n", cfg.Command[0], err)
		return 1
	}

	f, err := os.OpenFile(cfg.LockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "install-gt: cannot open the lock file %s: %v\n", cfg.LockPath, err)
		return 1
	}
	// Reached on every return below; a successful exec never returns.
	defer f.Close()

	locked, err := r.lock(f, cfg.Wait)
	if err != nil {
		fmt.Fprintf(cfg.Stderr, "install-gt: cannot lock %s: %v\n", cfg.LockPath, err)
		return 1
	}
	if !locked {
		fmt.Fprintf(cfg.Stderr, "install-gt: %s stayed locked for %s\n", cfg.LockPath, cfg.Wait)
		return ExitLockBusy
	}

	// The fd was opened close-on-exec; the command has to inherit it, because
	// holding it is what keeps the lock.
	if _, err := unix.FcntlInt(f.Fd(), unix.F_SETFD, 0); err != nil {
		fmt.Fprintf(cfg.Stderr, "install-gt: cannot keep the lock across exec: %v\n", err)
		return 1
	}
	// A marker that cannot be removed would tell the caller the command never
	// started when it did, and the caller would print a second result over the
	// install's own. Refuse to start rather than break that.
	if cfg.Marker != "" {
		if err := os.Remove(cfg.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(cfg.Stderr, "install-gt: cannot remove the marker %s, so the install is not started: %v\n", cfg.Marker, err)
			return 1
		}
	}
	err = r.exec(path, cfg.Command, withLocked(cfg.Env))
	// exec returns only when it failed.
	fmt.Fprintf(cfg.Stderr, "install-gt: cannot exec %s: %v\n", cfg.Command[0], err)
	if cfg.Marker != "" {
		if err := os.WriteFile(cfg.Marker, nil, 0o644); err != nil {
			fmt.Fprintf(cfg.Stderr, "install-gt: cannot put the marker %s back, so the caller will not print its fallback result: %v\n", cfg.Marker, err)
		}
	}
	return 1
}

// lock tries for the exclusive flock on f until wait runs out, sleeping between
// tries on the runner's clock. It reports (false, nil) when the wait ran out
// first. Run and Acquire both wait through it, on the seams their caller
// supplied, so there is one wait loop (gt-5f05j).
func (r runner) lock(f *os.File, wait time.Duration) (bool, error) {
	deadline := r.now().Add(wait)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return false, err
		}
		if !r.now().Before(deadline) {
			return false, nil
		}
		pause := retryInterval
		if remaining := deadline.Sub(r.now()); remaining < pause {
			pause = remaining
		}
		r.sleep(pause)
	}
}

// withLocked returns env with INSTALL_GT_LOCKED=1 set, replacing any value it
// already carries.
func withLocked(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "INSTALL_GT_LOCKED=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "INSTALL_GT_LOCKED=1")
}
