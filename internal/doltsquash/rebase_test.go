package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// hash returns a Dolt-shaped commit hash that differs per n: 32 characters of
// the base32 alphabet 0-9a-v. The tail holds letters past f, so a validator
// that accepts only hex would refuse every fixture and the tests would catch
// it (gt-1w9790).
func hash(n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuv"
	const tail = "jlup0pifvs02kgdldm3n3s548m9qtum"
	return string(alphabet[n%len(alphabet)]) + tail
}

// single returns the one-value answer a scalar or count read expects.
func single(column, value string) Rows {
	return Rows{Columns: []string{column}, Data: [][]string{{value}}}
}

// cutOf returns a cut seven days before the package's fixed epoch, the
// keep window of docs/plans/2026-11-15-dolt-squash-design.md.
func cutOf() time.Time {
	return testEpoch().Add(-7 * 24 * time.Hour)
}

// The statements the BEFORE record's happy script answers, keyed so a test can
// replace one answer and keep the rest.
const (
	sqlCheckoutMain   = "CALL DOLT_CHECKOUT('main')"
	sqlActiveBranch   = "SELECT active_branch()"
	sqlMainTip        = "SELECT dolt_hashof('main')"
	sqlMainCommits    = "SELECT COUNT(*) FROM dolt_log('main')"
	sqlShowTables     = "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'"
	sqlIgnoreComments = "SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND 'comments' LIKE pattern"
	sqlIgnoreEvents   = "SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND 'events' LIKE pattern"
	sqlIgnoreIssues   = "SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND 'issues' LIKE pattern"
	sqlCountComments  = "SELECT COUNT(*) FROM `comments`"
	sqlCountIssues    = "SELECT COUNT(*) FROM `issues`"
	sqlHashComments   = "SELECT dolt_hashof_table('comments')"
	sqlHashIssues     = "SELECT dolt_hashof_table('issues')"
)

// recordFake answers the BEFORE record's reads for a four-table catalog whose
// events table is dolt_ignored and whose dolt_ignore system table is skipped,
// replacing the answers changes names, so the failure a test expects is the one
// it injected.
func recordFake(changes map[string]Rows) *FakeExec {
	f := NewFakeExec()
	answer := func(statement string, rows Rows) {
		if replaced, ok := changes[statement]; ok {
			rows = replaced
		}
		f.Respond(statement, rows)
	}
	answer(sqlCheckoutMain, Rows{})
	answer(sqlActiveBranch, single("branch", "main"))
	answer(sqlMainTip, single("dolt_hashof('main')", hash(1)))
	answer(sqlMainCommits, single("n", "12002"))
	answer(sqlShowTables, Rows{
		Columns: []string{"Tables_in_gt"},
		Data:    [][]string{{"comments"}, {"dolt_ignore"}, {"events"}, {"issues"}},
	})
	answer(sqlIgnoreComments, single("n", "0"))
	answer(sqlIgnoreEvents, single("n", "1"))
	answer(sqlIgnoreIssues, single("n", "0"))
	answer(sqlCountComments, single("n", "3"))
	answer(sqlCountIssues, single("n", "9"))
	answer(sqlHashComments, single("h", hash(2)))
	answer(sqlHashIssues, single("h", hash(3)))
	return f
}

