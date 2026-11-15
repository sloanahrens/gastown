package doltsquash

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// The statements design step 9's happy script answers.
const (
	sqlRetainDatabase = "SELECT DATABASE()"
	sqlRetainMainLog  = "SELECT commit_hash, author_date, message FROM dolt_log('main')"
	sqlRetainPreLog   = "SELECT commit_hash, author_date, message FROM dolt_log('main-pre-squash')"
	sqlRetainCatalog  = "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'"
	sqlRetainDropPre  = "CALL DOLT_BRANCH('-D', 'main-pre-squash')"
	sqlRetainDropBase = "CALL DOLT_BRANCH('-D', 'compact-base')"
)

// The identities both branches hold in the fixture. The swap replays commits,
// so each identity sits under a different hash on each branch.
var (
	retainNewest = commitIdentity{AuthorDate: "2026-11-15 21:00:00.000", Message: "fix: b"}
	retainOlder  = commitIdentity{AuthorDate: "2026-11-15 20:00:00.000", Message: "feat: a"}
	retainGone   = commitIdentity{AuthorDate: "2026-11-01 09:00:00.000", Message: "chore: squashed away"}
)

// retainCase is one commit pair the second verification reads, with the tables
// each side holds at it.
type retainCase struct {
	main       historyCommit
	retained   historyCommit
	mainAt     revisionTables
	retainedAt revisionTables
}

// retainFixture is the database design step 9's tests script: the two commits
// present on both branches, an older commit only main-pre-squash holds, and
// per pair the tables each side's commit holds.
type retainFixture struct {
	// base is the database name a revision database is built on; database is
	// what SELECT DATABASE() answers, which is the same name unless the
	// session is on a revision database.
	base     string
	database string
	mainLog  []historyCommit
	preLog   []historyCommit
	compare  []retainCase
	// drop scripts the two drops; a test that expects a kept branch leaves it
	// false, so a drop the run should not send fails instead of answering.
	drop bool
}

// tables returns the revision tables of the given name-to-hash map, in
// table-name order.
func tables(hashes map[string]string) revisionTables {
	names := make([]string, 0, len(hashes))
	for name := range hashes {
		names = append(names, name)
	}
	slices.Sort(names)
	return revisionTables{names: names, hashes: hashes}
}

// newRetainFixture returns the fixture every test starts from: two commits on
// both branches with equal tables, and the drop scripted.
func newRetainFixture() retainFixture {
	newestMain := historyCommit{hash: hash(1), identity: retainNewest}
	olderMain := historyCommit{hash: hash(2), identity: retainOlder}
	newestRetained := historyCommit{hash: hash(3), identity: retainNewest}
	olderRetained := historyCommit{hash: hash(4), identity: retainOlder}
	goneRetained := historyCommit{hash: hash(5), identity: retainGone}
	return retainFixture{
		base:     "gt",
		database: "gt",
		mainLog:  []historyCommit{newestMain, olderMain},
		preLog:   []historyCommit{newestRetained, olderRetained, goneRetained},
		compare: []retainCase{
			{
				main:       newestMain,
				retained:   newestRetained,
				mainAt:     tables(map[string]string{"comments": hash(10), "issues": hash(11)}),
				retainedAt: tables(map[string]string{"comments": hash(10), "issues": hash(11)}),
			},
			{
				main:       olderMain,
				retained:   olderRetained,
				mainAt:     tables(map[string]string{"comments": hash(12), "issues": hash(13)}),
				retainedAt: tables(map[string]string{"comments": hash(12), "issues": hash(13)}),
			},
		},
		drop: true,
	}
}

// fake answers the whole of design step 9, in the order the verification sends
// its statements.
func (fx retainFixture) fake() *FakeExec {
	f := NewFakeExec()
	f.Respond(sqlRetainDatabase, single("DATABASE()", fx.database))
	f.Respond(sqlRetainMainLog, historyRows(fx.mainLog))
	f.Respond(sqlRetainPreLog, historyRows(fx.preLog))
	for _, c := range fx.compare {
		fx.queueRevision(f, c.main.hash, c.mainAt)
		fx.queueRevision(f, c.retained.hash, c.retainedAt)
	}
	if fx.drop {
		f.Respond(sqlRetainDropPre, Rows{})
		f.Respond(sqlRetainDropBase, Rows{})
	}
	return f
}

