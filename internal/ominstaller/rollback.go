package ominstaller

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// smokeFailed ends a run whose swapped om did not report the built commit. hadPrevious
// is what keepPrevious found: true when this run kept the om it replaced as om.prev.
// The rollback follows it: the kept om goes back over om, or, on a first install, the
// new om is removed. The run is rolled_back when that works. When it does not, a
// broken om may be in force, so the run is failed with rollback-failed and never
// reads as a rollback.
func (c Config) smokeFailed(commit, prev string, hadPrevious bool, msg string) (Result, Exit) {
	c.logf("[install-om] FAILED: %s", msg)
	if err := c.rollback(hadPrevious); err != nil {
		c.logf("[install-om] CRITICAL: rollback failed after %s: %v", msg, err)
		return Result{Event: eventFailed, Commit: commit, Prev: prev, Reason: reasonRollbackFailed}, ExitFailed
	}
	c.logf("[install-om] Rolled back to %s.", orNone(prev))
	return Result{Event: eventRolledBack, Commit: commit, Prev: prev, Reason: reasonSmokeFailed}, ExitFailed
}

// rollback undoes the swap. When this run kept the previous om (hadPrevious), that
// copy goes back over om through swap, so the flag and rename rules hold on the way
// back. When it did not, a first install, there is no previous om from this run, and
// the new om is removed. An om.prev left by an earlier install is never read here,
// so it cannot be swapped in as this run's rollback target.
func (c Config) rollback(hadPrevious bool) error {
	if !hadPrevious {
		return c.removeNewBin()
	}
	prev := filepath.Join(filepath.Dir(c.OMPath), omPrevName)
	if err := c.swap(prev, c.OMPath); err != nil {
		return fmt.Errorf("cannot put %s back over %s: %w", prev, c.OMPath, err)
	}
	return nil
}

// removeNewBin removes the new om a first install put in place. A missing om is
// already gone, so it is not an error. Any other failure is returned, so the run
// reports it rather than reading the broken om as rolled back.
func (c Config) removeNewBin() error {
	err := c.files().Remove(c.OMPath)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("cannot remove the new om %s: %w", c.OMPath, err)
}
