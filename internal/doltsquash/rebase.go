package doltsquash

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// TableState is one versioned table's row count and Dolt content hash, as
// design step 3 records them.
type TableState struct {
	Table string
	Rows  int64
	Hash  string
}

// Before is design step 3's record of what main held before the squash: its
// tip, its commit count, and per versioned table the row count and
// dolt_hashof_table, ordered by table name.
type Before struct {
	Tip     string
	Commits int64
	Tables  []TableState
}

// RebaseResult is what design step 4's rebase did.
type RebaseResult struct {
	// Marked is the number of dolt_rebase rows whose action is squash, read
	// back after the marking update; the first plan row stays a pick.
	Marked int
}

// designStep numbers the statements of one design step, so a failure names the
// position of the statement that stopped the run.
type designStep struct {
	ex Exec
	n  int
}

// exec runs one statement and numbers it.
func (s *designStep) exec(ctx context.Context, statement string) (Rows, error) {
	s.n++
	rows, err := s.ex.Exec(ctx, statement)
	if err != nil {
		return Rows{}, fmt.Errorf("doltsquash: statement %d (%s): %w", s.n, shortStatement(statement), err)
	}
	return rows, nil
}

// scalar runs statement and returns the one value of its one record.
func (s *designStep) scalar(ctx context.Context, statement string) (string, error) {
	rows, err := s.exec(ctx, statement)
	if err != nil {
		return "", err
	}
	if len(rows.Data) != 1 {
		return "", fmt.Errorf("doltsquash: statement %d (%s) returned %d records, want one",
			s.n, shortStatement(statement), len(rows.Data))
	}
	if len(rows.Data[0]) != 1 {
		return "", fmt.Errorf("doltsquash: statement %d (%s) returned a record of %d values, want one",
			s.n, shortStatement(statement), len(rows.Data[0]))
	}
	return rows.Data[0][0], nil
}

// number runs statement and returns its one value as a count.
func (s *designStep) number(ctx context.Context, statement string) (int64, error) {
	value, err := s.scalar(ctx, statement)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("doltsquash: statement %d (%s) returned %q, want a count",
			s.n, shortStatement(statement), value)
	}
	return n, nil
}

// column runs statement and returns the first value of every record.
func (s *designStep) column(ctx context.Context, statement string) ([]string, error) {
	rows, err := s.exec(ctx, statement)
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, len(rows.Data))
	for i, record := range rows.Data {
		if len(record) == 0 {
			return nil, fmt.Errorf("doltsquash: statement %d (%s) returned record %d with no values",
				s.n, shortStatement(statement), i)
		}
		values = append(values, record[0])
	}
	return values, nil
}

// RecordBefore reads and returns design step 3's BEFORE record of main.
//
// The row counts and table hashes are read from the session's working set, so
// the session is put on main and checked there first: on any other branch the
// record would describe that branch while its tip named main (gt-1w9790).
func RecordBefore(ctx context.Context, ex Exec) (Before, error) {
	s := &designStep{ex: ex}
	if _, err := s.exec(ctx, "CALL DOLT_CHECKOUT('main')"); err != nil {
		return Before{}, err
	}
	branch, err := s.scalar(ctx, "SELECT active_branch()")
	if err != nil {
		return Before{}, err
	}
	if branch != "main" {
		return Before{}, fmt.Errorf(
			"doltsquash: statement %d (SELECT active_branch()) answered %q, want main: the BEFORE record must be read on main",
			s.n, branch)
	}
	tip, err := s.scalar(ctx, "SELECT dolt_hashof('main')")
	if err != nil {
		return Before{}, err
	}
	if err := checkCommitHash("main's tip", tip); err != nil {
		return Before{}, err
	}
	commits, err := s.number(ctx, "SELECT COUNT(*) FROM dolt_log('main')")
	if err != nil {
		return Before{}, err
	}
	names, err := s.column(ctx, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return Before{}, err
	}
	slices.Sort(names)
	before := Before{Tip: tip, Commits: commits}
	for _, name := range names {
		// Dolt's own tables are computed from the history rather than part of
		// it, so there is no table hash a later comparison could hold them to.
		if strings.HasPrefix(name, "dolt_") {
			continue
		}
		ignored, err := s.number(ctx, fmt.Sprintf(
			"SELECT COUNT(*) FROM dolt_ignore WHERE ignored = 1 AND %s LIKE pattern", quoteLiteral(name)))
		if err != nil {
			return Before{}, err
		}
		if ignored > 0 {
			// No commit holds a dolt_ignored table's rows, so its hash here
			// would not survive the branch switch that verification makes.
			continue
		}
		rows, err := s.number(ctx, "SELECT COUNT(*) FROM "+quoteIdent(name))
		if err != nil {
			return Before{}, err
		}
		hash, err := s.scalar(ctx, fmt.Sprintf("SELECT dolt_hashof_table(%s)", quoteLiteral(name)))
		if err != nil {
			return Before{}, err
		}
		if hash == "" {
			return Before{}, fmt.Errorf("doltsquash: dolt_hashof_table(%s) returned no hash", name)
		}
		before.Tables = append(before.Tables, TableState{Table: name, Rows: rows, Hash: hash})
	}
	return before, nil
}

