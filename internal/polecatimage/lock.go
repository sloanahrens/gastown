package polecatimage

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	return os.WriteFile(filepath.Join(r.Cfg.LockDir, "pid"), pid, 0o644)
}

// TakeLock takes the lock and reports whether it did, taking over one a dead run
// left behind.
//
// A takeover renames the stale directory aside first, so of two runs that both
// saw it stale only one rename succeeds; it then re-checks the directory it
// moved, because one that turns out fresh was another run's new lock and goes
// back.
func (r *Run) TakeLock() bool {
	_ = os.MkdirAll(filepath.Dir(r.Cfg.LockDir), 0o755)
	if r.ClaimLock() == nil {
		r.HaveLock = true
		return true
	}
	if !r.StaleLock(r.Cfg.LockDir) {
		return false
	}
	aside := r.Cfg.LockDir + ".stale." + strconv.Itoa(r.pid())
	if err := r.renamer()(r.Cfg.LockDir, aside); err != nil {
		return false
	}
	if !r.StaleLock(aside) {
		if !exists(r.Cfg.LockDir) {
			_ = r.renamer()(aside, r.Cfg.LockDir)
		}
		_ = os.RemoveAll(aside)
		return false
	}
	r.Logf("taking over a stale lock (%s)", r.Cfg.LockDir)
	_ = os.RemoveAll(aside)
	if r.ClaimLock() != nil {
		return false
	}
	r.HaveLock = true
	return true
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