// The statements design step 4's happy script answers. The filter's cut is
// cutOf(), 2026-11-08 20:00:00, and the fixture's first plan row is order 1.
const (
	sqlInitialCommit = "SELECT commit_hash FROM dolt_log('main') ORDER BY date ASC LIMIT 1"
	sqlBranchBase    = "CALL DOLT_BRANCH('compact-base', "
	sqlBranchWork    = "CALL DOLT_BRANCH('compact-work', "
	sqlCheckoutWork  = "CALL DOLT_CHECKOUT('compact-work')"
	sqlRebaseStart   = "CALL DOLT_REBASE('--interactive', 'compact-base')"
	sqlPlan          = "SELECT rebase_order, commit_hash FROM dolt_rebase"
	sqlSquashFilter  = "rebase_order > 1 AND commit_hash IN (SELECT commit_hash FROM dolt_log('main') WHERE date <= '2026-11-08 20:00:00')"
	sqlMarkCount     = "SELECT COUNT(*) FROM dolt_rebase WHERE " + sqlSquashFilter
	sqlMarkSquash    = "UPDATE dolt_rebase SET action = 'squash' WHERE " + sqlSquashFilter
	sqlSquashCount   = "SELECT COUNT(*) FROM dolt_rebase WHERE action = 'squash'"
	sqlRebaseResume  = "CALL DOLT_REBASE('--continue')"
	// sqlSessionLogCount is the same filter with the log left unqualified, the
	// way the 11-01 design read it inside the rebase session.
	sqlSessionLogCount = "SELECT COUNT(*) FROM dolt_rebase WHERE rebase_order > 1 AND commit_hash IN (SELECT commit_hash FROM dolt_log WHERE date <= "
)

// rebaseFixture is the database design step 4's tests script: main's initial
// commit, the tip the BEFORE record holds, a four-row plan whose first row
// stays a pick, and the count of rows the cut marks.
type rebaseFixture struct {
	initial, tip string
	plan         []planRow
	marked       int
}

func newRebaseFixture() rebaseFixture {
	return rebaseFixture{
		initial: hash(0x09),
		tip:     hash(0x01),
		plan: []planRow{
			{1, hash(0x11)},
			{2, hash(0x12)},
			{3, hash(0x13)},
			{4, hash(0x14)},
		},
		marked: 2,
	}
}

// fake answers the whole of design step 4, replacing the answers changes name.
func (fx rebaseFixture) fake(changes map[string]Rows) *FakeExec {
	f := NewFakeExec()
	answer := func(statement string, rows Rows) {
		if replaced, ok := changes[statement]; ok {
			rows = replaced
		}
		f.Respond(statement, rows)
	}
	plan := Rows{Columns: []string{"rebase_order", "commit_hash"}}
	for _, row := range fx.plan {
		plan.Data = append(plan.Data, []string{fmt.Sprint(row.order), row.commit})
	}
	answer(sqlInitialCommit, single("commit_hash", fx.initial))
	answer(sqlBranchBase, Rows{})
	answer(sqlBranchWork, Rows{})
	answer(sqlCheckoutWork, Rows{})
	answer(sqlRebaseStart, Rows{})
	answer(sqlPlan, plan)
	answer(sqlMarkCount, single("n", fmt.Sprint(fx.marked)))
	answer(sqlMarkSquash, Rows{})
	answer(sqlSquashCount, single("n", fmt.Sprint(fx.marked)))
	answer(sqlRebaseResume, Rows{})
	return f
}

