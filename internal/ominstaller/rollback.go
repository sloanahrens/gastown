package ominstaller

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// smokeFailed ends a run whose swapped om did not report the built commit. The
// previous om goes back over om, so the om in force is the one there was before the
// run. The run is rolled_back when that works. When it does not, a broken om may be
// in force, so the run is failed with rollback-failed and never reads as a rollback.
func (c Config) smokeFailed(commit, prev, msg string) (Result, Exit) {
	c.logf("[install-om] FAILED: %s", msg)
	if err := c.rollback(); err != nil {
		c.logf("[install-om] CRITICAL: rollback failed after %s: %v", msg, err)
		return Result{Event: eventFailed, Commit: commit, Prev: prev, Reason: reasonRollbackFailed}, ExitFailed
	}
	c.logf("[install-om] Rolled back to %s.", orNone(prev))
	return Result{Event: eventRolledBack, Commit: commit, Prev: prev, Reason: reasonSmokeFailed}, ExitFailed
}

// rollback puts the previous om back over the om at c.OMPath. With om.prev kept
// beside it, the copy goes back through swap, so the flag and rename rules hold on
// the way back. With no om.prev, a first install, there is no previous om, and the
// new om is removed instead.
func (c Config) rollback() error {
	prev := filepath.Join(filepath.Dir(c.OMPath), omPrevName)
	_, err := c.files().Stat(prev)
	if errors.Is(err, fs.ErrNotExist) {
		return c.removeNewBin()
	}
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", prev, err)
	}
	if err := c.swap(prev, c.OMPath); err != nil {
		// The swap's own error may carry a command's output, so only the step is named.
		return fmt.Errorf("cannot put %s back over %s", prev, c.OMPath)
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
