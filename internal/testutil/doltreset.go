package testutil

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// doltHead returns the commit name's main branch points at.
func doltHead(ctx context.Context, db *sql.DB, name string) (string, error) {
	var head string
	q := "SELECT hash FROM `" + name + "`.dolt_branches WHERE name = 'main'"
	if err := db.QueryRowContext(ctx, q).Scan(&head); err != nil {
		return "", fmt.Errorf("read the head of %s: %w", name, err)
	}
	return head, nil
}

// resetDoltDatabase puts database name back to commit, on the server behind
// db, without creating or dropping a database, and checks the result.
//
// First it ends the previous lessee's hold: a session still open on the
// database could commit into it after the reset, and the next lessee would
// start dirty without anything noticing. Sessions get sessionWait to close;
// any left are killed, which rolls back their transactions, and the reset
// refuses the database with an error that lists them. The pool never lends a
// database it refused.
//
// Then its stashes, tags and remotes go, every table commit does not hold is
// dropped — the tables bd keeps out of commits with dolt_ignore (wisps, local
// state) as well as a test's own, dropped before the reset so none keeps an
// AUTO_INCREMENT counter — main is reset hard to commit, and every other
// branch goes.
func resetDoltDatabase(db *sql.DB, name, commit string, sessionWait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer releaseResetConn(conn)
	if err := endSessionsOn(ctx, conn, name, sessionWait); err != nil {
		return err
	}
	exec := func(q string, args ...any) error {
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
		return nil
	}
	if err := exec("USE `" + name + "`"); err != nil {
		return err
	}
	if err := exec("CALL DOLT_CHECKOUT('main')"); err != nil {
		return err
	}
	stashes, err := queryStrings(ctx, conn, "SELECT DISTINCT name FROM dolt_stashes")
	if err != nil {
		return err
	}
	for _, st := range stashes {
		if err := exec("CALL DOLT_STASH('clear', ?)", st); err != nil {
			return err
		}
	}
	tags, err := queryStrings(ctx, conn, "SELECT tag_name FROM dolt_tags")
	if err != nil {
		return err
	}
	for _, tag := range tags {
		if err := exec("CALL DOLT_TAG('-d', ?)", tag); err != nil {
			return err
		}
	}
	remotes, err := queryStrings(ctx, conn, "SELECT name FROM dolt_remotes")
	if err != nil {
		return err
	}
	for _, r := range remotes {
		if err := exec("CALL DOLT_REMOTE('remove', ?)", r); err != nil {
			return err
		}
	}
	extra, err := tablesNotIn(ctx, conn, commit)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		if err := exec("SET FOREIGN_KEY_CHECKS = 0"); err != nil {
			return err
		}
		for _, table := range extra {
			if err := exec("DROP TABLE `" + table + "`"); err != nil {
				return err
			}
		}
		if err := exec("SET FOREIGN_KEY_CHECKS = 1"); err != nil {
			return err
		}
	}
	if err := exec("CALL DOLT_RESET('--hard', ?)", commit); err != nil {
		return err
	}
	branches, err := queryStrings(ctx, conn, "SELECT name FROM dolt_branches WHERE name <> 'main'")
	if err != nil {
		return err
	}
	for _, b := range branches {
		if err := exec("CALL DOLT_BRANCH('-D', ?)", b); err != nil {
			return err
		}
	}
	return checkDoltDatabaseAt(ctx, conn, name, commit)
}

