package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// tablesPresent lists the tables of the connected database.
func tablesPresent(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()")
	if err != nil {
		return nil, fmt.Errorf("migrate: list tables: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("migrate: list tables: %w", err)
		}
		out[strings.ToLower(n)] = true
	}
	return out, rows.Err()
}

// knownTables are the only tables Read queries.
var knownTables = map[string]bool{
	"issues": true, "wisps": true, "dependencies": true, "wisp_dependencies": true, "labels": true, "wisp_labels": true,
	"comments": true, "wisp_comments": true, "events": true, "wisp_events": true, "config": true, "custom_types": true,
	"custom_statuses": true, "child_counters": true, "wisp_child_counters": true,
}

// readTable returns every row of table as column name -> value. The table
// name is one of this package's constants, never caller input.
func readTable(ctx context.Context, db *sql.DB, table string) ([]map[string]any, error) {
	if !knownTables[table] {
		return nil, fmt.Errorf("migrate: %q is not a table this package reads", table)
	}
	rows, err := db.QueryContext(ctx, "SELECT * FROM `"+table+"`") //nolint:gosec // G202: table passed the knownTables check above
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", table, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", table, err)
	}
	var out []map[string]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", table, err)
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[strings.ToLower(c)] = vals[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", table, err)
	}
	return out, nil
}

// Read loads one rig's Dolt database into a Dataset. Wisp tables, custom
// statuses and child counters are read when present. Read is read-only.
func Read(ctx context.Context, db *sql.DB) (*Dataset, error) {
	have, err := tablesPresent(ctx, db)
	if err != nil {
		return nil, err
	}
	if !have["issues"] {
		return nil, fmt.Errorf("migrate: the connected database has no issues table; is it a beads database?")
	}
	ds := &Dataset{Config: map[string]string{}, ChildCounters: map[string]int{}}
	each := func(table string, required bool, fn func(row map[string]any)) error {
		if !have[table] {
			if required {
				return fmt.Errorf("migrate: the database has no %s table", table)
			}
			return nil
		}
		rows, err := readTable(ctx, db, table)
		if err != nil {
			return err
		}
		for _, r := range rows {
			fn(r)
		}
		return nil
	}
	steps := []struct {
		table    string
		required bool
		fn       func(map[string]any)
	}{
		{"issues", true, func(r map[string]any) { ds.Issues = append(ds.Issues, issueFromRow(r, false)) }},
		{"wisps", false, func(r map[string]any) { ds.Issues = append(ds.Issues, issueFromRow(r, true)) }},
		{"dependencies", true, func(r map[string]any) { ds.Deps = append(ds.Deps, depFromRow(r)) }},
		{"wisp_dependencies", false, func(r map[string]any) { ds.Deps = append(ds.Deps, depFromRow(r)) }},
		{"labels", true, func(r map[string]any) {
			ds.Labels = append(ds.Labels, LabelRow{IssueID: str(r, "issue_id"), Label: str(r, "label")})
		}},
		{"wisp_labels", false, func(r map[string]any) {
			ds.Labels = append(ds.Labels, LabelRow{IssueID: str(r, "issue_id"), Label: str(r, "label")})
		}},
		{"comments", true, func(r map[string]any) { ds.Comments = append(ds.Comments, commentFromRow(r)) }},
		{"wisp_comments", false, func(r map[string]any) { ds.Comments = append(ds.Comments, commentFromRow(r)) }},
		{"events", true, func(r map[string]any) { ds.Events = append(ds.Events, eventFromRow(r)) }},
		{"wisp_events", false, func(r map[string]any) { ds.Events = append(ds.Events, eventFromRow(r)) }},
		{"config", true, func(r map[string]any) { ds.Config[str(r, "key")] = str(r, "value") }},
		{"custom_types", false, func(r map[string]any) { ds.CustomTypes = append(ds.CustomTypes, str(r, "name")) }},
		{"custom_statuses", false, func(r map[string]any) {
			ds.CustomStatuses = append(ds.CustomStatuses, CustomStatus{Name: str(r, "name"), Category: str(r, "category")})
		}},
		{"child_counters", false, func(r map[string]any) {
			ds.ChildCounters[str(r, "parent_id")] = max(ds.ChildCounters[str(r, "parent_id")], integer(r, "last_child"))
		}},
		{"wisp_child_counters", false, func(r map[string]any) {
			ds.ChildCounters[str(r, "parent_id")] = max(ds.ChildCounters[str(r, "parent_id")], integer(r, "last_child"))
		}},
	}
	for _, s := range steps {
		if err := each(s.table, s.required, s.fn); err != nil {
			return nil, err
		}
	}
	return ds, nil
}
