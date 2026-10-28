package polecatimage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// ErrLockHeld reports that another run holds the plugin lock: a caller that
// wanted it for its own work defers rather than waiting for a build to finish.
var ErrLockHeld = errors.New("polecat-image lock is held")

// WithLockOptions carries the two seams a caller outside a plugin run answers:
// where the run's log lines go, and the clock that measures a lock's age. Its
// zero value gives a plugin run's own defaults.
type WithLockOptions struct {
	// Out receives the run's log lines, such as a stale lock's takeover; nil
	// sends them to os.Stdout, as a plugin run does.
	Out io.Writer
	// Now is the clock a lock's age is measured by; nil means time.Now.
	Now func() time.Time
}

// WithLock runs fn while holding the plugin lock — the one every build and push
// holds — and releases it when fn returns, also when fn fails.
//
// A lock another run holds returns ErrLockHeld without running fn, and one a
// dead run left behind is taken over as a plugin run takes it. Any other error
// is fn's own, or the lock's when it could not be read or made.
func WithLock(ctx context.Context, cfg Config, opts WithLockOptions, fn func(context.Context) error) error {
	r := &Run{Cfg: cfg, out: opts.Out, now: opts.Now}
	took, err := r.TakeLock()
	if err != nil {
		return fmt.Errorf("take the plugin lock %s: %w", cfg.LockDir, err)
	}
	if !took {
		return ErrLockHeld
	}
	defer r.Cleanup()
	return fn(ctx)
}

// StaleLock reports whether the directory at dir has gone unmodified for longer
// than StaleAfter, so it belongs to a run that died.
func (r *Run) StaleLock(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return r.clock().Sub(fi.ModTime()) > r.Cfg.StaleAfter
}

// ClaimLock makes the lock directory and records this run's pid in it.
func (r *Run) ClaimLock() error {
	if err := os.Mkdir(r.Cfg.LockDir, 0o755); err != nil {
		return err
	}
	pid := []byte(strconv.Itoa(r.pid()) + "\n")
	if err := r.pidWriter()(filepath.Join(r.Cfg.LockDir, "pid"), pid); err != nil {
		// A directory with no pid file would block the next run until it aged
		// out with nothing to show for it.
		_ = os.Remove(r.Cfg.LockDir)
		return err
	}
	return nil
}

// TakeLock takes the lock and reports whether it did, taking over one a dead run
// left behind; an error reports a lock it could not read or make.
//
// The flock beside the directory is what makes the state changes of a run alone
// in them: every claim and takeover holds it first, and a run that takes the
// lock keeps it until Cleanup. A stale read and the rename it leads to cannot
// then interleave with another run's, which is how two runs can otherwise move a
// newer run's directory aside and each believe they hold the lock (gt-tpg0p.5).
func (r *Run) TakeLock() (bool, error) {
	r.HaveLock = false
	if err := os.MkdirAll(filepath.Dir(r.Cfg.LockDir), 0o755); err != nil {
		return false, err
	}
	unlock, err := lock.FlockTryAcquireStable(r.Cfg.LockDir + ".flock")
	if err != nil {
		if errors.Is(err, lock.ErrFlockHeld) {
			return false, nil
		}
		return false, err
	}
	if err := r.claimOrTakeOver(); err != nil || !r.HaveLock {
		unlock()
		return r.HaveLock, err
	}
	r.unlock = unlock
	return true, nil
}

// claimOrTakeOver claims LockDir under the flock, or replaces the directory
// there when it has gone stale, and records in HaveLock whether the run holds
// the lock afterwards.
func (r *Run) claimOrTakeOver() error {
	claim := r.claimer()
	err := claim()
	if err == nil {
		r.HaveLock = true
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return err
	}
	if !r.StaleLock(r.Cfg.LockDir) {
		return nil
	}
	aside := r.Cfg.LockDir + ".stale." + strconv.Itoa(r.pid())
	if err := r.renamer()(r.Cfg.LockDir, aside); err != nil {
		return err
	}
	r.Logf("taking over a stale lock (%s)", r.Cfg.LockDir)
	// The stale directory is off the lock path already, so failing to delete it
	// leaves rubbish rather than a lock, and must not stop this run's claim.
	if err := os.RemoveAll(aside); err != nil {
		r.Logf("cannot remove the stale lock directory (%s): %v", aside, err)
	}
	if err := claim(); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// A run that takes no flock — the script, until gt-tpg0p.7 makes it
			// a shim — claimed the freed path first. That run holds the lock.
			return nil
		}
		return err
	}
	r.HaveLock = true
	return nil
}

// BuildLock takes the lock a build runs under: "" when this run may build, the
// script's line when another run holds the lock, and an error (already logged)
// when the lock cannot be read or made.
func (r *Run) BuildLock() (string, error) {
	took, err := r.TakeLock()
	if err != nil {
		return "", r.failf("cannot take the lock %s: %v", r.Cfg.LockDir, err)
	}
	if took {
		return "", nil
	}
	return "already running (lock " + r.Cfg.LockDir + ")", nil
}

// LockHeld reports whether the lock names this run's pid, which is the only
// lock Cleanup may release.
func (r *Run) LockHeld() bool {
	pid, err := os.ReadFile(filepath.Join(r.Cfg.LockDir, "pid"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(pid)) == strconv.Itoa(r.pid())
}
