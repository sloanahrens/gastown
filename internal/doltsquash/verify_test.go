package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// verifyTail is the fixed tail the fixtures' commit hashes share: hash(n)
// builds the same string, so the statement constants and the answers name the
// same commits.
const verifyTail = "jlup0pifvs02kgdldm3n3s548m9qtum"

const (
	verifyTip  = "1" + verifyTail // hash(1), the recorded and unmoved main tip
	verifyBase = "9" + verifyTail // hash(9), compact-base at main's initial commit
)

// The statements design step 6's happy script answers.
const (
	sqlVerifyCheckout      = "CALL DOLT_CHECKOUT('compact-work')"
	sqlVerifyBranch        = "SELECT active_branch()"
	sqlVerifyMainTip       = "SELECT dolt_hashof('main')"
	sqlVerifyBaseTip       = "SELECT dolt_hashof('compact-base')"
	sqlVerifyCatalog       = "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'"
	sqlVerifyCountComments = "SELECT COUNT(*) FROM `comments`"
	sqlVerifyHashComments  = "SELECT dolt_hashof_table('comments')"
	sqlVerifyCountIssues   = "SELECT COUNT(*) FROM `issues`"
	sqlVerifyHashIssues    = "SELECT dolt_hashof_table('issues')"
	sqlVerifyOldCommits    = "SELECT COUNT(*) FROM dolt_log('compact-work') WHERE author_date <= '2026-11-08 20:00:00' AND commit_hash <> '" + verifyBase + "'"
	sqlVerifyWorkNewer     = "SELECT author_date, message FROM dolt_log('compact-work') WHERE author_date > '2026-11-08 20:00:00' ORDER BY author_date"
	sqlVerifyMainNewer     = "SELECT author_date, message FROM dolt_log AS OF '" + verifyTip + "' WHERE author_date > '2026-11-08 20:00:00' ORDER BY author_date"
)

// verifyFixture is the database design step 6's tests script: a BEFORE record
// of two tables, a compact-work catalog that matches it, one rebased commit at
// or before the cut, and two commits newer than the cut on both branches.
type verifyFixture struct {
	before  Before
	base    string
	mainTip string
	catalog []string
	old     int64
	newer   []commitIdentity
}

func newVerifyFixture() verifyFixture {
	return verifyFixture{
		before: Before{
			Tip:     verifyTip,
			Commits: 12002,
			Tables: []TableState{
				{Table: "comments", Rows: 3, Hash: hash(2)},
				{Table: "issues", Rows: 9, Hash: hash(3)},
			},
		},
		base:    verifyBase,
		mainTip: verifyTip,
		catalog: []string{"comments", "dolt_ignore", "issues"},
		old:     1,
		newer: []commitIdentity{
			{AuthorDate: "2026-11-10 10:00:00.000", Message: "feat: a"},
			{AuthorDate: "2026-11-12 09:30:00.000", Message: "fix: b"},
		},
	}
}

// fake answers the whole of design step 6, replacing the answers changes name.
func (fx verifyFixture) fake(changes map[string]Rows) *FakeExec {
	f := NewFakeExec()
	answer := func(statement string, rows Rows) {
		if replaced, ok := changes[statement]; ok {
			rows = replaced
		}
		f.Respond(statement, rows)
	}
	answer(sqlVerifyCheckout, Rows{})
	answer(sqlVerifyBranch, single("branch", "compact-work"))
	answer(sqlVerifyMainTip, single("dolt_hashof('main')", fx.mainTip))
	answer(sqlVerifyBaseTip, single("dolt_hashof('compact-base')", fx.base))
	catalog := Rows{Columns: []string{"Tables_in_gt"}}
	for _, name := range fx.catalog {
		catalog.Data = append(catalog.Data, []string{name})
	}
	answer(sqlVerifyCatalog, catalog)
	answer(sqlVerifyCountComments, single("n", "3"))
	answer(sqlVerifyHashComments, single("h", hash(2)))
	answer(sqlVerifyCountIssues, single("n", "9"))
	answer(sqlVerifyHashIssues, single("h", hash(3)))
	answer(sqlVerifyOldCommits, single("n", strconv.FormatInt(fx.old, 10)))
	answer(sqlVerifyWorkNewer, identitiesRows(fx.newer))
	answer(sqlVerifyMainNewer, identitiesRows(fx.newer))
	return f
}

// identitiesRows renders identities as the two-column record the newer-commit
// reads answer.
func identitiesRows(identities []commitIdentity) Rows {
	rows := Rows{Columns: []string{"author_date", "message"}}
	for _, identity := range identities {
		rows.Data = append(rows.Data, []string{identity.AuthorDate, identity.Message})
	}
	return rows
}

