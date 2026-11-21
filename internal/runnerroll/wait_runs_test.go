package runnerroll

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// listing is one scripted run list read.
type listing struct {
	runs []WorkflowRun
	err  error
}

// scriptedRuns returns the listings in order and repeats the last one after that.
func scriptedRuns(ls ...listing) (func(context.Context) ([]WorkflowRun, error), *int) {
	calls := 0
	return func(context.Context) ([]WorkflowRun, error) {
		i := min(calls, len(ls)-1)
		calls++
		return ls[i].runs, ls[i].err
	}, &calls
}

// runAt is one run of the gate workflow in the gastown repository.
func runAt(id int64, workflow, status string, created time.Time) WorkflowRun {
	return WorkflowRun{Repo: "gastownhall/gastown", Workflow: workflow, ID: id, Status: status, Created: created}
}

var runsT0 = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func TestWaitRunsHoldsUntilRunEndsAndPrintsOnce(t *testing.T) {
	t.Parallel()
	gate := runAt(7, "gate.yml", "running", runsT0)
	list, _ := scriptedRuns(
		listing{runs: []WorkflowRun{gate}},
		listing{runs: []WorkflowRun{gate}},
		listing{},
	)
	clock := &fakeClock{now: runsT0}
	var out bytes.Buffer
	if err := WaitRunsIdle(context.Background(), list, clock, time.Minute, &out); err != nil {
		t.Fatalf("WaitRunsIdle() = %v, want nil", err)
	}
	if len(clock.sleeps) != 2 {
		t.Errorf("slept %d times, want 2 (one per listing with a run in flight)", len(clock.sleeps))
	}
	want := `runs: 1 in flight
  gastownhall/gastown gate.yml 0s
runs: none in flight
`
	if got := out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestWaitRunsTimeoutNamesTheWait(t *testing.T) {
	t.Parallel()
	gate := runAt(7, "gate.yml", "running", runsT0)
	list, _ := scriptedRuns(listing{runs: []WorkflowRun{gate}})
	clock := &fakeClock{now: runsT0}
	err := WaitRunsIdle(context.Background(), list, clock, 25*time.Second, &bytes.Buffer{})
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("WaitRunsIdle() = %v, want ErrWaitTimeout", err)
	}
	if !strings.Contains(err.Error(), "runs wait") {
		t.Errorf("error %q does not name the runs wait", err)
	}
	if !strings.Contains(err.Error(), "gastownhall/gastown gate.yml") {
		t.Errorf("error %q does not name the run still going", err)
	}
	if got := clock.Now().Sub(runsT0); got != 25*time.Second {
		t.Errorf("waited until %v, want exactly the 25s bound", got)
	}
}

func TestWaitRunsListErrorIsNotIdle(t *testing.T) {
	t.Parallel()
	boom := errors.New("forgejo unreachable")
	list, calls := scriptedRuns(listing{err: boom})
	err := WaitRunsIdle(context.Background(), list, &fakeClock{now: runsT0}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, boom) {
		t.Fatalf("WaitRunsIdle() = %v, want the list error wrapped", err)
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Errorf("list error reported as a timeout: %v", err)
	}
	if *calls != 1 {
		t.Errorf("listed %d times, want 1 (the wait stops at the error)", *calls)
	}
}

func TestWaitRunsUnknownStatusIsNotIdle(t *testing.T) {
	t.Parallel()
	// A status the wait does not know must not read as finished.
	list, _ := scriptedRuns(listing{runs: []WorkflowRun{runAt(7, "gate.yml", "mystery", runsT0)}})
	clock := &fakeClock{now: runsT0}
	err := WaitRunsIdle(context.Background(), list, clock, time.Minute, &bytes.Buffer{})
	if err == nil {
		t.Fatal("WaitRunsIdle() = nil on an unknown run status, want an error")
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("slept %d times on an unknown status, want 0", len(clock.sleeps))
	}
}

func TestWaitRunsEndedRunsDoNotHoldTheWait(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"cancelled", "failure", "success", "skipped"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			list, _ := scriptedRuns(listing{runs: []WorkflowRun{runAt(7, "gate.yml", status, runsT0)}})
			clock := &fakeClock{now: runsT0}
			var out bytes.Buffer
			if err := WaitRunsIdle(context.Background(), list, clock, time.Minute, &out); err != nil {
				t.Fatalf("WaitRunsIdle() = %v, want nil for a %s run", err, status)
			}
			if len(clock.sleeps) != 0 {
				t.Errorf("slept %d times for a %s run, want 0", len(clock.sleeps), status)
			}
			if got, want := out.String(), "runs: none in flight\n"; got != want {
				t.Errorf("output %q, want %q", got, want)
			}
		})
	}
}

