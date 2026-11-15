package doltsquash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// DifferenceKind names the rule design step 6 found answered differently.
type DifferenceKind string

// The rules design step 6 verifies. Each names what differs, so an item raised
// from a difference says which check failed before it says by how much.
const (
	// KindMissingTable means the BEFORE record holds a table that compact-work
	// does not have.
	KindMissingTable DifferenceKind = "missing table"
	// KindRowCount means a table's row count differs from the record's.
	KindRowCount DifferenceKind = "row count"
	// KindTableHash means a table's dolt_hashof_table differs from the record's.
	KindTableHash DifferenceKind = "table hash"
	// KindOldCommits means the squashed range did not leave exactly one commit
	// at or before the cut.
	KindOldCommits DifferenceKind = "commits at or before the cut"
	// KindMissingCommit means a commit newer than the cut that main held at the
	// last picked tip is not on compact-work.
	KindMissingCommit DifferenceKind = "newer commit missing"
	// KindExtraCommit means compact-work holds a commit newer than the cut that
	// main did not hold at the last picked tip.
	KindExtraCommit DifferenceKind = "newer commit unexpected"
)

// Difference is one way compact-work differs from main at the last picked tip.
// A table difference names its table; a commit difference names the commit by
// the two values a rebase keeps.
type Difference struct {
	// Table names the table the difference is in; empty for a commit
	// difference.
	Table string
	// Commit names a commit difference by its author date and message; empty
	// for a table difference.
	Commit string
	// Kind names the rule that answered differently.
	Kind DifferenceKind
	// Want is the value the record or the rule holds, Got the value
	// compact-work has. A missing or extra table or commit leaves one empty.
	Want string
	Got  string
}

// String renders d as the line an item raised from it carries.
func (d Difference) String() string {
	switch d.Kind {
	case KindMissingTable:
		return fmt.Sprintf("table %s is missing on compact-work", d.Table)
	case KindRowCount:
		return fmt.Sprintf("table %s: row count is %s on compact-work, %s in the record", d.Table, d.Got, d.Want)
	case KindTableHash:
		return fmt.Sprintf("table %s: hash is %s on compact-work, %s in the record", d.Table, d.Got, d.Want)
	case KindOldCommits:
		return fmt.Sprintf("commits at or before the cut: compact-work has %s, want exactly one", d.Got)
	case KindMissingCommit:
		return fmt.Sprintf("newer commit %s is missing on compact-work", d.Commit)
	case KindExtraCommit:
		return fmt.Sprintf("newer commit %s is not on main at the last picked tip", d.Commit)
	default:
		return fmt.Sprintf("%s: want %s, got %s", d.Kind, d.Want, d.Got)
	}
}

// VerifyResult is what design step 6 found on compact-work.
type VerifyResult struct {
	// MainTip is main's tip as verification read it. Moved says it is not the
	// tip the BEFORE record holds, so commits landed after the last pick; the
	// swap's reconcile replays that tail. A moved tip is reported, not failed,
	// because verification compares against the record.
	MainTip string
	Moved   bool
	// Differences lists every way compact-work differs from main at the last
	// picked tip: table differences in table-name order, then the commit rules.
	// Empty means compact-work verified.
	Differences []Difference
}

// OK reports whether compact-work verified.
func (r VerifyResult) OK() bool { return len(r.Differences) == 0 }

// Verify runs design step 6 on compact-work: every table the BEFORE record
// holds has the record's row count and dolt_hashof_table, exactly one commit of
// the squashed range is at or before cut, and the commits newer than cut are on
// compact-work one for one.
//
// The cut rules read author_date, not date: a rebase rewrites every replayed
// commit's hash and commit date to the moment it runs and keeps the author date
// and message, so a cut read from date would count the squashed commit as newer
// than cut and put it in the newer range on every run. The plan's base leaves
// the initial commit untouched and its author date is at or before cut too, so
// the one-commit rule counts the rebased range: the log without compact-base's
// tip.
func Verify(ctx context.Context, ex Exec, before Before, cut time.Time) (VerifyResult, error) {
	if err := checkCommitHash("the recorded main tip", before.Tip); err != nil {
		return VerifyResult{}, err
	}
	s := &designStep{ex: ex}
	if _, err := s.exec(ctx, "CALL DOLT_CHECKOUT('compact-work')"); err != nil {
		return VerifyResult{}, err
	}
	branch, err := s.scalar(ctx, "SELECT active_branch()")
	if err != nil {
		return VerifyResult{}, err
	}
	if branch != "compact-work" {
		return VerifyResult{}, fmt.Errorf(
			"doltsquash: statement %d (SELECT active_branch()) answered %q, want compact-work: the squashed tables must be read on compact-work",
			s.n, branch)
	}
	mainTip, err := s.scalar(ctx, "SELECT dolt_hashof('main')")
	if err != nil {
		return VerifyResult{}, err
	}
	if err := checkCommitHash("main's tip", mainTip); err != nil {
		return VerifyResult{}, err
	}
	base, err := s.scalar(ctx, "SELECT dolt_hashof('compact-base')")
	if err != nil {
		return VerifyResult{}, err
	}
	if err := checkCommitHash("compact-base's tip", base); err != nil {
		return VerifyResult{}, err
	}
	result := VerifyResult{MainTip: mainTip, Moved: mainTip != before.Tip}
	catalog, err := s.column(ctx, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return VerifyResult{}, err
	}
	if err := s.compareTables(ctx, before.Tables, catalog, &result); err != nil {
		return VerifyResult{}, err
	}
	cutLiteral := quoteLiteral(formatCut(cut))
	old, err := s.number(ctx, fmt.Sprintf(
		"SELECT COUNT(*) FROM dolt_log('compact-work') WHERE author_date <= %s AND commit_hash <> %s",
		cutLiteral, quoteLiteral(base)))
	if err != nil {
		return VerifyResult{}, err
	}
	if old != 1 {
		result.Differences = append(result.Differences, Difference{
			Kind: KindOldCommits,
			Want: "1",
			Got:  strconv.FormatInt(old, 10),
		})
	}
	workNewer, err := s.identities(ctx, fmt.Sprintf(
		"SELECT author_date, message FROM dolt_log('compact-work') WHERE author_date > %s ORDER BY author_date",
		cutLiteral))
	if err != nil {
		return VerifyResult{}, err
	}
	mainNewer, err := s.identities(ctx, fmt.Sprintf(
		"SELECT author_date, message FROM dolt_log AS OF %s WHERE author_date > %s ORDER BY author_date",
		quoteLiteral(before.Tip), cutLiteral))
	if err != nil {
		return VerifyResult{}, err
	}
	result.Differences = append(result.Differences, compareNewer(mainNewer, workNewer)...)
	return result, nil
}

