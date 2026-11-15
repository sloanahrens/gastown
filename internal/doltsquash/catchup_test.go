package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// sqlCherryPick is the prefix of every pick statement, so a test can script one
// answer for all picks or an exact statement for one commit.
const sqlCherryPick = "CALL DOLT_CHERRY_PICK"

// sqlDoltTail is the prefix of every tail list statement, so a test can pick
// the log reads out of the statements the fake received.
const sqlDoltTail = "SELECT commit_hash FROM dolt_log("

// tailRows answers a tail list with one record per commit, oldest first.
func tailRows(commits ...string) Rows {
	rows := Rows{Columns: []string{"commit_hash"}}
	for _, commit := range commits {
		rows.Data = append(rows.Data, []string{commit})
	}
	return rows
}

// pickedRow is what CALL DOLT_CHERRY_PICK returns for a commit it made: the
// new hash and three zero conflict counts.
func pickedRow() Rows {
	return Rows{
		Columns: []string{"hash", "data_conflicts", "schema_conflicts", "constraint_violations"},
		Data:    [][]string{{hash(0x70), "0", "0", "0"}},
	}
}

// catchupFake answers a replay's branch check and its first read of main's tip;
// a test queues later tip reads and tail lists on top.
func catchupFake(tip string) *FakeExec {
	f := NewFakeExec()
	f.Respond(sqlActiveBranch, single("branch", "compact-work"))
	f.Respond(sqlMainTip, single("dolt_hashof('main')", tip))
	return f
}

// pickStatements returns the pick statements the fake received, in order.
func pickStatements(f *FakeExec) []string {
	var picks []string
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, sqlCherryPick) {
			picks = append(picks, statement)
		}
	}
	return picks
}

// tailRanges returns the revision argument of each tail list the fake
// received, in order, so a test asserts the range the replay read rather than
// the statement it was rendered into.
func tailRanges(t *testing.T, f *FakeExec) []string {
	t.Helper()
	var ranges []string
	for _, statement := range f.Statements() {
		rest, ok := strings.CutPrefix(statement, sqlDoltTail+"'")
		if !ok {
			continue
		}
		revision, _, ok := strings.Cut(rest, "'")
		if !ok {
			t.Fatalf("the tail list %q does not close its revision", statement)
		}
		ranges = append(ranges, revision)
	}
	return ranges
}

// wantPicks returns the pick statement for each commit, in order.
func wantPicks(commits ...string) []string {
	statements := make([]string, 0, len(commits))
	for _, commit := range commits {
		statements = append(statements, pickStatement(commit))
	}
	return statements
}

func TestCatchupStopsAtAQuietTipWithNothingToPick(t *testing.T) {
	t.Parallel()
	from := hash(0x41)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(from)
	f.Respond(tailStatement(from, from), tailRows())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: from, Passes: 2}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
	// One wait, then one more look at the tip: the second look is what shows
	// the tip held still for the window.
	if want := []time.Duration{CatchupQuietTip}; !slices.Equal(sleep.waits, want) {
		t.Errorf("waits = %v, want %v", sleep.waits, want)
	}
}

// TestCatchupLooksAgainAfterTheQuietWait drives the rule's other side: main
// commits inside the quiet window, so the tip an empty pass saw is no longer
// main's tip when the window ends. A replay that stops at the first empty tail
// leaves that commit unpicked.
func TestCatchupLooksAgainAfterTheQuietWait(t *testing.T) {
	t.Parallel()
	from, tip2 := hash(0x41), hash(0x52)
	c3 := hash(0x63)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(from)
	f.Respond(sqlMainTip, single("dolt_hashof('main')", tip2))
	f.Respond(tailStatement(from, from), tailRows())
	f.Respond(tailStatement(from, tip2), tailRows(c3))
	f.Respond(tailStatement(tip2, tip2), tailRows())
	f.Respond(sqlCherryPick, pickedRow())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: tip2, Passes: 4, Picks: 1}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
	if want := []time.Duration{CatchupQuietTip, CatchupQuietTip}; !slices.Equal(sleep.waits, want) {
		t.Errorf("waits = %v, want %v: one window before each empty-tail look", sleep.waits, want)
	}
}

