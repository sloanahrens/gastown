package forgejoprobe

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	testContext = "ci / gate (push)"
	testSHA     = "3f9c2e1d"
)

// fakeClock is a Clock whose Sleep advances Now, so a poll loop finishes at
// once and the sleeps it made are on record.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
}

// fakeStatuses is an in-process Statuses. Each method answers from its field;
// a method whose field is unset fails, so a test sees every call the code makes.
type fakeStatuses struct {
	// combined answers the nth CombinedStatus call, counting from 1.
	combined func(n int) (*forgejo.CombinedStatus, error)
	reads    int
	// deadlines holds the deadline each CombinedStatus call carried; a call
	// without one records the zero time.
	deadlines []time.Time

	runs    *forgejo.RunList
	runsErr error
	jobs    []forgejo.ActionRunJob
	jobsErr error
	log     string
	logErr  error
	// jobsRun is the run ID the last ListRunJobs call asked for, 0 if none.
	jobsRun int64
	// loggedJob is the job ID the last JobLogsTail call asked for, 0 if none.
	loggedJob int64
}

var errNoAnswer = errors.New("fake: no answer")

func (f *fakeStatuses) CombinedStatus(ctx context.Context, _, _, _ string) (*forgejo.CombinedStatus, error) {
	f.reads++
	d, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, d)
	if f.combined == nil {
		return nil, errNoAnswer
	}
	return f.combined(f.reads)
}

func (f *fakeStatuses) ListRuns(_ context.Context, _, _ string, _ forgejo.RunFilter) (*forgejo.RunList, error) {
	return f.runs, f.runsErr
}

func (f *fakeStatuses) ListRunJobs(_ context.Context, _, _ string, runID int64) ([]forgejo.ActionRunJob, error) {
	f.jobsRun = runID
	return f.jobs, f.jobsErr
}

func (f *fakeStatuses) JobLogsTail(_ context.Context, _, _ string, jobID, _ int64) (string, error) {
	f.loggedJob = jobID
	return f.log, f.logErr
}

// answers returns a combined-status answer that reads the given statuses in
// order and fails the test's read budget once they are spent.
func answers(seq ...*forgejo.CombinedStatus) func(int) (*forgejo.CombinedStatus, error) {
	return func(n int) (*forgejo.CombinedStatus, error) {
		if n > len(seq) {
			return nil, errNoAnswer
		}
		return seq[n-1], nil
	}
}

// reporting is a combined status with one status on contextName per entry.
func reporting(statuses ...forgejo.CommitStatus) *forgejo.CombinedStatus {
	return &forgejo.CombinedStatus{Statuses: statuses}
}

func status(id int64, contextName string, state forgejo.CommitState) forgejo.CommitStatus {
	return forgejo.CommitStatus{ID: id, Context: contextName, Status: state}
}

func TestWaitPollsUntilItReports(t *testing.T) {
	t.Parallel()
	const poll = 5 * time.Second
	cases := []struct {
		name       string
		reads      []*forgejo.CombinedStatus
		wantReads  int
		wantSleeps []time.Duration
	}{
		{
			name: "pending twice then success",
			reads: []*forgejo.CombinedStatus{
				reporting(status(1, testContext, forgejo.StatePending)),
				reporting(status(1, testContext, forgejo.StatePending)),
				reporting(status(1, testContext, forgejo.StateSuccess)),
			},
			wantReads:  3,
			wantSleeps: []time.Duration{poll, poll},
		},
		{
			name: "absent status polls through",
			reads: []*forgejo.CombinedStatus{
				reporting(status(1, "other / job (push)", forgejo.StateSuccess)),
				reporting(status(2, testContext, forgejo.StateSuccess)),
			},
			wantReads:  2,
			wantSleeps: []time.Duration{poll},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := &fakeStatuses{combined: answers(tc.reads...)}
			clock := &fakeClock{now: time.Unix(1000, 0)}

			v := Wait(context.Background(), st, clock, "acme", "rig", testSHA, testContext, time.Hour, poll)

			if v.Outcome != Green {
				t.Fatalf("outcome = %v, want Green", v.Outcome)
			}
			if st.reads != tc.wantReads {
				t.Errorf("reads = %d, want %d", st.reads, tc.wantReads)
			}
			if !slices.Equal(clock.sleeps, tc.wantSleeps) {
				t.Errorf("sleeps = %v, want %v", clock.sleeps, tc.wantSleeps)
			}
		})
	}
}

