package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// runFixture is the database a run's tests script: main at the BEFORE record's
// tip, later at the tip the catch-up reaches with one picked commit, a table
// the writer added a row to, and one commit newer than the cut on both sides.
// work overrides the tables verification reads on compact-work, so a test can
// make compact-work differ from the record; nil means it matches after.
// recordTip is the tip the record at the last picked tip reads; empty means it
// reads the catch-up's, so a test can put the two on different commits.
type runFixture struct {
	before    Before
	after     Before
	work      []TableState
	initial   string
	picked    string
	made      string
	newer     []commitIdentity
	old       int64
	recordTip string
	// swapped is main's commit count after the swap, distinct from either
	// record so the summary's after figure is proved read after the swap.
	swapped int64
	// oldTip is main-pre-squash's tip after the swap; empty means the verified
	// tip, so the old main has no tail.
	oldTip string
	// tail is the old main's tail after the verified tip: one commit per
	// entry, each picked onto the new main.
	tail []string
	// pickRetries is how many picks answer "uncommitted changes" before one
	// succeeds, so the summary's retry count is exercised.
	pickRetries int
	// afterErr, when set, is the error the summary's after-read answers, so a
	// test drives the run's refusal to report a count it could not read.
	afterErr error
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
		swapped: 6003,
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
	// The recovery's first read finds no dead run: one branch, main.
	f.Respond(branchesStatement, Rows{Columns: []string{"name", "hash"}, Data: [][]string{{"main", fx.before.Tip}}})
	// The branch the session is on, in the order the steps check it: each record
	// on main, the catch-up and verification on compact-work, and the swap's
	// check before and after its renames.
	for _, branch := range []string{"main", "compact-work", "main", "compact-work", "compact-work", "main"} {
		f.Respond(sqlActiveBranch, single("branch", branch))
	}
	f.Respond(sqlCheckoutMain, Rows{})
	f.Respond(sqlMainTip, single("dolt_hashof('main')", fx.before.Tip))
	// The catch-up reads main's tip once per pass: the pass that picks, the empty
	// pass that records the tip, and the pass that finds it quiet. The record at
	// the last picked tip reads it once more.
	for range 3 {
		f.Respond(sqlMainTip, single("dolt_hashof('main')", fx.after.Tip))
	}
	if fx.recordTip != "" {
		f.Respond(sqlMainTip, single("dolt_hashof('main')", fx.recordTip))
	}
	f.Respond(sqlMainCommits, single("n", strconv.FormatInt(fx.before.Commits, 10)))
	f.Respond(sqlMainCommits, single("n", strconv.FormatInt(fx.after.Commits, 10)))
	if fx.afterErr != nil {
		f.RespondErr(sqlMainCommits, fx.afterErr)
	} else {
		f.Respond(sqlMainCommits, single("n", strconv.FormatInt(fx.swapped, 10)))
	}
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
	for range fx.pickRetries {
		f.RespondErr(sqlCherryPick, errors.New("error 1105: uncommitted changes in a table"))
	}
	f.Respond(sqlCherryPick, pickedAs(fx.made))
	f.Respond(sqlVerifyRecordCommits, single("n", strconv.FormatInt(fx.after.Commits, 10)))
	f.Respond(sqlVerifyBaseTip, single("dolt_hashof('compact-base')", fx.initial))
	f.Respond(sqlVerifyOldCommits, single("n", strconv.FormatInt(fx.old, 10)))
	f.Respond(sqlVerifyWorkNewer, identitiesRows(fx.newer))
	f.Respond(sqlVerifyMainNewer, identitiesRows(fx.newer))
	f.Respond(sqlCompactWorkTip, single("dolt_hashof('compact-work')", fx.made))
	f.Respond(sqlMainPreSquashCount, single("COUNT(*)", "0"))
	f.Respond(sqlDoltBranch, Rows{})
	oldTip := fx.after.Tip
	if fx.oldTip != "" {
		oldTip = fx.oldTip
	}
	f.Respond(sqlMainPreSquashTip, single("dolt_hashof('main-pre-squash')", oldTip))
	if oldTip != fx.after.Tip {
		f.Respond(tailStatement(fx.after.Tip, oldTip), tailRows(fx.tail...))
		for i := range fx.tail {
			f.Respond(sqlCherryPick, pickedAs(hash(0x40+i)))
		}
	}
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

// TestRunRefusesARecordAtATipTheCatchUpNeverReached drives the refusal between a
// record taken at one tip and a catch-up that stopped at another: verification
// would prove compact-work against a state the picks never reached, and the swap
// would replay the tail from a different one, so the run must stop before
// either, with main untouched.
func TestRunRefusesARecordAtATipTheCatchUpNeverReached(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.recordTip = hash(0x07)
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
		t.Fatal("Run returned no error on a record at a tip the catch-up never reached")
	}
	for _, want := range []string{
		"the record at the last picked tip read main at " + fx.recordTip,
		"the catch-up stopped at " + fx.after.Tip,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got.Verify.MainTip != "" || got.Swap.SwappedTip != "" {
		t.Errorf("the refused run verified and swapped anyway: verify %+v, swap %+v", got.Verify, got.Swap)
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

// runStep names one step of a run by a statement it sends, which occurrence of
// that statement in the run it is, and how many of the design's steps have
// finished when that statement fails. A step shares statements with the steps
// around it, so the occurrence alone cannot say which step a statement belongs
// to; done is what proves the label names the step the statement is in.
type runStep struct {
	name      string
	statement string
	nth       int
	done      int
}

// completedSteps returns how many of a run's steps finished before it stopped,
// read from the result: a step assigns its field only when it returns, and a
// step that fails at its first statement returns that field zero, so the first
// zero field names the step the run stopped in.
func completedSteps(r RunResult) int {
	switch {
	case r.Swap.SwappedTip != "":
		return 6
	case r.Verify.MainTip != "":
		return 5
	case r.Verified.Tip != "":
		return 4
	case r.Catchup.Tip != "":
		return 3
	case r.Rebase.Marked > 0:
		return 2
	case r.Before.Tip != "":
		return 1
	}
	return 0
}

// TestRunStopsAtTheStepThatFails drives every step's error return: one
// statement of the step fails, and the run must return that error at once,
// having sent nothing after it, with the steps before it finished and that step
// and every later one not, and the marker released. The statements are located
// by index in a run that succeeded, so a failure lands in the step it names,
// and the count of finished steps proves the label and the statement agree.
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
		{"the BEFORE record", sqlCheckoutMain, 1, 0},
		{"the rebase", sqlRebaseStart, 1, 1},
		{"the catch-up", sqlActiveBranch, 2, 2},
		{"the record at the last picked tip", sqlCheckoutMain, 2, 3},
		{"verification", sqlCheckoutWork, 2, 4},
		{"the swap", sqlCompactWorkTip, 1, 5},
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

			got, err := Run(context.Background(), RunInput{
				Exec:    f,
				Marker:  marker,
				Clock:   clk,
				Sleeper: &testSleeper{clk: clk},
				Cut:     cutOf(),
			})
			if !errors.Is(err, refused) {
				t.Fatalf("Run returned %v, want the failed step's error", err)
			}
			if sent := len(f.Statements()); sent != at+1 {
				t.Errorf("the run sent %d statements, want %d: it stopped at the failed statement", sent, at+1)
			}
			if finished := completedSteps(got); finished != step.done {
				t.Errorf("the run finished %d steps, want %d: %q does not name the step that sent the failed statement",
					finished, step.done, step.name)
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

// The recovery statements no other test constant names, spelled out so a test
// pins the SQL rather than the renderer that sends it.
const (
	sqlDropWork       = "CALL DOLT_BRANCH('-D', 'compact-work')"
	sqlDropBase       = "CALL DOLT_BRANCH('-D', 'compact-base')"
	sqlRenameWorkBack = "CALL DOLT_BRANCH('-m', '--force', 'compact-work', 'main')"
)

// errDeath stands for a process that dies at a statement: the run stops there,
// and the test starts the next run over the state the death left.
var errDeath = errors.New("the process died")

// recoveryNewerStatement is the read a recovery makes of one branch's commits
// newer than cutOf().
func recoveryNewerStatement(branch string) string {
	return fmt.Sprintf("SELECT author_date, message FROM dolt_log('%s') WHERE author_date > '%s' ORDER BY author_date", branch, formatCut(cutOf()))
}

// branchesRows renders a branch catalog the way the recovery's read answers it,
// in name order.
func branchesRows(tips map[string]string) Rows {
	names := make([]string, 0, len(tips))
	for name := range tips {
		names = append(names, name)
	}
	slices.Sort(names)
	rows := Rows{Columns: []string{"name", "hash"}}
	for _, name := range names {
		rows.Data = append(rows.Data, []string{name, tips[name]})
	}
	return rows
}

// checkRecoveryStopsAtEveryStatement runs the recovery of one scripted state
// once per statement, failing that statement, and requires every attempt to
// stop there with the injected error: a recovery that cannot read or write a
// statement must not continue on a state it could not establish.
func checkRecoveryStopsAtEveryStatement(t *testing.T, script func(*FakeExec), statements int) {
	t.Helper()
	for at := 1; at <= statements; at++ {
		f := NewFakeExec()
		script(f)
		f.FailAt(at, errDeath)
		if _, err := Recover(context.Background(), f, cutOf()); !errors.Is(err, errDeath) {
			t.Errorf("statement %d returned %v, want the injected death", at, err)
		}
		if got := len(f.Statements()); got != at {
			t.Errorf("the recovery sent %d statements before stopping at %d", got, at)
		}
	}
}

// TestRecoverBeforeTheSwapDropsTheWorkBranches drives the design's first
// recovery row: main was never renamed, so a work branch is dropped once main
// is proved to hold every commit it has after the cut, and main is checked when
// the catalog lists only it and what the recovery kept.
func TestRecoverBeforeTheSwapDropsTheWorkBranches(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip}
	script := func(f *FakeExec) {
		f.Respond(branchesStatement, branchesRows(found))
		f.Respond("CALL DOLT_CHECKOUT", Rows{})
		f.Respond("CALL DOLT_BRANCH", Rows{})
		f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
		f.Respond(branchesStatement, branchesRows(after))
	}
	f := NewFakeExec()
	script(f)

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoveryBeforeSwap {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoveryBeforeSwap)
	}
	if want := []string{"compact-work", "compact-base"}; !slices.Equal(got.Dropped, want) {
		t.Errorf("the dropped branches are %q, want %q", got.Dropped, want)
	}
	if len(got.Kept) != 0 {
		t.Errorf("the recovery kept %q, want none", got.Kept)
	}
	want := []string{
		branchesStatement,
		sqlCheckoutMain,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("compact-work"),
		sqlDropWork,
		recoveryNewerStatement("compact-base"),
		sqlDropBase,
		branchesStatement,
	}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("the recovery sent\n%q\nwant\n%q", got, want)
	}
	checkRecoveryStopsAtEveryStatement(t, script, len(want))
}

// TestRecoverKeepsAWorkBranchHoldingACommitMainLacks drives the proof every
// drop follows: a compact-work that holds a commit main does not is history a
// delete would destroy, so it stays beside main and the recovery reports it.
func TestRecoverKeepsAWorkBranchHoldingACommitMainLacks(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed on the branch that became main"}
	found := map[string]string{"main": fx.before.Tip, "compact-work": hash(0x50), "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip, "compact-work": hash(0x50)}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
	f.Respond(branchesStatement, branchesRows(after))

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoveryBeforeSwap {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoveryBeforeSwap)
	}
	if want := []string{"compact-work"}; !slices.Equal(got.Kept, want) {
		t.Errorf("the kept branches are %q, want %q", got.Kept, want)
	}
	if want := []string{landed.String()}; !slices.Equal(got.Landed, want) {
		t.Errorf("the commits the kept branch holds are %q, want %q", got.Landed, want)
	}
	if want := []string{"compact-base"}; !slices.Equal(got.Dropped, want) {
		t.Errorf("the dropped branches are %q, want %q", got.Dropped, want)
	}
	for _, statement := range f.Statements() {
		if statement == sqlDropWork {
			t.Errorf("the recovery dropped compact-work while it held a commit main does not: %q", statement)
		}
	}
}

// TestRecoverAfterTheFirstRenamePutsTheOldMainBack drives the design's second
// recovery row: no branch is named main, so main-pre-squash is renamed back and
// a work branch is dropped only after the old main's tip is read there and main
// is proved to hold its commits.
func TestRecoverAfterTheFirstRenamePutsTheOldMainBack(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	restored := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip}
	script := func(f *FakeExec) {
		f.Respond(branchesStatement, branchesRows(found))
		f.Respond("CALL DOLT_BRANCH", Rows{})
		f.Respond("CALL DOLT_CHECKOUT", Rows{})
		f.Respond(branchesStatement, branchesRows(restored))
		f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
		f.Respond(branchesStatement, branchesRows(after))
	}
	f := NewFakeExec()
	script(f)

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoveryFirstRename {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoveryFirstRename)
	}
	if want := []string{"compact-work", "compact-base"}; !slices.Equal(got.Dropped, want) {
		t.Errorf("the dropped branches are %q, want %q", got.Dropped, want)
	}
	want := []string{
		branchesStatement,
		swapRenameBack[1],
		sqlCheckoutMain,
		branchesStatement,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("compact-work"),
		sqlDropWork,
		recoveryNewerStatement("compact-base"),
		sqlDropBase,
		branchesStatement,
	}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("the recovery sent\n%q\nwant\n%q", got, want)
	}
	checkRecoveryStopsAtEveryStatement(t, script, len(want))
}

