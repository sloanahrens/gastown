package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// statusContext is the context every status-mode test configures.
const statusContext = "ci / post-land (push)"

// stepClock is a fake clock whose After moves it by the wait, so a poll loop
// reaches its bound with no real sleep (the test-engineering rule).
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func newStepClock(at time.Time) *stepClock { return &stepClock{now: at} }

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stepClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	at := c.now
	c.mu.Unlock()
	ch := make(chan time.Time, 1)
	ch <- at
	return ch
}

type fakeStatusRead struct {
	status *forgejo.CombinedStatus
	err    error
}

// fakeStatusReads is a status source that answers from a script, repeating
// its last entry, and counts reads.
type fakeStatusReads struct {
	mu        sync.Mutex
	responses []fakeStatusRead
	calls     int
}

func (f *fakeStatusReads) read(context.Context, PostLand) (*forgejo.CombinedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if len(f.responses) == 0 {
		return &forgejo.CombinedStatus{}, nil
	}
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	return f.responses[i].status, f.responses[i].err
}

func (f *fakeStatusReads) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// combinedFor is a combined status carrying one status per state, all with
// the given context.
func combinedFor(name string, states ...forgejo.CommitState) *forgejo.CombinedStatus {
	cs := &forgejo.CombinedStatus{}
	for _, st := range states {
		cs.Statuses = append(cs.Statuses, forgejo.CommitStatus{Context: name, Status: st})
	}
	return cs
}

// statusVerdicts records what a runner's callbacks and log received.
type statusVerdicts struct {
	mu       sync.Mutex
	greens   int
	reds     int
	redTails []string
	unknowns []PostLandUnknown
	logs     []string
}

// runner builds a status-mode runner over reads for the given context.
func (v *statusVerdicts) runner(reads *fakeStatusReads, name string) *PostLandRunner {
	return &PostLandRunner{
		Rig:           "gastown",
		StatusContext: func() string { return name },
		Status:        reads.read,
		Clock:         newStepClock(time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)),
		Timeout:       10 * time.Minute,
		Logf: func(format string, args ...any) {
			v.mu.Lock()
			v.logs = append(v.logs, fmt.Sprintf(format, args...))
			v.mu.Unlock()
		},
		OnGreen: func(context.Context, string, PostLand, PostLandResult) {
			v.mu.Lock()
			v.greens++
			v.mu.Unlock()
		},
		OnRed: func(_ context.Context, _ string, _ PostLand, res PostLandResult) {
			v.mu.Lock()
			v.reds++
			v.redTails = append(v.redTails, res.Tail)
			v.mu.Unlock()
		},
		OnUnknown: func(_ context.Context, u PostLandUnknown) {
			v.mu.Lock()
			v.unknowns = append(v.unknowns, u)
			v.mu.Unlock()
		},
	}
}

func (v *statusVerdicts) logText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return strings.Join(v.logs, "\n")
}

func (v *statusVerdicts) unknownCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.unknowns)
}

func TestPostLandStatusSuccessAndFailure(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}

	t.Run("success calls OnGreen once", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
		v.runner(reads, statusContext).runOne(context.Background(), pl)
		if v.greens != 1 || v.reds != 0 || v.unknownCount() != 0 {
			t.Fatalf("green %d red %d unknown %d, want 1/0/0", v.greens, v.reds, v.unknownCount())
		}
		if reads.count() != 1 {
			t.Fatalf("reads = %d, want one successful read", reads.count())
		}
	})

	t.Run("failure calls OnRed once with the status description", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		status := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
			Context: statusContext, Status: forgejo.StateFailure, Description: "gate: 2 tests failed",
		}}}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: status}}}
		v.runner(reads, statusContext).runOne(context.Background(), pl)
		if v.reds != 1 || v.greens != 0 || v.unknownCount() != 0 {
			t.Fatalf("green %d red %d unknown %d, want 0/1/0", v.greens, v.reds, v.unknownCount())
		}
		if len(v.redTails) != 1 || v.redTails[0] != "gate: 2 tests failed" {
			t.Fatalf("red tails = %q, want the status description", v.redTails)
		}
	})

	t.Run("error calls OnRed once", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		status := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
			Context: statusContext, Status: forgejo.StateError, Description: "the workflow could not start",
		}}}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: status}}}
		v.runner(reads, statusContext).runOne(context.Background(), pl)
		if v.reds != 1 || v.greens != 0 {
			t.Fatalf("green %d red %d, want 0/1", v.greens, v.reds)
		}
		if len(v.redTails) != 1 || v.redTails[0] != "the workflow could not start" {
			t.Fatalf("red tails = %q", v.redTails)
		}
	})
}