// RebaseHistory runs design step 4: compact-base at main's initial commit,
// compact-work at before.Tip, the interactive rebase, squash marked on every
// plan row after the first whose commit main held at or before cut, and the
// continue. The filter reads dolt_log('main') by name, because inside a rebase
// session an unqualified log answers only the base (gt-1w9790). On error the
// branches and any open rebase session are left for the caller's recovery by
// state.
func RebaseHistory(ctx context.Context, ex Exec, before Before, cut time.Time) (RebaseResult, error) {
	if err := checkCommitHash("the recorded main tip", before.Tip); err != nil {
		return RebaseResult{}, err
	}
	s := &designStep{ex: ex}
	initial, err := s.scalar(ctx, "SELECT commit_hash FROM dolt_log('main') ORDER BY date ASC LIMIT 1")
	if err != nil {
		return RebaseResult{}, err
	}
	if err := checkCommitHash("main's initial commit", initial); err != nil {
		return RebaseResult{}, err
	}
	for _, statement := range []string{
		fmt.Sprintf("CALL DOLT_BRANCH('compact-base', %s)", quoteLiteral(initial)),
		fmt.Sprintf("CALL DOLT_BRANCH('compact-work', %s)", quoteLiteral(before.Tip)),
		"CALL DOLT_CHECKOUT('compact-work')",
		"CALL DOLT_REBASE('--interactive', 'compact-base')",
	} {
		if _, err := s.exec(ctx, statement); err != nil {
			return RebaseResult{}, err
		}
	}
	plan, err := readPlan(ctx, s)
	if err != nil {
		return RebaseResult{}, err
	}
	if len(plan) == 0 {
		return RebaseResult{}, fmt.Errorf("doltsquash: the rebase plan on compact-work is empty, so there is nothing to squash")
	}
	first := plan[0].order
	matched, err := s.number(ctx, countStatement(first, cut))
	if err != nil {
		return RebaseResult{}, err
	}
	if matched == 0 {
		return RebaseResult{}, fmt.Errorf(
			"doltsquash: none of the %d plan rows is on main at or before %s, so the cut squashes nothing",
			len(plan), formatCut(cut))
	}
	if _, err := s.exec(ctx, markStatement(first, cut)); err != nil {
		return RebaseResult{}, err
	}
	marked, err := s.number(ctx, squashRowsStatement)
	if err != nil {
		return RebaseResult{}, err
	}
	if _, err := s.exec(ctx, "CALL DOLT_REBASE('--continue')"); err != nil {
		return RebaseResult{}, err
	}
	return RebaseResult{Marked: int(marked)}, nil
}

// planRow is one row of the interactive rebase plan Dolt holds in dolt_rebase.
// commit is validated as a hash when the plan is read: a row that names no
// commit means the session is not holding the plan this step asked for, so the
// run stops rather than marking rows it cannot trust.
type planRow struct {
	order  int64
	commit string
}

