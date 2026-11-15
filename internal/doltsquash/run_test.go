package doltsquash

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// runFixture is the database a run's tests script: main at the BEFORE record's
// tip, later at the tip the catch-up reaches with one picked commit, a table
// the writer added a row to, and one commit newer than the cut on both sides.
// work overrides the tables verification reads on compact-work, so a test can
// make compact-work differ from the record; nil means it matches after.
type runFixture struct {
	before  Before
	after   Before
	work    []TableState
	initial string
	picked  string
	made    string
	newer   []commitIdentity
	old     int64
}

func newRunFixture() runFixture {
	return runFixture{
		before: Before{
			Tip:     hash(0x01),
			Commits: 12002,
			Tables: []TableState{
				{Table: "comments", Rows: 3, Hash: hash(0x02)},
				{Table: "issues", Rows: 9, Hash: hash(0x03)},
			},
		},
		after: Before{
			Tip:     hash(0x04),
			Commits: 12003,
			Tables: []TableState{
				{Table: "comments", Rows: 4, Hash: hash(0x05)},
				{Table: "issues", Rows: 9, Hash: hash(0x03)},
			},
		},
		initial: hash(0x09),
		picked:  hash(0x21),
		made:    hash(0x22),
		newer:   []commitIdentity{{AuthorDate: "2026-11-12 09:30:00.000", Message: "fix: b"}},
		old:     1,
	}
}

// workTables returns the tables verification reads on compact-work.
func (fx runFixture) workTables() []TableState {
	if fx.work != nil {
		return fx.work
	}
	return fx.after.Tables
}

// fake answers one whole run: the two BEFORE records, the rebase, a catch-up
// that picks one commit and finds a quiet tip, verification, and a swap with no
// tail. Every answer is queued in the order the steps ask for it, and the fake
// refuses a statement no queue answers, so a run that reads or writes something
// else fails.
func (fx runFixture) fake() *FakeExec {
	f := NewFakeExec()
	// The branch the session is on, in the order the steps check it: each record
	// on main, the catch-up and verification on compact-work, and the swap's
	// check before and after its renames.
	for _, branch := range []string{"main", "compact-work", "main", "compact-work", "compact-work", "main"} {
		f.Respond(sqlActiveBranch, single("branch", branch))
	}
	f.Respond(sqlCheckoutMain, Rows{})
	f.Respond(sqlMainTip, single("dolt_hashof('main')", fx.before.Tip))
	f.Respond(sqlMainTip, single("dolt_hashof('main')", fx.after.Tip))
	f.Respond(sqlMainCommits, single("n", strconv.FormatInt(fx.before.Commits, 10)))
	f.Respond(sqlMainCommits, single("n", strconv.FormatInt(fx.after.Commits, 10)))
	f.Respond(sqlShowTables, Rows{
		Columns: []string{"Tables_in_gt"},
		Data:    [][]string{{"comments"}, {"dolt_ignore"}, {"issues"}},
	})
	f.Respond(sqlIgnoreComments, single("n", "0"))
	f.Respond(sqlIgnoreIssues, single("n", "0"))
	for _, states := range [][]TableState{fx.before.Tables, fx.after.Tables, fx.workTables()} {
		for _, state := range states {
			f.Respond(verifyCountSQL(state.Table), single("n", strconv.FormatInt(state.Rows, 10)))
			f.Respond(verifyHashSQL(state.Table), single("h", state.Hash))
		}
	}
	f.Respond(sqlInitialCommit, single("commit_hash", fx.initial))
	f.Respond(sqlBranchBase, Rows{})
	f.Respond(sqlBranchWork, Rows{})
	f.Respond(sqlCheckoutWork, Rows{})
	f.Respond(sqlRebaseStart, Rows{})
	f.Respond(sqlPlan, Rows{
		Columns: []string{"rebase_order", "commit_hash"},
		Data:    [][]string{{"1", hash(0x11)}, {"2", hash(0x12)}},
	})
	f.Respond(sqlMarkCount, single("n", "1"))
	f.Respond(sqlMarkSquash, Rows{})
	f.Respond(sqlSquashCount, single("n", "1"))
	f.Respond(sqlRebaseResume, Rows{})
	f.Respond(tailStatement(fx.before.Tip, fx.after.Tip), tailRows(fx.picked))
	f.Respond(tailStatement(fx.after.Tip, fx.after.Tip), tailRows())
	f.Respond(sqlCherryPick, pickedAs(fx.made))
	f.Respond(sqlVerifyRecordCommits, single("n", strconv.FormatInt(fx.after.Commits, 10)))
	f.Respond(sqlVerifyBaseTip, single("dolt_hashof('compact-base')", fx.initial))
	f.Respond(sqlVerifyOldCommits, single("n", strconv.FormatInt(fx.old, 10)))
	f.Respond(sqlVerifyWorkNewer, identitiesRows(fx.newer))
	f.Respond(sqlVerifyMainNewer, identitiesRows(fx.newer))
	f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", fx.made))
	f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "0"))
	f.Respond(sqlDoltBranch, Rows{})
	f.Respond(sqlMainPreSquashTip, single("dolt_hashof('main-pre-squash')", fx.after.Tip))
	return f
}