// TestRecoverOnlyRecoversADeadRunsBranches drives RecoverOnly over the catalog a
// run leaves when it stops between the swap's two renames: main-pre-squash holds
// the old main and no branch is named main. RecoverOnly recovers it the way Run's
// own recovery does, without starting a squash, and returns the Recovery naming
// the state. The statements are the recovery's alone (gt-an57lb).
func TestRecoverOnlyRecoversADeadRunsBranches(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	restored := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond(branchesStatement, branchesRows(restored))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
	f.Respond(branchesStatement, branchesRows(after))

	marker := &runMarker{}
	got, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: marker, Cut: cutOf()})
	if err != nil {
		t.Fatalf("RecoverOnly: %v", err)
	}
	if marker.holds != 1 || marker.releases != 1 || marker.held {
		t.Errorf("RecoverOnly held the marker %d times and released it %d times (held now %v), want one hold and one release", marker.holds, marker.releases, marker.held)
	}
	if got == nil || got.State != RecoveryFirstRename {
		t.Fatalf("the recovery is %+v, want state %q", got, RecoveryFirstRename)
	}
	if want := []string{"compact-work", "compact-base"}; !slices.Equal(got.Dropped, want) {
		t.Errorf("the dropped branches are %q, want %q", got.Dropped, want)
	}
	want := []string{
		branchesStatement,
		swapRenameBack[1],
		sqlCheckoutMain,
		branchesStatement,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("compact-work"),
		sqlDropWork,
		recoveryNewerStatement("compact-base"),
		sqlDropBase,
		branchesStatement,
	}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("RecoverOnly sent\n%q\nwant the recovery's statements alone\n%q", got, want)
	}
}