func TestRecordBeforeHoldsTipCommitsAndEachVersionedTable(t *testing.T) {
	t.Parallel()
	f := recordFake(nil)

	got, err := RecordBefore(context.Background(), f)
	if err != nil {
		t.Fatalf("RecordBefore: %v", err)
	}
	want := Before{
		Tip:     hash(1),
		Commits: 12002,
		Tables: []TableState{
			{Table: "comments", Rows: 3, Hash: hash(2)},
			{Table: "issues", Rows: 9, Hash: hash(3)},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RecordBefore returned %+v, want %+v", got, want)
	}
	// The counts and hashes come from the session's working set, so the session
	// is put on main and checked there before any of them is read.
	opens := []string{sqlCheckoutMain, sqlActiveBranch, sqlMainTip}
	if sent := f.Statements(); len(sent) < len(opens) || !slices.Equal(sent[:len(opens)], opens) {
		t.Errorf("the record opened with %q, want checkout main, the branch check, then main's tip", f.Statements())
	}
	for _, statement := range f.Statements() {
		if strings.Contains(statement, "FROM `events`") || strings.Contains(statement, "dolt_hashof_table('events')") {
			t.Errorf("a dolt_ignored table was read: %q", statement)
		}
	}
}

// TestRecordBeforeSkipsDoltSystemTables drives the catalog branch that skips a
// dolt_-prefixed name: dolt_ignore is listed as a base table, but it is neither
// counted nor hashed, because its state is not part of the history a later
// comparison could hold it to.
func TestRecordBeforeSkipsDoltSystemTables(t *testing.T) {
	t.Parallel()
	f := recordFake(nil)

	got, err := RecordBefore(context.Background(), f)
	if err != nil {
		t.Fatalf("RecordBefore: %v", err)
	}
	for _, table := range got.Tables {
		if strings.HasPrefix(table.Table, "dolt_") {
			t.Errorf("the record holds the system table %q", table.Table)
		}
	}
	for _, statement := range f.Statements() {
		if strings.Contains(statement, "`dolt_") || strings.Contains(statement, "'dolt_") {
			t.Errorf("the record read a system table: %q", statement)
		}
	}
}

// TestRecordBeforeRefusesASessionNotOnMain drives the branch check: the record
// must be refused, with the branch check's position, before any count or hash
// is read from the session's working set.
func TestRecordBeforeRefusesASessionNotOnMain(t *testing.T) {
	t.Parallel()
	f := recordFake(map[string]Rows{sqlActiveBranch: single("branch", "compact-work")})

	_, err := RecordBefore(context.Background(), f)
	if err == nil {
		t.Fatal("RecordBefore returned no error on a session that is not on main")
	}
	for _, want := range []string{"statement 2 (SELECT active_branch())", `"compact-work"`, "want main"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := len(f.Statements()); got != 2 {
		t.Errorf("the refused record sent %d statements, want 2: nothing is read before the branch is checked", got)
	}
}

func TestRecordBeforeQuotesCatalogNames(t *testing.T) {
	t.Parallel()
	const name = "we`ird'name\\x"
	f := NewFakeExec()
	f.Respond(sqlCheckoutMain, Rows{})
	f.Respond(sqlActiveBranch, single("branch", "main"))
	f.Respond(sqlMainTip, single("dolt_hashof('main')", hash(1)))
	f.Respond(sqlMainCommits, single("n", "1"))
	f.Respond(sqlShowTables, Rows{Columns: []string{"Tables_in_gt"}, Data: [][]string{{name}}})
	f.Respond("SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND 'we`ird''name\\\\x' LIKE pattern", single("n", "0"))
	f.Respond("SELECT COUNT(*) FROM `we``ird'name\\x`", single("n", "5"))
	f.Respond("SELECT dolt_hashof_table('we`ird''name\\\\x')", single("h", hash(4)))

	got, err := RecordBefore(context.Background(), f)
	if err != nil {
		t.Fatalf("RecordBefore: %v", err)
	}
	want := []string{
		sqlCheckoutMain,
		sqlActiveBranch,
		sqlMainTip,
		sqlMainCommits,
		sqlShowTables,
		"SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND 'we`ird''name\\\\x' LIKE pattern",
		"SELECT COUNT(*) FROM `we``ird'name\\x`",
		"SELECT dolt_hashof_table('we`ird''name\\\\x')",
	}
	if sent := f.Statements(); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
	if len(got.Tables) != 1 || got.Tables[0].Table != name {
		t.Errorf("the record holds %+v, want the table %q by its own name", got.Tables, name)
	}
}

func TestRebaseHistoryMarksAfterTheFirstAndReturnsTheCount(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	f := fx.fake(nil)

	got, err := RebaseHistory(context.Background(), f, Before{Tip: fx.tip}, cutOf())
	if err != nil {
		t.Fatalf("RebaseHistory: %v", err)
	}
	if got.Marked != fx.marked {
		t.Errorf("RebaseHistory marked %d rows, want %d, the count its filter answered", got.Marked, fx.marked)
	}
	want := []string{
		sqlInitialCommit,
		"CALL DOLT_BRANCH('compact-base', '" + fx.initial + "')",
		"CALL DOLT_BRANCH('compact-work', '" + fx.tip + "')",
		sqlCheckoutWork,
		sqlRebaseStart,
		sqlPlan + " ORDER BY rebase_order",
		sqlMarkCount,
		sqlMarkSquash,
		sqlSquashCount,
		sqlRebaseResume,
	}
	if sent := f.Statements(); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

// TestRebaseHistoryReturnsTheRowsTheUpdateMarked drives the readback: the count
// the filter gives before the update and the count of squash rows after it are
// different answers, and the run must return the second, because that is what
// the plan holds once it is marked.
func TestRebaseHistoryReturnsTheRowsTheUpdateMarked(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	f := fx.fake(map[string]Rows{sqlSquashCount: single("n", "3")})

	got, err := RebaseHistory(context.Background(), f, Before{Tip: fx.tip}, cutOf())
	if err != nil {
		t.Fatalf("RebaseHistory: %v", err)
	}
	if got.Marked != 3 {
		t.Errorf("RebaseHistory marked %d rows, want 3, the count read back after the update, not the filter's %d",
			got.Marked, fx.marked)
	}
}

// TestRebaseHistoryReadsTheOrderDoltRenders drives the plan's order as Dolt
// renders it: dolt_rebase.rebase_order is a DECIMAL(6,2), so the number arrives
// as "1.00" rather than "1". The lab (gt-dmxf57) is where the integer-only parse
// met it; this keeps the case in the unit tier, off a real server.
func TestRebaseHistoryReadsTheOrderDoltRenders(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	plan := Rows{Columns: []string{"rebase_order", "commit_hash"}}
	for _, row := range fx.plan {
		plan.Data = append(plan.Data, []string{fmt.Sprintf("%d.00", row.order), row.commit})
	}
	f := fx.fake(map[string]Rows{sqlPlan: plan})

	got, err := RebaseHistory(context.Background(), f, Before{Tip: fx.tip}, cutOf())
	if err != nil {
		t.Fatalf("RebaseHistory refused the order Dolt renders: %v", err)
	}
	if got.Marked != fx.marked {
		t.Errorf("RebaseHistory marked %d rows, want %d", got.Marked, fx.marked)
	}
}

// TestRebaseFilterReadsTheLogByBranchName pins the statement text the engine
// sends for the case the 11-01 design got wrong: inside a rebase session an
// unqualified dolt_log sees only the base, so commit_hash IN (SELECT commit_hash
// FROM dolt_log WHERE date <= cut) counts no plan row, while the same filter
// read from dolt_log('main') counts every old row after the first. The fake
// answers the unqualified count with zero and the qualified one with the marked
// rows, so an engine that drops the branch name is refused before it marks
// anything. This pins the statement, not Dolt's behaviour; the behaviour is
// pinned by the lab integration bead (D11).
func TestRebaseFilterReadsTheLogByBranchName(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	f := fx.fake(nil)
	f.Respond(sqlSessionLogCount, single("n", "0"))

	got, err := RebaseHistory(context.Background(), f, Before{Tip: fx.tip}, cutOf())
	if err != nil {
		t.Fatalf("RebaseHistory refused the branch-qualified filter: %v", err)
	}
	if got.Marked != fx.marked {
		t.Errorf("RebaseHistory marked %d rows, want %d", got.Marked, fx.marked)
	}
	counted := false
	for _, statement := range f.Statements() {
		switch {
		case strings.HasPrefix(statement, sqlMarkCount):
			counted = true
			if !strings.Contains(statement, "dolt_log('main')") {
				t.Errorf("the count reads the log without the branch name: %q", statement)
			}
		case strings.HasPrefix(statement, "UPDATE dolt_rebase"):
			if !strings.Contains(statement, "dolt_log('main')") {
				t.Errorf("the update reads the log without the branch name: %q", statement)
			}
		}
	}
	if !counted {
		t.Error("the run sent no count statement, so the rows marked were never counted")
	}
}

func TestStatementErrorsStopTheRunWithTheirPosition(t *testing.T) {
	t.Parallel()
	refused := errors.New("server closed the connection")
	fx := newRebaseFixture()
	runs := []struct {
		name string
		fake func() *FakeExec
		call func(context.Context, Exec) error
	}{
		{
			name: "RecordBefore",
			fake: func() *FakeExec { return recordFake(nil) },
			call: func(ctx context.Context, ex Exec) error {
				_, err := RecordBefore(ctx, ex)
				return err
			},
		},
		{
			name: "RebaseHistory",
			fake: func() *FakeExec { return fx.fake(nil) },
			call: func(ctx context.Context, ex Exec) error {
				_, err := RebaseHistory(ctx, ex, Before{Tip: fx.tip}, cutOf())
				return err
			},
		},
	}
	for _, run := range runs {
		t.Run(run.name, func(t *testing.T) {
			clean := run.fake()
			if err := run.call(context.Background(), clean); err != nil {
				t.Fatalf("the scripted run failed before any injection: %v", err)
			}
			sent := clean.Statements()
			if len(sent) < 5 {
				t.Fatalf("the scripted run sent %d statements, too few to test every position", len(sent))
			}
			for n := 1; n <= len(sent); n++ {
				f := run.fake()
				f.FailAt(n, refused)
				err := run.call(context.Background(), f)
				if !errors.Is(err, refused) {
					t.Fatalf("statement %d: error is %v, want the injected one", n, err)
				}
				if want := fmt.Sprintf("statement %d (", n); !strings.Contains(err.Error(), want) {
					t.Errorf("statement %d: error %q does not carry %q", n, err, want)
				}
				if !strings.Contains(err.Error(), shortStatement(sent[n-1])) {
					t.Errorf("statement %d: error %q does not name the statement %q", n, err, shortStatement(sent[n-1]))
				}
				if got := len(f.Statements()); got != n {
					t.Errorf("statement %d failed, but the run sent %d statements: it did not stop", n, got)
				}
			}
		})
	}
}

func TestRecordBeforeRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes map[string]Rows
		want    string
	}{
		{
			name:    "the branch check answers no record",
			changes: map[string]Rows{sqlActiveBranch: {}},
			want:    "returned 0 records, want one",
		},
		{
			name:    "the tip read answers no record",
			changes: map[string]Rows{sqlMainTip: {}},
			want:    "returned 0 records, want one",
		},
		{
			name:    "the tip is not a commit hash",
			changes: map[string]Rows{sqlMainTip: single("h", "HEAD-of-main")},
			want:    "want a 32-character Dolt commit hash",
		},
		{
			name:    "the tip holds a character outside the base32 alphabet",
			changes: map[string]Rows{sqlMainTip: single("h", "chub53jlup0pifvs02kgdldm3n3s548w")},
			want:    "want a 32-character Dolt commit hash",
		},
		{
			name:    "the tip is a character short",
			changes: map[string]Rows{sqlMainTip: single("h", "chub53jlup0pifvs02kgdldm3n3s548")},
			want:    "want a 32-character Dolt commit hash",
		},
		{
			name:    "the tip is a character long",
			changes: map[string]Rows{sqlMainTip: single("h", "chub53jlup0pifvs02kgdldm3n3s548mm")},
			want:    "want a 32-character Dolt commit hash",
		},
		{
			name: "the tip read answers a record of several values",
			changes: map[string]Rows{sqlMainTip: {
				Columns: []string{"hash", "branch"},
				Data:    [][]string{{hash(1), "main"}},
			}},
			want: "returned a record of 2 values, want one",
		},
		{
			name:    "the commit count is not a number",
			changes: map[string]Rows{sqlMainCommits: single("n", "many")},
			want:    "want a count",
		},
		{
			name:    "a catalog record holds no value",
			changes: map[string]Rows{sqlShowTables: {Columns: nil, Data: [][]string{{}}}},
			want:    "with no values",
		},
		{
			name:    "a table count is not a number",
			changes: map[string]Rows{sqlCountComments: single("n", "three")},
			want:    "want a count",
		},
		{
			name:    "a table hash is empty",
			changes: map[string]Rows{sqlHashComments: single("h", "")},
			want:    "returned no hash",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := RecordBefore(context.Background(), recordFake(tc.changes))
			if err == nil {
				t.Fatalf("RecordBefore returned no error, want one holding %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
		})
	}
}

func TestRebaseHistoryRefusals(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	tests := []struct {
		name    string
		before  Before
		changes map[string]Rows
		want    string
	}{
		{
			name:   "the recorded tip is not a commit hash",
			before: Before{Tip: "main"},
			want:   "want a 32-character Dolt commit hash",
		},
		{
			name:    "main's log answers no initial commit",
			before:  Before{Tip: fx.tip},
			changes: map[string]Rows{sqlInitialCommit: {}},
			want:    "returned 0 records, want one",
		},
		{
			name:    "main's initial commit is not a commit hash",
			before:  Before{Tip: fx.tip},
			changes: map[string]Rows{sqlInitialCommit: single("commit_hash", "the-root")},
			want:    "want a 32-character Dolt commit hash",
		},
		{
			name:    "the plan is empty",
			before:  Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {}},
			want:    "the rebase plan on compact-work is empty",
		},
		{
			name:   "a plan record is short",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {
				Columns: []string{"rebase_order"},
				Data:    [][]string{{"1"}},
			}},
			want: "want order and commit",
		},
		{
			name:   "a plan order is not a number",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {
				Columns: []string{"rebase_order", "commit_hash"},
				Data:    [][]string{{"first", hash(0x11)}},
			}},
			want: "want a number",
		},
		{
			name:   "a plan order has a non-zero fraction",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {
				Columns: []string{"rebase_order", "commit_hash"},
				Data:    [][]string{{"1.50", hash(0x11)}},
			}},
			want: "want a number",
		},
		{
			name:   "a plan commit is not a hash",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {
				Columns: []string{"rebase_order", "commit_hash"},
				Data:    [][]string{{"1", "c1"}},
			}},
			want: "want a 32-character Dolt commit hash",
		},
		{
			name:    "the mark count is not a number",
			before:  Before{Tip: fx.tip},
			changes: map[string]Rows{sqlMarkCount: single("n", "some")},
			want:    "want a count",
		},
		{
			name:    "the cut reaches no plan row",
			before:  Before{Tip: fx.tip},
			changes: map[string]Rows{sqlMarkCount: single("n", "0")},
			want:    "the cut squashes nothing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := fx.fake(tc.changes)
			_, err := RebaseHistory(context.Background(), f, tc.before, cutOf())
			if err == nil {
				t.Fatalf("RebaseHistory returned no error, want one holding %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if tc.name == "the recorded tip is not a commit hash" && len(f.Statements()) != 0 {
				t.Errorf("a refused tip sent %d statements, want none", len(f.Statements()))
			}
		})
	}
}

func TestQuoteHelpersDoubleWhatSQLReadsAsAnEscape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"an identifier keeps its quotes inside one backtick pair", quoteIdent("a`b"), "`a``b`"},
		{"an identifier does not escape its backslash", quoteIdent(`a\b`), "`a\\b`"},
		{"a literal doubles its quote", quoteLiteral("it's"), "'it''s'"},
		{"a literal doubles its backslash", quoteLiteral(`a\b`), `'a\\b'`},
		{"a literal of a backslash and a quote", quoteLiteral(`\'`), `'\\'''`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %s, want %s", tc.got, tc.want)
			}
		})
	}
}
