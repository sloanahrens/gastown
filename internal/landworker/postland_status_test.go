package landworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// statusContext is the context every status-mode test configures.
const statusContext = "ci / post-land (push)"

var statusT0 = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// manualClock is a fake clock that moves only when a test advances it.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
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

// byCommit answers each landed commit from its own script.
func byCommit(reads map[string]*fakeStatusReads) func(context.Context, PostLand) (*forgejo.CombinedStatus, error) {
	return func(ctx context.Context, pl PostLand) (*forgejo.CombinedStatus, error) {
		return reads[pl.Commit].read(ctx, pl)
	}
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

// statusVerdicts records what a runner's callbacks, log and bead comments
// received.
type statusVerdicts struct {
	mu           sync.Mutex
	greens       int
	reds         int
	redTails     []string
	unknowns     []PostLandUnknown
	unknownErr   error // what OnUnknown answers; nil confirms the alarm stored
	storeFails   []*PostLandStoreFailure
	storeCleared int
	logs         []string
	comments     []string
}

// runner builds a status-mode runner over status and the pending verdicts in
// store, reading its deadlines from clk.
func (v *statusVerdicts) runner(status func(context.Context, PostLand) (*forgejo.CombinedStatus, error), store MainStateStore, clk PostLandClock, timeout time.Duration) *PostLandRunner {
	return &PostLandRunner{
		Rig:           "gastown",
		StatusContext: func() string { return statusContext },
		Status:        status,
		State:         store,
		Clock:         clk,
		Timeout:       timeout,
		Beads:         v,
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
		OnUnknown: func(_ context.Context, u PostLandUnknown) error {
			v.mu.Lock()
			v.unknowns = append(v.unknowns, u)
			v.mu.Unlock()
			return v.unknownErr
		},
		OnStoreFailure: func(_ context.Context, f *PostLandStoreFailure) {
			v.mu.Lock()
			if f == nil {
				v.storeCleared++
			} else {
				v.storeFails = append(v.storeFails, f)
			}
			v.mu.Unlock()
		},
	}
}

// storeFailureRaises is how many store-failure items the runner raised, and
// storeClears how many times it said the store is readable again.
func (v *statusVerdicts) storeFailureRaises() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.storeFails)
}

func (v *statusVerdicts) storeClears() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.storeCleared
}

// AddComment records a bead comment the runner wrote.
func (v *statusVerdicts) AddComment(_, text string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.comments = append(v.comments, text)
	return nil
}

func (v *statusVerdicts) counts() (greens, reds, unknowns int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.greens, v.reds, len(v.unknowns)
}

func (v *statusVerdicts) logText() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return strings.Join(v.logs, "\n")
}

func (v *statusVerdicts) unknownCount() int {
	_, _, n := v.counts()
	return n
}

// pendingOf is the pending verdicts the store holds.
func pendingOf(t *testing.T, store MainStateStore) []PendingPostLand {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatalf("loading the main state: %v", err)
	}
	return st.PostLandPending
}

// failingMainState is a MainStateStore whose reads or writes fail.
type failingMainState struct {
	loadErr error
	saveErr error
}

func (f failingMainState) Load() (MainState, error) { return MainState{}, f.loadErr }
func (f failingMainState) Save(MainState) error     { return f.saveErr }

// flakyMainState is a working store a test can make fail at reads or writes.
// A failed read still holds what the last good write stored, so a test can
// assert the entries survived it.
type flakyMainState struct {
	inner   MemoryMainState
	mu      sync.Mutex
	loadErr error
	saveErr error
}

func (f *flakyMainState) Load() (MainState, error) {
	f.mu.Lock()
	err := f.loadErr
	f.mu.Unlock()
	if err != nil {
		return MainState{}, err
	}
	return f.inner.Load()
}

func (f *flakyMainState) Save(st MainState) error {
	f.mu.Lock()
	err := f.saveErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.inner.Save(st)
}

func (f *flakyMainState) failLoad(err error) { f.mu.Lock(); f.loadErr = err; f.mu.Unlock() }
func (f *flakyMainState) failSave(err error) { f.mu.Lock(); f.saveErr = err; f.mu.Unlock() }