// TestRecoverOnlyErrorAndCleanCatalog pins the two results the squash check reads
// from RecoverOnly: a recovery error comes back as it is, and a catalog with
// nothing to recover returns a nil Recovery and a nil error with no write sent
// (gt-hr0ozv).
func TestRecoverOnlyErrorAndCleanCatalog(t *testing.T) {
	t.Parallel()
	t.Run("the recovery error is returned", func(t *testing.T) {
		t.Parallel()
		f := NewFakeExec()
		f.RespondErr(branchesStatement, errDeath)
		marker := &runMarker{}

		got, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: marker, Cut: cutOf()})
		if !errors.Is(err, errDeath) {
			t.Errorf("RecoverOnly returned %v, want the recovery's error %v", err, errDeath)
		}
		if got != nil {
			t.Errorf("RecoverOnly returned the recovery %+v beside the error, want nil", got)
		}
		if marker.held || marker.releases != 1 {
			t.Errorf("the failed recovery held %v with %d releases, want the marker released", marker.held, marker.releases)
		}
	})
	t.Run("a clean catalog returns nothing and writes nothing", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		f := NewFakeExec()
		f.Respond(branchesStatement, branchesRows(map[string]string{"main": fx.before.Tip}))

		got, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: &runMarker{}, Cut: cutOf()})
		if err != nil {
			t.Fatalf("RecoverOnly: %v", err)
		}
		if got != nil {
			t.Errorf("RecoverOnly returned %+v for a clean catalog, want a nil Recovery", got)
		}
		if want := []string{branchesStatement}; !slices.Equal(f.Statements(), want) {
			t.Errorf("RecoverOnly sent\n%q\nwant the catalog read alone\n%q", f.Statements(), want)
		}
	})
}

// TestRecoverOnlyRefusesAHeldMarker drives RecoverOnly over the catalog a dead
// run leaves while a live run holds the marker. Without the marker the live
// run's branches would read as a dead run's, so RecoverOnly refuses before it
// sends a statement and leaves the live run's hold in place (gt-2x4rx5).
func TestRecoverOnlyRefusesAHeldMarker(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}))
	marker := &runMarker{held: true}

	got, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: marker, Cut: cutOf()})
	if !errors.Is(err, ErrRunMarkerHeld) {
		t.Fatalf("RecoverOnly returned %v, want the held marker's refusal", err)
	}
	if got != nil {
		t.Errorf("RecoverOnly returned the recovery %+v beside the refusal, want nil", got)
	}
	if n := len(f.Statements()); n != 0 {
		t.Errorf("the refused recovery sent %d statements, want none", n)
	}
	if !marker.held || marker.releases != 0 {
		t.Errorf("the refused recovery held %v with %d releases, want the live run's hold untouched", marker.held, marker.releases)
	}
}

// TestRecoverOnlyRefusesNoMarker pins the fail-closed branch: a RecoverOnly
// with no marker would recover beside a live run, so it refuses before it reads
// the catalog (gt-2x4rx5).
func TestRecoverOnlyRefusesNoMarker(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()

	if _, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Cut: cutOf()}); err == nil {
		t.Error("RecoverOnly with no marker returned nil, want a refusal")
	}
	if n := len(f.Statements()); n != 0 {
		t.Errorf("RecoverOnly with no marker sent %d statements, want none", n)
	}
}

// heldWatch wraps the Exec seam and records whether the run marker is held when
// each statement is sent, so a test sees the marker during the recovery and not
// only before and after it.
type heldWatch struct {
	inner  Exec
	marker *runMarker
	heldAt []bool
}

func (w *heldWatch) Exec(ctx context.Context, statement string) (Rows, error) {
	w.heldAt = append(w.heldAt, w.marker.held)
	return w.inner.Exec(ctx, statement)
}

