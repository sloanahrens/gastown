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

// hash returns a 32-character hex commit hash that differs per n, so a value
// that is not a hash cannot pass for one by accident.
func hash(n int) string {
	return fmt.Sprintf("%032x", 0xc000+n)
}

// single returns the one-value answer a scalar or count read expects.
func single(column, value string) Rows {
	return Rows{Columns: []string{column}, Data: [][]string{{value}}}
}

// values returns the first value of every record, the shape a test needs to
// read rows it fetched itself.
func values(rows Rows) []string {
	out := make([]string, 0, len(rows.Data))
	for _, record := range rows.Data {
		out = append(out, record[0])
	}
	return out
}

// cutOf returns a cut seven days before the package's fixed epoch, the
// keep window of docs/plans/2026-11-15-dolt-squash-design.md.
func cutOf() time.Time {
	return testEpoch().Add(-7 * 24 * time.Hour)
}

// The statements the BEFORE record's happy script answers, keyed so a test can
// replace one answer and keep the rest.
const (
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

// recordFake answers the BEFORE record's reads for a three-table database
// whose events table is dolt_ignored, replacing the answers changes names, so
// the failure a test expects is the one it injected.
func recordFake(changes map[string]Rows) *FakeExec {
	f := NewFakeExec()
	answer := func(statement string, rows Rows) {
		if replaced, ok := changes[statement]; ok {
			rows = replaced
		}
		f.Respond(statement, rows)
	}
	answer(sqlMainTip, single("dolt_hashof('main')", hash(1)))
	answer(sqlMainCommits, single("n", "12002"))
	answer(sqlShowTables, Rows{
		Columns: []string{"Tables_in_gt"},
		Data:    [][]string{{"comments"}, {"events"}, {"issues"}},
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

// The statements design step 4's happy script answers.
const (
	sqlInitialCommit = "SELECT commit_hash FROM dolt_log('main') ORDER BY date ASC LIMIT 1"
	sqlBranchBase    = "CALL DOLT_BRANCH('compact-base', "
	sqlBranchWork    = "CALL DOLT_BRANCH('compact-work', "
	sqlCheckoutWork  = "CALL DOLT_CHECKOUT('compact-work')"
	sqlRebaseStart   = "CALL DOLT_REBASE('--interactive', 'compact-base')"
	sqlPlan          = "SELECT rebase_order, commit_hash FROM dolt_rebase"
	sqlMainLogOld    = "SELECT commit_hash FROM dolt_log('main') WHERE date <= "
	sqlMarkSquash    = "UPDATE dolt_rebase SET action = 'squash'"
	sqlRebaseResume  = "CALL DOLT_REBASE('--continue')"
)

// rebaseFixture is the database design step 4's tests script: main's initial
// commit, the tip the BEFORE record holds, four plan rows of which the first
// three main held before the cut, and one newer than it.
type rebaseFixture struct {
	initial, tip string
	plan         []planRow
	old          []string
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
		old: []string{hash(0x10), hash(0x11), hash(0x12), hash(0x13)},
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
	old := Rows{Columns: []string{"commit_hash"}}
	for _, commit := range fx.old {
		old.Data = append(old.Data, []string{commit})
	}
	answer(sqlInitialCommit, single("commit_hash", fx.initial))
	answer(sqlBranchBase, Rows{})
	answer(sqlBranchWork, Rows{})
	answer(sqlCheckoutWork, Rows{})
	answer(sqlRebaseStart, Rows{})
	answer(sqlPlan, plan)
	answer(sqlMainLogOld, old)
	answer(sqlMarkSquash, Rows{})
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
	for _, statement := range f.Statements() {
		if strings.Contains(statement, "FROM `events`") || strings.Contains(statement, "dolt_hashof_table('events')") {
			t.Errorf("a dolt_ignored table was read: %q", statement)
		}
	}
}

func TestRecordBeforeQuotesCatalogNames(t *testing.T) {
	t.Parallel()
	const name = "we`ird'name\\x"
	f := NewFakeExec()
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

func TestRebaseHistoryMarksEveryOldRowAfterTheFirst(t *testing.T) {
	t.Parallel()
	fx := newRebaseFixture()
	f := fx.fake(nil)

	got, err := RebaseHistory(context.Background(), f, Before{Tip: fx.tip}, cutOf())
	if err != nil {
		t.Fatalf("RebaseHistory: %v", err)
	}
	if got.Marked != 2 {
		t.Errorf("RebaseHistory marked %d rows, want 2: the first plan row stays a pick and the fourth is newer than the cut", got.Marked)
	}
	want := []string{
		sqlInitialCommit,
		"CALL DOLT_BRANCH('compact-base', '" + fx.initial + "')",
		"CALL DOLT_BRANCH('compact-work', '" + fx.tip + "')",
		sqlCheckoutWork,
		sqlRebaseStart,
		sqlPlan + " ORDER BY rebase_order",
		"SELECT commit_hash FROM dolt_log('main') WHERE date <= '2026-11-08 20:00:00'",
		"UPDATE dolt_rebase SET action = 'squash' WHERE rebase_order > 1 AND commit_hash IN ('" +
			hash(0x12) + "', '" + hash(0x13) + "')",
		sqlRebaseResume,
	}
	if sent := f.Statements(); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

// TestOldSessionLogSelectsNothingAndMainLogSelectsEveryOldCommit pins the
// defect the v2 design replaces: a log read inside a rebase session answers
// only the base, so the 11-01 filter matches no plan row; the same filter read
// from main matches every commit older than the cut.
func TestOldSessionLogSelectsNothingAndMainLogSelectsEveryOldCommit(t *testing.T) {
	t.Parallel()
	base, c1, c2, c3 := hash(0x20), hash(0x21), hash(0x22), hash(0x23)
	f := NewFakeExec()
	// A rebase session's log holds the base and nothing the plan replays.
	f.Respond("SELECT commit_hash FROM dolt_log WHERE date <= ",
		Rows{Columns: []string{"commit_hash"}, Data: [][]string{{base}}})
	// main's log holds the base and every commit on top of it.
	f.Respond(sqlMainLogOld,
		Rows{Columns: []string{"commit_hash"}, Data: [][]string{{base}, {c1}, {c2}, {c3}}})
	plan := []planRow{{1, c1}, {2, c2}, {3, c3}}

	oldStatement := fmt.Sprintf("SELECT commit_hash FROM dolt_log WHERE date <= %s", quoteLiteral(formatCut(cutOf())))
	rows, err := f.Exec(context.Background(), oldStatement)
	if err != nil {
		t.Fatalf("the 11-01 statement: %v", err)
	}
	if got := values(rows); !slices.Equal(got, []string{base}) {
		t.Fatalf("the session log answered %q, want only the base", got)
	}
	if got := marksSquash(plan, values(rows)); len(got) != 0 {
		t.Errorf("the 11-01 filter marked %d rows, want 0: the plan holds no base commit", len(got))
	}

	old, err := oldCommits(context.Background(), &designStep{ex: f}, cutOf())
	if err != nil {
		t.Fatalf("oldCommits: %v", err)
	}
	if want := []string{base, c1, c2, c3}; !slices.Equal(old, want) {
		t.Errorf("the filter read from main selected %q, want every old commit %q", old, want)
	}
	// Every plan row is old, so every row after the first is marked and the
	// first stays a pick.
	if got := marksSquash(plan, old); len(got) != 2 {
		t.Errorf("the v2 filter marked %d rows, want 2: every old plan row after the first", len(got))
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
			name:    "the tip read answers no record",
			changes: map[string]Rows{sqlMainTip: {}},
			want:    "returned 0 records, want one",
		},
		{
			name:    "the tip is not a commit hash",
			changes: map[string]Rows{sqlMainTip: single("h", "HEAD-of-main")},
			want:    "want a 32-character hex commit hash",
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
			want:   "want a 32-character hex commit hash",
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
			want:    "want a 32-character hex commit hash",
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
			name:   "a plan commit is not a hash",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlPlan: {
				Columns: []string{"rebase_order", "commit_hash"},
				Data:    [][]string{{"1", "c1"}},
			}},
			want: "want a 32-character hex commit hash",
		},
		{
			name:   "a main log record is not a commit hash",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{sqlMainLogOld: {
				Columns: []string{"commit_hash"},
				Data:    [][]string{{"the-root"}},
			}},
			want: "want a 32-character hex commit hash",
		},
		{
			name:   "the cut reaches no plan row",
			before: Before{Tip: fx.tip},
			changes: map[string]Rows{
				sqlPlan: {
					Columns: []string{"rebase_order", "commit_hash"},
					Data:    [][]string{{"1", hash(0x11)}, {"2", hash(0x14)}},
				},
				sqlMainLogOld: {
					Columns: []string{"commit_hash"},
					Data:    [][]string{{hash(0x10)}},
				},
			},
			want: "the cut squashes nothing",
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