func TestWaitRunsCancelDuringListIsNotIdle(t *testing.T) {
	t.Parallel()
	// The list is canceled after it returns an empty listing: that listing was
	// read while the wait was being stopped, so it must not decide the wait.
	ctx, cancel := context.WithCancel(context.Background())
	list := func(context.Context) ([]WorkflowRun, error) {
		cancel()
		return nil, nil
	}
	err := WaitRunsIdle(ctx, list, &fakeClock{now: runsT0}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitRunsIdle() = %v, want context.Canceled, not idle", err)
	}
}

func TestWaitRunsCanceledContextStops(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	list, calls := scriptedRuns(listing{})
	err := WaitRunsIdle(ctx, list, &fakeClock{now: runsT0}, time.Minute, &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitRunsIdle() = %v, want context.Canceled", err)
	}
	if *calls != 0 {
		t.Errorf("listed %d times on a canceled context, want 0", *calls)
	}
}

// fakeRunLister records each run list request and answers with answer.
type fakeRunLister struct {
	filters []forgejo.RunFilter
	answer  func(owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
}

func (l *fakeRunLister) ListRuns(_ context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error) {
	l.filters = append(l.filters, f)
	return l.answer(owner, repo, f)
}

func TestWaitRunsForgejoListerReadsEachWorkflowInFlight(t *testing.T) {
	t.Parallel()
	lister := &fakeRunLister{answer: func(string, string, forgejo.RunFilter) (*forgejo.RunList, error) {
		return &forgejo.RunList{Runs: []forgejo.ActionRun{{ID: 1, Status: "running", Created: "2026-11-19T12:00:00Z"}}}, nil
	}}
	runs, err := ForgejoRuns(lister, []RepoRef{{Owner: "gastownhall", Name: "gastown"}}, []string{"gate.yml", "post-land.yml"})(context.Background())
	if err != nil {
		t.Fatalf("ForgejoRuns() = %v, want nil", err)
	}
	if len(lister.filters) != 2 {
		t.Fatalf("listed %d times, want 2 (one per workflow)", len(lister.filters))
	}
	for i, wf := range []string{"gate.yml", "post-land.yml"} {
		// The whole filter, not only the workflow: the page limit is what makes
		// the first page decide idle, and the status set is the in-flight filter.
		want := forgejo.RunFilter{
			Status:     []string{forgejo.RunStatusWaiting, forgejo.RunStatusBlocked, forgejo.RunStatusRunning},
			WorkflowID: wf,
			Limit:      runsPageLimit,
		}
		if got := lister.filters[i]; !reflect.DeepEqual(got, want) {
			t.Errorf("request %d filter = %+v, want %+v", i, got, want)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].Repo != "gastownhall/gastown" || runs[0].Workflow != "gate.yml" {
		t.Errorf("first run = %s %s, want gastownhall/gastown gate.yml", runs[0].Repo, runs[0].Workflow)
	}
	if !runs[0].Created.Equal(runsT0) {
		t.Errorf("first run created %v, want %v", runs[0].Created, runsT0)
	}
}

func TestWaitRunsForgejoListerErrorIsNotEmpty(t *testing.T) {
	t.Parallel()
	boom := errors.New("forgejo unreachable")
	lister := &fakeRunLister{answer: func(string, string, forgejo.RunFilter) (*forgejo.RunList, error) {
		return nil, boom
	}}
	runs, err := ForgejoRuns(lister, []RepoRef{{Owner: "gastownhall", Name: "gastown"}}, []string{"gate.yml"})(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("ForgejoRuns() = %v, want the list error wrapped", err)
	}
	if runs != nil {
		t.Errorf("ForgejoRuns() returned %d runs with the error, want none", len(runs))
	}
}

func TestWaitRunsForgejoListerRefusesUnreadableCreatedTime(t *testing.T) {
	t.Parallel()
	lister := &fakeRunLister{answer: func(string, string, forgejo.RunFilter) (*forgejo.RunList, error) {
		return &forgejo.RunList{Runs: []forgejo.ActionRun{{ID: 1, Status: "running", Created: "yesterday"}}}, nil
	}}
	_, err := ForgejoRuns(lister, []RepoRef{{Owner: "gastownhall", Name: "gastown"}}, []string{"gate.yml"})(context.Background())
	if err == nil {
		t.Fatal("ForgejoRuns() = nil on an unreadable created time, want an error")
	}
}