func TestPostLandStatusUnknownIsNeitherGreenNorRed(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	unreadable := errors.New("dial tcp: connection refused")

	cases := []struct {
		name    string
		reads   *fakeStatusReads
		timeout time.Duration
		wantLog string
	}{
		{
			name:    "absent",
			reads:   &fakeStatusReads{responses: []fakeStatusRead{{status: &forgejo.CombinedStatus{}}}},
			timeout: 10 * time.Minute,
			wantLog: "is missing",
		},
		{
			name:    "pending",
			reads:   &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}},
			timeout: 10 * time.Minute,
			wantLog: "is pending",
		},
		{
			name:    "unreadable",
			reads:   &fakeStatusReads{responses: []fakeStatusRead{{err: unreadable}}},
			timeout: 10 * time.Minute,
			wantLog: "reading the combined status",
		},
		{
			// Timeout 0 takes the default, so this row is the real
			// post_land_timeout bound.
			name:    "pending past post_land_timeout",
			reads:   &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}},
			timeout: 0,
			wantLog: "UNKNOWN: no verdict",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			r := v.runner(tc.reads, statusContext)
			r.Timeout = tc.timeout
			r.runOne(context.Background(), pl)
			if v.greens != 0 || v.reds != 0 {
				t.Fatalf("green %d red %d, want neither callback", v.greens, v.reds)
			}
			if logs := v.logText(); !strings.Contains(logs, tc.wantLog) {
				t.Fatalf("logs do not name the reason %q:\n%s", tc.wantLog, logs)
			}
		})
	}
}

func TestPostLandStatusContextWinsOverCommand(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}

	t.Run("with both set the command does not run", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
		ran := 0
		r := v.runner(reads, statusContext)
		r.Command = func() string { return "make test-slow" }
		r.Run = func(context.Context, string, PostLand) PostLandResult {
			ran++
			return PostLandResult{}
		}
		r.runOne(context.Background(), pl)
		if ran != 0 {
			t.Fatalf("the command ran %d times with a status context set", ran)
		}
		if v.greens != 1 {
			t.Fatalf("green %d, want the status verdict", v.greens)
		}
		if logs := v.logText(); !strings.Contains(logs, "not run") {
			t.Fatalf("the log does not say the command was not run:\n%s", logs)
		}
	})

	t.Run("with no context the command runs as before", func(t *testing.T) {
		t.Parallel()
		ran := 0
		r := &PostLandRunner{
			Rig:     "gastown",
			Command: func() string { return "make test-slow" },
			Run: func(context.Context, string, PostLand) PostLandResult {
				ran++
				return PostLandResult{}
			},
			Logf: t.Logf,
		}
		r.runOne(context.Background(), pl)
		if ran != 1 {
			t.Fatalf("the command ran %d times with no context, want once", ran)
		}
	})
}