// TestRecoverOnlyReleasesTheMarkerOnEveryPath pins the run marker across
// RecoverOnly's exits: it is held for every statement the recovery sends, and it
// is released after a successful recovery, after a recovery error and after a
// clean catalog that recovers nothing (gt-grsuzd, gt-2x4rx5).
func TestRecoverOnlyReleasesTheMarkerOnEveryPath(t *testing.T) {
	t.Parallel()
	t.Run("a successful recovery holds the marker for every statement and releases it", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
		restored := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
		after := map[string]string{"main": fx.before.Tip}
		f := NewFakeExec()
		f.Respond(branchesStatement, branchesRows(found))
		f.Respond("CALL DOLT_BRANCH", Rows{})
		f.Respond("CALL DOLT_CHECKOUT", Rows{})
		f.Respond(branchesStatement, branchesRows(restored))
		f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
		f.Respond(branchesStatement, branchesRows(after))

		marker := &runMarker{}
		watch := &heldWatch{inner: f, marker: marker}
		got, err := RecoverOnly(context.Background(), RecoverInput{Exec: watch, Marker: marker, Cut: cutOf()})
		if err != nil {
			t.Fatalf("RecoverOnly: %v", err)
		}
		if got == nil || got.State != RecoveryFirstRename {
			t.Fatalf("the recovery is %+v, want state %q", got, RecoveryFirstRename)
		}
		if len(watch.heldAt) == 0 {
			t.Fatal("the recovery sent no statement, so the marker was never read while held")
		}
		for i, held := range watch.heldAt {
			if !held {
				t.Errorf("statement %d ran with the run marker released, want it held while Recover runs", i)
			}
		}
		if marker.held || marker.releases != 1 {
			t.Errorf("after the successful recovery the marker is held %v with %d releases, want released once", marker.held, marker.releases)
		}
	})
	t.Run("a recovery error releases the marker", func(t *testing.T) {
		t.Parallel()
		f := NewFakeExec()
		f.RespondErr(branchesStatement, errDeath)
		marker := &runMarker{}

		_, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: marker, Cut: cutOf()})
		if !errors.Is(err, errDeath) {
			t.Fatalf("RecoverOnly returned %v, want the recovery's error %v", err, errDeath)
		}
		if marker.held || marker.releases != 1 {
			t.Errorf("after the recovery error the marker is held %v with %d releases, want released once", marker.held, marker.releases)
		}
	})
	t.Run("a clean catalog releases the marker", func(t *testing.T) {
		t.Parallel()
		fx := newRunFixture()
		f := NewFakeExec()
		f.Respond(branchesStatement, branchesRows(map[string]string{"main": fx.before.Tip}))
		marker := &runMarker{}

		got, err := RecoverOnly(context.Background(), RecoverInput{Exec: f, Marker: marker, Cut: cutOf()})
		if err != nil {
			t.Fatalf("RecoverOnly: %v", err)
		}
		if got != nil {
			t.Errorf("RecoverOnly returned %+v for a clean catalog, want a nil Recovery", got)
		}
		if marker.held || marker.releases != 1 {
			t.Errorf("after the clean catalog the marker is held %v with %d releases, want released once", marker.held, marker.releases)
		}
	})
}

// TestRecoverFirstRenameKeepsALandedCommit drives the first-rename row over the
// catalog a row-three recovery leaves when it dies between its own renames:
// compact-work holds a writer's commit, so it is kept rather than dropped
// blindly, and main is the old main again.
func TestRecoverFirstRenameKeepsALandedCommit(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed while main was renamed"}
	found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": hash(0x50), "compact-base": fx.initial}
	restored := map[string]string{"main": fx.before.Tip, "compact-work": hash(0x50), "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip, "compact-work": hash(0x50)}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(swapRenameBack[1], Rows{})
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond(branchesStatement, branchesRows(restored))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(branchesStatement, branchesRows(after))

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoveryFirstRename {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoveryFirstRename)
	}
	if want := []string{"compact-work"}; !slices.Equal(got.Kept, want) {
		t.Errorf("the kept branches are %q, want %q", got.Kept, want)
	}
	if want := []string{landed.String()}; !slices.Equal(got.Landed, want) {
		t.Errorf("the commits the kept branch holds are %q, want %q", got.Landed, want)
	}
	for _, statement := range f.Statements() {
		if statement == sqlDropWork {
			t.Errorf("the recovery dropped compact-work while it held a commit main does not: %q", statement)
		}
	}
}

// TestRecoverAfterTheSwapRenamesBothBranchesBack drives the design's third
// recovery row: the old main holds a commit the new main does not (the
// reconcile had not replayed it) and nothing on the new main would be lost, so
// both branches are renamed back, the commit the rename moved is read back, and
// only then are the work branches dropped.
func TestRecoverAfterTheSwapRenamesBothBranchesBack(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	tail := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: tail not replayed"}
	oldNewer := append(slices.Clone(fx.newer), tail)
	found := map[string]string{"main": fx.made, "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	renamed := map[string]string{"main": fx.before.Tip, "compact-work": fx.made, "compact-base": fx.initial}
	after := map[string]string{"main": fx.before.Tip}
	script := func(f *FakeExec) {
		f.Respond(branchesStatement, branchesRows(found))
		f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(oldNewer))
		f.Respond("CALL DOLT_BRANCH", Rows{})
		f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
		f.Respond(recoveryNewerStatement("main"), identitiesRows(oldNewer))
		f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
		f.Respond("CALL DOLT_CHECKOUT", Rows{})
		f.Respond(branchesStatement, branchesRows(renamed))
		f.Respond(branchesStatement, branchesRows(after))
	}
	f := NewFakeExec()
	script(f)

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoverySwapped {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoverySwapped)
	}
	if want := []string{tail.String()}; !slices.Equal(got.Unreplayed, want) {
		t.Errorf("the commits the new main lacks are %q, want %q", got.Unreplayed, want)
	}
	if len(got.Landed) != 0 {
		t.Errorf("the recovery named landed commits %q, want none", got.Landed)
	}
	if want := []string{"compact-work", "compact-base"}; !slices.Equal(got.Dropped, want) {
		t.Errorf("the dropped branches are %q, want %q", got.Dropped, want)
	}
	want := []string{
		branchesStatement,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("main-pre-squash"),
		swapRenameBack[0],
		recoveryNewerStatement("compact-work"),
		swapRenameBack[1],
		sqlCheckoutMain,
		branchesStatement,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("compact-work"),
		sqlDropWork,
		recoveryNewerStatement("compact-base"),
		sqlDropBase,
		branchesStatement,
	}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("the recovery sent\n%q\nwant\n%q", got, want)
	}
	checkRecoveryStopsAtEveryStatement(t, script, len(want))
}

// TestRecoverKeepsTheNewMainWhenTheSwapCompleted drives the catalog a run that
// finished its swap leaves for the retention check: the new main holds every
// commit main-pre-squash has after the cut, so the recovery keeps both branches
// and renames nothing. Renaming back would undo a finished squash.
func TestRecoverKeepsTheNewMainWhenTheSwapCompleted(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main": fx.made, "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(fx.newer))

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoverySwappedKept {
		t.Errorf("the recovery state is %q, want %q", got.State, RecoverySwappedKept)
	}
	if !got.Complete() {
		t.Errorf("the recovery of a complete swap reports %+v, want Complete", got)
	}
	if want := []string{"compact-base", "main-pre-squash"}; !slices.Equal(got.Kept, want) {
		t.Errorf("the kept branches are %q, want %q", got.Kept, want)
	}
	if len(got.Dropped) != 0 || len(got.Landed) != 0 || len(got.Unreplayed) != 0 {
		t.Errorf("the recovery dropped %q, landed %q and unreplayed %q, want nothing moved", got.Dropped, got.Landed, got.Unreplayed)
	}
	if got := len(f.Statements()); got != 3 {
		t.Errorf("the kept recovery sent %d statements, want the branch read and the two identity reads", got)
	}
}