// readPlan returns the rebase plan in order.
func readPlan(ctx context.Context, s *designStep) ([]planRow, error) {
	const statement = "SELECT rebase_order, commit_hash FROM dolt_rebase ORDER BY rebase_order"
	rows, err := s.exec(ctx, statement)
	if err != nil {
		return nil, err
	}
	plan := make([]planRow, 0, len(rows.Data))
	for i, record := range rows.Data {
		if len(record) != 2 {
			return nil, fmt.Errorf("doltsquash: statement %d (%s) returned record %d of %d values, want order and commit",
				s.n, shortStatement(statement), i, len(record))
		}
		order, err := strconv.ParseInt(record[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("doltsquash: statement %d (%s) returned order %q, want a number",
				s.n, shortStatement(statement), record[0])
		}
		if err := checkCommitHash(fmt.Sprintf("plan row %d", i), record[1]); err != nil {
			return nil, err
		}
		plan = append(plan, planRow{order: order, commit: record[1]})
	}
	return plan, nil
}

// squashPredicate is design step 4's filter: every plan row after the first
// whose commit main held at or before cut. The first row stays a pick because a
// squash folds into the row before it. The log is read by branch name, because
// inside a rebase session dolt_log answers only the base, so a filter that names
// no branch selects nothing (gt-1w9790).
func squashPredicate(first int64, cut time.Time) string {
	return fmt.Sprintf(
		"rebase_order > %d AND commit_hash IN (SELECT commit_hash FROM dolt_log('main') WHERE date <= %s)",
		first, quoteLiteral(formatCut(cut)))
}

// countStatement returns the count of plan rows the marking update will mark:
// it shares the update's predicate, so the number returned is the number of
// rows the update touches.
func countStatement(first int64, cut time.Time) string {
	return "SELECT COUNT(*) FROM dolt_rebase WHERE " + squashPredicate(first, cut)
}

// markStatement renders the update that marks the predicate's rows squash.
func markStatement(first int64, cut time.Time) string {
	return "UPDATE dolt_rebase SET action = 'squash' WHERE " + squashPredicate(first, cut)
}

// squashRowsStatement reads back what the update marked, so the run returns the
// rows the plan holds after the update rather than the count the filter gave
// before it.
const squashRowsStatement = "SELECT COUNT(*) FROM dolt_rebase WHERE action = 'squash'"

// formatCut renders cut the way Dolt's DATETIME comparison reads it: UTC, and
// without a sub-second part, so the window is never wider than the clock's cut.
func formatCut(cut time.Time) string {
	return cut.UTC().Format("2006-01-02 15:04:05")
}

// checkCommitHash refuses a value that is not a Dolt commit hash. Dolt encodes
// a commit hash as 32 characters of the base32 alphabet 0-9a-v, so a hex-only
// check would refuse a real tip (gt-1w9790). Every hash reaches a SQL literal,
// so a value that is not one is a read the run cannot trust.
func checkCommitHash(what, value string) error {
	valid := len(value) == 32
	for _, c := range value {
		valid = valid && (c >= '0' && c <= '9' || c >= 'a' && c <= 'v')
	}
	if !valid {
		return fmt.Errorf("doltsquash: %s is %q, want a 32-character Dolt commit hash over the alphabet 0-9a-v", what, value)
	}
	return nil
}

// quoteIdent renders name as a backtick identifier, doubling a backtick so a
// name read from the catalog cannot end it early.
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteLiteral renders s as a single-quoted SQL string, doubling the quote and
// the backslash, which MySQL reads as an escape inside one.
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `''`)
	return "'" + s + "'"
}

// shortStatementWidth bounds the statement a failure message carries: a marking
// update names thousands of hashes, and the position is what locates it.
const shortStatementWidth = 80

// shortStatement shortens statement to shortStatementWidth runes.
func shortStatement(statement string) string {
	runes := []rune(statement)
	if len(runes) <= shortStatementWidth {
		return statement
	}
	return string(runes[:shortStatementWidth]) + "..."
}
