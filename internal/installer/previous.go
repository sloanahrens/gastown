package installer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errNoPrevious is a rollback with nothing to restore.
var errNoPrevious = errors.New("install-gt: no gt.prev to roll back to")

// KeepPrevious is the script's "Keep the previous binary": gt.prev becomes a
// copy of the installed binary, the one a rollback restores.
func (r *Run) KeepPrevious() error {
	gt := r.Paths.GT()
	if !r.Files.isFile(gt) {
		return nil
	}
	// The pid keeps two runs from writing one temporary file; the rename leaves
	// gt.prev as either the old copy or the new one, never a partial write.
	tmp := filepath.Join(r.Paths.BinDir, fmt.Sprintf(".gt.prev.%d", os.Getpid()))
	if err := r.Files.copy(tmp, gt); err != nil {
		return err
	}
	if err := r.Files.rename(tmp, r.Paths.Prev()); err != nil {
		_ = r.Files.remove(tmp)
		return err
	}
	return nil
}

// Rollback puts gt.prev back over the installed binary atomically. An
// OS-immutable binary cannot be renamed over until its flag is off, and the
// restored file gets the flag back — whether the rename worked or not, so a
// failed rollback still leaves the broken binary protected (gt-vya0s).
func (r *Run) Rollback() error {
	prev := r.Paths.Prev()
	if !r.Files.isFile(prev) {
		return errNoPrevious
	}
	gt := r.Paths.GT()
	tmp := filepath.Join(r.Paths.BinDir, fmt.Sprintf(".gt.rollback.%d", os.Getpid()))
	if err := r.Files.copy(tmp, prev); err != nil {
		return err
	}
	r.Files.clearImmutable(gt)
	if err := r.Files.rename(tmp, gt); err != nil {
		_ = r.Files.remove(tmp)
		r.Files.setImmutable(gt)
		return err
	}
	r.Files.setImmutable(gt)
	return nil
}

// FailInstall is the script's fail_install(): it logs the failure, puts the
// previous binary back when the installed file changed, escalates, leaves the
// receipt and ends the run failed. reason is the receipt's code, event the
// result's, fingerprint the escalation's, and severity the caller's: "medium"
// for a failed build, "high" for a failed smoke.
func (r *Run) FailInstall(reason, message, severity, fingerprint, event string) Exit {
	r.C.Log("FAILED: " + message)
	prev := r.Paths.Prev()
	if r.Files.isFile(prev) && !r.Files.sameContent(prev, r.Paths.GT()) {
		if err := r.Rollback(); err == nil {
			r.C.Log("Rolled back to " + r.C.Prev + ".")
		} else {
			r.C.Escalate("critical", "install-gt:rollback-failed",
				message+"; and restoring "+prev+" FAILED — the town may be running a broken gt")
			r.C.Receipt("failed", r.C.Expected, r.C.Prev, "rollback-failed")
			return r.C.Finish(Result{Event: "failed", Commit: r.C.Expected, Prev: r.C.Prev, Reason: "rollback-failed"}, ExitFailed)
		}
	}
	r.C.Escalate(severity, fingerprint, message)
	r.C.Receipt(event, r.C.Expected, r.C.Prev, reason)
	return r.C.Finish(Result{Event: event, Commit: r.C.Expected, Prev: r.C.Prev, Reason: reason}, ExitFailed)
}