// TestRecoverKeepsBothBranchesWhenACommitReachedTheNewMain drives the row both
// sides moved in: a writer landed on the new main and the reconcile had not
// replayed an old-main commit, so renaming back would move the writer's commit
// off main and dropping the old branch would lose the other. Both stay, and the
// run must stop.
func TestRecoverKeepsBothBranchesWhenACommitReachedTheNewMain(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed after the swap"}
	tail := commitIdentity{AuthorDate: "2026-11-12 10:30:00.000", Message: "fix: tail not replayed"}
	oldNewer := append(slices.Clone(fx.newer), tail)
	found := map[string]string{"main": hash(0x50), "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(oldNewer))

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoverySwappedKept {
		t.Fatalf("the recovery state is %q, want %q", got.State, RecoverySwappedKept)
	}
	if got.Complete() {
		t.Errorf("the recovery of a new main that lacks an old-main commit reports Complete: %+v", got)
	}
	if want := []string{landed.String()}; !slices.Equal(got.Landed, want) {
		t.Errorf("the commits that reached the new main are %q, want %q", got.Landed, want)
	}
	if want := []string{tail.String()}; !slices.Equal(got.Unreplayed, want) {
		t.Errorf("the commits the new main lacks are %q, want %q", got.Unreplayed, want)
	}
	if len(got.Dropped) != 0 {
		t.Errorf("the recovery dropped %q, want no branch touched", got.Dropped)
	}
	if got := len(f.Statements()); got != 3 {
		t.Errorf("the kept recovery sent %d statements, want the branch read and the two identity reads", got)
	}
}

// TestRecoverUndoesItsRenameWhenACommitLandsMidSwap drives the window between
// the identity read and the rename: a commit that reaches main there is found
// in the renamed branch, and the rename is undone so it stays on main.
func TestRecoverUndoesItsRenameWhenACommitLandsMidSwap(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed mid swap"}
	tail := commitIdentity{AuthorDate: "2026-11-12 10:30:00.000", Message: "fix: tail not replayed"}
	oldNewer := append(slices.Clone(fx.newer), tail)
	found := map[string]string{"main": hash(0x50), "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(oldNewer))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(append(slices.Clone(fx.newer), landed)))

	got, err := Recover(context.Background(), f, cutOf())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got.State != RecoverySwappedKept {
		t.Fatalf("the recovery state is %q, want %q", got.State, RecoverySwappedKept)
	}
	if want := []string{landed.String()}; !slices.Equal(got.Landed, want) {
		t.Errorf("the commits that reached the new main are %q, want %q", got.Landed, want)
	}
	want := []string{
		branchesStatement,
		recoveryNewerStatement("main"),
		recoveryNewerStatement("main-pre-squash"),
		swapRenameBack[0],
		recoveryNewerStatement("compact-work"),
		sqlRenameWorkBack,
	}
	if got := f.Statements(); !slices.Equal(got, want) {
		t.Errorf("the recovery sent\n%q\nwant the rename undone and no branch dropped, %q", got, want)
	}
}

// TestRecoverDropsNothingBeforeMainIsBack drives the proof that guards every
// drop: when the renamed branch does not hold the old main's tip, the recovery
// stops with the work branches still there.
func TestRecoverDropsNothingBeforeMainIsBack(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	tail := commitIdentity{AuthorDate: "2026-11-12 10:30:00.000", Message: "fix: tail not replayed"}
	oldNewer := append(slices.Clone(fx.newer), tail)
	found := map[string]string{"main": fx.made, "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	wrong := map[string]string{"main": hash(0x60), "compact-work": fx.made, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(oldNewer))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond(branchesStatement, branchesRows(wrong))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "want the old main's tip") {
		t.Fatalf("Recover returned %v, want a refusal naming the tip it wanted", err)
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "CALL DOLT_BRANCH('-D'") {
			t.Errorf("the recovery dropped a branch before main was proved back: %q", statement)
		}
	}
}

// TestRecoverDropsNothingWhenTheFirstRenameRestoresAnotherTip drives the proof
// of the first-rename recovery: a main that does not hold the old main's tip
// stops the recovery before a work branch is dropped.
func TestRecoverDropsNothingWhenTheFirstRenameRestoresAnotherTip(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	restored := map[string]string{"main": hash(0x60), "compact-work": fx.before.Tip, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond(branchesStatement, branchesRows(restored))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "want the old main's tip") {
		t.Fatalf("Recover returned %v, want a refusal naming the tip it wanted", err)
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "CALL DOLT_BRANCH('-D'") {
			t.Errorf("the recovery dropped a branch before main was proved back: %q", statement)
		}
	}
}

// TestRecoverReportsARenameBackItCannotUndo drives the window between the
// identity read and the rename: a commit shows up in the renamed branch, and
// the rename that would put it back on main fails, so the recovery reports
// both.
func TestRecoverReportsARenameBackItCannotUndo(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed mid swap"}
	tail := commitIdentity{AuthorDate: "2026-11-12 10:30:00.000", Message: "fix: tail not replayed"}
	oldNewer := append(slices.Clone(fx.newer), tail)
	found := map[string]string{"main": hash(0x50), "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	renameErr := errors.New("the rename is refused while a session is on the branch")
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(oldNewer))
	f.Respond(swapRenameBack[0], Rows{})
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.RespondErr(sqlRenameWorkBack, renameErr)

	_, err := Recover(context.Background(), f, cutOf())
	if !errors.Is(err, renameErr) {
		t.Fatalf("Recover returned %v, want the failed rename's error", err)
	}
	if !strings.Contains(err.Error(), "renaming it to main failed") {
		t.Errorf("error %q does not say the rename back failed", err)
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "CALL DOLT_BRANCH('-D'") {
			t.Errorf("the recovery dropped a branch after a failed rename back: %q", statement)
		}
	}
}

// TestRecoverRefusesAMainTipItDidNotRestore drives the last proof: a recovery
// whose final catalog holds main at a tip other than the one it restored is a
// failure, not a redo.
func TestRecoverRefusesAMainTipItDidNotRestore(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main-pre-squash": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	restored := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip, "compact-base": fx.initial}
	moved := map[string]string{"main": hash(0x62)}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond(branchesStatement, branchesRows(restored))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
	f.Respond(branchesStatement, branchesRows(moved))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "the recovery left main at") {
		t.Fatalf("Recover returned %v, want a refusal naming the tip main held", err)
	}
}

// TestRecoverRefusesALeftoverBranchAfterDropping drives the catalog read that
// proves a recovery dropped what it meant to: a branch still listed that the
// recovery does not report keeping is a failure, not a quiet success.
func TestRecoverRefusesALeftoverBranchAfterDropping(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
	f.Respond(branchesStatement, branchesRows(found))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "the recovery left compact-work") {
		t.Fatalf("Recover returned %v, want a refusal naming the branch left behind", err)
	}
}