// releaseResetConn hands a reset's connection back to the pool's *sql.DB
// holding no database, so the next release's session check does not count it
// as a session on the database it reset. A connection that cannot leave the
// database is discarded instead.
func releaseResetConn(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(ctx, "USE information_schema"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	_ = conn.Close()
}

// endSessionsOn waits up to wait for every other session on database name —
// or on one of its branches, name/branch — to end, and kills the ones that do
// not. Killed sessions are an error: their lessee left them open.
func endSessionsOn(ctx context.Context, conn *sql.Conn, name string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		open, err := sessionsOn(ctx, conn, name)
		if err != nil {
			return err
		}
		if len(open) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			var killErrs []string
			for _, s := range open {
				if _, err := conn.ExecContext(ctx, "KILL "+s.id); err != nil {
					killErrs = append(killErrs, fmt.Sprintf("KILL %s: %v", s.id, err))
				}
			}
			msg := fmt.Sprintf("%d sessions were still open on it %s after the lease ended (%s); killed them, rolling back their transactions",
				len(open), wait, strings.Join(sessionDescriptions(open), "; "))
			if len(killErrs) > 0 {
				msg += "; " + strings.Join(killErrs, "; ")
			}
			return errors.New(msg + ". Close every store and connection the test opened on it before the test ends")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type doltSession struct{ id, user, command, info string }

func sessionDescriptions(open []doltSession) []string {
	out := make([]string, len(open))
	for i, s := range open {
		out[i] = fmt.Sprintf("id %s %s %s %q", s.id, s.user, s.command, s.info)
	}
	return out
}

// sessionsOn lists the sessions other than conn's own whose database is name
// or one of its branches.
func sessionsOn(ctx context.Context, conn *sql.Conn, name string) ([]doltSession, error) {
	const q = "SELECT CAST(id AS CHAR), COALESCE(user, ''), COALESCE(command, ''), COALESCE(info, '') " +
		"FROM information_schema.processlist WHERE (db = ? OR db LIKE ?) AND id <> CONNECTION_ID()"
	rows, err := conn.QueryContext(ctx, q, name, name+"/%")
	if err != nil {
		return nil, fmt.Errorf("list the sessions on %s: %w", name, err)
	}
	defer rows.Close()
	var open []doltSession
	for rows.Next() {
		var s doltSession
		if err := rows.Scan(&s.id, &s.user, &s.command, &s.info); err != nil {
			return nil, fmt.Errorf("list the sessions on %s: %w", name, err)
		}
		open = append(open, s)
	}
	return open, rows.Err()
}

// tablesNotIn returns the tables the session's database has that commit does
// not.
func tablesNotIn(ctx context.Context, conn *sql.Conn, commit string) ([]string, error) {
	now, err := queryStrings(ctx, conn, "SHOW FULL TABLES WHERE Table_type = 'BASE TABLE'")
	if err != nil {
		return nil, err
	}
	then, err := queryStrings(ctx, conn, "SHOW TABLES AS OF '"+commit+"'")
	if err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(then))
	for _, t := range then {
		held[t] = true
	}
	var extra []string
	for _, t := range now {
		if !held[t] {
			extra = append(extra, t)
		}
	}
	return extra, nil
}

// checkDoltDatabaseAt fails unless the session's database, name, is exactly
// commit: main at commit and alone, a clean working set, no stashes, tags or
// remotes, no table commit does not hold, and no AUTO_INCREMENT column.
//
// A hard reset restores rows but not AUTO_INCREMENT counters, and Dolt 2.0.7
// reports no counter after one (information_schema and SHOW CREATE TABLE both
// drop it, while the next insert still continues from the lessee's), so a
// moved counter cannot be detected. A reset point that keeps such a table is
// refused instead: the pool's reset points are an empty database and bd's
// migrated schema, which has none (UUID keys since migration 0037).
func checkDoltDatabaseAt(ctx context.Context, conn *sql.Conn, name, commit string) error {
	var problems []string
	if heads, err := queryStrings(ctx, conn, "SELECT CONCAT(name, '@', hash) FROM dolt_branches"); err != nil {
		return err
	} else if len(heads) != 1 || heads[0] != "main@"+commit {
		problems = append(problems, fmt.Sprintf("branches %v, want only main@%s", heads, commit))
	}
	for _, c := range []struct{ what, q string }{
		{"uncommitted changes", "SELECT CONCAT(table_name, ' ', status) FROM dolt_status"},
		{"stashes", "SELECT name FROM dolt_stashes"},
		{"tags", "SELECT tag_name FROM dolt_tags"},
		{"remotes", "SELECT name FROM dolt_remotes"},
	} {
		got, err := queryStrings(ctx, conn, c.q)
		if err != nil {
			return err
		}
		if len(got) > 0 {
			problems = append(problems, fmt.Sprintf("%s %v", c.what, got))
		}
	}
	extra, err := tablesNotIn(ctx, conn, commit)
	if err != nil {
		return err
	}
	if len(extra) > 0 {
		problems = append(problems, fmt.Sprintf("tables %v not in the commit", extra))
	}
	counted, err := queryStrings(ctx, conn, "SELECT CONCAT(table_name, '.', column_name) FROM information_schema.columns "+
		"WHERE table_schema = '"+name+"' AND LOWER(extra) LIKE '%auto_increment%'")
	if err != nil {
		return err
	}
	if len(counted) > 0 {
		sort.Strings(counted)
		problems = append(problems, fmt.Sprintf("AUTO_INCREMENT columns %v, whose counters a reset cannot restore", counted))
	}
	if len(problems) > 0 {
		return fmt.Errorf("reset left %s", strings.Join(problems, "; "))
	}
	return nil
}

// queryStrings returns the first column of every row q returns.
func queryStrings(ctx context.Context, conn *sql.Conn, q string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("%s: %w", q, err)
		}
		out = append(out, vals[0].String)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", q, err)
	}
	return out, nil
}