// stored is the pending verdicts the store holds, as a good read sees them.
func (f *flakyMainState) stored(t *testing.T) []PendingPostLand {
	t.Helper()
	return pendingOf(t, &f.inner)
}

func assertNoControl(t *testing.T, what, s string) {
	t.Helper()
	if i := strings.IndexFunc(s, unicode.IsControl); i >= 0 {
		t.Fatalf("%s has a control character at byte %d: %q", what, i, s)
	}
}

func TestPostLandStatusVerdictRules(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	hand := &forgejo.User{ID: 7, Login: "mallory"}
	unreadable := errors.New("dial tcp: connection refused")

	cases := []struct {
		name      string
		read      fakeStatusRead
		wantGreen int
		wantRed   int
		wantTail  string
		wantLog   string
	}{
		{
			name:      "success is green once",
			read:      fakeStatusRead{status: combinedFor(statusContext, forgejo.StateSuccess)},
			wantGreen: 1,
			wantLog:   "is success",
		},
		{
			name: "failure is red once with its description",
			read: fakeStatusRead{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
				Context: statusContext, Status: forgejo.StateFailure, Description: "gate: 2 tests failed",
			}}}},
			wantRed:  1,
			wantTail: "gate: 2 tests failed",
			wantLog:  "RED at",
		},
		{
			name: "error is red once with its description",
			read: fakeStatusRead{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
				Context: statusContext, Status: forgejo.StateError, Description: "the workflow could not start",
			}}}},
			wantRed:  1,
			wantTail: "the workflow could not start",
			wantLog:  "RED at",
		},
		{
			name: "a failure beside a success reads red",
			read: fakeStatusRead{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
				{Context: statusContext, Status: forgejo.StateSuccess},
				{Context: statusContext, Status: forgejo.StateFailure, Description: "one job failed"},
			}}},
			wantRed:  1,
			wantTail: "one job failed",
			wantLog:  "RED at",
		},
		{name: "pending is unknown", read: fakeStatusRead{status: combinedFor(statusContext, forgejo.StatePending)}, wantLog: "is now pending"},
		{name: "warning is unknown", read: fakeStatusRead{status: combinedFor(statusContext, forgejo.StateWarning)}, wantLog: "is now warning"},
		{name: "skipped is unknown", read: fakeStatusRead{status: combinedFor(statusContext, forgejo.StateSkipped)}, wantLog: "is now skipped"},
		{name: "missing is unknown", read: fakeStatusRead{status: &forgejo.CombinedStatus{}}, wantLog: "is now missing"},
		{name: "unreadable is unknown", read: fakeStatusRead{err: unreadable}, wantLog: "is now unreadable"},
		{
			name: "a hand-posted green is unknown",
			read: fakeStatusRead{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
				{Context: statusContext, Status: forgejo.StateSuccess, Creator: hand},
			}}},
			wantLog: "is now posted by hand",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			store := &MemoryMainState{}
			clk := &manualClock{now: statusT0}
			reads := &fakeStatusReads{responses: []fakeStatusRead{tc.read}}
			r := v.runner(reads.read, store, clk, 0)
			r.Trigger(context.Background(), pl)
			r.PollPending(context.Background())

			greens, reds, unknowns := v.counts()
			if greens != tc.wantGreen || reds != tc.wantRed {
				t.Fatalf("green %d red %d, want %d/%d", greens, reds, tc.wantGreen, tc.wantRed)
			}
			if unknowns != 0 {
				t.Fatalf("unknown items = %d before the timeout, want none", unknowns)
			}
			if tc.wantRed == 1 {
				if len(v.redTails) != 1 || v.redTails[0] != tc.wantTail {
					t.Fatalf("red tails = %q, want %q", v.redTails, tc.wantTail)
				}
				if len(pendingOf(t, store)) != 0 {
					t.Fatalf("a decided verdict left its pending entry in the store")
				}
			} else if tc.wantGreen == 0 {
				if n := len(pendingOf(t, store)); n != 1 {
					t.Fatalf("an UNKNOWN read left %d pending entries, want the one it waits on", n)
				}
			}
			if logs := v.logText(); !strings.Contains(logs, tc.wantLog) {
				t.Fatalf("logs do not name the reason %q:\n%s", tc.wantLog, logs)
			}
		})
	}
}