// compareTables appends one difference per table the record holds that
// compact-work lacks, and one per row count or hash that differs. The record is
// read in table-name order so a run reports the same list every time.
func (s *designStep) compareTables(ctx context.Context, record []TableState, catalog []string, result *VerifyResult) error {
	present := make(map[string]bool, len(catalog))
	for _, name := range catalog {
		present[name] = true
	}
	tables := slices.Clone(record)
	slices.SortFunc(tables, func(a, b TableState) int { return cmp.Compare(a.Table, b.Table) })
	for _, want := range tables {
		if !present[want.Table] {
			result.Differences = append(result.Differences, Difference{Table: want.Table, Kind: KindMissingTable})
			continue
		}
		rows, err := s.number(ctx, "SELECT COUNT(*) FROM "+quoteIdent(want.Table))
		if err != nil {
			return err
		}
		hash, err := s.scalar(ctx, fmt.Sprintf("SELECT dolt_hashof_table(%s)", quoteLiteral(want.Table)))
		if err != nil {
			return err
		}
		if hash == "" {
			return fmt.Errorf("doltsquash: statement %d (SELECT dolt_hashof_table(%s)) returned no hash for %s",
				s.n, quoteLiteral(want.Table), want.Table)
		}
		if rows != want.Rows {
			result.Differences = append(result.Differences, Difference{
				Table: want.Table,
				Kind:  KindRowCount,
				Want:  strconv.FormatInt(want.Rows, 10),
				Got:   strconv.FormatInt(rows, 10),
			})
		}
		if hash != want.Hash {
			result.Differences = append(result.Differences, Difference{
				Table: want.Table,
				Kind:  KindTableHash,
				Want:  want.Hash,
				Got:   hash,
			})
		}
	}
	return nil
}

// commitIdentity is how a commit is recognized across a rebase: the rebase
// rewrites the hash and the commit date and keeps the author date and message.
type commitIdentity struct {
	// AuthorDate is author_date as Dolt renders it.
	AuthorDate string
	Message    string
}

// String renders the identity as the commit an item names.
func (c commitIdentity) String() string {
	return fmt.Sprintf("%s %q", c.AuthorDate, c.Message)
}

// identities reads a two-value record of author_date and message, one per
// commit.
func (s *designStep) identities(ctx context.Context, statement string) ([]commitIdentity, error) {
	rows, err := s.exec(ctx, statement)
	if err != nil {
		return nil, err
	}
	identities := make([]commitIdentity, 0, len(rows.Data))
	for i, record := range rows.Data {
		if len(record) != 2 {
			return nil, fmt.Errorf(
				"doltsquash: statement %d (%s) returned record %d of %d values, want author_date and message",
				s.n, shortStatement(statement), i, len(record))
		}
		identities = append(identities, commitIdentity{AuthorDate: record[0], Message: record[1]})
	}
	return identities, nil
}

// compareNewer returns one difference per commit the two newer ranges do not
// hold one for one, in author date then message order so a run reports them in
// one order.
func compareNewer(main, work []commitIdentity) []Difference {
	main = slices.Clone(main)
	work = slices.Clone(work)
	slices.SortFunc(main, compareIdentity)
	slices.SortFunc(work, compareIdentity)
	var differences []Difference
	i, j := 0, 0
	for i < len(main) && j < len(work) {
		switch c := compareIdentity(main[i], work[j]); {
		case c == 0:
			i++
			j++
		case c < 0:
			differences = append(differences, Difference{Commit: main[i].String(), Kind: KindMissingCommit})
			i++
		default:
			differences = append(differences, Difference{Commit: work[j].String(), Kind: KindExtraCommit})
			j++
		}
	}
	for ; i < len(main); i++ {
		differences = append(differences, Difference{Commit: main[i].String(), Kind: KindMissingCommit})
	}
	for ; j < len(work); j++ {
		differences = append(differences, Difference{Commit: work[j].String(), Kind: KindExtraCommit})
	}
	return differences
}

// compareIdentity orders identities by author date, then message.
func compareIdentity(a, b commitIdentity) int {
	if c := cmp.Compare(a.AuthorDate, b.AuthorDate); c != 0 {
		return c
	}
	return cmp.Compare(a.Message, b.Message)
}