// queueRevision queues the reads of one commit: the revision database, its
// catalog, one hash per table, and the database again.
func (fx retainFixture) queueRevision(f *FakeExec, commit string, at revisionTables) {
	f.Respond("USE "+quoteIdent(fx.base+"/"+commit), Rows{})
	catalog := Rows{Columns: []string{"Tables_in_" + fx.base + "/" + commit}}
	for _, name := range at.names {
		catalog.Data = append(catalog.Data, []string{name})
	}
	f.Respond(sqlRetainCatalog, catalog)
	for _, name := range at.names {
		f.Respond("SELECT dolt_hashof_table("+quoteLiteral(name)+")", single("hash", at.hashes[name]))
	}
	f.Respond("USE "+quoteIdent(fx.base), Rows{})
}

// expected returns the statements a run that reads compared, in order, and
// then drops the branches when the fixture scripts the drops.
func (fx retainFixture) expected(compared []retainCase) []string {
	sent := []string{sqlRetainDatabase, sqlRetainMainLog, sqlRetainPreLog}
	for _, c := range compared {
		sent = append(sent, fx.revision(c.main.hash, c.mainAt)...)
		sent = append(sent, fx.revision(c.retained.hash, c.retainedAt)...)
	}
	if fx.drop {
		sent = append(sent, sqlRetainDropPre, sqlRetainDropBase)
	}
	return sent
}

// revision returns the statements that read one commit's tables.
func (fx retainFixture) revision(commit string, at revisionTables) []string {
	sent := []string{"USE " + quoteIdent(fx.base+"/"+commit), sqlRetainCatalog}
	for _, name := range at.names {
		sent = append(sent, "SELECT dolt_hashof_table("+quoteLiteral(name)+")")
	}
	return append(sent, "USE "+quoteIdent(fx.base))
}

// historyRows renders commits as the three-column record a branch's log
// answers.
func historyRows(commits []historyCommit) Rows {
	rows := Rows{Columns: []string{"commit_hash", "author_date", "message"}}
	for _, c := range commits {
		rows.Data = append(rows.Data, []string{c.hash, c.identity.AuthorDate, c.identity.Message})
	}
	return rows
}

// afterSwap returns the clock a run reads when it starts d after the swap the
// package's fixed epoch records.
func afterSwap(d time.Duration) time.Time { return testEpoch().Add(d) }

func TestRetainDropsBothBranchesOnAnExactMatch(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	f := fx.fake()

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !got.Due {
		t.Errorf("a run a whole period after the swap is not due: %q", got.Reason)
	}
	if !got.Dropped {
		t.Errorf("an exact match did not drop the branches: %v", got.Mismatch)
	}
	if got.Mismatch != nil {
		t.Errorf("an exact match reports the mismatch %v", got.Mismatch)
	}
	if got.Compared != len(fx.compare) {
		t.Errorf("Compared = %d, want %d, one per commit present on both", got.Compared, len(fx.compare))
	}
	if sent, want := f.Statements(), fx.expected(fx.compare); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
	// The commit only main-pre-squash holds is neither paired nor read.
	for _, statement := range f.Statements() {
		if strings.Contains(statement, hash(5)) {
			t.Errorf("the run read a commit the branches do not share: %q", statement)
		}
	}
}

