package landworker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

func TestPostLandOrphanMark(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		ends   []RunEnd
		marked []bool
		at     time.Time
		run    string
	}{
		{"an ended answer marks once", []RunEnd{{Run: 11205, Status: "cancelled", Ended: true}}, []bool{true}, t0, "11205 cancelled"},
		{"a second ended answer keeps the first", []RunEnd{{Run: 11205, Status: "cancelled", Ended: true}, {Run: 11206, Status: "failure", Ended: true}}, []bool{true, false}, t0, "11205 cancelled"},
		{"an unended answer marks nothing", []RunEnd{{Run: 11205, Status: "running"}}, []bool{false}, time.Time{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var e PendingPostLand
			for i, end := range tt.ends {
				if got := e.markOrphan(t0.Add(time.Duration(i)*time.Minute), end); got != tt.marked[i] {
					t.Fatalf("answer %d marked = %v, want %v", i+1, got, tt.marked[i])
				}
			}
			if !e.OrphanedAt.Equal(tt.at) || e.OrphanRun != tt.run {
				t.Errorf("marked at %v as %q, want %v as %q", e.OrphanedAt, e.OrphanRun, tt.at, tt.run)
			}
		})
	}
}

func TestPostLandOrphanFieldsSurviveJSON(t *testing.T) {
	t.Parallel()
	e := PendingPostLand{Land: PostLand{Commit: "abc1234"}, OrphanedAt: time.Date(2026, 11, 21, 12, 10, 0, 0, time.UTC), OrphanRun: "11205 cancelled"}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PendingPostLand
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OrphanedAt.Equal(e.OrphanedAt) || got.OrphanRun != e.OrphanRun {
		t.Errorf("after a round trip: OrphanedAt %v, OrphanRun %q; want %v and %q", got.OrphanedAt, got.OrphanRun, e.OrphanedAt, e.OrphanRun)
	}
}

// orphanSeam is a RunEnded that answers from a script, counts its asks and
// records the deadline each ask was given.
type orphanSeam struct {
	mu          sync.Mutex
	end         RunEnd
	err         error
	block       bool
	asks        int
	hasDeadline bool
	bound       time.Duration
}

func (s *orphanSeam) ask(ctx context.Context, _ PostLand) (RunEnd, error) {
	s.mu.Lock()
	s.asks++
	if d, ok := ctx.Deadline(); ok {
		s.hasDeadline, s.bound = true, time.Until(d)
	}
	block, end, err := s.block, s.end, s.err
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return RunEnd{}, ctx.Err()
	}
	return end, err
}

func (s *orphanSeam) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asks
}

// pollWithin runs one worker pass and fails rather than hang when the pass does
// not return before its guard ends.
func pollWithin(t *testing.T, r *PostLandRunner, ctx context.Context) {
	t.Helper()
	guard, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The poll runs on a context the guard cancels, so a poll that hangs does not outlive the test.
	pollCtx, cancelPoll := context.WithCancel(ctx)
	defer cancelPoll()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PollPending(pollCtx)
	}()
	select {
	case <-done:
	case <-guard.Done():
		cancelPoll()
		t.Fatalf("PollPending did not return within its guard: %v", guard.Err())
	}
}