// TestRecoverRefusesACatalogWithoutMain drives the last proof's first refusal: a
// catalog left with no main at all is a failure, not a recovery, because nothing
// the recovery did can be checked against the branch the run leaves in place.
func TestRecoverRefusesACatalogWithoutMain(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main": fx.before.Tip, "compact-work": fx.before.Tip}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
	f.Respond(branchesStatement, branchesRows(nil))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "want main and the branches it kept") {
		t.Fatalf("Recover returned %v, want a refusal naming the missing main", err)
	}
}

// TestRecoverRefusesAKeptBranchTheCatalogLost drives the last proof's second
// refusal: a recovery that reports keeping a branch the catalog no longer lists
// has not accounted for the commits it kept it for, so it fails rather than
// passing as a clean recovery.
func TestRecoverRefusesAKeptBranchTheCatalogLost(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed on the branch that became main"}
	found := map[string]string{"main": fx.before.Tip, "compact-work": hash(0x50), "compact-base": fx.initial}
	left := map[string]string{"main": fx.before.Tip}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond("CALL DOLT_CHECKOUT", Rows{})
	f.Respond("CALL DOLT_BRANCH", Rows{})
	f.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	f.Respond(recoveryNewerStatement("compact-work"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
	f.Respond(branchesStatement, branchesRows(left))

	_, err := Recover(context.Background(), f, cutOf())
	if err == nil || !strings.Contains(err.Error(), "reports keeping compact-work") {
		t.Fatalf("Recover returned %v, want a refusal naming the kept branch the catalog lost", err)
	}
}

// TestRecoverRefusesAStateNoRowNames drives each catalog the design's table has
// no row for: the recovery stops before a statement that moves a branch.
func TestRecoverRefusesAStateNoRowNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		branches map[string]string
		want     string
	}{
		{"no branch at all", map[string]string{}, "lists no branch at all"},
		{"a work branch without main", map[string]string{"compact-work": hash(0x51), "compact-base": hash(0x52)}, "but no main"},
		{"main-pre-squash alone", map[string]string{"main-pre-squash": hash(0x53)}, "which no row of the recovery table names"},
		{"main, main-pre-squash and compact-work", map[string]string{
			"main": hash(0x54), "main-pre-squash": hash(0x55), "compact-work": hash(0x56),
		}, "which no row of the recovery table names"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := NewFakeExec()
			f.Respond(branchesStatement, branchesRows(tc.branches))
			got, err := Recover(context.Background(), f, cutOf())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Recover returned %v, want an error holding %q", err, tc.want)
			}
			if got != nil {
				t.Errorf("the refused recovery returned %+v, want nil", got)
			}
			if got := f.Statements(); !slices.Equal(got, []string{branchesStatement}) {
				t.Errorf("the refused recovery sent %q, want only the branch read", got)
			}
		})
	}
}

// TestRecoverRefusesAWiringItCannotTrust drives the recovery's own input checks
// and its first read: a missing Exec, a missing cut, and a read it cannot trust
// stop it before any statement that moves a branch.
func TestRecoverRefusesAWiringItCannotTrust(t *testing.T) {
	t.Parallel()
	if _, err := Recover(context.Background(), nil, cutOf()); err == nil || !strings.Contains(err.Error(), "has no Exec") {
		t.Errorf("Recover with no Exec returned %v, want a refusal naming the Exec", err)
	}
	f := NewFakeExec()
	if _, err := Recover(context.Background(), f, time.Time{}); err == nil || !strings.Contains(err.Error(), "has no cut") {
		t.Errorf("Recover with no cut returned %v, want a refusal naming the cut", err)
	}
	if got := len(f.Statements()); got != 0 {
		t.Errorf("the refused recovery sent %d statements, want none", got)
	}
	readErr := errors.New("the catalog is unreadable")
	f.RespondErr(branchesStatement, readErr)
	if _, err := Recover(context.Background(), f, cutOf()); !errors.Is(err, readErr) {
		t.Errorf("Recover returned %v, want the unreadable catalog's error", err)
	}
	f = NewFakeExec()
	f.Respond(branchesStatement, Rows{Columns: []string{"name"}, Data: [][]string{{"main"}}})
	if _, err := Recover(context.Background(), f, cutOf()); err == nil || !strings.Contains(err.Error(), "want a name and a tip") {
		t.Errorf("Recover with a one-value branch record returned %v, want a refusal naming the record", err)
	}
	f = NewFakeExec()
	f.Respond(branchesStatement, Rows{Columns: []string{"name", "hash"}, Data: [][]string{{"main", "not a hash"}}})
	if _, err := Recover(context.Background(), f, cutOf()); err == nil || !strings.Contains(err.Error(), "want a 32-character Dolt commit hash") {
		t.Errorf("Recover with an untrusted tip returned %v, want a refusal naming the hash", err)
	}
}

// TestRunRefusesToStartWhenADeadRunLeftCommitsOnTheNewMain proves the wiring:
// the run recovers first, and a kept catalog whose new main lacks a commit of
// the old main stops the run before a statement of its own.
func TestRunRefusesToStartWhenADeadRunLeftCommitsOnTheNewMain(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	landed := commitIdentity{AuthorDate: "2026-11-12 10:00:00.000", Message: "fix: landed after the swap"}
	tail := commitIdentity{AuthorDate: "2026-11-12 10:30:00.000", Message: "fix: tail not replayed"}
	found := map[string]string{"main": hash(0x50), "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	f := NewFakeExec()
	f.Respond(branchesStatement, branchesRows(found))
	f.Respond(recoveryNewerStatement("main"), identitiesRows(append(slices.Clone(fx.newer), landed)))
	f.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(append(slices.Clone(fx.newer), tail)))
	ex := newBranchExec(f, maps.Clone(found))
	marker := &runMarker{}
	clk := newTestClock()

	got, err := Run(context.Background(), RunInput{Exec: ex, Marker: marker, Clock: clk, Sleeper: &testSleeper{clk: clk}, Cut: cutOf()})
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("Run returned %v, want a refusal naming the dead run", err)
	}
	if got.Recovered == nil || got.Recovered.State != RecoverySwappedKept {
		t.Fatalf("the run reported recovery %+v, want the kept state", got.Recovered)
	}
	if got.Held {
		t.Errorf("the run reports Held for a new main that lacks an old-main commit: %+v", got.Recovered)
	}
	if got := len(ex.Statements()); got != 3 {
		t.Errorf("the refused run sent %d statements, want only the recovery's three reads", got)
	}
	if !reflect.DeepEqual(ex.runBranches(), found) {
		t.Errorf("the refused run left the branches as %v, want %v", ex.runBranches(), found)
	}
	if marker.held || marker.releases != 1 {
		t.Errorf("the refused run held %v with %d releases, want the marker released", marker.held, marker.releases)
	}
}