func TestRetainKeepsBothBranchesOnAMismatch(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	fx.drop = false
	fx.compare[0].retainedAt = tables(map[string]string{"comments": hash(10), "issues": hash(14)})
	f := fx.fake()

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !got.Due {
		t.Errorf("a run a whole period after the swap is not due: %q", got.Reason)
	}
	if got.Dropped {
		t.Error("a mismatch dropped the branches")
	}
	want := RetentionMismatch{
		Commit:   retainNewest.String(),
		Table:    "issues",
		Main:     hash(11),
		Retained: hash(14),
	}
	if got.Mismatch == nil || *got.Mismatch != want {
		t.Fatalf("Mismatch = %v, want %+v", got.Mismatch, want)
	}
	if got.Compared != 1 {
		t.Errorf("Compared = %d, want 1: the comparison stops at the first differing pair", got.Compared)
	}
	if !strings.Contains(got.Reason, want.String()) {
		t.Errorf("Reason %q does not carry the mismatch %q", got.Reason, want.String())
	}
	if sent, want := f.Statements(), fx.expected(fx.compare[:1]); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "CALL DOLT_BRANCH") {
			t.Errorf("a mismatch sent %q: a differing pair keeps both branches", statement)
		}
	}
}

func TestRetainKeepsBothBranchesOnATableOnlyOneSideHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mainAt     map[string]string
		retainedAt map[string]string
		want       RetentionMismatch
	}{
		{
			name:       "a table only main holds",
			mainAt:     map[string]string{"comments": hash(10), "issues": hash(11), "labels": hash(15)},
			retainedAt: map[string]string{"comments": hash(10), "issues": hash(11)},
			want:       RetentionMismatch{Commit: retainNewest.String(), Table: "labels", Main: hash(15)},
		},
		{
			name:       "a table only main-pre-squash holds",
			mainAt:     map[string]string{"comments": hash(10), "issues": hash(11)},
			retainedAt: map[string]string{"comments": hash(10), "issues": hash(11), "labels": hash(16)},
			want:       RetentionMismatch{Commit: retainNewest.String(), Table: "labels", Retained: hash(16)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRetainFixture()
			fx.drop = false
			fx.compare[0].mainAt = tables(tc.mainAt)
			fx.compare[0].retainedAt = tables(tc.retainedAt)

			got, err := Retain(context.Background(), fx.fake(), afterSwap(RetainPeriod), testEpoch())
			if err != nil {
				t.Fatalf("Retain: %v", err)
			}
			if got.Dropped {
				t.Error("a table one side lacks dropped the branches")
			}
			if got.Mismatch == nil || *got.Mismatch != tc.want {
				t.Errorf("Mismatch = %v, want %+v", got.Mismatch, tc.want)
			}
		})
	}
}

func TestRetainRefusesToPairAnIdentityHeldByMoreThanOneCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		onMain int
		onPre  int
	}{
		{name: "a second commit on main holds the identity", onMain: 2, onPre: 1},
		{name: "a second commit on main-pre-squash holds the identity", onMain: 1, onPre: 2},
		{name: "a second commit on each branch holds the identity", onMain: 2, onPre: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRetainFixture()
			fx.drop = false
			for i := 1; i < tc.onMain; i++ {
				fx.mainLog = append(fx.mainLog, historyCommit{hash: hash(20 + i), identity: retainNewest})
			}
			for i := 1; i < tc.onPre; i++ {
				fx.preLog = append(fx.preLog, historyCommit{hash: hash(24 + i), identity: retainNewest})
			}
			f := fx.fake()

			_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
			if err == nil {
				t.Fatal("Retain paired an identity more than one commit holds")
			}
			want := fmt.Sprintf("the identity %s is held by %d commits on main and %d on main-pre-squash",
				retainNewest, tc.onMain, tc.onPre)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not hold %q", err, want)
			}
			if !strings.Contains(err.Error(), "keeps both branches") {
				t.Errorf("error %q does not say the branches are kept", err)
			}
			wantSent := []string{sqlRetainDatabase, sqlRetainMainLog, sqlRetainPreLog}
			if sent := f.Statements(); !slices.Equal(sent, wantSent) {
				t.Errorf("statements sent:\n%s\nwant only the three reads:\n%s",
					strings.Join(sent, "\n"), strings.Join(wantSent, "\n"))
			}
		})
	}
}