func TestVerifyPassesEqualRecords(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(nil)

	got, err := Verify(context.Background(), f, fx.before, cutOf())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !got.OK() {
		t.Errorf("an equal record did not verify: %v", got.Differences)
	}
	if got.Moved {
		t.Errorf("a tip equal to the record is reported moved (MainTip %q, record %q)", got.MainTip, fx.before.Tip)
	}
	if got.MainTip != fx.before.Tip {
		t.Errorf("MainTip = %q, want the record's %q", got.MainTip, fx.before.Tip)
	}
	want := []string{
		sqlVerifyCheckout,
		sqlVerifyBranch,
		sqlVerifyMainTip,
		sqlVerifyBaseTip,
		sqlVerifyCatalog,
		sqlVerifyCountComments,
		sqlVerifyHashComments,
		sqlVerifyCountIssues,
		sqlVerifyHashIssues,
		sqlVerifyOldCommits,
		sqlVerifyWorkNewer,
		sqlVerifyMainNewer,
	}
	if sent := f.Statements(); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

// TestVerifyNamesTheTableOfEachTableDifference drives the two table rules and
// asserts that every difference they make names its table, which is what an
// item raised from the run must say.
func TestVerifyNamesTheTableOfEachTableDifference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes map[string]Rows
		want    []Difference
	}{
		{
			name:    "a row count differing from the record",
			changes: map[string]Rows{sqlVerifyCountIssues: single("n", "10")},
			want: []Difference{{
				Table: "issues", Kind: KindRowCount, Want: "9", Got: "10",
			}},
		},
		{
			name:    "a table hash differing from the record",
			changes: map[string]Rows{sqlVerifyHashIssues: single("h", hash(4))},
			want: []Difference{{
				Table: "issues", Kind: KindTableHash, Want: hash(3), Got: hash(4),
			}},
		},
		{
			name: "a row count and a hash differing in the same table",
			changes: map[string]Rows{
				sqlVerifyCountIssues: single("n", "10"),
				sqlVerifyHashIssues:  single("h", hash(4)),
			},
			want: []Difference{
				{Table: "issues", Kind: KindRowCount, Want: "9", Got: "10"},
				{Table: "issues", Kind: KindTableHash, Want: hash(3), Got: hash(4)},
			},
		},
		{
			name: "differences in two tables, reported in table-name order",
			changes: map[string]Rows{
				sqlVerifyCountIssues:   single("n", "10"),
				sqlVerifyCountComments: single("n", "4"),
			},
			want: []Difference{
				{Table: "comments", Kind: KindRowCount, Want: "3", Got: "4"},
				{Table: "issues", Kind: KindRowCount, Want: "9", Got: "10"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newVerifyFixture()
			got, err := Verify(context.Background(), fx.fake(tc.changes), fx.before, cutOf())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.OK() {
				t.Fatal("a record that differs verified")
			}
			if !slices.Equal(got.Differences, tc.want) {
				t.Errorf("differences = %+v, want %+v", got.Differences, tc.want)
			}
			for _, difference := range got.Differences {
				if !strings.Contains(difference.String(), difference.Table) {
					t.Errorf("the line %q does not name the table %q", difference, difference.Table)
				}
			}
		})
	}
}

func TestVerifyFailsAMissingTable(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(map[string]Rows{sqlVerifyCatalog: {
		Columns: []string{"Tables_in_gt"},
		Data:    [][]string{{"comments"}, {"dolt_ignore"}},
	}})

	got, err := Verify(context.Background(), f, fx.before, cutOf())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := []Difference{{Table: "issues", Kind: KindMissingTable}}
	if !slices.Equal(got.Differences, want) {
		t.Errorf("differences = %+v, want %+v", got.Differences, want)
	}
	// A table that is not there has no count or hash to read, so no statement
	// names it as one.
	for _, statement := range f.Statements() {
		if strings.Contains(statement, "`issues`") || strings.Contains(statement, "'issues'") {
			t.Errorf("the run read the missing table: %q", statement)
		}
	}
}

func TestVerifyOneCommitAtOrBeforeTheCut(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		old  int64
		want []Difference
	}{
		{name: "the squashed range leaves one commit", old: 1},
		{name: "no commit is left", old: 0, want: []Difference{
			{Kind: KindOldCommits, Want: "1", Got: "0"},
		}},
		{name: "two commits are left", old: 2, want: []Difference{
			{Kind: KindOldCommits, Want: "1", Got: "2"},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newVerifyFixture()
			f := fx.fake(map[string]Rows{sqlVerifyOldCommits: single("n", strconv.FormatInt(tc.old, 10))})
			got, err := Verify(context.Background(), f, fx.before, cutOf())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !slices.Equal(got.Differences, tc.want) {
				t.Errorf("differences = %+v, want %+v", got.Differences, tc.want)
			}
		})
	}
}