func TestPostLandOrphanAsk(t *testing.T) {
	t.Parallel()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	ended := RunEnd{Run: 11205, Status: "cancelled", Ended: true}
	pending := combinedFor(statusContext, forgejo.StatePending)
	green := combinedFor(statusContext, forgejo.StateSuccess)
	red := &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{{Context: statusContext, Status: forgejo.StateFailure, Description: "gate failed"}}}
	missing := &forgejo.CombinedStatus{}

	tests := []struct {
		name       string
		clock      time.Time
		advance    time.Duration
		responses  []fakeStatusRead
		end        RunEnd
		passes     int
		wantAsks   int
		wantAt     time.Time
		wantOrphan string
		wantGreens int
		wantReds   int
		wantLeft   int
	}{
		{
			name:  "an ended run after the window marks the entry once and logs once",
			clock: t0, advance: PostLandOrphanAfter,
			responses: []fakeStatusRead{{status: pending}}, end: ended,
			passes: 2, wantAsks: 1, wantAt: t0.Add(PostLandOrphanAfter), wantOrphan: "11205 cancelled", wantLeft: 1,
		},
		{
			name:  "before the window the seam is not asked",
			clock: t0, advance: PostLandOrphanAfter - time.Second,
			responses: []fakeStatusRead{{status: pending}}, end: ended,
			passes: 1, wantAsks: 0, wantLeft: 1,
		},
		{
			name:  "a green read at the window is not marked",
			clock: t0, advance: PostLandOrphanAfter,
			responses: []fakeStatusRead{{status: green}}, end: ended,
			passes: 1, wantAsks: 0, wantGreens: 1, wantLeft: 0,
		},
		{
			name:  "a red read at the window is not marked",
			clock: t0, advance: PostLandOrphanAfter,
			responses: []fakeStatusRead{{status: red}}, end: ended,
			passes: 1, wantAsks: 0, wantReds: 1, wantLeft: 0,
		},
		{
			name:  "a read that is not pending is not marked",
			clock: t0, advance: PostLandOrphanAfter,
			responses: []fakeStatusRead{{status: missing}}, end: ended,
			passes: 1, wantAsks: 0, wantLeft: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			v := &statusVerdicts{}
			store := &MemoryMainState{}
			clk := &manualClock{now: tt.clock}
			seam := &orphanSeam{end: tt.end}
			r := v.runner((&fakeStatusReads{responses: tt.responses}).read, store, clk, 0)
			r.RunEnded = seam.ask

			r.Trigger(ctx, pl)
			clk.Advance(tt.advance)
			for i := 0; i < tt.passes; i++ {
				if i > 0 {
					clk.Advance(PostLandStatusPollInterval)
				}
				pollWithin(t, r, ctx)
			}

			if got := seam.count(); got != tt.wantAsks {
				t.Fatalf("the seam was asked %d times, want %d", got, tt.wantAsks)
			}
			if tt.wantAsks > 0 && (!seam.hasDeadline || seam.bound <= 0 || seam.bound > PostLandStatusReadTimeout) {
				t.Fatalf("the ask had deadline %v (seen %v), want one at most %v away", seam.bound, seam.hasDeadline, PostLandStatusReadTimeout)
			}
			if greens, reds, _ := v.counts(); greens != tt.wantGreens || reds != tt.wantReds {
				t.Fatalf("green %d red %d, want %d/%d", greens, reds, tt.wantGreens, tt.wantReds)
			}
			left := pendingOf(t, store)
			if len(left) != tt.wantLeft {
				t.Fatalf("%d pending entries left, want %d: %+v", len(left), tt.wantLeft, left)
			}
			if len(left) == 1 {
				e := left[0]
				if got := e.OrphanRun; got != tt.wantOrphan {
					t.Fatalf("orphan run %q, want %q", got, tt.wantOrphan)
				}
				if marked := !e.OrphanedAt.IsZero(); marked != (tt.wantOrphan != "") {
					t.Fatalf("orphaned at %v, want marked=%v", e.OrphanedAt, tt.wantOrphan != "")
				}
				if tt.wantOrphan != "" && !e.OrphanedAt.Equal(tt.wantAt) {
					t.Fatalf("orphaned at %v, want %v", e.OrphanedAt, tt.wantAt)
				}
			}
			wantLogs := 0
			if tt.wantOrphan != "" {
				wantLogs = 1
			}
			if got := strings.Count(v.logText(), "orphaned at "); got != wantLogs {
				t.Fatalf("%d orphan lines, want %d:\n%s", got, wantLogs, v.logText())
			}
			if tt.wantOrphan != "" {
				want := "orphaned at abc12345: run " + tt.wantOrphan + " ended with the status still pending"
				if logs := v.logText(); !strings.Contains(logs, want) {
					t.Fatalf("logs do not carry %q:\n%s", want, logs)
				}
			}
		})
	}
}

func TestPostLandOrphanAskIsBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	pending := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "cancelled", Ended: true}, block: true}
	r := v.runner(pending.read, store, clk, 0)
	r.readTimeoutOverride = time.Nanosecond
	r.RunEnded = seam.ask

	r.Trigger(ctx, pl)
	clk.Advance(PostLandOrphanAfter)
	pollWithin(t, r, ctx)

	if got := seam.count(); got != 1 {
		t.Fatalf("the seam was asked %d times, want once", got)
	}
	left := pendingOf(t, store)
	if len(left) != 1 || !left[0].OrphanedAt.IsZero() {
		t.Fatalf("pending = %+v, want the entry unmarked by an ask that was cut off", left)
	}
	if logs := v.logText(); !strings.Contains(logs, "keeps waiting") {
		t.Fatalf("logs do not say the ask was cut off:\n%s", logs)
	}
}

// TestPostLandOrphanLogFollowsTheSave: the orphaned line is logged only once the
// mark is saved. A save that fails leaves the stored entry unmarked, so the next
// pass asks and marks again: a line logged at the mark would then appear twice for
// one orphan (gt-xgjwe0, rejection 1).
func TestPostLandOrphanLogFollowsTheSave(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	v := &statusVerdicts{}
	store := &flakyMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "cancelled", Ended: true}}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
	r := v.runner(reads.read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, pl)
	clk.Advance(PostLandOrphanAfter)

	store.failSave(errors.New("disk full"))
	pollWithin(t, r, ctx)
	if got := strings.Count(v.logText(), "orphaned at "); got != 0 {
		t.Fatalf("%d orphaned lines after a failed save, want none:\n%s", got, v.logText())
	}
	if marked := store.stored(t)[0].OrphanedAt; !marked.IsZero() {
		t.Fatalf("the store holds a mark %v after a failed save", marked)
	}

	store.failSave(nil)
	clk.Advance(PostLandStatusPollInterval)
	pollWithin(t, r, ctx)
	if got := strings.Count(v.logText(), "orphaned at "); got != 1 {
		t.Fatalf("%d orphaned lines after the save succeeded, want one:\n%s", got, v.logText())
	}
	if seam.count() != 2 {
		t.Fatalf("the seam was asked %d times, want 2 (the failed pass and the pass that saved)", seam.count())
	}
}

// TestPostLandOrphanBudget: an ask the pass budget ended is not a seam failure.
// A pass whose context ends as the ask starts logs the budget ending and not the
// WARNING line; a seam that returns another error keeps the WARNING line
// (gt-2r4okg). The budget row ends its pass through an injected passContext, not
// a real clock, so the ask starts on any host (gt-jpmyud).
func TestPostLandOrphanBudget(t *testing.T) {
	t.Parallel()
	const budgetLine = "the pass budget ended while asking whether the run behind abc12345 ended; asked again on a later pass"
	const warningLine = "WARNING asking whether the run behind abc12345 (landed by gt-abc) ended: seam down; the entry keeps waiting"
	tests := []struct {
		name    string
		seam    *orphanSeam
		endsAsk bool
		budget  time.Duration
		want    string
		notWant string
	}{
		{"a budget that ends the ask is not a seam failure", &orphanSeam{block: true}, true, 0, budgetLine, "WARNING asking"},
		{"another error keeps the WARNING", &orphanSeam{err: errors.New("seam down")}, false, time.Minute, warningLine, "the pass budget ended"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			v := &statusVerdicts{}
			store := &MemoryMainState{}
			clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
			reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
			r := v.runner(reads.read, store, clk, time.Hour)
			if tt.endsAsk {
				// The ask cancels the pass context as it starts, so the seam's wait ends
				// at once and the budget, not a clock, ends the ask on any host.
				var endPass context.CancelFunc
				r.passContext = func(parent context.Context) (context.Context, context.CancelFunc) {
					pass, cancel := context.WithCancel(parent)
					endPass = cancel
					return pass, cancel
				}
				r.RunEnded = func(ctx context.Context, pl PostLand) (RunEnd, error) {
					endPass()
					return tt.seam.ask(ctx, pl)
				}
			} else {
				r.passBudgetOverride = tt.budget
				r.RunEnded = tt.seam.ask
			}
			r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
			clk.Advance(PostLandOrphanAfter)

			pollWithin(t, r, ctx)

			logs := v.logText()
			if !strings.Contains(logs, tt.want) {
				t.Fatalf("logs do not carry %q:\n%s", tt.want, logs)
			}
			if strings.Contains(logs, tt.notWant) {
				t.Fatalf("logs carry %q, want it absent:\n%s", tt.notWant, logs)
			}
		})
	}
}

