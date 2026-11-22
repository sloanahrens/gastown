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
		{
			name:  "a zero clock asks nothing and marks nothing",
			clock: time.Time{}, advance: 0,
			responses: []fakeStatusRead{{status: pending}}, end: ended,
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