func TestVerifyNewerCommitsOneForOne(t *testing.T) {
	t.Parallel()
	first := commitIdentity{AuthorDate: "2026-11-10 10:00:00.000", Message: "feat: a"}
	second := commitIdentity{AuthorDate: "2026-11-12 09:30:00.000", Message: "fix: b"}
	third := commitIdentity{AuthorDate: "2026-11-13 08:00:00.000", Message: "chore: c"}
	tests := []struct {
		name string
		work []commitIdentity
		main []commitIdentity
		want []Difference
	}{
		{
			name: "the same commits in the same order",
			work: []commitIdentity{first, second},
			main: []commitIdentity{first, second},
		},
		{
			name: "the same commits in a different order",
			work: []commitIdentity{second, first},
			main: []commitIdentity{first, second},
		},
		{
			name: "a commit main held that compact-work lost",
			work: []commitIdentity{first},
			main: []commitIdentity{first, second},
			want: []Difference{{Commit: second.String(), Kind: KindMissingCommit}},
		},
		{
			name: "a commit compact-work holds that main did not",
			work: []commitIdentity{first, second, third},
			main: []commitIdentity{first, second},
			want: []Difference{{Commit: third.String(), Kind: KindExtraCommit}},
		},
		{
			name: "a lost commit beside an extra one",
			work: []commitIdentity{first, third},
			main: []commitIdentity{first, second},
			want: []Difference{
				{Commit: second.String(), Kind: KindMissingCommit},
				{Commit: third.String(), Kind: KindExtraCommit},
			},
		},
		{
			name: "two commits that share an author date and a message",
			work: []commitIdentity{first},
			main: []commitIdentity{first, first},
			want: []Difference{{Commit: first.String(), Kind: KindMissingCommit}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newVerifyFixture()
			f := fx.fake(map[string]Rows{
				sqlVerifyWorkNewer: identitiesRows(tc.work),
				sqlVerifyMainNewer: identitiesRows(tc.main),
			})
			got, err := Verify(context.Background(), f, fx.before, cutOf())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !slices.Equal(got.Differences, tc.want) {
				t.Errorf("differences = %+v, want %+v", got.Differences, tc.want)
			}
		})
	}
}

func TestVerifyReportsAMovedTipAsMoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes map[string]Rows
		want    []Difference
	}{
		{name: "a moved tip with equal tables"},
		{
			name:    "a moved tip beside a row count that differs",
			changes: map[string]Rows{sqlVerifyCountIssues: single("n", "10")},
			want:    []Difference{{Table: "issues", Kind: KindRowCount, Want: "9", Got: "10"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newVerifyFixture()
			moved := hash(5)
			changes := map[string]Rows{sqlVerifyMainTip: single("dolt_hashof('main')", moved)}
			for statement, rows := range tc.changes {
				changes[statement] = rows
			}
			f := fx.fake(changes)

			got, err := Verify(context.Background(), f, fx.before, cutOf())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if !got.Moved {
				t.Errorf("a tip %q against the record's %q is not reported moved", got.MainTip, fx.before.Tip)
			}
			if got.MainTip != moved {
				t.Errorf("MainTip = %q, want the moved tip %q", got.MainTip, moved)
			}
			if !slices.Equal(got.Differences, tc.want) {
				t.Errorf("differences = %+v, want %+v (a moved tip is not a difference)", got.Differences, tc.want)
			}
			// The newer commits are main's at the last picked tip, not at the
			// moved tip: the tail after the pick is the swap's to replay.
			for _, statement := range f.Statements() {
				if strings.HasPrefix(statement, "SELECT author_date, message FROM dolt_log AS OF") {
					if !strings.Contains(statement, "AS OF '"+fx.before.Tip+"'") {
						t.Errorf("the main newer reads are not bounded at the recorded tip: %q", statement)
					}
				}
			}
		})
	}
}