// TestPostLandOrphanLogSanitizesTheStatus: the run status comes from Forgejo, so a
// control character in it never reaches the orphaned line: the line formats the
// status through sanitizeStatusText like the WARNING path does.
func TestPostLandOrphanLogSanitizesTheStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "cancel\nled\x1b[31m", Ended: true}}
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
	r := v.runner(reads.read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, pl)
	clk.Advance(PostLandOrphanAfter)

	pollWithin(t, r, ctx)

	logs := v.logText()
	if !strings.Contains(logs, "orphaned at abc12345: run 11205 cancel led [31m ended with the status still pending") {
		t.Fatalf("the orphaned line does not carry the sanitized status:\n%q", logs)
	}
	if strings.ContainsAny(logs, "\x1b") {
		t.Fatalf("a control character reached the log: %q", logs)
	}
}

// TestPostLandOrphanAskBackoffIsPerEntry: a seam that answers not-ended, or fails, is
// asked about an entry at most once per PostLandOrphanAskEvery, so a long pending list
// does not cost one Forgejo run list per entry on every 30 s pass (gt-1rmtiy).
func TestPostLandOrphanAskBackoffIsPerEntry(t *testing.T) {
	t.Parallel()
	answers := []struct {
		name string
		end  RunEnd
		err  error
	}{
		{"a not-ended answer", RunEnd{Run: 11205, Status: "running"}, nil},
		{"an error", RunEnd{}, errors.New("forgejo answered 500")},
	}
	for _, tt := range answers {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
			v := &statusVerdicts{}
			clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
			seam := &orphanSeam{end: tt.end, err: tt.err}
			reads := &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
			r := v.runner(reads.read, &MemoryMainState{}, clk, 0)
			r.RunEnded = seam.ask
			r.Trigger(ctx, pl)
			clk.Advance(PostLandOrphanAfter)

			pollWithin(t, r, ctx)
			clk.Advance(PostLandStatusPollInterval)
			pollWithin(t, r, ctx)
			if got := seam.count(); got != 1 {
				t.Fatalf("%s: the seam was asked %d times inside the ask interval, want 1", tt.name, got)
			}
			clk.Advance(PostLandOrphanAskEvery)
			pollWithin(t, r, ctx)
			if got := seam.count(); got != 2 {
				t.Fatalf("%s: the seam was asked %d times after the ask interval, want 2", tt.name, got)
			}
		})
	}
}

