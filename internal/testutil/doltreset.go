package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
// db, without creating or dropping a database: its tags and remotes go, main
// is reset hard to commit, every other branch goes, and so does every table
// commit does not hold — the tables bd keeps out of commits with dolt_ignore
// (wisps, local state) as well as a test's own. It then checks the result.
func resetDoltDatabase(db *sql.DB, name, commit string) error {
	ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
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
	return checkDoltDatabaseAt(ctx, conn, commit)
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

// checkDoltDatabaseAt fails unless the session's database is exactly commit:
// main at commit and alone, a clean working set, no tags, no remotes, and no
// table commit does not hold.
func checkDoltDatabaseAt(ctx context.Context, conn *sql.Conn, commit string) error {
	var problems []string
	if heads, err := queryStrings(ctx, conn, "SELECT CONCAT(name, '@', hash) FROM dolt_branches"); err != nil {
		return err
	} else if len(heads) != 1 || heads[0] != "main@"+commit {
		problems = append(problems, fmt.Sprintf("branches %v, want only main@%s", heads, commit))
	}
	for _, c := range []struct{ what, q string }{
		{"uncommitted changes", "SELECT CONCAT(table_name, ' ', status) FROM dolt_status"},
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
