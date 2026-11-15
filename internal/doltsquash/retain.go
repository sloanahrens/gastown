package doltsquash

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RetainPeriod is how long main-pre-squash stays after the swap before the
// second verification may drop it: the design's 48 hours.
const RetainPeriod = 48 * time.Hour

// RetainCommits is how many of the newest commits present on both branches the
// second verification compares.
const RetainCommits = 20

// RetentionMismatch names the first commit pair whose tables differed, which
// is what an item raised from a failed second verification reports.
type RetentionMismatch struct {
	// Commit is the pair's identity, its author date and message: the swap
	// replays commits, so the two branches share these and not their hashes.
	Commit string
	// Table names the table whose state differed.
	Table string
	// Main and Retained hold Table's hash at main's commit and at
	// main-pre-squash's; a table missing on one side leaves that hash empty.
	Main     string
	Retained string
}

// String renders the mismatch as the line an item raised from it carries.
func (m RetentionMismatch) String() string {
	switch {
	case m.Main == "":
		return fmt.Sprintf("commit %s: table %s is missing on main", m.Commit, m.Table)
	case m.Retained == "":
		return fmt.Sprintf("commit %s: table %s is missing on main-pre-squash", m.Commit, m.Table)
	default:
		return fmt.Sprintf("commit %s: table %s hashes %s on main and %s on main-pre-squash",
			m.Commit, m.Table, m.Main, m.Retained)
	}
}

// RetainResult is what design step 9 did.
type RetainResult struct {
	// Due reports whether the retention period had elapsed on the clock the
	// run gave; a check inside it compares nothing and drops nothing.
	Due bool
	// Compared is how many commit pairs the verification compared, newest
	// first.
	Compared int
	// Mismatch names the first pair that differed, nil when every compared
	// pair matched.
	Mismatch *RetentionMismatch
	// Dropped reports that main-pre-squash and compact-base were dropped,
	// which happens only when every compared pair matched.
	Dropped bool
	// Reason is the log line: why the check held, what differed, or what it
	// dropped.
	Reason string
}