func TestWaitNewestStatusWins(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		listed  []forgejo.CommitStatus
		want    Outcome
		wantRed forgejo.CommitState
	}{
		{
			name: "older success, newer failure",
			listed: []forgejo.CommitStatus{
				status(1, testContext, forgejo.StateSuccess),
				status(2, testContext, forgejo.StateFailure),
			},
			want:    Red,
			wantRed: forgejo.StateFailure,
		},
		{
			name: "newer success listed first",
			listed: []forgejo.CommitStatus{
				status(2, testContext, forgejo.StateSuccess),
				status(1, testContext, forgejo.StateFailure),
			},
			want: Green,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := &fakeStatuses{combined: answers(reporting(tc.listed...))}
			clock := &fakeClock{now: time.Unix(1000, 0)}

			v := Wait(context.Background(), st, clock, "acme", "rig", testSHA, testContext, time.Hour, time.Second)

			if v.Outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", v.Outcome, tc.want)
			}
			if v.State != tc.wantRed {
				t.Errorf("state = %q, want %q", v.State, tc.wantRed)
			}
		})
	}
}

func TestWaitTimeoutAndUnreadable(t *testing.T) {
	t.Parallel()
	const (
		timeout = 30 * time.Second
		poll    = 10 * time.Second
	)
	// runaway bounds a read loop that ignores the deadline, so the test fails
	// on its assertions instead of hanging.
	const runaway = 100

	t.Run("pending past the deadline times out", func(t *testing.T) {
		t.Parallel()
		st := &fakeStatuses{combined: func(n int) (*forgejo.CombinedStatus, error) {
			if n > runaway {
				return nil, errNoAnswer
			}
			return reporting(status(1, testContext, forgejo.StatePending)), nil
		}}
		start := time.Unix(1000, 0)
		clock := &fakeClock{now: start}

		v := Wait(context.Background(), st, clock, "acme", "rig", testSHA, testContext, timeout, poll)

		if v.Outcome != TimedOut {
			t.Fatalf("outcome = %v, want TimedOut", v.Outcome)
		}
		// Reads at 0, 10, 20 and 30 seconds: timeout/poll sleeps, one read each.
		if want := int(timeout/poll) + 1; st.reads != want {
			t.Errorf("reads = %d, want %d", st.reads, want)
		}
		if got := clock.now.Sub(start); got != timeout {
			t.Errorf("waited %v, want %v", got, timeout)
		}
	})

	t.Run("a read error is unreadable", func(t *testing.T) {
		t.Parallel()
		readErr := errors.New("connection refused")
		st := &fakeStatuses{combined: func(int) (*forgejo.CombinedStatus, error) {
			return nil, readErr
		}}
		clock := &fakeClock{now: time.Unix(1000, 0)}

		v := Wait(context.Background(), st, clock, "acme", "rig", testSHA, testContext, timeout, poll)

		if v.Outcome != Unreadable {
			t.Fatalf("outcome = %v, want Unreadable", v.Outcome)
		}
		if !errors.Is(v.Err, readErr) {
			t.Errorf("err = %v, want %v", v.Err, readErr)
		}
		if st.reads != 1 {
			t.Errorf("reads = %d, want 1: an unreadable verdict stops the wait", st.reads)
		}
	})
}

func TestWaitCallTimeout(t *testing.T) {
	t.Parallel()
	st := &fakeStatuses{combined: answers(
		reporting(status(1, testContext, forgejo.StatePending)),
		reporting(status(1, testContext, forgejo.StateSuccess)),
	)}
	start := time.Unix(1000, 0)
	clock := &fakeClock{now: start}

	Wait(context.Background(), st, clock, "acme", "rig", testSHA, testContext, time.Hour, time.Second)

	if len(st.deadlines) != 2 {
		t.Fatalf("calls = %d, want 2", len(st.deadlines))
	}
	// The second call is made after one poll, so its deadline is measured from
	// the fake's time at that call, not from the start.
	if want := start.Add(callTimeout); !st.deadlines[0].Equal(want) {
		t.Errorf("first call deadline = %v, want %v", st.deadlines[0], want)
	}
	if want := start.Add(time.Second).Add(callTimeout); !st.deadlines[1].Equal(want) {
		t.Errorf("second call deadline = %v, want %v", st.deadlines[1], want)
	}
}

func TestVerdictLine(t *testing.T) {
	t.Parallel()
	const timeout = 90 * time.Second
	cases := []struct {
		name string
		v    Verdict
		want string
	}{
		{"green", Verdict{Outcome: Green}, "green rig's gate is green on " + testSHA + " (" + testContext + ")"},
		{"timed out", Verdict{Outcome: TimedOut}, "timeout " + testContext + " reported nothing on " + testSHA + " within 90s"},
		{"red", Verdict{Outcome: Red, State: forgejo.StateFailure}, "other " + testContext + " is failure on " + testSHA},
		{"unreadable", Verdict{Outcome: Unreadable, Err: errors.New("boom")}, "other " + testContext + " is unreadable (boom) on " + testSHA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.v.Line("rig", testContext, testSHA, timeout); got != tc.want {
				t.Errorf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}