// TestPostLandStatusTriggersWithoutACommand: a rig with only a status context
// must start a run; the enabled check used to look at Command alone.
func TestPostLandStatusTriggersWithoutACommand(t *testing.T) {
	t.Parallel()
	v := &statusVerdicts{}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
	r := v.runner(reads, statusContext)
	r.Trigger(context.Background(), PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
	r.Wait()
	if v.greens != 1 {
		t.Fatalf("green %d, want the status verdict", v.greens)
	}
}

func TestPostLandStatusIgnoresAHandPostedStatus(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	hand := &forgejo.User{ID: 7, Login: "mallory"}

	unknown := []struct {
		name    string
		status  forgejo.CommitStatus
		wantLog string
	}{
		{
			name:    "hand posted green",
			status:  forgejo.CommitStatus{Context: statusContext, Status: forgejo.StateSuccess, Creator: hand},
			wantLog: "posted by hand",
		},
		{
			name:    "skipped",
			status:  forgejo.CommitStatus{Context: statusContext, Status: forgejo.StateSkipped},
			wantLog: "is skipped",
		},
		{
			name:    "warning",
			status:  forgejo.CommitStatus{Context: statusContext, Status: forgejo.StateWarning},
			wantLog: "is warning",
		},
	}
	for _, tc := range unknown {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			status := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{tc.status}}
			r := v.runner(&fakeStatusReads{responses: []fakeStatusRead{{status: status}}}, statusContext)
			r.runOne(context.Background(), pl)
			if v.greens != 0 || v.reds != 0 {
				t.Fatalf("green %d red %d, want neither callback", v.greens, v.reds)
			}
			if logs := v.logText(); !strings.Contains(logs, tc.wantLog) {
				t.Fatalf("logs do not name the reason %q:\n%s", tc.wantLog, logs)
			}
		})
	}

	t.Run("two statuses for the context with one failure read red", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		status := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
			{Context: statusContext, Status: forgejo.StateSuccess},
			{Context: statusContext, Status: forgejo.StateFailure, Description: "one job failed"},
		}}
		r := v.runner(&fakeStatusReads{responses: []fakeStatusRead{{status: status}}}, statusContext)
		r.runOne(context.Background(), pl)
		if v.reds != 1 || v.greens != 0 {
			t.Fatalf("green %d red %d, want red once", v.greens, v.reds)
		}
		if len(v.redTails) != 1 || v.redTails[0] != "one job failed" {
			t.Fatalf("red tails = %q", v.redTails)
		}
	})
}

func TestPostLandStatusTimeoutRaisesOneAttentionItem(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}

	t.Run("pending raises one item keyed by rig and landed sha", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
		r := v.runner(reads, statusContext)
		r.Timeout = 60 * time.Minute
		r.runOne(context.Background(), pl)
		if v.unknownCount() != 1 {
			t.Fatalf("unknown items = %d, want one", v.unknownCount())
		}
		u := v.unknowns[0]
		if want := "post-land-unknown:gastown:" + pl.Commit; u.Key() != want {
			t.Fatalf("key = %q, want %q", u.Key(), want)
		}
		if u.Rig != "gastown" || u.SHA != pl.Commit || u.Context != statusContext || u.State != "pending" {
			t.Fatalf("record = %+v", u)
		}
		if s := u.Summary(); !strings.Contains(s, statusContext) || !strings.Contains(s, "pending") {
			t.Fatalf("summary %q does not name the context and last state", s)
		}

		// The same landed commit timing out again raises no second item.
		r.runOne(context.Background(), pl)
		if got := v.unknownCount(); got != 1 {
			t.Fatalf("unknown items after a second timeout = %d, want one", got)
		}
	})

	t.Run("missing raises one item naming the last state", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: &forgejo.CombinedStatus{}}}}
		r := v.runner(reads, statusContext)
		r.Timeout = 60 * time.Minute
		r.runOne(context.Background(), pl)
		if v.unknownCount() != 1 {
			t.Fatalf("unknown items = %d, want one", v.unknownCount())
		}
		if u := v.unknowns[0]; u.State != "missing" || !strings.Contains(u.Summary(), "missing") {
			t.Fatalf("record = %+v", u)
		}
	})

	t.Run("a verdict that resolves before the timeout raises none", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{
			{status: combinedFor(statusContext, forgejo.StatePending)},
			{status: combinedFor(statusContext, forgejo.StateSuccess)},
		}}
		r := v.runner(reads, statusContext)
		r.Timeout = 60 * time.Minute
		r.runOne(context.Background(), pl)
		if v.unknownCount() != 0 {
			t.Fatalf("unknown items = %d, want none", v.unknownCount())
		}
		if v.greens != 1 {
			t.Fatalf("green %d, want the resolved success", v.greens)
		}
	})
}