// Retain runs design step 9: after the retention period it compares the table
// hashes at the newest commits present on both main and main-pre-squash, and
// drops both that branch and compact-base only on an exact match.
//
// A check before the period has elapsed drops nothing and sends no statement;
// an unrecorded or future swappedAt, and a pair of branches with no commit in
// common, are errors rather than a drop, because neither can verify anything.
func Retain(ctx context.Context, ex Exec, now, swappedAt time.Time) (RetainResult, error) {
	if swappedAt.IsZero() {
		return RetainResult{}, fmt.Errorf(
			"doltsquash: the swap time is not recorded, so the retention period has no start and main-pre-squash stays")
	}
	if swappedAt.After(now) {
		return RetainResult{}, fmt.Errorf(
			"doltsquash: the swap time %s is after the clock %s, so the retention period has no trustworthy start and main-pre-squash stays",
			swappedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	if now.Before(swappedAt.Add(RetainPeriod)) {
		return RetainResult{Reason: fmt.Sprintf("held: main-pre-squash is retained until %s",
			swappedAt.Add(RetainPeriod).UTC().Format(time.RFC3339))}, nil
	}
	s := &designStep{ex: ex}
	db, err := s.database(ctx)
	if err != nil {
		return RetainResult{}, err
	}
	main, err := s.history(ctx, "main")
	if err != nil {
		return RetainResult{}, err
	}
	retained, err := s.history(ctx, "main-pre-squash")
	if err != nil {
		return RetainResult{}, err
	}
	pairs := commonCommits(main, retained)
	if len(pairs) == 0 {
		return RetainResult{}, fmt.Errorf(
			"doltsquash: no commit is present on both main and main-pre-squash, so the second verification has nothing to compare and keeps both branches")
	}
	for i, pair := range pairs {
		mainTables, err := s.tablesAt(ctx, db, pair.main.hash)
		if err != nil {
			return RetainResult{}, err
		}
		retainedTables, err := s.tablesAt(ctx, db, pair.retained.hash)
		if err != nil {
			return RetainResult{}, err
		}
		if m := compareRevisionTables(pair.main.identity.String(), mainTables, retainedTables); m != nil {
			return RetainResult{Due: true, Compared: i + 1, Mismatch: m, Reason: "kept both branches: " + m.String()}, nil
		}
	}
	for _, branch := range []string{"main-pre-squash", "compact-base"} {
		if _, err := s.exec(ctx, "CALL DOLT_BRANCH('-D', "+quoteLiteral(branch)+")"); err != nil {
			return RetainResult{}, err
		}
	}
	return RetainResult{
		Due:      true,
		Compared: len(pairs),
		Dropped:  true,
		Reason:   fmt.Sprintf("dropped main-pre-squash and compact-base: %d commit pairs matched", len(pairs)),
	}, nil
}

// database returns the session's database name without a revision suffix: the
// reads of one commit build a revision database name from it.
func (s *designStep) database(ctx context.Context) (string, error) {
	name, err := s.scalar(ctx, "SELECT DATABASE()")
	if err != nil {
		return "", err
	}
	base, _, _ := strings.Cut(name, "/")
	if base == "" {
		return "", fmt.Errorf(
			"doltsquash: statement %d (SELECT DATABASE()) returned no database name, so a commit's tables cannot be read", s.n)
	}
	return base, nil
}

// historyCommit is one commit as a branch's log answers it.
type historyCommit struct {
	hash     string
	identity commitIdentity
}

// history reads a branch's commits in the order its log answers them, with
// each hash checked before it reaches a revision database name.
func (s *designStep) history(ctx context.Context, branch string) ([]historyCommit, error) {
	statement := fmt.Sprintf("SELECT commit_hash, author_date, message FROM dolt_log(%s)", quoteLiteral(branch))
	rows, err := s.exec(ctx, statement)
	if err != nil {
		return nil, err
	}
	commits := make([]historyCommit, 0, len(rows.Data))
	for i, record := range rows.Data {
		if len(record) != 3 {
			return nil, fmt.Errorf(
				"doltsquash: statement %d (%s) returned record %d of %d values, want commit_hash, author_date and message",
				s.n, shortStatement(statement), i, len(record))
		}
		if err := checkCommitHash("commit "+record[0]+" in "+branch+"'s log", record[0]); err != nil {
			return nil, err
		}
		commits = append(commits, historyCommit{
			hash:     record[0],
			identity: commitIdentity{AuthorDate: record[1], Message: record[2]},
		})
	}
	return commits, nil
}

// commitPair is one commit held by both branches: the same author date and
// message under main's hash and under main-pre-squash's.
type commitPair struct {
	main     historyCommit
	retained historyCommit
}

// commonCommits pairs the commits both branches hold by identity, newest
// first, and keeps at most RetainCommits of them. Commits that share an
// identity are paired in log order.
func commonCommits(main, retained []historyCommit) []commitPair {
	queued := make(map[commitIdentity][]historyCommit, len(retained))
	for _, c := range retained {
		queued[c.identity] = append(queued[c.identity], c)
	}
	var pairs []commitPair
	for _, m := range main {
		held := queued[m.identity]
		if len(held) == 0 {
			continue
		}
		pairs = append(pairs, commitPair{main: m, retained: held[0]})
		queued[m.identity] = held[1:]
	}
	slices.SortStableFunc(pairs, func(a, b commitPair) int {
		return cmp.Compare(b.main.identity.AuthorDate, a.main.identity.AuthorDate)
	})
	if len(pairs) > RetainCommits {
		pairs = pairs[:RetainCommits]
	}
	return pairs
}

// revisionTables is one commit's versioned tables and their hashes, in
// table-name order.
type revisionTables struct {
	names  []string
	hashes map[string]string
}

// tablesAt reads every versioned table's hash at one commit, which history
// has already checked is a Dolt hash: the revision database name is built
// from it.
//
// dolt_hashof_table reads the session's working set and takes no revision, so
// the commit is read through a revision database, db/commit; the session goes
// back to the database before this returns. A failure part-way leaves it on
// the revision database: the run stops there, and the caller positions the
// session by the recovery state.
func (s *designStep) tablesAt(ctx context.Context, db, commit string) (revisionTables, error) {
	if _, err := s.exec(ctx, "USE "+quoteIdent(db+"/"+commit)); err != nil {
		return revisionTables{}, err
	}
	catalog, err := s.column(ctx, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return revisionTables{}, err
	}
	tables := revisionTables{hashes: make(map[string]string, len(catalog))}
	for _, name := range catalog {
		if strings.HasPrefix(name, "dolt_") {
			// Dolt's own tables are not the database's data, and their rows
			// are not versioned the way a data table's are.
			continue
		}
		hash, err := s.scalar(ctx, fmt.Sprintf("SELECT dolt_hashof_table(%s)", quoteLiteral(name)))
		if err != nil {
			return revisionTables{}, err
		}
		if hash == "" {
			return revisionTables{}, fmt.Errorf(
				"doltsquash: statement %d (SELECT dolt_hashof_table(%s)) returned no hash for %s at %s",
				s.n, quoteLiteral(name), name, commit)
		}
		tables.names = append(tables.names, name)
		tables.hashes[name] = hash
	}
	slices.Sort(tables.names)
	if _, err := s.exec(ctx, "USE "+quoteIdent(db)); err != nil {
		return revisionTables{}, err
	}
	return tables, nil
}

// compareRevisionTables returns the first table that main's commit and
// main-pre-squash's commit do not hold alike, nil when they hold the same
// tables with the same hashes. Tables are compared in name order, main's
// catalog first, so a run reports the same table every time.
func compareRevisionTables(commit string, main, retained revisionTables) *RetentionMismatch {
	for _, name := range main.names {
		want, ok := retained.hashes[name]
		if !ok {
			return &RetentionMismatch{Commit: commit, Table: name, Main: main.hashes[name]}
		}
		if got := main.hashes[name]; got != want {
			return &RetentionMismatch{Commit: commit, Table: name, Main: got, Retained: want}
		}
	}
	for _, name := range retained.names {
		if _, ok := main.hashes[name]; !ok {
			return &RetentionMismatch{Commit: commit, Table: name, Retained: retained.hashes[name]}
		}
	}
	return nil
}