// TestCatchupReplaysTheTailBoundedByTheRecordedTip drives the race that an
// unbounded range loses: main advances between a pass's tip read and its list
// read, so the live log holds a commit the recorded tip does not. The fake
// answers the bounded range with the two commits main held at tip1 and the
// unbounded log with those plus c3, so a replay that lists the live tip picks
// c3 in the first pass and again in the next.
func TestCatchupReplaysTheTailBoundedByTheRecordedTip(t *testing.T) {
	t.Parallel()
	from, tip1, tip2 := hash(0x41), hash(0x51), hash(0x52)
	c1, c2, c3 := hash(0x61), hash(0x62), hash(0x63)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip1)
	f.Respond(sqlMainTip, single("dolt_hashof('main')", tip2))
	f.Respond(tailStatement(from, tip1), tailRows(c1, c2))
	f.Respond("SELECT commit_hash FROM dolt_log('main')", tailRows(c1, c2, c3))
	f.Respond(tailStatement(tip1, tip2), tailRows(c3))
	f.Respond(tailStatement(tip2, tip2), tailRows())
	f.Respond(sqlCherryPick, pickedRow())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: tip2, Passes: 4, Picks: 3}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
	if got, want := pickStatements(f), wantPicks(c1, c2, c3); !slices.Equal(got, want) {
		t.Errorf("picks = %q, want %q: a range read past its recorded tip picks a commit twice", got, want)
	}
	if got, want := tailRanges(t, f), []string{
		from + ".." + tip1,
		tip1 + ".." + tip2,
		tip2 + ".." + tip2,
		// The second look at the empty tail that shows the tip held still.
		tip2 + ".." + tip2,
	}; !slices.Equal(got, want) {
		t.Errorf("the replay read the ranges %q, want %q: every range starts where the last pass ended and ends at the tip that pass recorded", got, want)
	}
}

// TestCatchupCountsQuietWaitsPerStretch drives main while the replay waits:
// main is quiet for one window, lands a commit, and repeats, more times than
// one stretch's bound. Each landing is picked, which starts a new quiet
// stretch; a budget counted across the whole run would stop a replay that is
// keeping up.
func TestCatchupCountsQuietWaitsPerStretch(t *testing.T) {
	t.Parallel()
	const landings = 12
	from := hash(0x41)
	// tip(0) is the tip the replay starts at; tip(i) follows the i-th landing.
	tip := func(i int) string {
		if i == 0 {
			return from
		}
		return hash(0x71 + i)
	}
	commit := func(i int) string { return hash(0x61 + i) }
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(from)
	for i := 1; i <= landings; i++ {
		f.Respond(sqlMainTip, single("dolt_hashof('main')", tip(i)))
		f.Respond(sqlMainTip, single("dolt_hashof('main')", tip(i)))
	}
	// The last tip is read once more, by the pass that finds it quiet.
	f.Respond(sqlMainTip, single("dolt_hashof('main')", tip(landings)))
	f.Respond(tailStatement(from, from), tailRows())
	for i := 1; i <= landings; i++ {
		f.Respond(tailStatement(tip(i-1), tip(i)), tailRows(commit(i)))
		f.Respond(tailStatement(tip(i), tip(i)), tailRows())
	}
	f.Respond(sqlCherryPick, pickedRow())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: tip(landings), Passes: 2*landings + 2, Picks: landings}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
	if want := landings + 1; len(sleep.waits) != want {
		t.Errorf("waits = %v, want %d quiet windows: one per empty pass", sleep.waits, want)
	}
}

func TestCatchupTreatsNothingToCommitAsBenign(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1, c2 := hash(0x61), hash(0x62)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows(c1, c2))
	f.Respond(tailStatement(tip, tip), tailRows())
	f.RespondErr(pickStatement(c1), errors.New("no changes were made, nothing to commit"))
	f.Respond(pickStatement(c2), pickedRow())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: tip, Passes: 3, Picks: 2, Empty: 1}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
}

func TestCatchupRetriesUncommittedChangesWithBackoff(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1 := hash(0x61)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	held := errors.New("cannot cherry-pick with uncommitted changes")
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows(c1))
	f.Respond(tailStatement(tip, tip), tailRows())
	f.RespondErr(pickStatement(c1), held)
	f.RespondErr(pickStatement(c1), held)
	f.Respond(pickStatement(c1), pickedRow())

	got, err := Catchup(context.Background(), f, from, clk, sleep)
	if err != nil {
		t.Fatalf("Catchup: %v", err)
	}
	if want := (CatchupResult{Tip: tip, Passes: 3, Picks: 1, Retries: 2}); got != want {
		t.Errorf("Catchup returned %+v, want %+v", got, want)
	}
	want := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, CatchupQuietTip}
	if !slices.Equal(sleep.waits, want) {
		t.Errorf("waits = %v, want %v", sleep.waits, want)
	}
}