// TestPostLandOrphanFutureStamps: a stamp later than the clock, from a clock
// that stepped back or a state file written by a runner whose clock was ahead,
// counts as waited: the entry is asked at the next due read, the ask repairs
// the stamp, and the ask interval runs from the repair. An entry asked exactly
// PostLandOrphanAskEvery ago is asked again, one asked a second less is not
// (gt-ctavmh).
func TestPostLandOrphanFutureStamps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pl := PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"}
	t0 := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		entry   PendingPostLand
		at      time.Time
		asks    []int // seam asks after each pass, one pass per entry and PostLandOrphanAskEvery apart
		stamped bool  // the first pass asks and so writes OrphanAsked = the clock
	}{
		{
			name:    "an OrphanAsked after the clock",
			entry:   PendingPostLand{Land: pl, Context: statusContext, Since: t0, OrphanAsked: t0.Add(time.Hour)},
			at:      t0.Add(PostLandOrphanAfter),
			asks:    []int{1, 2},
			stamped: true,
		},
		{
			name:    "a Since after the clock",
			entry:   PendingPostLand{Land: pl, Context: statusContext, Since: t0.Add(24 * time.Hour)},
			at:      t0,
			asks:    []int{1, 2},
			stamped: true,
		},
		{
			name:  "asked exactly the interval ago",
			entry: PendingPostLand{Land: pl, Context: statusContext, Since: t0.Add(-PostLandOrphanAfter), OrphanAsked: t0.Add(-PostLandOrphanAskEvery)},
			at:    t0,
			asks:  []int{1},
		},
		{
			name:  "asked a second less",
			entry: PendingPostLand{Land: pl, Context: statusContext, Since: t0.Add(-PostLandOrphanAfter), OrphanAsked: t0.Add(-PostLandOrphanAskEvery + time.Second)},
			at:    t0,
			asks:  []int{0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &MemoryMainState{}
			if err := store.Save(MainState{PostLandPending: []PendingPostLand{tt.entry}}); err != nil {
				t.Fatalf("seeding the store: %v", err)
			}
			clk := &manualClock{now: tt.at}
			seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "running"}}
			r := (&statusVerdicts{}).runner(orphanPendingRead().read, store, clk, 0)
			r.RunEnded = seam.ask

			for i, want := range tt.asks {
				if i > 0 {
					clk.Advance(PostLandOrphanAskEvery)
				}
				pollWithin(t, r, ctx)
				if got := seam.count(); got != want {
					t.Fatalf("pass %d: the seam was asked %d times, want %d", i+1, got, want)
				}
				if i == 0 && tt.stamped {
					if left := pendingOf(t, store); len(left) != 1 || !left[0].OrphanAsked.Equal(tt.at) {
						t.Fatalf("pending = %+v, want the ask to stamp OrphanAsked at %v", left, tt.at)
					}
				}
			}
		})
	}
}

func TestPostLandOrphanAskBackoffSurvivesJSON(t *testing.T) {
	t.Parallel()
	e := PendingPostLand{Land: PostLand{Commit: "abc1234"}, OrphanAsked: time.Date(2026, 11, 21, 12, 10, 0, 0, time.UTC)}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got PendingPostLand
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.OrphanAsked.Equal(e.OrphanAsked) {
		t.Errorf("after a round trip OrphanAsked = %v, want %v", got.OrphanAsked, e.OrphanAsked)
	}
}

// orphanPendingRead is the status source every orphan test starts from: one
// pending read that repeats, so the entry keeps waiting between passes.
func orphanPendingRead() *fakeStatusReads {
	return &fakeStatusReads{responses: []fakeStatusRead{{status: combinedFor(statusContext, forgejo.StatePending)}}}
}

// TestPostLandOrphanLiveRun: a seam that answers a live run leaves the entry
// unmarked. Only an ended run is an orphan: a check that can still report a
// verdict keeps waiting (gt-vdudjh).
func TestPostLandOrphanLiveRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "running"}}
	r := v.runner(orphanPendingRead().read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
	clk.Advance(PostLandOrphanAfter)

	pollWithin(t, r, ctx)

	if got := seam.count(); got != 1 {
		t.Fatalf("the seam was asked %d times, want 1", got)
	}
	left := pendingOf(t, store)
	if len(left) != 1 || !left[0].OrphanedAt.IsZero() || left[0].OrphanRun != "" {
		t.Fatalf("pending = %+v, want the entry unmarked for a live run", left)
	}
	if logs := v.logText(); strings.Contains(logs, "orphaned at ") {
		t.Fatalf("a live run was logged as orphaned:\n%s", logs)
	}
}

