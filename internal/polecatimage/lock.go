package polecatimage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/lock"
)

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