// TestRunHoldsAfterACompletedSwap proves the start after a run finished its
// swap: the new main holds every commit of the old one, so the run sends no
// step statement, reports Held, and leaves the branches for the retention
// check instead of undoing the squash.
func TestRunHoldsAfterACompletedSwap(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	found := map[string]string{"main": fx.made, "main-pre-squash": fx.before.Tip, "compact-base": fx.initial}
	// Only the recovery's three reads are scripted: a held run sends nothing
	// else, and the fake refuses a statement no answer covers.
	script := NewFakeExec()
	script.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
	script.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(fx.newer))
	ex := newBranchExec(script, maps.Clone(found))
	marker := &runMarker{}
	clk := newTestClock()

	got, err := Run(context.Background(), RunInput{Exec: ex, Marker: marker, Clock: clk, Sleeper: &testSleeper{clk: clk}, Cut: cutOf()})
	if err != nil {
		t.Fatalf("Run after a completed swap: %v", err)
	}
	if !got.Held {
		t.Errorf("the run reports Held %v, want true for a standing swap", got.Held)
	}
	if got.Recovered == nil || !got.Recovered.Complete() {
		t.Fatalf("the run reported recovery %+v, want the complete row", got.Recovered)
	}
	if got.Summary != (RunSummary{}) {
		t.Errorf("a held run reports summary %+v, want no step recorded", got.Summary)
	}
	if got := len(ex.Statements()); got != 3 {
		t.Errorf("the held run sent %d statements, want only the recovery's three reads: %q", got, ex.Statements())
	}
	if !reflect.DeepEqual(ex.runBranches(), found) {
		t.Errorf("the held run left the branches as %v, want %v", ex.runBranches(), found)
	}
	if marker.held || marker.releases != 1 {
		t.Errorf("the held run held %v with %d releases, want the marker released", marker.held, marker.releases)
	}
}

// recoveryRows is the design's recovery table written as the branch catalogs a
// run can leave behind, in name order. The sweep below looks the expected row
// up here rather than recomputing which statement the death landed on.
var recoveryRows = []struct {
	branches []string
	state    RecoveryState
}{
	{[]string{"main"}, ""},
	{[]string{"compact-base", "main"}, RecoveryBeforeSwap},
	{[]string{"compact-base", "compact-work", "main"}, RecoveryBeforeSwap},
	{[]string{"compact-base", "compact-work", "main-pre-squash"}, RecoveryFirstRename},
	{[]string{"compact-base", "main", "main-pre-squash"}, RecoverySwappedKept},
}

// recoveryStateFor returns the design's row for the catalog a death left, and
// fails the test for a catalog the table does not name.
func recoveryStateFor(t *testing.T, left map[string]string) RecoveryState {
	t.Helper()
	names := make([]string, 0, len(left))
	for name := range left {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, row := range recoveryRows {
		if slices.Equal(row.branches, names) {
			return row.state
		}
	}
	t.Fatalf("the branches a death left (%s) match no row of the recovery table", strings.Join(names, ", "))
	return ""
}

// TestRunRecoversADeathAfterEveryStatement drives the recovery from every point
// a run can stop: a death is injected at each statement of a healthy run, and
// the next start must recover the state the design's table names for the
// database the death left.
func TestRunRecoversADeathAfterEveryStatement(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	cut := cutOf()
	start := map[string]string{"main": fx.before.Tip}

	healthyClock := newTestClock()
	healthy := newBranchExec(fx.fake(), maps.Clone(start))
	if _, err := Run(context.Background(), RunInput{
		Exec: healthy, Marker: &runMarker{}, Clock: healthyClock, Sleeper: &testSleeper{clk: healthyClock}, Cut: cut,
	}); err != nil {
		t.Fatalf("the healthy run every index comes from: %v", err)
	}
	statements := healthy.Statements()

	for died := 0; died <= len(statements); died++ {
		t.Run(fmt.Sprintf("death_after_statement_%d", died), func(t *testing.T) {
			t.Parallel()
			dead := newBranchExec(fx.fake(), maps.Clone(start))
			dead.FailAt(died+1, errDeath)
			clock := newTestClock()
			if _, err := Run(context.Background(), RunInput{
				Exec: dead, Marker: &runMarker{}, Clock: clock, Sleeper: &testSleeper{clk: clock}, Cut: cut,
			}); died < len(statements) && !errors.Is(err, errDeath) {
				t.Fatalf("the run that dies after statement %d returned %v, want the injected death", died, err)
			}
			left := dead.runBranches()
			want := recoveryStateFor(t, left)

			next := fx.fake()
			next.Respond(recoveryNewerStatement("main"), identitiesRows(fx.newer))
			next.Respond(recoveryNewerStatement("main-pre-squash"), identitiesRows(fx.newer))
			next.Respond(recoveryNewerStatement("compact-work"), identitiesRows(fx.newer))
			next.Respond(recoveryNewerStatement("compact-base"), identitiesRows(nil))
			nextExec := newBranchExec(next, maps.Clone(left))
			clock = newTestClock()
			got, err := Run(context.Background(), RunInput{
				Exec: nextExec, Marker: &runMarker{}, Clock: clock, Sleeper: &testSleeper{clk: clock}, Cut: cut,
			})
			if err != nil {
				t.Fatalf("the next start after a death at statement %d: %v, branches left %v", died, err, left)
			}
			var gotState RecoveryState
			if got.Recovered != nil {
				gotState = got.Recovered.State
			}
			if gotState != want {
				t.Errorf("the next start recovered %q (%+v), want %q: a death after statement %d left %v",
					gotState, got.Recovered, want, died, left)
			}
			if want == RecoverySwappedKept && !got.Held {
				t.Errorf("the next start over a standing swap reports Held %v, want true: a death after statement %d left %v",
					got.Held, died, left)
			}
			if first := nextExec.Statements()[0]; first != branchesStatement {
				t.Errorf("the next start's first statement is %q, want the branch read", first)
			}
		})
	}
}

// TestRunSummaryCountsTheCommitsDurationsTailAndRetries drives the summary line
// the design records per run: the commit count before and after, the three
// durations, the tail length and the retries.
func TestRunSummaryCountsTheCommitsDurationsTailAndRetries(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	fx.oldTip = hash(0x31)
	fx.tail = []string{hash(0x41)}
	fx.pickRetries = 1
	f := fx.fake()
	clk := newTickingClock(time.Second)

	got, err := Run(context.Background(), RunInput{
		Exec: f, Marker: &runMarker{}, Clock: clk, Sleeper: noWait{}, Cut: cutOf(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	summary := got.Summary
	if summary.CommitsBefore != fx.before.Commits {
		t.Errorf("CommitsBefore is %d, want the BEFORE record's %d", summary.CommitsBefore, fx.before.Commits)
	}
	if !summary.AfterRead || summary.CommitsAfter != fx.swapped {
		t.Errorf("the after count is %d (read %v), want the count read after the swap, %d", summary.CommitsAfter, summary.AfterRead, fx.swapped)
	}
	if summary.Tail != len(fx.tail) {
		t.Errorf("Tail is %d, want %d", summary.Tail, len(fx.tail))
	}
	if summary.Retries != 1 {
		t.Errorf("Retries is %d, want the catch-up's one retry", summary.Retries)
	}
	for _, step := range []struct {
		name string
		took time.Duration
	}{
		{"rebase", summary.Rebase},
		{"catch-up", summary.Catchup},
		{"swap", summary.Swap},
	} {
		if step.took <= 0 {
			t.Errorf("the %s duration is %v, want the time the clock moved during the step", step.name, step.took)
		}
	}
}

// TestRunStopsWhenTheAfterCountCannotBeRead drives the summary's last read: a
// run whose new main cannot be counted reports the error instead of a summary
// that reads as a database with no commits.
func TestRunStopsWhenTheAfterCountCannotBeRead(t *testing.T) {
	t.Parallel()
	fx := newRunFixture()
	afterErr := errors.New("the connection closed")
	fx.afterErr = afterErr
	f := fx.fake()
	clk := newTestClock()

	got, err := Run(context.Background(), RunInput{
		Exec: f, Marker: &runMarker{}, Clock: clk, Sleeper: &testSleeper{clk: clk}, Cut: cutOf(),
	})
	if !errors.Is(err, afterErr) {
		t.Fatalf("Run returned %v, want the after count's error", err)
	}
	if got.Summary.AfterRead {
		t.Errorf("the summary reports an after count it could not read: %+v", got.Summary)
	}
	if got.Summary.CommitsBefore != fx.before.Commits {
		t.Errorf("the summary lost the before count: %+v", got.Summary)
	}
}

// branchExec is the package's fake with a branch catalog: it applies every
// branch statement to a map of branch tips and answers a read of the branches
// from that map, so a test sees the database a run leaves behind. Every other
// statement is answered by the scripted fake it wraps.
//
// It keeps its own statement list and failure index, so a death is injected
// over the statements the run sends rather than the ones the scripted fake
// answers.
type branchExec struct {
	*FakeExec
	mu       sync.Mutex
	branches map[string]string
	all      []string
	failAt   int
	failErr  error
}

// newBranchExec wraps script with a branch catalog holding branches.
func newBranchExec(script *FakeExec, branches map[string]string) *branchExec {
	return &branchExec{FakeExec: script, branches: branches}
}

// FailAt makes statement n, counting from 1 over every statement the run
// sends, fail with err.
func (b *branchExec) FailAt(n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failAt, b.failErr = n, err
}

// Statements returns every statement the run sent, in order.
func (b *branchExec) Statements() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.all)
}

// runBranches returns the branch catalog the run left.
func (b *branchExec) runBranches() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.branches)
}

