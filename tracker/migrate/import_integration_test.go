//go:build integration

package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

func at(min int) time.Time      { return time.Date(2026, 10, 1, 10, min, 0, 0, time.UTC) }
func sp(s string) *string       { return &s }
func tp(t time.Time) *time.Time { return &t }

// sample is a small rig with the awkward cases in it.
func sample() *Dataset {
	return &Dataset{
		Issues: []IssueRow{
			{ID: "gt-a", Title: "open task", Status: "open", Type: "task", Priority: 2, CreatedAt: at(0), UpdatedAt: at(1), Legacy: map[string]any{"row_lock": "L1", "content_hash": "H1"}},
			{ID: "gt-b", Title: "closed bug", Status: "closed", Type: "bug", Priority: 1, CloseReason: "fixed", CreatedAt: at(0), UpdatedAt: at(5), ClosedAt: tp(at(5)), Assignee: "me"},
			{ID: "gt-p", Title: "epic", Status: "open", Type: "epic", Priority: 0, CreatedAt: at(0), UpdatedAt: at(0)},
			{ID: "gt-p.1", Title: "child", Status: "open", Type: "task", CreatedAt: at(2), UpdatedAt: at(2)},
			{ID: "gt-wisp-m", Title: "molecule", Status: "open", Type: "molecule", Wisp: true, CreatedAt: at(3), UpdatedAt: at(3), Payload: `{"k": [1, 2]}`, EventKind: "k"},
			{ID: "gt-wisp-s", Title: "step", Status: "open", Type: "task", Wisp: true, CreatedAt: at(3), UpdatedAt: at(3), DeferUntil: tp(at(30))},
			{ID: "hq-c", Title: "town bead", Status: "open", Type: "message", CreatedAt: at(4), UpdatedAt: at(4)},
		},
		Deps: []DepRow{
			{ID: "d1", IssueID: "gt-a", DependsOn: "gt-b", Type: "blocks", CreatedAt: at(1)},
			{ID: "d2", IssueID: "gt-a", DependsOn: "be-9yi", Type: "blocks", CreatedAt: at(1)}, // another rig, bare id
			{ID: "d3", IssueID: "gt-p.1", DependsOn: "gt-p", Type: "parent-child", CreatedAt: at(2)},
			{ID: "d4", IssueID: "gt-wisp-s", DependsOn: "gt-wisp-m", Type: "parent-child", CreatedAt: at(3)},
			{ID: "d5", IssueID: "gt-a", DependsOn: "gt-b", Type: "blocks", CreatedAt: at(1)},     // duplicate of d1
			{ID: "d6", IssueID: "gt-ghost", DependsOn: "gt-a", Type: "blocks", CreatedAt: at(1)}, // issue not present
			{ID: "d7", IssueID: "gt-a", DependsOn: "", Type: "blocks", CreatedAt: at(1)},         // no target
		},
		Labels:   []LabelRow{{"gt-a", "x"}, {"gt-a", "y"}, {"gt-a", "x"}, {"gt-wisp-s", "w"}, {"gt-ghost", "z"}, {"gt-a", ""}},
		Comments: []CommentRow{{ID: "c1", IssueID: "gt-a", Author: "me", Text: "one", CreatedAt: at(1)}, {ID: "c2", IssueID: "gt-ghost", Author: "me", Text: "orphan", CreatedAt: at(1)}},
		Events: []EventRow{
			{ID: "x3", IssueID: "gt-a", Op: "closed", Actor: "me", CreatedAt: at(9)},
			{ID: "z1", IssueID: "gt-a", Op: "created", Actor: "me", NewValue: sp("open task"), CreatedAt: at(0)},
			{ID: "y2", IssueID: "gt-wisp-s", Op: "created", CreatedAt: at(3)}, // from wisp_events, between the others
			{ID: "w4", IssueID: "gt-deleted", Op: "deleted", CreatedAt: at(10)},
		},
		Config:         map[string]string{"issue_prefix": "gt", "kv.memory.x": "remember this"},
		CustomTypes:    []string{"agent", "molecule"},
		CustomStatuses: []CustomStatus{{Name: "staged_ready"}},
		ChildCounters:  map[string]int{"gt-p": 3},
	}
}

func newPool(t *testing.T) *pgxpool.Pool { return pgtest.New(t, "migrated") }