func TestPostLandStatusPendingDoesNotBlockAnotherLanding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := PostLand{BeadID: "gt-a", Commit: "aaaa111", Target: "main"}
	b := PostLand{BeadID: "gt-b", Commit: "bbbb222", Target: "main"}
	reads := map[string]*fakeStatusReads{
		a.Commit: {responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}},
		b.Commit: {responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}},
	}
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: statusT0}
	r := v.runner(byCommit(reads), store, clk, 0)
	busy := 0
	r.Busy = func(bool) { busy++ }

	r.Trigger(ctx, a)
	r.PollPending(ctx)
	clk.Advance(time.Minute)
	r.Trigger(ctx, b)
	r.PollPending(ctx)
	if greens, _, _ := v.counts(); greens != 1 {
		t.Fatalf("green %d after B's pass, want B's green", greens)
	}
	if got := reads[b.Commit].count(); got != 1 {
		t.Fatalf("B read %d times, want once", got)
	}
	if got := reads[a.Commit].count(); got != 2 {
		t.Fatalf("A read %d times across two passes a minute apart, want 2", got)
	}

	clk.Advance(10 * time.Second)
	r.PollPending(ctx)
	if got := reads[a.Commit].count(); got != 2 {
		t.Fatalf("A read %d times inside one poll interval, want 2", got)
	}

	clk.Advance(30 * time.Second)
	r.PollPending(ctx)
	if got := reads[a.Commit].count(); got != 3 {
		t.Fatalf("A read %d times after the interval, want 3", got)
	}
	if greens, reds, unknowns := v.counts(); greens != 1 || reds != 0 || unknowns != 0 {
		t.Fatalf("green %d red %d unknown %d, want only B's green", greens, reds, unknowns)
	}
	if left := pendingOf(t, store); len(left) != 1 || left[0].Land.Commit != a.Commit {
		t.Fatalf("pending = %+v, want only A still waiting", left)
	}
	if busy != 0 {
		t.Fatalf("Busy was called %d times: a status wait must not hold an upgrade restart", busy)
	}
}

func TestPostLandStatusPendingSurvivesARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	pending := fakeStatusRead{status: combinedFor(statusContext, forgejo.StatePending)}
	cases := []struct {
		name       string
		after      fakeStatusRead
		wantGreens int
		wantReds   int
		wantUnk    int
	}{
		{name: "green", after: fakeStatusRead{status: combinedFor(statusContext, forgejo.StateSuccess)}, wantGreens: 1},
		{
			name: "red",
			after: fakeStatusRead{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
				Context: statusContext, Status: forgejo.StateFailure, Description: "gate failed",
			}}}},
			wantReds: 1,
		},
		{name: "timeout", after: pending, wantUnk: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			store := &MemoryMainState{}
			clk := &manualClock{now: statusT0}
			before := &fakeStatusReads{responses: []fakeStatusRead{pending}}
			first := v.runner(before.read, store, clk, 3*time.Minute)
			first.Trigger(ctx, pl)
			first.PollPending(ctx)

			// The daemon restarts: a new runner over the same store resumes the wait.
			clk.Advance(time.Minute)
			after := &fakeStatusReads{responses: []fakeStatusRead{pending, tc.after}}
			second := v.runner(after.read, store, clk, 3*time.Minute)
			second.PollPending(ctx)
			clk.Advance(time.Minute)
			second.PollPending(ctx)
			clk.Advance(time.Minute)
			second.PollPending(ctx)

			greens, reds, unknowns := v.counts()
			if greens != tc.wantGreens || reds != tc.wantReds || unknowns != tc.wantUnk {
				t.Fatalf("green %d red %d unknown %d, want %d/%d/%d",
					greens, reds, unknowns, tc.wantGreens, tc.wantReds, tc.wantUnk)
			}
			if n := len(pendingOf(t, store)); n != 0 {
				t.Fatalf("%d pending entries left after the verdict, want none", n)
			}
		})
	}
}