// Exec records the statement, fails it when the test's death lands there, and
// answers it from the branch catalog when it names a branch.
func (b *branchExec) Exec(ctx context.Context, statement string) (Rows, error) {
	if err := ctx.Err(); err != nil {
		return Rows{}, err
	}
	b.mu.Lock()
	b.all = append(b.all, statement)
	if len(b.all) == b.failAt {
		err := b.failErr
		b.mu.Unlock()
		return Rows{}, err
	}
	if handled, rows, err := b.branchLocked(statement); handled {
		b.mu.Unlock()
		return rows, err
	}
	b.mu.Unlock()
	return b.FakeExec.Exec(ctx, statement)
}

// branchLocked answers the statements that read or move branches and reports
// whether it handled the statement. The caller holds b.mu.
func (b *branchExec) branchLocked(statement string) (bool, Rows, error) {
	switch {
	case statement == branchesStatement:
		return true, branchesRows(b.branches), nil
	case strings.HasPrefix(statement, "CALL DOLT_BRANCH("):
		return true, Rows{}, b.moveLocked(statement)
	case strings.HasPrefix(statement, "CALL DOLT_CHECKOUT("):
		return true, Rows{}, nil
	default:
		return false, Rows{}, nil
	}
}

// moveLocked applies one DOLT_BRANCH statement to the catalog. A rename or a
// delete of a branch the catalog does not hold, and a create of one it does,
// are errors: the run must not lean on a statement the real Dolt would refuse.
func (b *branchExec) moveLocked(statement string) error {
	args := quotedLiterals(statement)
	if len(args) == 0 {
		return fmt.Errorf("fake: no literal in %q", statement)
	}
	switch args[0] {
	case "-m":
		if len(args) < 3 {
			return fmt.Errorf("fake: rename %q names no two branches", statement)
		}
		from, to := args[len(args)-2], args[len(args)-1]
		tip, ok := b.branches[from]
		if !ok {
			return fmt.Errorf("fake: rename of branch %q, which does not exist: %s", from, statement)
		}
		delete(b.branches, from)
		b.branches[to] = tip
	case "-D", "-d":
		if len(args) < 2 {
			return fmt.Errorf("fake: drop %q names no branch", statement)
		}
		if _, ok := b.branches[args[1]]; !ok {
			return fmt.Errorf("fake: drop of branch %q, which does not exist: %s", args[1], statement)
		}
		delete(b.branches, args[1])
	default:
		if len(args) < 2 {
			return fmt.Errorf("fake: create %q names no tip", statement)
		}
		if _, ok := b.branches[args[0]]; ok {
			return fmt.Errorf("fake: create of branch %q, which already exists: %s", args[0], statement)
		}
		b.branches[args[0]] = args[1]
	}
	return nil
}

// quotedLiterals returns the single-quoted literals of statement, in order.
func quotedLiterals(statement string) []string {
	parts := strings.Split(statement, "'")
	literals := make([]string, 0, len(parts)/2)
	for i := 1; i < len(parts); i += 2 {
		literals = append(literals, parts[i])
	}
	return literals
}

// tickingClock is a Clock that moves one step on every read, so a step that
// reads it twice reports a duration even though the test runs at once.
type tickingClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

// newTickingClock returns the summary's clock, starting at the package epoch.
func newTickingClock(step time.Duration) *tickingClock {
	return &tickingClock{now: testEpoch(), step: step}
}

// Now returns the next reading, one step after the last.
func (c *tickingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

// noWait is a Sleeper that returns at once: the ticking clock is what moves in
// the summary's test.
type noWait struct{}

// Sleep returns without waiting.
func (noWait) Sleep(time.Duration) {}