func TestCatchupReturnsUncommittedChangesAfterTheBound(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1 := hash(0x61)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	held := errors.New("cannot cherry-pick with uncommitted changes")
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows(c1))
	f.RespondErr(pickStatement(c1), held)

	_, err := Catchup(context.Background(), f, from, clk, sleep)
	if !errors.Is(err, held) {
		t.Fatalf("Catchup returned %v, want the held working set's error", err)
	}
	for _, want := range []string{pickStatement(c1), "statement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q: the stop must name the statement and the commit", err, want)
		}
	}
	if got, want := len(pickStatements(f)), 1+catchupRetries; got != want {
		t.Errorf("the pick was attempted %d times, want %d: the retry bound is enforced", got, want)
	}
	wantWaits := []time.Duration{
		25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond,
		time.Second, time.Second,
	}
	if !slices.Equal(sleep.waits, wantWaits) {
		t.Errorf("waits = %v, want %v", sleep.waits, wantWaits)
	}
}

func TestCatchupStopsOnAnotherError(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1, c2 := hash(0x61), hash(0x62)
	refused := errors.New("the connection was closed")
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows(c1, c2))
	f.Respond(pickStatement(c1), pickedRow())
	f.RespondErr(pickStatement(c2), refused)

	_, err := Catchup(context.Background(), f, from, clk, sleep)
	if !errors.Is(err, refused) {
		t.Fatalf("Catchup returned %v, want the injected error", err)
	}
	for _, want := range []string{"statement 5 (", pickStatement(c2), c2} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := pickStatements(f); !slices.Equal(got, wantPicks(c1, c2)) {
		t.Errorf("picks = %q, want %q: the run stopped after the failing pick", got, wantPicks(c1, c2))
	}
	if sent := f.Statements(); len(sent) == 0 || sent[len(sent)-1] != pickStatement(c2) {
		t.Errorf("the last statement is %q, want the failing pick: the run must stop there", sent)
	}
	if len(sleep.waits) != 0 {
		t.Errorf("waits = %v, want none: an error that is not a held working set is not retried", sleep.waits)
	}
}

// TestCatchupStopsOnAPickResultThatNamesNoCommit drives the results
// CALL DOLT_CHERRY_PICK returns without a new hash. A conflicting pick is not
// an error at the statement, so the result row is the only thing that shows it.
func TestCatchupStopsOnAPickResultThatNamesNoCommit(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1 := hash(0x61)
	columns := []string{"hash", "data_conflicts", "schema_conflicts", "constraint_violations"}
	tests := []struct {
		name string
		rows Rows
		want string
	}{
		{
			name: "a conflicting pick",
			rows: Rows{Columns: columns, Data: [][]string{{"", "1", "0", "0"}}},
			want: "left conflicts",
		},
		{
			name: "no commit and no conflict",
			rows: Rows{Columns: columns, Data: [][]string{{"", "0", "0", "0"}}},
			want: "returned no commit hash",
		},
		{
			name: "a record one value short",
			rows: Rows{Columns: columns[:3], Data: [][]string{{hash(0x70), "0", "0"}}},
			want: "want the new hash and three conflict counts",
		},
		{
			name: "no record at all",
			rows: Rows{Columns: columns},
			want: "returned 0 records",
		},
		{
			name: "two records",
			rows: Rows{Columns: columns, Data: [][]string{
				{hash(0x70), "0", "0", "0"},
				{hash(0x71), "0", "0", "0"},
			}},
			want: "returned 2 records",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := newTestClock()
			sleep := &testSleeper{clk: clk}
			f := catchupFake(tip)
			f.Respond(tailStatement(from, tip), tailRows(c1))
			f.Respond(pickStatement(c1), tc.rows)

			_, err := Catchup(context.Background(), f, from, clk, sleep)
			if err == nil {
				t.Fatalf("Catchup returned no error, want one holding %q", tc.want)
			}
			for _, want := range []string{tc.want, pickStatement(c1)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
		})
	}
}

func TestCatchupRefusesASessionNotOnCompactWork(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := NewFakeExec()
	f.Respond(sqlActiveBranch, single("branch", "main"))

	_, err := Catchup(context.Background(), f, hash(0x41), clk, sleep)
	if err == nil {
		t.Fatal("Catchup returned no error on a session that is not on compact-work")
	}
	for _, want := range []string{`"main"`, "want compact-work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := len(f.Statements()); got != 1 {
		t.Errorf("the refused replay sent %d statements, want 1: nothing is picked before the branch is checked", got)
	}
}