func TestRetainPairsOnlyTheCommitsBothBranchesHold(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	// Two commits on main share an identity that main-pre-squash does not
	// hold: neither is a pair, so they neither compare nor refuse the drop.
	onlyMain := commitIdentity{AuthorDate: "2026-11-15 21:30:00.000", Message: "fix: only main"}
	fx.mainLog = append(fx.mainLog,
		historyCommit{hash: hash(20), identity: onlyMain},
		historyCommit{hash: hash(21), identity: onlyMain},
	)
	f := fx.fake()

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !got.Dropped || got.Compared != len(fx.compare) {
		t.Errorf("Dropped = %v, Compared = %d, want a drop after the %d pairs both branches hold",
			got.Dropped, got.Compared, len(fx.compare))
	}
}

func TestRetainHoldsUntilThePeriodHasElapsed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		now   time.Time
		due   bool
		until string
	}{
		{
			name:  "one second before the period ends",
			now:   afterSwap(RetainPeriod - time.Second),
			until: "held: main-pre-squash is retained until 2026-11-17T20:00:00Z",
		},
		{
			name: "exactly at the period",
			now:  afterSwap(RetainPeriod),
			due:  true,
		},
		{
			name: "a day after the period",
			now:  afterSwap(RetainPeriod + 24*time.Hour),
			due:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRetainFixture()
			f := fx.fake()

			got, err := Retain(context.Background(), f, tc.now, testEpoch())
			if err != nil {
				t.Fatalf("Retain: %v", err)
			}
			if got.Due != tc.due {
				t.Fatalf("Due = %v at %v, want %v", got.Due, tc.now, tc.due)
			}
			if got.Due {
				if !got.Dropped {
					t.Errorf("a due run that matched did not drop the branches: %v", got.Mismatch)
				}
				return
			}
			if got.Dropped {
				t.Error("a run inside the period dropped the branches")
			}
			if got.Reason != tc.until {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.until)
			}
			if sent := f.Statements(); len(sent) != 0 {
				t.Errorf("a run inside the period sent %q, want nothing", sent)
			}
		})
	}
}

func TestRetainRefusesAnUnrecordedOrFutureSwapTime(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		swappedAt time.Time
		want      string
	}{
		{
			name: "no swap time recorded",
			want: "the swap time is not recorded",
		},
		{
			name:      "a swap time after the clock",
			swappedAt: testEpoch().Add(time.Second),
			want:      "is after the clock",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := NewFakeExec()
			_, err := Retain(context.Background(), f, testEpoch(), tc.swappedAt)
			if err == nil {
				t.Fatal("Retain accepted a swap time it cannot start a period from")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not hold %q", err, tc.want)
			}
			if sent := f.Statements(); len(sent) != 0 {
				t.Errorf("the refused run sent %q, want nothing", sent)
			}
		})
	}
}

