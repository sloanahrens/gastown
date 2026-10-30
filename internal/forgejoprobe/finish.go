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
	// RemoveWorkDir removes the probe's temporary directory.
	RemoveWorkDir() error
	// Logf writes one line of the verdict.
	Logf(format string, args ...any)
}

// Finish ends a probe run and returns its exit code. rc is the code the run has
// so far. A worktree that will not come off, a branch that will not be deleted
// or a record that will not be written fails the run, since a branch left
// behind would be found by every later run. A green run whose push did not run
// the rig's hooks exits ExitHooksNotExercised, not ExitOK.
func Finish(f Finisher, green bool, mode PushMode, rc int) int {
	if err := f.Cleanup(); err != nil {
		rc = ExitFailure
	}
	if f.BranchCreated() {
		rc = finishBranch(f, rc)
	}
	if green && rc == ExitOK {
		rc = finishGreen(f, mode)
	}
	if err := f.RemoveWorkDir(); err != nil {
		f.Logf("warning: could not remove the probe's work directory (%v)", err)
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
// reads the record as the proof that they did.
func finishGreen(f Finisher, mode PushMode) int {
	if !mode.HooksExercised() {
		f.Logf("the probe is green, but the rig's hooks were NOT exercised: no rig repository at %s, so no record was written", f.Bare())
		return ExitHooksNotExercised
	}
	path, err := f.Record()
	if err != nil {
		f.Logf("%v", err)
		return ExitFailure
	}
	f.Logf("recorded the green probe in %s", path)
	return ExitOK
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