func TestCatchupRefusesAStartThatIsNotACommitHash(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := NewFakeExec()

	_, err := Catchup(context.Background(), f, "main", clk, sleep)
	if err == nil {
		t.Fatal("Catchup returned no error for a start that is not a commit hash")
	}
	for _, want := range []string{"the commit the replay starts after", "want a 32-character Dolt commit hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := len(f.Statements()); got != 0 {
		t.Errorf("a refused start sent %d statements, want none", got)
	}
}

func TestCatchupRefusesATipThatIsNotACommitHash(t *testing.T) {
	t.Parallel()
	from := hash(0x41)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := NewFakeExec()
	f.Respond(sqlActiveBranch, single("branch", "compact-work"))
	f.Respond(sqlMainTip, single("dolt_hashof('main')", "HEAD"))

	_, err := Catchup(context.Background(), f, from, clk, sleep)
	if err == nil {
		t.Fatal("Catchup returned no error for a tip that is not a commit hash")
	}
	for _, want := range []string{"main's tip", "want a 32-character Dolt commit hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := len(f.Statements()); got != 2 {
		t.Errorf("the refused replay sent %d statements, want 2: no range is read for a tip it cannot trust", got)
	}
}

func TestCatchupRefusesATailCommitThatIsNotAHash(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows("HEAD"))

	_, err := Catchup(context.Background(), f, from, clk, sleep)
	if err == nil {
		t.Fatal("Catchup returned no error for a tail commit that is not a hash")
	}
	for _, want := range []string{"a commit in main's tail", "want a 32-character Dolt commit hash"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if picks := pickStatements(f); len(picks) != 0 {
		t.Errorf("picks = %q, want none: nothing is picked from a tail the run cannot read", picks)
	}
}

func TestCatchupRefusesAnEmptyTailAtAMovedTip(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows())

	_, err := Catchup(context.Background(), f, from, clk, sleep)
	if err == nil {
		t.Fatal("Catchup returned no error for an empty tail at a tip it never picked")
	}
	for _, want := range []string{from, tip, "left unpicked"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
}

func TestCatchupStopsOnAReadError(t *testing.T) {
	t.Parallel()
	from := hash(0x41)
	refused := errors.New("the connection was closed")
	tests := []struct {
		name   string
		failAt int
	}{
		{"the branch check", 1},
		{"the tip read", 2},
		{"the tail list", 3},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := newTestClock()
			sleep := &testSleeper{clk: clk}
			f := catchupFake(from)
			f.Respond(tailStatement(from, from), tailRows())
			f.FailAt(tc.failAt, refused)

			_, err := Catchup(context.Background(), f, from, clk, sleep)
			if !errors.Is(err, refused) {
				t.Fatalf("Catchup returned %v, want the injected error", err)
			}
			if want := fmt.Sprintf("statement %d (", tc.failAt); !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if got := len(f.Statements()); got != tc.failAt {
				t.Errorf("the run sent %d statements, want %d: it stopped at the failed read", got, tc.failAt)
			}
		})
	}
}

// movingTail is an Exec whose main tip moves one commit on every read, so
// every pass finds a commit to pick and no tip ever holds still.
type movingTail struct {
	n int
}

func (m *movingTail) Exec(ctx context.Context, statement string) (Rows, error) {
	switch {
	case strings.HasPrefix(statement, sqlActiveBranch):
		return single("branch", "compact-work"), nil
	case strings.HasPrefix(statement, sqlMainTip):
		m.n++
		return single("dolt_hashof('main')", hash(m.n)), nil
	case strings.HasPrefix(statement, sqlDoltTail):
		return tailRows(hash(m.n)), nil
	case strings.HasPrefix(statement, sqlCherryPick):
		return pickedRow(), nil
	}
	return Rows{}, fmt.Errorf("movingTail: unscripted statement %q", statement)
}