// runMarker is the Marker seam's fake: it hands the marker to one run and
// records every attempt, so a test sees a run take it, a second run refused,
// and the release.
type runMarker struct {
	held       bool
	holds      int
	releases   int
	releaseErr error
}

func (m *runMarker) Hold() error {
	m.holds++
	if m.held {
		return ErrRunMarkerHeld
	}
	m.held = true
	return nil
}

func (m *runMarker) Release() error {
	m.releases++
	m.held = false
	return m.releaseErr
}

// nthStatement returns the index of the nth (counting from 1) statement that is
// exactly want, failing the test when the run sent fewer.
func nthStatement(t *testing.T, statements []string, want string, n int) int {
	t.Helper()
	seen := 0
	for i, statement := range statements {
		if statement != want {
			continue
		}
		seen++
		if seen == n {
			return i
		}
	}
	t.Fatalf("the run sent %d statements equal to %q, want at least %d", seen, want, n)
	return 0
}

func TestRunRunsTheStepsInTheDesignsOrder(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	f := fx.fake()
	clk := newTestClock()
	marker := &runMarker{}

	got, err := Run(context.Background(), RunInput{
		Exec:    f,
		Marker:  marker,
		Clock:   clk,
		Sleeper: &testSleeper{clk: clk},
		Cut:     cutOf(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Each step's result is the one its own fixture describes, so the order the
	// statements report below is the order of the work, not of a replayed script.
	if !reflect.DeepEqual(got.Before, fx.before) {
		t.Errorf("the BEFORE record is %+v, want %+v", got.Before, fx.before)
	}
	if got.Rebase.Marked != 1 {
		t.Errorf("the rebase marked %d rows, want 1", got.Rebase.Marked)
	}
	if want := (CatchupResult{Tip: fx.after.Tip, Passes: 3, Picks: 1}); got.Catchup != want {
		t.Errorf("the catch-up is %+v, want %+v", got.Catchup, want)
	}
	if !reflect.DeepEqual(got.Verified, fx.after) {
		t.Errorf("the record at the last picked tip is %+v, want %+v", got.Verified, fx.after)
	}
	if !got.Verify.OK() {
		t.Errorf("verification found %v", got.Verify.Differences)
	}
	wantSwap := SwapResult{
		SwappedTip: fx.made,
		OldTip:     fx.after.Tip,
		Reason:     "swapped: no commit reached the old main after the verified tip",
	}
	if got.Swap != wantSwap {
		t.Errorf("the swap is %+v, want %+v", got.Swap, wantSwap)
	}
	if marker.holds != 1 || marker.releases != 1 || marker.held {
		t.Errorf("the marker was held %d times and released %d, still held %v, want one hold released",
			marker.holds, marker.releases, marker.held)
	}

	// The step each marker belongs to, in the design's order. The second record
	// and verification each open with a checkout, so the nth occurrence is the
	// one that names the step.
	steps := []struct {
		name      string
		statement string
		nth       int
	}{
		{"the BEFORE record", sqlCheckoutMain, 1},
		{"the rebase", sqlRebaseStart, 1},
		{"the catch-up", tailStatement(fx.before.Tip, fx.after.Tip), 1},
		{"the record at the last picked tip", sqlCheckoutMain, 2},
		{"verification", sqlCheckoutWork, 2},
		{"the swap", swapRenames[0], 1},
	}
	statements := f.Statements()
	last := -1
	for _, step := range steps {
		at := nthStatement(t, statements, step.statement, step.nth)
		if at <= last {
			t.Fatalf("%s ran before the step before it:\n%s", step.name, strings.Join(statements, "\n"))
		}
		last = at
	}
	// The catch-up's first tail read is the range from the BEFORE record's tip,
	// so a commit that landed during the rebase is replayed and not lost.
	if got, want := pickStatements(f), wantPicks(fx.picked); !reflect.DeepEqual(got, want) {
		t.Errorf("picks = %q, want %q", got, want)
	}
}

func TestRunStopsBeforeTheSwapWhenVerificationDiffers(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.work = []TableState{
		{Table: "comments", Rows: 5, Hash: hash(0x06)},
		{Table: "issues", Rows: 9, Hash: hash(0x03)},
	}
	f := fx.fake()
	clk := newTestClock()
	marker := &runMarker{}

	got, err := Run(context.Background(), RunInput{
		Exec:    f,
		Marker:  marker,
		Clock:   clk,
		Sleeper: &testSleeper{clk: clk},
		Cut:     cutOf(),
	})
	if err == nil {
		t.Fatal("Run returned no error on a compact-work that does not verify")
	}
	for _, want := range []string{
		"compact-work does not verify against main at " + fx.after.Tip,
		"table comments: row count is 5 on compact-work, 4 in the record",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got.Verify.OK() {
		t.Errorf("the run reported a verified compact-work: %+v", got.Verify)
	}
	// Only the rebase's branch creation ran: neither of the swap's renames was
	// sent, so main is untouched.
	if got, want := branchStatements(f), []string{sqlBranchBase + quoteLiteral(fx.initial) + ")", sqlBranchWork + quoteLiteral(fx.before.Tip) + ")"}; !reflect.DeepEqual(got, want) {
		t.Errorf("branch statements = %q, want only the rebase's two, %q", got, want)
	}
	if marker.held || marker.releases != 1 {
		t.Errorf("the stopped run held %v with %d releases, want the marker released", marker.held, marker.releases)
	}
}

func TestRunRefusesASecondRunWhileTheMarkerIsHeld(t *testing.T) {
	t.Parallel()
	// The marker a previous run holds is what a second, concurrent run finds.
	f := NewFakeExec()
	marker := &runMarker{held: true}
	clk := newTestClock()

	_, err := Run(context.Background(), RunInput{
		Exec:    f,
		Marker:  marker,
		Clock:   clk,
		Sleeper: &testSleeper{clk: clk},
		Cut:     cutOf(),
	})
	if !errors.Is(err, ErrRunMarkerHeld) {
		t.Fatalf("Run returned %v, want the held marker's refusal", err)
	}
	if got := len(f.Statements()); got != 0 {
		t.Errorf("the refused run sent %d statements, want none", got)
	}
	if marker.releases != 0 {
		t.Errorf("the refused run released the marker %d times, want none: it never held it", marker.releases)
	}
}

// runStep names one step of a run by a statement it sends, and which
// occurrence of that statement in the run it is: a step shares statements with
// the steps before it, so the occurrence is what locates it.
type runStep struct {
	name      string
	statement string
	nth       int
}

// TestRunStopsAtTheStepThatFails drives every step's error return: one
// statement of the step fails, and the run must return that error at once,
// having sent nothing after it, with the marker released. The statements are
// located by index in a run that succeeded, so a failure lands in the step it
// names and the count proves the run stopped there.
func TestRunStopsAtTheStepThatFails(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	healthyClk := newTestClock()
	healthy := fx.fake()
	if _, err := Run(context.Background(), RunInput{
		Exec:    healthy,
		Marker:  &runMarker{},
		Clock:   healthyClk,
		Sleeper: &testSleeper{clk: healthyClk},
		Cut:     cutOf(),
	}); err != nil {
		t.Fatalf("the run the indexes come from failed: %v", err)
	}
	statements := healthy.Statements()
	steps := []runStep{
		{"the BEFORE record", sqlCheckoutMain, 1},
		{"the rebase", sqlRebaseStart, 1},
		{"the catch-up", sqlActiveBranch, 2},
		{"the record at the last picked tip", sqlCheckoutMain, 2},
		{"verification", sqlCheckoutWork, 2},
		{"the swap", sqlCompactWorkTip, 1},
	}
	refused := errors.New("server closed the connection")
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			t.Parallel()
			at := nthStatement(t, statements, step.statement, step.nth)
			f := fx.fake()
			f.FailAt(at+1, refused)
			marker := &runMarker{}
			clk := newTestClock()

			_, err := Run(context.Background(), RunInput{
				Exec:    f,
				Marker:  marker,
				Clock:   clk,
				Sleeper: &testSleeper{clk: clk},
				Cut:     cutOf(),
			})
			if !errors.Is(err, refused) {
				t.Fatalf("Run returned %v, want the failed step's error", err)
			}
			if got := len(f.Statements()); got != at+1 {
				t.Errorf("the run sent %d statements, want %d: it stopped at the failed statement", got, at+1)
			}
			if marker.held || marker.releases != 1 {
				t.Errorf("the failed run held %v with %d releases, want the marker released", marker.held, marker.releases)
			}
		})
	}
}

func TestRunReportsAMarkerItCannotRelease(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	f := fx.fake()
	releaseErr := errors.New("the marker file is read-only")
	marker := &runMarker{releaseErr: releaseErr}
	clk := newTestClock()

	got, err := Run(context.Background(), RunInput{
		Exec:    f,
		Marker:  marker,
		Clock:   clk,
		Sleeper: &testSleeper{clk: clk},
		Cut:     cutOf(),
	})
	if !errors.Is(err, releaseErr) {
		t.Fatalf("Run returned %v, want the release error", err)
	}
	// The run did its work before the release failed, and the result holds it.
	if !got.Verify.OK() || got.Swap.SwappedTip != fx.made {
		t.Errorf("the run's result lost the work it did: %+v", got)
	}
}

func TestRunRefusesAWiringThatCannotRun(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		clear func(*RunInput)
		want  string
	}{
		{"no Exec", func(in *RunInput) { in.Exec = nil }, "has no Exec"},
		{"no marker", func(in *RunInput) { in.Marker = nil }, "has no marker"},
		{"no clock", func(in *RunInput) { in.Clock = nil }, "has no clock"},
		{"no sleeper", func(in *RunInput) { in.Sleeper = nil }, "has no sleeper"},
		{"no cut", func(in *RunInput) { in.Cut = time.Time{} }, "has no cut"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFakeExec()
			clk := newTestClock()
			marker := &runMarker{}
			in := RunInput{Exec: f, Marker: marker, Clock: clk, Sleeper: &testSleeper{clk: clk}, Cut: cutOf()}
			tc.clear(&in)

			_, err := Run(context.Background(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Run returned %v, want an error holding %q", err, tc.want)
			}
			if got := len(f.Statements()); got != 0 {
				t.Errorf("the refused run sent %d statements, want none", got)
			}
			if marker.holds != 0 {
				t.Errorf("the refused run held the marker %d times, want none", marker.holds)
			}
		})
	}
}

func TestRunRefusesACancelledContextBeforeHoldingTheMarker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewFakeExec()
	marker := &runMarker{}
	clk := newTestClock()

	_, err := Run(ctx, RunInput{
		Exec:    f,
		Marker:  marker,
		Clock:   clk,
		Sleeper: &testSleeper{clk: clk},
		Cut:     cutOf(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if marker.holds != 0 {
		t.Errorf("the cancelled run held the marker %d times, want none", marker.holds)
	}
	if got := len(f.Statements()); got != 0 {
		t.Errorf("the cancelled run sent %d statements, want none", got)
	}
}