func TestPostLandStatusTimeoutRaisesOneAttentionItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	const timeout = 10 * time.Minute

	cases := []struct {
		name      string
		status    *forgejo.CombinedStatus
		wantState string
	}{
		{name: "pending", status: combinedFor(statusContext, forgejo.StatePending), wantState: "pending"},
		{name: "missing", status: &forgejo.CombinedStatus{}, wantState: "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			store := &MemoryMainState{}
			clk := &manualClock{now: statusT0}
			reads := &fakeStatusReads{responses: []fakeStatusRead{{status: tc.status}}}
			r := v.runner(reads.read, store, clk, timeout)
			r.Trigger(ctx, pl)
			r.PollPending(ctx)

			clk.Advance(timeout - time.Second)
			r.PollPending(ctx)
			if n := v.unknownCount(); n != 0 {
				t.Fatalf("unknown items = %d one second before the timeout, want none", n)
			}

			clk.Advance(time.Second)
			r.PollPending(ctx)
			if n := v.unknownCount(); n != 1 {
				t.Fatalf("unknown items at the timeout = %d, want one", n)
			}
			u := v.unknowns[0]
			if want := "post-land-unknown:gastown:" + pl.Commit; u.Key() != want {
				t.Fatalf("key = %q, want %q", u.Key(), want)
			}
			if u.Rig != "gastown" || u.SHA != pl.Commit || u.Context != statusContext || u.State != tc.wantState {
				t.Fatalf("record = %+v", u)
			}
			if s := u.Summary(); !strings.Contains(s, statusContext) || !strings.Contains(s, tc.wantState) {
				t.Fatalf("summary %q does not name the context and last state", s)
			}

			clk.Advance(time.Minute)
			r.PollPending(ctx)
			if n := v.unknownCount(); n != 1 {
				t.Fatalf("unknown items on a later pass = %d, want still one", n)
			}
		})
	}

	t.Run("a verdict that resolves before the timeout raises none", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		store := &MemoryMainState{}
		clk := &manualClock{now: statusT0}
		reads := &fakeStatusReads{responses: []fakeStatusRead{
			{status: combinedFor(statusContext, forgejo.StatePending)},
			{status: combinedFor(statusContext, forgejo.StateSuccess)},
		}}
		r := v.runner(reads.read, store, clk, timeout)
		r.Trigger(ctx, pl)
		r.PollPending(ctx)
		clk.Advance(time.Minute)
		r.PollPending(ctx)
		clk.Advance(timeout)
		r.PollPending(ctx)
		if greens, _, unknowns := v.counts(); greens != 1 || unknowns != 0 {
			t.Fatalf("green %d unknown %d, want the resolved success and no item", greens, unknowns)
		}
	})
}

func TestPostLandStatusUnrecordedRaisesUnknown(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	cases := []struct {
		name  string
		store failingMainState
	}{
		{name: "the main state cannot be read", store: failingMainState{loadErr: errors.New("permission denied")}},
		{name: "the main state cannot be written", store: failingMainState{saveErr: errors.New("disk full")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &statusVerdicts{}
			reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
			r := v.runner(reads.read, tc.store, &manualClock{now: statusT0}, 0)
			r.Trigger(context.Background(), pl)
			if n := v.unknownCount(); n != 1 {
				t.Fatalf("unknown items = %d, want the silence raised at once", n)
			}
			if u := v.unknowns[0]; u.State != "unrecorded" {
				t.Fatalf("state = %q, want unrecorded", u.State)
			}
			if greens, reds, _ := v.counts(); greens != 0 || reds != 0 {
				t.Fatalf("green %d red %d, want neither callback", greens, reds)
			}
			if logs := v.logText(); !strings.Contains(logs, "WARNING could not record") {
				t.Fatalf("logs do not name the failure:\n%s", logs)
			}
		})
	}
}

func TestPostLandStatusPendingListIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
	r := v.runner(reads.read, store, &manualClock{now: statusT0}, 0)
	for i := range postLandPendingLimit + 1 {
		r.Trigger(ctx, PostLand{Commit: fmt.Sprintf("commit%02d", i), Target: "main"})
	}
	left := pendingOf(t, store)
	if len(left) != postLandPendingLimit {
		t.Fatalf("pending = %d, want the newest %d", len(left), postLandPendingLimit)
	}
	if left[0].Land.Commit != "commit01" || left[len(left)-1].Land.Commit != fmt.Sprintf("commit%02d", postLandPendingLimit) {
		t.Fatalf("pending runs %s..%s, want commit01..the newest", left[0].Land.Commit, left[len(left)-1].Land.Commit)
	}
	if logs := v.logText(); !strings.Contains(logs, "dropping the pending verdict") {
		t.Fatalf("logs do not say the oldest was dropped:\n%s", logs)
	}

	r.Trigger(ctx, PostLand{Commit: fmt.Sprintf("commit%02d", postLandPendingLimit), Target: "main"})
	if n := len(pendingOf(t, store)); n != postLandPendingLimit {
		t.Fatalf("a repeated commit made %d entries, want the same %d", n, postLandPendingLimit)
	}
}

// TestPostLandStatusStoreFailures is the store's failure half: a read that
// fails keeps every pending entry for the next pass instead of reading the
// failure as no verdicts, and after PostLandStatusStoreFailureAfter passes
// running it raises one attention item that the next good pass clears; a write
// that fails keeps the entry and fires its verdict once.
func TestPostLandStatusStoreFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a failed load keeps every entry and raises one item after three passes", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		store := &flakyMainState{}
		clk := &manualClock{now: statusT0}
		if err := store.inner.Save(MainState{PostLandPending: []PendingPostLand{
			{Land: PostLand{Commit: "aaaa111", Target: "main"}, Context: statusContext, Since: statusT0},
			{Land: PostLand{Commit: "bbbb222", Target: "main"}, Context: statusContext, Since: statusT0},
		}}); err != nil {
			t.Fatalf("seeding the store: %v", err)
		}
		store.failLoad(errors.New("permission denied"))
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
		r := v.runner(reads.read, store, clk, time.Hour)

		for pass := 1; pass < PostLandStatusStoreFailureAfter; pass++ {
			r.PollPending(ctx)
			if n := v.storeFailureRaises(); n != 0 {
				t.Fatalf("item raised on failed pass %d, want none before %d", pass, PostLandStatusStoreFailureAfter)
			}
		}
		r.PollPending(ctx)
		if n := v.storeFailureRaises(); n != 1 {
			t.Fatalf("items raised after %d failed passes = %d, want one", PostLandStatusStoreFailureAfter, n)
		}
		f := v.storeFails[0]
		if f.Rig != "gastown" || f.Key() != "post-land-store-failure:gastown" {
			t.Fatalf("record = %+v key %q, want one keyed by the rig", f, f.Key())
		}
		if !strings.Contains(f.Error, "permission denied") {
			t.Fatalf("record error %q does not name the failure", f.Error)
		}
		if !strings.Contains(f.Summary(), "gastown") || !strings.Contains(f.Summary(), "permission denied") {
			t.Fatalf("summary %q does not name the rig and the failure", f.Summary())
		}
		if n := len(store.stored(t)); n != 2 {
			t.Fatalf("%d pending entries after the failed passes, want both kept", n)
		}
		if logs := v.logText(); !strings.Contains(logs, "gastown") || !strings.Contains(logs, "passes running") {
			t.Fatalf("logs do not name the rig and the failed passes:\n%s", logs)
		}

		// A fourth failed pass does not raise a second item.
		r.PollPending(ctx)
		if n := v.storeFailureRaises(); n != 1 {
			t.Fatalf("items raised on a later failed pass = %d, want still one", n)
		}

		// The next good pass keeps the entries and clears the item.
		store.failLoad(nil)
		r.PollPending(ctx)
		if n := v.storeClears(); n != 1 {
			t.Fatalf("clears = %d, want the good pass to clear the item", n)
		}
		if n := len(store.stored(t)); n != 2 {
			t.Fatalf("%d pending entries after the good pass, want both kept", n)
		}
	})

	t.Run("a failed save keeps the entry and fires the verdict once", func(t *testing.T) {
		t.Parallel()
		pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
		v := &statusVerdicts{}
		store := &flakyMainState{}
		clk := &manualClock{now: statusT0}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
		r := v.runner(reads.read, store, clk, time.Hour)
		r.Trigger(ctx, pl)
		store.failSave(errors.New("disk full"))

		r.PollPending(ctx)
		if greens, _, _ := v.counts(); greens != 1 {
			t.Fatalf("green = %d after the read, want one", greens)
		}
		if n := len(store.stored(t)); n != 1 {
			t.Fatalf("%d pending entries after the failed save, want the entry kept", n)
		}

		clk.Advance(time.Minute)
		r.PollPending(ctx)
		if greens, _, _ := v.counts(); greens != 1 {
			t.Fatalf("green = %d across the retry, want the callback fired once", greens)
		}
		if n := len(store.stored(t)); n != 1 {
			t.Fatalf("%d pending entries after the retry, want the entry kept", n)
		}
		if n := v.storeFailureRaises(); n != 0 {
			t.Fatalf("items raised after two failed passes = %d, want none before %d", n, PostLandStatusStoreFailureAfter)
		}

		store.failSave(nil)
		r.PollPending(ctx)
		if greens, _, _ := v.counts(); greens != 1 {
			t.Fatalf("green = %d after the removal, want still one", greens)
		}
		if n := len(store.stored(t)); n != 0 {
			t.Fatalf("%d pending entries after the removal saved, want none", n)
		}
	})
}