// TestPostLandOrphanNilSeam: a runner with no run seam checks nothing. The pass
// still reads the status and leaves the entry unmarked, so the wait goes on as
// it did before the seam existed (gt-vdudjh).
func TestPostLandOrphanNilSeam(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	reads := orphanPendingRead()
	r := v.runner(reads.read, store, clk, 0)
	r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
	clk.Advance(PostLandOrphanAfter)

	pollWithin(t, r, ctx)

	if got := reads.count(); got != 1 {
		t.Fatalf("the status was read %d times, want 1", got)
	}
	left := pendingOf(t, store)
	if len(left) != 1 || !left[0].OrphanedAt.IsZero() || left[0].OrphanRun != "" {
		t.Fatalf("pending = %+v, want the entry unmarked with no seam to ask", left)
	}
}

// TestPostLandOrphanErrorLeavesUnmarked: a seam error is not an end. The entry
// stays unmarked, the WARNING says the entry keeps waiting, and once the ask
// interval has passed a later due read asks the seam again (gt-vdudjh).
func TestPostLandOrphanErrorLeavesUnmarked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	seam := &orphanSeam{err: errors.New("forgejo answered 500")}
	r := v.runner(orphanPendingRead().read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
	clk.Advance(PostLandOrphanAfter)

	pollWithin(t, r, ctx)

	if got := seam.count(); got != 1 {
		t.Fatalf("the seam was asked %d times at the window, want 1", got)
	}
	left := pendingOf(t, store)
	if len(left) != 1 || !left[0].OrphanedAt.IsZero() || left[0].OrphanRun != "" {
		t.Fatalf("pending = %+v, want the entry unmarked after a seam error", left)
	}
	if logs := v.logText(); !strings.Contains(logs, "the entry keeps waiting") {
		t.Fatalf("logs do not say the entry keeps waiting:\n%s", logs)
	}

	clk.Advance(PostLandOrphanAskEvery)
	pollWithin(t, r, ctx)

	if got := seam.count(); got != 2 {
		t.Fatalf("the seam was asked %d times after the ask interval, want 2", got)
	}
	if left := pendingOf(t, store); len(left) != 1 || !left[0].OrphanedAt.IsZero() {
		t.Fatalf("pending = %+v, want the entry still unmarked while the seam errors", left)
	}
}

// TestPostLandOrphanGreenDecided: a green read at the orphan window decides the
// entry as before and the seam is never asked: a decided read is not evidence
// the run is silent (gt-vdudjh).
func TestPostLandOrphanGreenDecided(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{now: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)}
	green := combinedFor(statusContext, forgejo.StateSuccess)
	reads := &fakeStatusReads{responses: []fakeStatusRead{{status: green}}}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "cancelled", Ended: true}}
	r := v.runner(reads.read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})
	clk.Advance(PostLandOrphanAfter)

	pollWithin(t, r, ctx)

	if got := seam.count(); got != 0 {
		t.Fatalf("the seam was asked %d times for a green verdict, want 0", got)
	}
	if greens, _, _ := v.counts(); greens != 1 {
		t.Fatalf("green verdicts = %d, want 1", greens)
	}
	if left := pendingOf(t, store); len(left) != 0 {
		t.Fatalf("%d pending entries left after a green verdict, want 0: %+v", len(left), left)
	}
}

// TestPostLandOrphanZeroClock: a zero clock is no clock to mark by, so the seam
// is not asked and nothing is marked. Moved from the gt-xgjwe0 table row.
func TestPostLandOrphanZeroClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	v := &statusVerdicts{}
	store := &MemoryMainState{}
	clk := &manualClock{}
	seam := &orphanSeam{end: RunEnd{Run: 11205, Status: "cancelled", Ended: true}}
	r := v.runner(orphanPendingRead().read, store, clk, 0)
	r.RunEnded = seam.ask
	r.Trigger(ctx, PostLand{BeadID: "gt-abc", Commit: "abc1234567890", Target: "main"})

	pollWithin(t, r, ctx)

	if got := seam.count(); got != 0 {
		t.Fatalf("the seam was asked %d times with a zero clock, want 0", got)
	}
	left := pendingOf(t, store)
	if len(left) != 1 || !left[0].OrphanedAt.IsZero() {
		t.Fatalf("pending = %+v, want the entry unmarked with no clock", left)
	}
}
