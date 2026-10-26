// Package installlock is the install lock scripts/install-gt.sh takes: an
// exclusive flock that lives exactly as long as the install (gt-1jgci).
//
// Run takes the lock and then replaces the calling process with the install
// command (syscall.Exec), with the lock fd inherited. The process holding the
// flock is therefore the install itself, and the kernel drops the lock when
// that process exits or dies. There is no helper left beside the install that
// could outlive it, or die while the install runs on without the lock.
//
// Because the fd is inherited, anything the install starts inherits it too.
// scripts/install-gt.sh deals with that where it matters: git runs with auto
// maintenance off, and the image-build supervisor closes every inherited fd
// (see image_build), so a detached build never holds the lock past the install.
//
// Because Run replaces the process, the install's own exit status is the
// process's. The one status Run itself returns is ExitLockBusy, and that only
// when the wait ran out; scripts/install-gt.sh exits only 0 to 3, so the two
// cannot be confused.
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

// retryInterval is how often a waiting Run retries the lock.
const retryInterval = 500 * time.Millisecond

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
	if cfg.Marker != "" {
		_ = os.Remove(cfg.Marker)
	}
	err = r.exec(path, cfg.Command, withLocked(cfg.Env))
	// exec returns only when it failed.
	fmt.Fprintf(cfg.Stderr, "install-gt: cannot exec %s: %v\n", cfg.Command[0], err)
	if cfg.Marker != "" {
		_ = os.WriteFile(cfg.Marker, nil, 0o644)
	}
	return 1
}

// lock tries for the exclusive flock on f until wait runs out. It reports
// (false, nil) when the wait ran out first.
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