// pollCounter is a trigger that counts the passes that polled it.
type pollCounter struct{ polls int }

func (p *pollCounter) Trigger(context.Context, PostLand) {}
func (p *pollCounter) PollPending(context.Context)       { p.polls++ }

// TestPassPollsPendingVerdicts: each worker pass reads the pending verdicts
// once, and a draining worker, which is about to restart, does not.
func TestPassPollsPendingVerdicts(t *testing.T) {
	t.Parallel()
	t.Run("a pass polls once", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		poll := &pollCounter{}
		h.w.PostLand = poll
		h.w.Pass(context.Background())
		if poll.polls != 1 {
			t.Fatalf("polls = %d, want one per pass", poll.polls)
		}
	})
	t.Run("a draining pass does not poll", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		poll := &pollCounter{}
		h.w.PostLand = poll
		h.w.Draining = func() bool { return true }
		h.w.Pass(context.Background())
		if poll.polls != 0 {
			t.Fatalf("polls = %d while draining, want none", poll.polls)
		}
	})
}

func TestPostLandStatusPollStopsOnCancel(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
	r := v.runner(reads.read, store, &manualClock{now: statusT0}, 0)
	r.Trigger(context.Background(), pl)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.PollPending(ctx)
	if got := reads.count(); got != 0 {
		t.Fatalf("a cancelled pass read the status %d times, want none", got)
	}
	if n := len(pendingOf(t, store)); n != 1 {
		t.Fatalf("a cancelled pass left %d entries, want the one still waiting", n)
	}
}

func TestPostLandStatusDescriptionIsSanitized(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	raw := "gate: 2 tests failed\n\x1b[31mFAIL\x1b[0m\r\n" + strings.Repeat("x", 5000)
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{
		Context: statusContext, Status: forgejo.StateFailure, Description: raw,
	}}}}}}
	r := v.runner(reads.read, store, &manualClock{now: statusT0}, 0)
	r.Trigger(context.Background(), pl)
	r.PollPending(context.Background())

	if _, reds, _ := v.counts(); reds != 1 {
		t.Fatalf("red %d, want the failure read red once", reds)
	}
	tail := v.redTails[0]
	if !strings.HasPrefix(tail, "gate: 2 tests failed") {
		t.Fatalf("tail = %q, want the description's first line", tail)
	}
	if n := utf8.RuneCountInString(tail); n > postLandStatusTextRunes {
		t.Fatalf("tail is %d runes, want at most %d", n, postLandStatusTextRunes)
	}
	assertNoControl(t, "red tail", tail)
	if len(v.comments) != 1 {
		t.Fatalf("bead comments = %d, want the red verdict commented once", len(v.comments))
	}
	assertNoControl(t, "bead comment", v.comments[0])
	for _, line := range v.logs {
		assertNoControl(t, "log line", line)
	}
}