// Integration tier: real postgres, one clone per case.
func TestImportThenVerifyIsEqualAndReportsWhatItSkipped(t *testing.T) {
	t.Parallel()
	pool, ds := newPool(t), sample()
	rep, err := Import(context.Background(), pool, ds, Options{RigNames: map[string]string{"gt": "gastown"}})
	if err != nil {
		t.Fatal(err)
	}
	wantWritten := map[string]int{"issues": 7, "issues.ephemeral": 2, "dependencies": 4, "labels": 3, "comments": 1, "events": 4, "config": 1, "kv": 1, "custom_types": 2, "custom_statuses": 1, "child_counters": 1}
	for k, v := range wantWritten {
		if rep.Written[k] != v {
			t.Errorf("Written[%q] = %d, want %d", k, rep.Written[k], v)
		}
	}
	wantSkipped := map[string]int{
		"dependencies: duplicate link": 1, "dependencies: issue not in the dataset": 1, "dependencies: no target": 1,
		"labels: duplicate": 1, "labels: issue not in the dataset": 1, "labels: empty label": 1, "comments: issue not in the dataset": 1,
	}
	for k, v := range wantSkipped {
		if rep.Skipped[k] != v {
			t.Errorf("Skipped[%q] = %d, want %d", k, rep.Skipped[k], v)
		}
	}
	diff, err := Verify(context.Background(), pool, ds)
	if err != nil || !diff.Equal() {
		t.Fatalf("Verify = %v, %v; want equal", diff.Problems, err)
	}
}

func TestImportMapsParentsWispsMetadataRigsAndEventOrder(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	ctx := context.Background()
	if _, err := Import(ctx, pool, sample(), Options{RigNames: map[string]string{"gt": "gastown"}}); err != nil {
		t.Fatal(err)
	}
	var parent, wispParent string
	var eph bool
	var meta, payload string
	if err := pool.QueryRow(ctx, "SELECT coalesce(parent_id,'') FROM tracker.issues WHERE id='gt-p.1'").Scan(&parent); err != nil || parent != "gt-p" {
		t.Errorf("parent of gt-p.1 = %q, %v; want gt-p from the parent-child link", parent, err)
	}
	if err := pool.QueryRow(ctx, "SELECT coalesce(parent_id,''), ephemeral FROM tracker.issues WHERE id='gt-wisp-s'").Scan(&wispParent, &eph); err != nil || wispParent != "gt-wisp-m" || !eph {
		t.Errorf("wisp step = parent %q ephemeral %v, %v; want gt-wisp-m and true", wispParent, eph, err)
	}
	if err := pool.QueryRow(ctx, "SELECT metadata::text FROM tracker.issues WHERE id='gt-a'").Scan(&meta); err != nil || !strings.Contains(meta, "row_lock") || !strings.Contains(meta, "H1") {
		t.Errorf("metadata of gt-a = %s, %v; want the legacy row_lock and content_hash kept", meta, err)
	}
	if err := pool.QueryRow(ctx, "SELECT payload::text FROM tracker.issues WHERE id='gt-wisp-m'").Scan(&payload); err != nil || !strings.Contains(payload, "[1, 2]") {
		t.Errorf("payload = %s, %v; want the JSON kept", payload, err)
	}
	var name string
	if err := pool.QueryRow(ctx, "SELECT name FROM tracker.rigs WHERE prefix='gt'").Scan(&name); err != nil || name != "gastown" {
		t.Errorf("rig gt name = %q, %v; want the display name given", name, err)
	}
	var hq string
	if err := pool.QueryRow(ctx, "SELECT name FROM tracker.rigs WHERE prefix='hq'").Scan(&hq); err != nil || hq != "hq" {
		t.Errorf("rig hq name = %q, %v; want its prefix when no name was given", hq, err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tracker.rigs").Scan(&n); err != nil || n != 2 {
		t.Errorf("rigs = %d, %v; want only gt and hq (a bare cross-rig id does not create a rig)", n, err)
	}
	rows, err := pool.Query(ctx, "SELECT issue_id, op FROM tracker.events ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, op string
		if err := rows.Scan(&id, &op); err != nil {
			t.Fatal(err)
		}
		got = append(got, id+":"+op)
	}
	want := []string{"gt-a:created", "gt-wisp-s:created", "gt-a:closed", "gt-deleted:deleted"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("events by seq = %v, want chronological %v (the event of a gone issue is kept)", got, want)
	}
	var k, v string
	if err := pool.QueryRow(ctx, "SELECT key, value FROM tracker.kv").Scan(&k, &v); err != nil || k != "kv.memory.x" {
		t.Errorf("kv = %q, %v; want the memory moved out of config", k, err)
	}
	var last int
	if err := pool.QueryRow(ctx, "SELECT last_child FROM tracker.child_counters WHERE parent_id='gt-p'").Scan(&last); err != nil || last != 3 {
		t.Errorf("child counter = %d, %v; want 3", last, err)
	}
}

func TestASecondImportIsRefusedAndChangesNothing(t *testing.T) {
	t.Parallel()
	pool, ds := newPool(t), sample()
	ctx := context.Background()
	if _, err := Import(ctx, pool, ds, Options{}); err != nil {
		t.Fatal(err)
	}
	_, err := Import(ctx, pool, ds, Options{})
	if !errors.Is(err, api.ErrConflict) {
		t.Fatalf("second Import = %v, want api.ErrConflict", err)
	}
	if diff, _ := Verify(ctx, pool, ds); !diff.Equal() {
		t.Errorf("after the refused second import: %v", diff.Problems)
	}
}

func TestAFailingRowRollsBackTheWholeImport(t *testing.T) {
	t.Parallel()
	pool := newPool(t)
	ds := sample()
	// An id with no rig prefix is refused by the schema, after the good rows
	// above it were already copied inside the transaction.
	ds.Issues = append(ds.Issues, IssueRow{ID: "weird", Title: "t", Status: "open", Type: "task", CreatedAt: at(0), UpdatedAt: at(0)})
	if _, err := Import(context.Background(), pool, ds, Options{}); err == nil {
		t.Fatal("an import with an unusable row succeeded")
	}
	for _, tbl := range []string{"issues", "rigs", "dependencies", "labels", "events", "kv", "config", "custom_types"} {
		var n int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM tracker."+tbl).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s has %d rows after a failed import, want 0 (all or nothing)", tbl, n)
		}
	}
}

