package forgejoprobe

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// Finisher is what the probe's exit touches. Finish calls it in the order the
// script's exit trap ran its cleanup.
type Finisher interface {
	// Cleanup removes the worktree a push added from the rig's repository.
	Cleanup() error
	// BranchCreated reports whether the push created the probe branch.
	BranchCreated() bool
	// Branch is the probe branch, and Repo the Forgejo copy it lives on.
	Branch() string
	Repo() string
	// DeleteBranch deletes Branch on Repo through the Forgejo API.
	DeleteBranch() error
	// Bare is the rig's own repository, named when the hooks did not run.
	Bare() string
	// Record writes the green record and returns its path.
	Record() (string, error)
	// RecordPath is the path of the rig's record, named when it cannot be removed.
	RecordPath() string
	// RemoveRecord removes the rig's record left by an earlier run. A record
	// that is already absent is not an error.
	RemoveRecord() error
	// RemoveWorkDir removes the probe's temporary directory.
	RemoveWorkDir() error
	// WorkDir is the probe's temporary directory, named when it cannot be removed.
	WorkDir() string
	// Logf writes one line of the verdict.
	Logf(format string, args ...any)
}

// Finish ends a probe run and returns its exit code. rc is the code the run has
// so far. A worktree that will not come off, a branch that will not be deleted
// or a record that will not be written fails the run, since a branch left
// behind would be found by every later run. A red verdict exits ExitFailure
// whatever rc says. A green run whose push did not run the rig's hooks exits
// ExitHooksNotExercised, not ExitOK. Only a run that proved green writes a
// record; every other outcome removes the record an earlier run left, so the
// cutover never reads a stale green.
func Finish(f Finisher, green bool, mode PushMode, rc int) int {
	if err := f.Cleanup(); err != nil {
		rc = ExitFailure
	}
	if f.BranchCreated() {
		rc = finishBranch(f, rc)
	}
	recorded := false
	if green && rc == ExitOK {
		rc, recorded = finishGreen(f, mode)
	}
	if !green && rc == ExitOK {
		rc = ExitFailure
	}
	if !recorded {
		rc = removeStaleRecord(f, rc)
	}
	if err := f.RemoveWorkDir(); err != nil {
		// It stays a warning, and not a failed run, because Cleanup already
		// removed the worktree's registration in the rig's repository before this
		// runs, and a Cleanup failure has already failed the run. What is left is
		// the copy, which holds no credential, so the path is named for removal by hand.
		f.Logf("warning: could not remove the probe's work directory %s (%v); remove it by hand", f.WorkDir(), err)
	}
	return rc
}

// finishBranch deletes the probe branch. A branch that is already gone counts as
// deleted, since the run's goal is that it is not there.
func finishBranch(f Finisher, rc int) int {
	err := f.DeleteBranch()
	var api *forgejo.APIError
	switch {
	case err == nil:
		f.Logf("deleted branch %s on %s", f.Branch(), f.Repo())
	case errors.As(err, &api) && api.StatusCode == http.StatusNotFound:
		f.Logf("branch %s on %s was already gone", f.Branch(), f.Repo())
	default:
		f.Logf("warning: could not delete branch %s on %s (%v)", f.Branch(), f.Repo(), err)
		return ExitFailure
	}
	return rc
}

// finishGreen writes the green record only when the rig's hooks ran: the cutover
// reads the record as the proof that they did. It reports whether the record
// was written.
func finishGreen(f Finisher, mode PushMode) (int, bool) {
	if !mode.HooksExercised() {
		f.Logf("the probe is green, but the rig's hooks were NOT exercised: no rig repository at %s, so no record was written", f.Bare())
		return ExitHooksNotExercised, false
	}
	path, err := f.Record()
	if err != nil {
		f.Logf("%v", err)
		return ExitFailure, false
	}
	f.Logf("recorded the green probe in %s", path)
	return ExitOK, true
}

// removeStaleRecord removes the record an earlier run left, since this run did not
// write one. A removal that fails is named by its path and fails the run whatever
// rc was: the stale record would still be there for the cutover to read, and a
// refused command line's exit 2 would not say so.
func removeStaleRecord(f Finisher, rc int) int {
	err := f.RemoveRecord()
	if err == nil {
		return rc
	}
	f.Logf("warning: could not remove the stale probe record %s (%v)", f.RecordPath(), err)
	return ExitFailure
}

// DryRunPlan is what a dry run would do, named by the values the run would use.
type DryRunPlan struct {
	Main    string        // the branch the copy is taken from
	SHA     string        // the commit the probe would push
	GitURL  string        // the Forgejo remote the branch would be pushed to
	Branch  string        // the probe branch
	Bare    string        // the rig's own repository
	RigRepo bool          // whether Bare is present
	Repo    string        // OWNER/NAME of the Forgejo copy
	Context string        // the status context the probe would wait for
	Timeout time.Duration // how long the probe would wait
}

// DryRunLines returns the lines a dry run writes to stderr, in order. They
// describe every step, and a dry run performs none of them.
func DryRunLines(p DryRunPlan) []string {
	lines := []string{fmt.Sprintf("would push %s (%s) to %s as %s", p.Main, p.SHA, p.GitURL, p.Branch)}
	if p.RigRepo {
		lines = append(lines, fmt.Sprintf("would push from a throwaway worktree of %s, so the rig's hooks run, and remove the worktree afterwards", p.Bare))
	} else {
		lines = append(lines, fmt.Sprintf("warning: no rig repository at %s, so the probe would push from the clone and the rig's hooks would not be exercised", p.Bare))
	}
	return append(lines,
		fmt.Sprintf("would wait up to %ds for %s on %s", int64(p.Timeout/time.Second), p.Context, p.SHA),
		fmt.Sprintf("would delete branch %s on %s", p.Branch, p.Repo),
		"dry run: no push and no deletion was sent",
	)
}
