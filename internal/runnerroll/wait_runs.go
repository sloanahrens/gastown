package runnerroll

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// runsPollInterval is how often the run list is read while the runs wait runs.
const runsPollInterval = 10 * time.Second

// runsPageLimit bounds one run list request. A non-empty page holds the wait and
// an empty one means no run is in flight, so the first page decides idle; the
// printed set can leave out runs past it.
const runsPageLimit = 50

// The statuses of a run that has ended. Every other status the wait does not
// know is an error, so an unfamiliar status cannot read as finished.
const (
	runStatusSuccess   = "success"
	runStatusFailure   = "failure"
	runStatusCancelled = "cancelled" //nolint:misspell // the status the Forgejo API reports
	runStatusSkipped   = "skipped"
)

// WorkflowRun is one workflow run the runs wait is watching.
type WorkflowRun struct {
	// Repo is owner/name of the repository the run belongs to.
	Repo     string
	Workflow string
	ID       int64
	Status   string
	Created  time.Time
}

// WaitRunsIdle polls list every runsPollInterval until no run is in flight, then
// returns nil. A run is in flight while it is waiting, blocked or running. It
// prints the in-flight set each time the set changes: each run's repo, workflow
// and age, or a line saying none is in flight.
//
// It never reports idle on a listing it cannot trust: a list error is returned,
// and a run with an unknown status is an error. A wait still not idle when bound
// passes returns an error wrapping ErrWaitTimeout that names the runs still going.
func WaitRunsIdle(ctx context.Context, list func(ctx context.Context) ([]WorkflowRun, error), clock Clock, bound time.Duration, out io.Writer) error {
	deadline := clock.Now().Add(bound)
	var printed bool
	var lastKey string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		runs, err := list(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return fmt.Errorf("list workflow runs: %w", err)
		}
		inFlight, err := inFlightRuns(runs)
		if err != nil {
			return err
		}
		now := clock.Now()
		if key := runsKey(inFlight); !printed || key != lastKey {
			printRuns(out, inFlight, now)
			printed, lastKey = true, key
		}
		if len(inFlight) == 0 {
			return nil
		}
		if !now.Before(deadline) {
			return fmt.Errorf("%w: runs wait: %d runs still going after %s: %s", ErrWaitTimeout, len(inFlight), bound, runsNames(inFlight))
		}
		clock.Sleep(ctx, min(deadline.Sub(now), runsPollInterval))
	}
}

// ForgejoRunLister is the part of the Forgejo client the runs wait reads.
// *forgejo.Client satisfies it; tests inject a fake.
type ForgejoRunLister interface {
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
}

// RepoRef names one repository by its owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

// ForgejoRuns returns the list argument of WaitRunsIdle: the in-flight runs of
// each named workflow in each repository, read through client. A failed read, or
// a run whose created time cannot be read, is an error, never an empty listing.
func ForgejoRuns(client ForgejoRunLister, repos []RepoRef, workflows []string) func(context.Context) ([]WorkflowRun, error) {
	return func(ctx context.Context) ([]WorkflowRun, error) {
		var runs []WorkflowRun
		for _, repo := range repos {
			name := repo.Owner + "/" + repo.Name
			for _, wf := range workflows {
				list, err := client.ListRuns(ctx, repo.Owner, repo.Name, forgejo.RunFilter{
					Status:     []string{forgejo.RunStatusWaiting, forgejo.RunStatusBlocked, forgejo.RunStatusRunning},
					WorkflowID: wf,
					Limit:      runsPageLimit,
				})
				if err != nil {
					return nil, fmt.Errorf("list %s %s runs: %w", name, wf, err)
				}
				for _, run := range list.Runs {
					created, err := time.Parse(time.RFC3339, run.Created)
					if err != nil {
						return nil, fmt.Errorf("run of %s %s has an unreadable created time", name, wf)
					}
					runs = append(runs, WorkflowRun{Repo: name, Workflow: wf, ID: run.ID, Status: run.Status, Created: created})
				}
			}
		}
		return runs, nil
	}
}

// inFlightRuns returns the runs still going, sorted so the printed set and its
// key are the same from one read to the next. A status the wait does not know is
// an error.
func inFlightRuns(runs []WorkflowRun) ([]WorkflowRun, error) {
	var out []WorkflowRun
	for _, run := range runs {
		live, err := runInFlight(run.Status)
		if err != nil {
			return nil, fmt.Errorf("run of %s %s: %w", run.Repo, run.Workflow, err)
		}
		if live {
			out = append(out, run)
		}
	}
	sort.Slice(out, func(i, j int) bool { return runKey(out[i]) < runKey(out[j]) })
	return out, nil
}

// runInFlight reports whether a run in status is still going. The server-side
// filter asks only for these statuses, and the wait checks again here so a server
// that ignores the filter cannot hold the wait on a finished run.
func runInFlight(status string) (bool, error) {
	switch status {
	case forgejo.RunStatusWaiting, forgejo.RunStatusBlocked, forgejo.RunStatusRunning:
		return true, nil
	case runStatusSuccess, runStatusFailure, runStatusCancelled, runStatusSkipped:
		return false, nil
	}
	return false, fmt.Errorf("unknown status %q", status)
}

// runKey identifies a run for the set-change check. The age is left out, because
// it moves on every read.
func runKey(r WorkflowRun) string {
	return fmt.Sprintf("%s %s %d", r.Repo, r.Workflow, r.ID)
}

// runsKey is the identity of an in-flight set, in the order inFlightRuns sorts it.
func runsKey(runs []WorkflowRun) string {
	keys := make([]string, 0, len(runs))
	for _, run := range runs {
		keys = append(keys, runKey(run))
	}
	return strings.Join(keys, "\n")
}

// printRuns writes the in-flight set with each run's repo, workflow and age.
func printRuns(out io.Writer, runs []WorkflowRun, now time.Time) {
	if len(runs) == 0 {
		fmt.Fprintln(out, "runs: none in flight")
		return
	}
	fmt.Fprintf(out, "runs: %d in flight\n", len(runs))
	for _, run := range runs {
		fmt.Fprintf(out, "  %s %s %s\n", run.Repo, run.Workflow, now.Sub(run.Created).Round(time.Second))
	}
}

// runsNames names each run by its repo and workflow for the timeout error.
func runsNames(runs []WorkflowRun) string {
	names := make([]string, 0, len(runs))
	for _, run := range runs {
		names = append(names, run.Repo+" "+run.Workflow)
	}
	return strings.Join(names, ", ")
}