// TestCatchupStopsAtThePassBound drives a main that moves on every read: no
// tip holds still, so the replay stops at the pass bound rather than reading
// tips without end.
func TestCatchupStopsAtThePassBound(t *testing.T) {
	t.Parallel()
	from := hash(0x41)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	ex := &movingTail{}

	got, err := Catchup(context.Background(), ex, from, clk, sleep)
	if err == nil {
		t.Fatalf("Catchup returned %+v, want an error: main never held still", got)
	}
	for _, want := range []string{fmt.Sprintf("read main's tip %d times", catchupMaxPasses), "not holding still", hash(ex.n)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got.Passes != catchupMaxPasses {
		t.Errorf("the replay read main's tip %d times, want %d: the pass bound ends it", got.Passes, catchupMaxPasses)
	}
}

// cancelAfterExec cancels a run's context once statement n has been answered,
// so a test reaches the replay's between-passes check.
type cancelAfterExec struct {
	ex     Exec
	cancel context.CancelFunc
	after  int
	n      int
}

func (c *cancelAfterExec) Exec(ctx context.Context, statement string) (Rows, error) {
	rows, err := c.ex.Exec(ctx, statement)
	c.n++
	if c.n == c.after {
		c.cancel()
	}
	return rows, err
}

func TestCatchupStopsWhenItsContextEndsBetweenPasses(t *testing.T) {
	t.Parallel()
	from, tip := hash(0x41), hash(0x51)
	c1 := hash(0x61)
	clk := newTestClock()
	sleep := &testSleeper{clk: clk}
	f := catchupFake(tip)
	f.Respond(tailStatement(from, tip), tailRows(c1))
	f.Respond(sqlCherryPick, pickedRow())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The context ends after the first pass's pick, so the loop stops at its
	// next check rather than reading main's tip again.
	wrapped := &cancelAfterExec{ex: f, cancel: cancel, after: 4}

	_, err := Catchup(ctx, wrapped, from, clk, sleep)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Catchup returned %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "between passes") {
		t.Errorf("error %q does not say the replay stopped between passes", err)
	}
	if got := len(pickStatements(f)); got != 1 {
		t.Errorf("the cancelled replay attempted %d picks, want 1: it stopped at the next pass", got)
	}
}

// clockStepSleeper is the package's sleeper with a fixed step: Sleep records
// the wait and advances the clock by step instead of by the wait, so a table
// drives the quiet rule to, past and short of the window.
type clockStepSleeper struct {
	clk   *clockwork.FakeClock
	step  time.Duration
	waits []time.Duration
}

func (s *clockStepSleeper) Sleep(d time.Duration) {
	s.waits = append(s.waits, d)
	s.clk.Advance(s.step)
}

// TestCatchupQuietTipOnTheInjectedClock is the quiet-tip rule's table, driven
// through Catchup: an empty tail at the tip the replay reached ends the replay
// only once the clock has advanced CatchupQuietTip past that observation. A
// clock that never gets there is stopped by the wait bound instead of looping.
func TestCatchupQuietTipOnTheInjectedClock(t *testing.T) {
	t.Parallel()
	from := hash(0x41)
	tests := []struct {
		name string
		step time.Duration
		// ends is whether the empty tail at that step is the quiet tip.
		ends bool
	}{
		{"exactly the window", CatchupQuietTip, true},
		{"past the window", 2 * CatchupQuietTip, true},
		{"before the window", CatchupQuietTip - time.Millisecond, false},
		{"a clock that stands still", 0, false},
		{"a clock that steps back", -CatchupQuietTip, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := newTestClock()
			sleep := &clockStepSleeper{clk: clk, step: tc.step}
			f := catchupFake(from)
			f.Respond(tailStatement(from, from), tailRows())

			got, err := Catchup(context.Background(), f, from, clk, sleep)
			if tc.ends {
				if err != nil {
					t.Fatalf("Catchup: %v", err)
				}
				// The first empty pass is not the quiet tip: the replay ends on
				// the second look at the same tip, not the first.
				if want := (CatchupResult{Tip: from, Passes: 2}); got != want {
					t.Errorf("Catchup returned %+v, want %+v", got, want)
				}
				if want := []time.Duration{CatchupQuietTip}; !slices.Equal(sleep.waits, want) {
					t.Errorf("waits = %v, want %v", sleep.waits, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("Catchup returned %+v, want an error: a clock that does not reach the quiet window cannot end the replay", got)
			}
			for _, want := range []string{from, "quiet windows"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not hold %q", err, want)
				}
			}
			if want := catchupQuietWaits + 1; got.Passes != want {
				t.Errorf("the replay read main's tip %d times, want %d: it waited out the bound", got.Passes, want)
			}
			if want := catchupQuietWaits; len(sleep.waits) != want {
				t.Errorf("waits = %v, want %d quiet windows", sleep.waits, want)
			}
		})
	}
}