func TestRetainRefusesTheDropWithNoCommonCommit(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	fx.mainLog = []historyCommit{{hash: hash(6), identity: commitIdentity{
		AuthorDate: "2026-11-15 22:00:00.000",
		Message:    "docs: only main holds this",
	}}}
	f := fx.fake()

	_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err == nil {
		t.Fatal("Retain dropped the branches with no commit to verify")
	}
	if !strings.Contains(err.Error(), "no commit is present on both") {
		t.Errorf("error %q does not say nothing is common", err)
	}
	want := []string{sqlRetainDatabase, sqlRetainMainLog, sqlRetainPreLog}
	if sent := f.Statements(); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant only the three reads:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

// wideRetainHash returns a Dolt hash distinct per side and n, past the 32
// values hash(n) reaches.
func wideRetainHash(side byte, n int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuv"
	const tail = "jlup0pifvs02kgdldm3n3s548m9qtum"
	return (string([]byte{side, alphabet[n%len(alphabet)]}) + tail)[:32]
}

// wideRetainFixture returns a fixture of n commits present on both branches,
// each holding one table that matches across the branches, newest first.
func wideRetainFixture(n int) retainFixture {
	fx := retainFixture{base: "gt", database: "gt", drop: true}
	newest := time.Date(2026, 11, 15, 21, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		identity := commitIdentity{
			AuthorDate: newest.Add(-time.Duration(i) * time.Minute).Format("2006-01-02 15:04:05.000"),
			Message:    fmt.Sprintf("commit %d", i),
		}
		main := historyCommit{hash: wideRetainHash('m', i), identity: identity}
		retained := historyCommit{hash: wideRetainHash('p', i), identity: identity}
		fx.mainLog = append(fx.mainLog, main)
		fx.preLog = append(fx.preLog, retained)
		at := tables(map[string]string{"issues": wideRetainHash('t', i)})
		fx.compare = append(fx.compare, retainCase{main: main, retained: retained, mainAt: at, retainedAt: at})
	}
	return fx
}

func TestRetainComparesTheTwentyNewestCommonCommits(t *testing.T) {
	t.Parallel()
	fx := wideRetainFixture(RetainCommits + 1)
	f := fx.fake()

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !got.Dropped {
		t.Errorf("an exact match over %d commits did not drop the branches: %v", RetainCommits, got.Mismatch)
	}
	if got.Compared != RetainCommits {
		t.Errorf("Compared = %d, want %d", got.Compared, RetainCommits)
	}
	oldest := fx.compare[RetainCommits]
	sent := f.Statements()
	for _, commit := range []string{oldest.main.hash, oldest.retained.hash} {
		statement := "USE " + quoteIdent(fx.base+"/"+commit)
		if slices.Contains(sent, statement) {
			t.Errorf("the run read commit %s, older than the %d newest present on both", commit, RetainCommits)
		}
	}
	if sent, want := sent, fx.expected(fx.compare[:RetainCommits]); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestRetainReadsADatabaseFromARevisionSession(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	fx.database = "gt/main-pre-squash"
	f := fx.fake()

	if _, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch()); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	// The revision database names and the restore are built on the bare
	// database name, not on the revision the session is reading.
	if sent, want := f.Statements(), fx.expected(fx.compare); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestRetainSkipsDoltSystemTables(t *testing.T) {
	t.Parallel()
	main := historyCommit{hash: hash(1), identity: retainNewest}
	retained := historyCommit{hash: hash(2), identity: retainNewest}
	f := NewFakeExec()
	f.Respond(sqlRetainDatabase, single("DATABASE()", "gt"))
	f.Respond(sqlRetainMainLog, historyRows([]historyCommit{main}))
	f.Respond(sqlRetainPreLog, historyRows([]historyCommit{retained}))
	for _, commit := range []string{main.hash, retained.hash} {
		f.Respond("USE "+quoteIdent("gt/"+commit), Rows{})
		f.Respond(sqlRetainCatalog, Rows{
			Columns: []string{"Tables_in_gt/" + commit},
			// Dolt reserves the prefix ignoring case, as CREATE TABLE does.
			Data: [][]string{{"dolt_ignore"}, {"DOLT_IGNORE"}, {"issues"}},
		})
		// No answer is scripted for either system table: a run that reads one
		// fails, where a system table is Dolt's own and not the database's data.
		f.Respond("SELECT dolt_hashof_table('issues')", single("hash", hash(9)))
		f.Respond("USE "+quoteIdent("gt"), Rows{})
	}
	f.Respond(sqlRetainDropPre, Rows{})
	f.Respond(sqlRetainDropBase, Rows{})

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if !got.Dropped || got.Compared != 1 {
		t.Errorf("Dropped = %v, Compared = %d, want a drop after one matching pair", got.Dropped, got.Compared)
	}
	for _, statement := range f.Statements() {
		if strings.Contains(strings.ToLower(statement), "dolt_ignore") {
			t.Errorf("the run read a Dolt system table: %q", statement)
		}
	}
}

func TestRetainReadsADoltPrefixedTableThatIsNotADoltSystemTable(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	fx.drop = false
	fx.compare[0].mainAt = tables(map[string]string{"dolt_probe": hash(17), "issues": hash(11)})
	fx.compare[0].retainedAt = tables(map[string]string{"dolt_probe": hash(18), "issues": hash(11)})
	f := fx.fake()

	got, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err != nil {
		t.Fatalf("Retain: %v", err)
	}
	if got.Dropped {
		t.Error("a difference at a dolt_-prefixed table the system set does not name dropped the branches")
	}
	want := RetentionMismatch{Commit: retainNewest.String(), Table: "dolt_probe", Main: hash(17), Retained: hash(18)}
	if got.Mismatch == nil || *got.Mismatch != want {
		t.Errorf("Mismatch = %v, want %+v", got.Mismatch, want)
	}
	if got.Compared != 1 {
		t.Errorf("Compared = %d, want 1: the difference at the first pair stops the comparison", got.Compared)
	}
	if sent, want := f.Statements(), fx.expected(fx.compare[:1]); !slices.Equal(sent, want) {
		t.Errorf("statements sent:\n%s\nwant:\n%s", strings.Join(sent, "\n"), strings.Join(want, "\n"))
	}
}

func TestRetainRefusesAnEmptyDatabaseName(t *testing.T) {
	t.Parallel()
	f := NewFakeExec()
	f.Respond(sqlRetainDatabase, single("DATABASE()", ""))

	_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err == nil {
		t.Fatal("Retain read commits with no database selected")
	}
	if !strings.Contains(err.Error(), "no database name") {
		t.Errorf("error %q does not say the database name is missing", err)
	}
	if sent, want := f.Statements(), []string{sqlRetainDatabase}; !slices.Equal(sent, want) {
		t.Errorf("statements sent = %q, want only the database read", sent)
	}
}

func TestRetainRefusesACommitHashThatIsNotADoltHash(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	f := NewFakeExec()
	f.Respond(sqlRetainDatabase, single("DATABASE()", fx.database))
	f.Respond(sqlRetainMainLog, Rows{
		Columns: []string{"commit_hash", "author_date", "message"},
		Data:    [][]string{{"not-a-hash", retainNewest.AuthorDate, retainNewest.Message}},
	})
	f.Respond(sqlRetainPreLog, historyRows(fx.preLog))

	_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err == nil {
		t.Fatal("Retain built a revision database name from a value that is not a commit hash")
	}
	if want := `commit not-a-hash in main's log`; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "USE ") {
			t.Errorf("the refused run sent %q, want no revision read", statement)
		}
	}
}