// TestPostLandStatusPrunesTheStore: a store holding more pending verdicts than
// the rig keeps has its oldest dropped from the store itself, with a log line,
// so the bound holds even for a list written by an older daemon.
func TestPostLandStatusPrunesTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	entries := make([]PendingPostLand, 0, postLandPendingLimit+1)
	for i := 0; i <= postLandPendingLimit; i++ {
		entries = append(entries, PendingPostLand{
			Land:    PostLand{Commit: fmt.Sprintf("commit%02d", i), Target: "main"},
			Context: statusContext,
			Since:   statusT0,
		})
	}
	store := &MemoryMainState{}
	if err := store.Save(MainState{PostLandPending: entries}); err != nil {
		t.Fatalf("seeding the store: %v", err)
	}
	v := &statusVerdicts{}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
	r := v.runner(reads.read, store, &manualClock{now: statusT0}, time.Hour)
	r.PollPending(ctx)

	left := pendingOf(t, store)
	if len(left) != postLandPendingLimit {
		t.Fatalf("the store holds %d pending verdicts, want the newest %d", len(left), postLandPendingLimit)
	}
	if left[0].Land.Commit != "commit01" || left[len(left)-1].Land.Commit != fmt.Sprintf("commit%02d", postLandPendingLimit) {
		t.Fatalf("the store runs %s..%s, want commit01..the newest", left[0].Land.Commit, left[len(left)-1].Land.Commit)
	}
	for _, e := range left {
		if e.Land.Commit == "commit00" {
			t.Fatalf("the oldest entry is still in the store after the prune")
		}
	}
	if logs := v.logText(); !strings.Contains(logs, "dropping the pending verdict") || !strings.Contains(logs, "commit00") {
		t.Fatalf("logs do not name the dropped entry:\n%s", logs)
	}
}

// TestPostLandStatusReadHasATimeout: each status read runs under a deadline, so
// a Forgejo that never answers is cut off at the named read timeout and the
// pass completes with an UNKNOWN, keeping the entry for a later pass.
func TestPostLandStatusReadHasATimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	clk := &manualClock{now: statusT0}

	// The read answers green when it was given no deadline, so a run without
	// the bound reads a verdict this test refuses.
	stalled := func(got context.Context, _ PostLand) (*forgejo.CombinedStatus, error) {
		if _, ok := got.Deadline(); !ok {
			return combinedFor(statusContext, forgejo.StateSuccess), nil
		}
		<-got.Done()
		return nil, got.Err()
	}
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	r := v.runner(stalled, store, clk, time.Hour)
	r.readTimeoutOverride = time.Nanosecond
	r.Trigger(ctx, pl)
	r.PollPending(ctx)

	if greens, reds, unknowns := v.counts(); greens != 0 || reds != 0 || unknowns != 0 {
		t.Fatalf("green %d red %d unknown %d, want no verdict from a stalled read", greens, reds, unknowns)
	}
	if n := len(pendingOf(t, store)); n != 1 {
		t.Fatalf("%d pending entries, want the stalled read kept for a later pass", n)
	}
	if logs := v.logText(); !strings.Contains(logs, "is now unreadable") {
		t.Fatalf("logs do not say the read was cut off:\n%s", logs)
	}

	// With no override the bound is the named constant.
	var bound time.Duration
	var sawDeadline bool
	reads := func(got context.Context, _ PostLand) (*forgejo.CombinedStatus, error) {
		if deadline, ok := got.Deadline(); ok {
			sawDeadline, bound = true, time.Until(deadline)
		}
		return combinedFor(statusContext, forgejo.StatePending), nil
	}
	r2 := v.runner(reads, &MemoryMainState{}, clk, time.Hour)
	r2.Trigger(ctx, pl)
	r2.PollPending(ctx)
	if !sawDeadline || bound <= 0 || bound > PostLandStatusReadTimeout {
		t.Fatalf("read deadline %v (set %v), want one at most %v away", bound, sawDeadline, PostLandStatusReadTimeout)
	}
}

func TestPostLandStatusContextWinsOverCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}

	t.Run("with both set the command does not run", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
		ran := 0
		r := v.runner(reads.read, &MemoryMainState{}, &manualClock{now: statusT0}, 0)
		r.Command = func() string { return "make test-slow" }
		r.Run = func(context.Context, string, PostLand) PostLandResult {
			ran++
			return PostLandResult{}
		}
		r.Trigger(ctx, pl)
		r.Wait()
		r.PollPending(ctx)
		if ran != 0 {
			t.Fatalf("the command ran %d times with a status context set", ran)
		}
		if greens, _, _ := v.counts(); greens != 1 {
			t.Fatalf("green %d, want the status verdict", greens)
		}
		if logs := v.logText(); !strings.Contains(logs, "not run") {
			t.Fatalf("the log does not say the command was not run:\n%s", logs)
		}
	})

	t.Run("a context alone starts a verdict with no command", func(t *testing.T) {
		t.Parallel()
		v := &statusVerdicts{}
		reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StateSuccess)}}}
		r := v.runner(reads.read, &MemoryMainState{}, &manualClock{now: statusT0}, 0)
		r.Trigger(ctx, pl)
		r.PollPending(ctx)
		if greens, _, _ := v.counts(); greens != 1 {
			t.Fatalf("green %d, want the status verdict", greens)
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
		r.Trigger(ctx, pl)
		r.Wait()
		if ran != 1 {
			t.Fatalf("the command ran %d times with no context, want once", ran)
		}
	})
}

// TestPostLandStatusTimeoutEntryOutlivesAnUnconfirmedAlarm: a timed-out entry is
// marked and kept in the store until OnUnknown confirms the alarm stored. A
// daemon that stops between the mark and the confirmation (or an OnUnknown that
// fails) loses nothing: the restarted runner finds the entry and raises the same
// keyed record again, and once confirmed the entry goes and no second alarm
// follows.
func TestPostLandStatusTimeoutEntryOutlivesAnUnconfirmedAlarm(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	const timeout = 10 * time.Minute
	store := &MemoryMainState{}
	clk := &manualClock{now: statusT0}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}

	// First daemon: the alarm cannot be stored, as when it stops right after
	// the timeout fires.
	first := &statusVerdicts{unknownErr: errors.New("attention store unavailable")}
	r := first.runner(reads.read, store, clk, timeout)
	r.Trigger(ctx, pl)
	r.PollPending(ctx)
	clk.Advance(timeout)
	r.PollPending(ctx)
	if n := first.unknownCount(); n != 1 {
		t.Fatalf("first daemon raised %d alarms at the timeout, want one attempt", n)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PostLandPending) != 1 || !st.PostLandPending[0].TimedOut {
		t.Fatalf("store after an unconfirmed alarm = %+v, want the one entry, marked timed out", st.PostLandPending)
	}

	// Restarted daemon over the same store: the alarm is confirmed this time.
	second := &statusVerdicts{}
	r2 := second.runner(reads.read, store, clk, timeout)
	r2.PollPending(ctx)
	if n := second.unknownCount(); n != 1 {
		t.Fatalf("restarted daemon raised %d alarms, want exactly one", n)
	}
	if got, want := second.unknowns[0].Key(), first.unknowns[0].Key(); got != want {
		t.Fatalf("restarted key %q differs from the first %q: the repeat would not be idempotent", got, want)
	}
	if st, _ := store.Load(); len(st.PostLandPending) != 0 {
		t.Fatalf("store after a confirmed alarm = %+v, want the entry removed", st.PostLandPending)
	}
	r2.PollPending(ctx)
	if n := second.unknownCount(); n != 1 {
		t.Fatalf("alarms after confirmation = %d, want still one", n)
	}
}

// TestPostLandStatusContextWithoutAReaderSaysSoOnce: a configured status context
// on a runner that has no status reader falls back to the command, and the
// runner says so once instead of switching modes silently.
func TestPostLandStatusContextWithoutAReaderSaysSoOnce(t *testing.T) {
	t.Parallel()
	var lines []string
	r := &PostLandRunner{
		Rig:           "gastown",
		StatusContext: func() string { return statusContext },
		Command:       func() string { return "" },
		Logf:          func(format string, a ...any) { lines = append(lines, fmt.Sprintf(format, a...)) },
	}
	for i := 0; i < 3; i++ {
		r.Trigger(context.Background(), PostLand{Commit: "abc1234567890", Target: "main"})
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "no status reader or state store") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("fallback line logged %d times over three triggers, want once:\n%s", n, strings.Join(lines, "\n"))
	}
}