func TestVerifyNamesWhatChangedAfterTheImport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, tc := range map[string]struct{ sql, want string }{
		"a title":         {"UPDATE tracker.issues SET title = 'tampered' WHERE id = 'gt-a'", "gt-a"},
		"a status":        {"UPDATE tracker.issues SET status = 'closed' WHERE id = 'gt-a'", "gt-a"},
		"a timestamp":     {"UPDATE tracker.issues SET updated_at = updated_at + interval '1 microsecond' WHERE id = 'gt-b'", "gt-b"},
		"a label":         {"DELETE FROM tracker.labels WHERE issue_id = 'gt-a' AND label = 'y'", "gt-a"},
		"a parent":        {"UPDATE tracker.issues SET parent_id = NULL WHERE id = 'gt-p.1'", "gt-p.1"},
		"a missing issue": {"DELETE FROM tracker.issues WHERE id = 'hq-c'", "hq-c"},
		"a link":          {"DELETE FROM tracker.dependencies WHERE depends_on = 'be-9yi'", "dependencies"},
		"an event":        {"DELETE FROM tracker.events WHERE op = 'deleted'", "events"},
		"a comment":       {"DELETE FROM tracker.comments", "comments"},
		"a memory":        {"DELETE FROM tracker.kv", "kv"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool, ds := newPool(t), sample()
			if _, err := Import(ctx, pool, ds, Options{}); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			diff, err := Verify(ctx, pool, ds)
			if err != nil {
				t.Fatal(err)
			}
			if diff.Equal() || !strings.Contains(strings.Join(diff.Problems, "\n"), tc.want) {
				t.Errorf("Verify after %s = %v, want a problem naming %q", name, diff.Problems, tc.want)
			}
		})
	}
}

func TestVerifyFailsLoudlyWhenAQueryFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, ds := newPool(t), sample()
	if _, err := Import(ctx, pool, ds, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "DROP TABLE tracker.custom_statuses"); err != nil {
		t.Fatal(err)
	}
	ds.CustomStatuses = nil // a count of 0 from the error would equal this expected 0
	if diff, err := Verify(ctx, pool, ds); err == nil {
		t.Errorf("Verify = %v, nil; a failed count query must be an error, not a zero", diff.Problems)
	}
}

func TestVerifyCountsEveryProblemEvenPastTheListLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := newPool(t)
	ds := &Dataset{}
	for i := 0; i < 3*problemLimit; i++ {
		ds.Issues = append(ds.Issues, IssueRow{ID: fmt.Sprintf("gt-%03d", i), Title: "t", Status: "open", Type: "task", CreatedAt: at(0), UpdatedAt: at(0)})
	}
	if _, err := Import(ctx, pool, ds, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE tracker.issues SET title = title || '!'"); err != nil {
		t.Fatal(err)
	}
	diff, err := Verify(ctx, pool, ds)
	if err != nil || diff.Total != len(ds.Issues) {
		t.Fatalf("Verify total = %d, %v; want %d (every changed issue counted)", diff.Total, err, len(ds.Issues))
	}
	if len(diff.Problems) > problemLimit+1 {
		t.Errorf("listed %d problems, want at most %d plus one summary line", len(diff.Problems), problemLimit)
	}
	if !strings.Contains(diff.Problems[len(diff.Problems)-1], "in all") {
		t.Errorf("last line %q should say how many there are in all", diff.Problems[len(diff.Problems)-1])
	}
}

func TestImportRefusesAndRollsBackWhenItsCopyDoesNotMatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, ds := newPool(t), sample()
	// A trigger that alters titles as they are copied makes the copy differ
	// from the dataset: Import must notice before it commits.
	for _, q := range []string{
		"CREATE FUNCTION tracker.mangle() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.title := NEW.title || '?'; RETURN NEW; END $$",
		"CREATE TRIGGER mangle BEFORE INSERT ON tracker.issues FOR EACH ROW EXECUTE FUNCTION tracker.mangle()",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_, err := Import(ctx, pool, ds, Options{})
	var ve *VerifyError
	if !errors.As(err, &ve) || ve.Diff.Total == 0 {
		t.Fatalf("Import = %v, want a *VerifyError listing the differences", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM tracker.issues").Scan(&n); err != nil || n != 0 {
		t.Errorf("issues after a mismatching import = %d, %v; want 0, nothing committed", n, err)
	}
}
