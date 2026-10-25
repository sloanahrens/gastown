package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Diff is what Verify found. An empty Problems list means the tracker holds
// what the Dolt dataset held, row for row in every table checked.
type Diff struct{ Problems []string }

// Equal reports whether nothing differs.
func (d Diff) Equal() bool { return len(d.Problems) == 0 }

const problemLimit = 10

type problems struct{ list []string }

func (p *problems) addf(format string, args ...any) {
	if len(p.list) < problemLimit*8 {
		p.list = append(p.list, fmt.Sprintf(format, args...))
	}
}

func us(t time.Time) string { return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano) }

func usPtr(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return us(*t)
}

// fingerprint is the part of an issue that must survive the move unchanged.
func fingerprint(id, typ, status, title string, priority int, assignee string, ephemeral bool, parent string, created, updated time.Time, closed *time.Time, labels []string) string {
	l := slices.Clone(labels)
	slices.Sort(l)
	return strings.Join([]string{id, typ, status, title, fmt.Sprint(priority), assignee, fmt.Sprint(ephemeral), parent, us(created), us(updated), usPtr(closed), strings.Join(l, ",")}, "\x1f")
}

// Verify compares ds with the tracker. It reads only.
func Verify(ctx context.Context, pool *pgxpool.Pool, ds *Dataset) (Diff, error) {
	var p problems
	parents := parentsOf(ds.Deps)
	labelsBy := map[string][]string{}
	seenLabel := map[[2]string]bool{}
	for _, l := range ds.Labels {
		if k := [2]string{l.IssueID, l.Label}; !seenLabel[k] && l.Label != "" {
			seenLabel[k] = true
			labelsBy[l.IssueID] = append(labelsBy[l.IssueID], l.Label)
		}
	}
	want := make(map[string]string, len(ds.Issues))
	for _, is := range ds.Issues {
		want[is.ID] = fingerprint(is.ID, is.Type, is.Status, is.Title, is.Priority, is.Assignee, is.Wisp, parents[is.ID], is.CreatedAt, is.UpdatedAt, is.ClosedAt, labelsBy[is.ID])
	}

	rows, err := pool.Query(ctx, `SELECT i.id, i.issue_type, i.status, i.title, i.priority, i.assignee, i.ephemeral, coalesce(i.parent_id, ''),
i.created_at, i.updated_at, i.closed_at, coalesce((SELECT array_agg(l.label) FROM tracker.labels l WHERE l.issue_id = i.id), '{}')
FROM tracker.issues i`)
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
	}
	got := map[string]string{}
	for rows.Next() {
		var id, typ, status, title, assignee, parent string
		var priority int16
		var ephemeral bool
		var created, updated time.Time
		var closed *time.Time
		var labels []string
		if err := rows.Scan(&id, &typ, &status, &title, &priority, &assignee, &ephemeral, &parent, &created, &updated, &closed, &labels); err != nil {
			rows.Close()
			return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
		}
		got[id] = fingerprint(id, typ, status, title, int(priority), assignee, ephemeral, parent, created, updated, closed, labels)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Diff{}, fmt.Errorf("migrate: verify issues: %w", err)
	}
	if len(got) != len(want) {
		p.addf("issues: %d in the tracker, %d in the dataset", len(got), len(want))
	}
	bad := 0
	for id, w := range want {
		g, ok := got[id]
		switch {
		case !ok:
			if bad++; bad <= problemLimit {
				p.addf("issue %s is missing from the tracker", id)
			}
		case g != w:
			if bad++; bad <= problemLimit {
				p.addf("issue %s differs:\n  dolt:    %s\n  tracker: %s", id, strings.ReplaceAll(w, "\x1f", "|"), strings.ReplaceAll(g, "\x1f", "|"))
			}
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			p.addf("issue %s is in the tracker but not in the dataset", id)
		}
	}

	// Links: the same set of (issue, target, type).
	wantDeps := map[string]bool{}
	for _, d := range ds.Deps {
		if _, ok := want[d.IssueID]; ok && d.DependsOn != "" {
			wantDeps[d.IssueID+"\x1f"+d.DependsOn+"\x1f"+d.Type] = true
		}
	}
	gotDeps := map[string]bool{}
	drows, err := pool.Query(ctx, "SELECT issue_id, depends_on, type FROM tracker.dependencies")
	if err != nil {
		return Diff{}, fmt.Errorf("migrate: verify dependencies: %w", err)
	}
	for drows.Next() {
		var a, b, c string
		if err := drows.Scan(&a, &b, &c); err != nil {
			drows.Close()
			return Diff{}, fmt.Errorf("migrate: verify dependencies: %w", err)
		}
		gotDeps[a+"\x1f"+b+"\x1f"+c] = true
	}
	drows.Close()
	diffSets(&p, "dependencies", wantDeps, gotDeps)

	// Counts of the rest.
	count := func(sql string) int {
		var n int
		if err := pool.QueryRow(ctx, sql).Scan(&n); err != nil {
			p.addf("count query failed: %v", err)
		}
		return n
	}
	commentsWant := 0
	for _, c := range ds.Comments {
		if _, ok := want[c.IssueID]; ok {
			commentsWant++
		}
	}
	check := func(name string, wantN, gotN int) {
		if wantN != gotN {
			p.addf("%s: %d in the tracker, %d expected", name, gotN, wantN)
		}
	}
	check("comments", commentsWant, count("SELECT count(*) FROM tracker.comments"))
	check("events", len(ds.Events), count("SELECT count(*) FROM tracker.events"))
	cfg, kv := splitConfig(ds.Config)
	check("config", len(cfg), count("SELECT count(*) FROM tracker.config"))
	check("kv", len(kv), count("SELECT count(*) FROM tracker.kv"))
	check("custom_types", len(ds.CustomTypes), count("SELECT count(*) FROM tracker.custom_types"))
	check("custom_statuses", len(ds.CustomStatuses), count("SELECT count(*) FROM tracker.custom_statuses"))
	check("child_counters", len(ds.ChildCounters), count("SELECT count(*) FROM tracker.child_counters"))
	return Diff{Problems: p.list}, nil
}

func diffSets(p *problems, what string, want, got map[string]bool) {
	n := 0
	for k := range want {
		if !got[k] {
			if n++; n <= problemLimit {
				p.addf("%s: %q is missing from the tracker", what, strings.ReplaceAll(k, "\x1f", "|"))
			}
		}
	}
	m := 0
	for k := range got {
		if !want[k] {
			if m++; m <= problemLimit {
				p.addf("%s: %q is in the tracker but not in the dataset", what, strings.ReplaceAll(k, "\x1f", "|"))
			}
		}
	}
	if n > problemLimit || m > problemLimit {
		p.addf("%s: %d missing and %d extra in all", what, n, m)
	}
}
