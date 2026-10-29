//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/migrate"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

// dottedChild is the dataset the import repairs a parent for: a dotted id with
// no dependency row, whose parent (the prefix before the dot) is in the same
// database.
func dottedChild() *migrate.Dataset {
	at := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	return &migrate.Dataset{Issues: []migrate.IssueRow{
		{ID: "gt-p", Title: "epic", Status: "open", Type: "epic", CreatedAt: at, UpdatedAt: at},
		{ID: "gt-p.1", Title: "child", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at},
	}}
}

// Integration tier: a real PostgreSQL 17 through migrate, ~1 s. The import
// needs the tracker, so this line cannot be driven from the unit tier.
func TestRunReportsTheSynthesizedParentChildLink(t *testing.T) {
	t.Parallel()
	pool := pgtest.New(t, "migrated")
	var out, errb bytes.Buffer
	ds := dottedChild()
	err := runWith(context.Background(), nil,
		env(map[string]string{"DOLT_DSN": "unused", "TRACKER_DSN": pool.Config().ConnString()}),
		&out, &errb, fakeRead(ds, nil))
	if err != nil {
		t.Fatalf("import failed: %v (%s)", err, errb.String())
	}
	want := "synthesized parent-child link: gt-p.1 -> gt-p\n"
	if !strings.Contains(out.String(), want) {
		t.Errorf("output %q lacks the line %q", out.String(), strings.TrimSpace(want))
	}
}