func TestRetainRefusesALogRecordThatIsNotThreeValues(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	f := NewFakeExec()
	f.Respond(sqlRetainDatabase, single("DATABASE()", fx.database))
	f.Respond(sqlRetainMainLog, Rows{
		Columns: []string{"commit_hash", "author_date"},
		Data:    [][]string{{hash(1), retainNewest.AuthorDate}},
	})
	f.Respond(sqlRetainPreLog, historyRows(fx.preLog))

	_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err == nil {
		t.Fatal("Retain accepted a log record that does not name a commit, its date and its message")
	}
	if want := "want commit_hash, author_date and message"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not say the shape it wanted", err)
	}
	for _, statement := range f.Statements() {
		if strings.HasPrefix(statement, "USE ") {
			t.Errorf("the refused run sent %q, want no revision read", statement)
		}
	}
}

func TestRetainRefusesAnEmptyTableHash(t *testing.T) {
	t.Parallel()
	fx := newRetainFixture()
	fx.compare[0].mainAt = tables(map[string]string{"comments": hash(10), "issues": ""})
	f := fx.fake()

	_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
	if err == nil {
		t.Fatal("Retain compared a table whose read returned no hash")
	}
	if want := "returned no hash for issues at " + fx.compare[0].main.hash; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

func TestRetainStatementErrorsStopTheRunWithTheirPosition(t *testing.T) {
	t.Parallel()
	refused := errors.New("server closed the connection")
	fx := newRetainFixture()
	clean := fx.fake()
	if _, err := Retain(context.Background(), clean, afterSwap(RetainPeriod), testEpoch()); err != nil {
		t.Fatalf("the scripted run failed before any injection: %v", err)
	}
	sent := clean.Statements()
	for n := 1; n <= len(sent); n++ {
		f := fx.fake()
		f.FailAt(n, refused)
		_, err := Retain(context.Background(), f, afterSwap(RetainPeriod), testEpoch())
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