// TestVerifyCutRulesReadAuthorDate pins the columns the run filters on. A
// rebase rewrites every replayed commit's commit date to the moment it runs and
// keeps its author date, so a filter on date would count the squashed commit as
// newer than the cut and fail every correct run.
func TestVerifyCutRulesReadAuthorDate(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(nil)
	if _, err := Verify(context.Background(), f, fx.before, cutOf()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	cutRules := 0
	for _, statement := range f.Statements() {
		if !strings.Contains(statement, "author_date >") && !strings.Contains(statement, "author_date <=") {
			continue
		}
		cutRules++
		if strings.Contains(statement, "WHERE date ") || strings.Contains(statement, "AND date ") {
			t.Errorf("a cut rule filters on the commit date: %q", statement)
		}
	}
	if cutRules != 3 {
		t.Errorf("%d statements filter on author_date, want 3: the old count and both newer reads", cutRules)
	}
}

func TestVerifyChecksTheSessionIsOnCompactWork(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(map[string]Rows{sqlVerifyBranch: single("branch", "main")})

	_, err := Verify(context.Background(), f, fx.before, cutOf())
	if err == nil {
		t.Fatal("Verify returned no error on a session that is not on compact-work")
	}
	for _, want := range []string{"statement 2 (SELECT active_branch())", `"main"`, "want compact-work"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not hold %q", err, want)
		}
	}
	if got := len(f.Statements()); got != 2 {
		t.Errorf("the refused run sent %d statements, want 2: nothing is read before the branch is checked", got)
	}
}

func TestVerifyRefusesAnInvalidTip(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	fx.before.Tip = "not-a-hash"
	f := fx.fake(nil)

	_, err := Verify(context.Background(), f, fx.before, cutOf())
	if err == nil {
		t.Fatal("Verify returned no error on a tip that is not a Dolt commit hash")
	}
	if !strings.Contains(err.Error(), "the recorded main tip") {
		t.Errorf("error %q does not name the recorded main tip", err)
	}
	if got := f.Statements(); len(got) != 0 {
		t.Errorf("the refused run sent %q, want nothing", got)
	}
}

// TestVerifyRefusesARefThatIsNotACommitHash drives the checks on the two refs
// the run reads after the branch check: compact-base's tip names a commit the
// one-commit rule excludes, and main's tip is compared with the record, so a
// value that is not a Dolt hash is a read the run cannot trust.
func TestVerifyRefusesARefThatIsNotACommitHash(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes map[string]Rows
		want    string
	}{
		{
			name:    "main's tip",
			changes: map[string]Rows{sqlVerifyMainTip: single("dolt_hashof('main')", "not-a-hash")},
			want:    "main's tip",
		},
		{
			name:    "compact-base's tip",
			changes: map[string]Rows{sqlVerifyBaseTip: single("dolt_hashof('compact-base')", "")},
			want:    "compact-base's tip",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newVerifyFixture()
			_, err := Verify(context.Background(), fx.fake(tc.changes), fx.before, cutOf())
			if err == nil {
				t.Fatalf("Verify accepted %s that is not a Dolt commit hash", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

func TestVerifyRefusesAnEmptyTableHash(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(map[string]Rows{sqlVerifyHashComments: single("h", "")})

	_, err := Verify(context.Background(), f, fx.before, cutOf())
	if err == nil {
		t.Fatal("Verify compared a table whose read returned no hash")
	}
	if !strings.Contains(err.Error(), "returned no hash for comments") {
		t.Errorf("error %q does not name the table whose hash was empty", err)
	}
}

func TestVerifyRefusesANewerCommitRecordThatIsNotTwoValues(t *testing.T) {
	t.Parallel()
	fx := newVerifyFixture()
	f := fx.fake(map[string]Rows{sqlVerifyWorkNewer: {
		Columns: []string{"author_date", "message", "extra"},
		Data:    [][]string{{"2026-11-10 10:00:00.000", "feat: a", "x"}},
	}})

	_, err := Verify(context.Background(), f, fx.before, cutOf())
	if err == nil {
		t.Fatal("Verify accepted a newer-commit record of three values")
	}
	if !strings.Contains(err.Error(), "want author_date and message") {
		t.Errorf("error %q does not say the shape it wanted", err)
	}
}

func TestVerifyStatementErrorsStopTheRunWithTheirPosition(t *testing.T) {
	t.Parallel()
	refused := errors.New("server closed the connection")
	fx := newVerifyFixture()
	clean := fx.fake(nil)
	if _, err := Verify(context.Background(), clean, fx.before, cutOf()); err != nil {
		t.Fatalf("the scripted run failed before any injection: %v", err)
	}
	sent := clean.Statements()
	if len(sent) < 5 {
		t.Fatalf("the scripted run sent %d statements, too few to test every position", len(sent))
	}
	for n := 1; n <= len(sent); n++ {
		f := fx.fake(nil)
		f.FailAt(n, refused)
		_, err := Verify(context.Background(), f, fx.before, cutOf())
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
			t.Errorf("statement %d: the fake recorded %d statements, want %d", n, got, n)
		}
	}
}
