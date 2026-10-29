package migrate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDepTargetTakesTheFirstNonEmptyColumn(t *testing.T) {
	t.Parallel() // unit tier: pure
	for name, tc := range map[string]struct{ issue, wisp, ext, want string }{
		"issue":      {"gt-a", "", "", "gt-a"},
		"wisp":       {"", "gt-wisp-b", "", "gt-wisp-b"},
		"external":   {"", "", "be-9yi", "be-9yi"},
		"all empty":  {"", "", "", ""},
		"issue wins": {"gt-a", "gt-wisp-b", "be-1", "gt-a"},
	} {
		if got := depTarget(tc.issue, tc.wisp, tc.ext); got != tc.want {
			t.Errorf("%s: depTarget = %q, want %q", name, got, tc.want)
		}
	}
}

func TestParentsComeFromParentChildLinks(t *testing.T) {
	t.Parallel()
	deps := []DepRow{
		{IssueID: "gt-k1", DependsOn: "gt-p", Type: "parent-child"},
		{IssueID: "gt-k2", DependsOn: "gt-p", Type: "parent-child"},
		{IssueID: "gt-x", DependsOn: "gt-y", Type: "blocks"},
		{IssueID: "gt-wisp-s", DependsOn: "gt-wisp-m", Type: "parent-child"},
	}
	got := parentsOf(deps)
	want := map[string]string{"gt-k1": "gt-p", "gt-k2": "gt-p", "gt-wisp-s": "gt-wisp-m"}
	if len(got) != len(want) {
		t.Fatalf("parentsOf = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("parent of %s = %q, want %q", k, got[k], v)
		}
	}
}

func TestSplitConfigSendsMemoriesToKV(t *testing.T) {
	t.Parallel()
	cfg, kv := splitConfig(map[string]string{
		"issue_prefix": "gt", "compact_batch_size": "50", "kv.archive.gt.feedback.x": "text", "kv.memory.y": "mem",
	})
	if len(cfg) != 2 || cfg["issue_prefix"] != "gt" {
		t.Errorf("config = %v, want issue_prefix and compact_batch_size", cfg)
	}
	if len(kv) != 2 || kv["kv.archive.gt.feedback.x"] != "text" || kv["kv.memory.y"] != "mem" {
		t.Errorf("kv = %v, want the two kv.* keys with the prefix kept", kv)
	}
}

func TestRigsOfIDs(t *testing.T) {
	t.Parallel()
	got := rigsOf([]string{"gt-a", "gt-wisp-b", "hq-c", "gt-a.1", "weird", ""})
	if !slices.Equal(got, []string{"gt", "hq"}) {
		t.Errorf("rigsOf = %v, want [gt hq] sorted and de-duplicated, ids with no prefix skipped", got)
	}
}

func TestLegacyMetadataKeepsOnlyWhatIsSet(t *testing.T) {
	t.Parallel()
	b := legacyMetadata(map[string]any{"row_lock": "abc", "content_hash": "h", "spec_id": "", "pinned": false, "compaction_level": int64(0), "work_type": "bug"})
	var got map[string]map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	legacy := got["legacy"]
	if len(legacy) != 3 || legacy["row_lock"] != "abc" || legacy["content_hash"] != "h" || legacy["work_type"] != "bug" {
		t.Errorf("legacy = %v, want exactly row_lock, content_hash and work_type", legacy)
	}
	if string(legacyMetadata(map[string]any{"spec_id": ""})) != "{}" {
		t.Errorf("nothing set must be {}, got %s", legacyMetadata(map[string]any{"spec_id": ""}))
	}
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestIssueFromRowMapsNamedColumnsAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	created := ts("2026-10-01T10:00:00Z")
	closed := ts("2026-10-02T10:00:00Z")
	row := map[string]any{
		"id": "gt-abc", "title": "t", "description": []byte("d"), "status": "closed", "priority": int64(1), "issue_type": "bug",
		"assignee": "me", "created_at": created, "updated_at": created, "closed_at": closed, "started_at": nil,
		"close_reason": "done", "payload": []byte(`{"a":1}`), "event_kind": "k", "hook_bead": "gt-h", "agent_state": "working",
		"row_lock": "lock-1", "content_hash": "h1", "is_blocked": int64(1), "ephemeral": int64(0), "spec_id": "", "metadata": nil,
	}
	got := issueFromRow(row, false)
	if got.ID != "gt-abc" || got.Title != "t" || got.Description != "d" || got.Status != "closed" || got.Priority != 1 || got.Type != "bug" || got.Assignee != "me" {
		t.Errorf("mapped fields wrong: %+v", got)
	}
	if got.ClosedAt == nil || !got.ClosedAt.Equal(closed) || got.StartedAt != nil {
		t.Errorf("times wrong: closed %v started %v", got.ClosedAt, got.StartedAt)
	}
	if got.Payload != `{"a":1}` || got.EventKind != "k" || got.HookBead != "gt-h" || got.AgentState != "working" {
		t.Errorf("payload/event/agent fields wrong: %+v", got)
	}
	if got.Legacy["row_lock"] != "lock-1" || got.Legacy["content_hash"] != "h1" {
		t.Errorf("Legacy = %v, want row_lock and content_hash kept", got.Legacy)
	}
	if _, derived := got.Legacy["is_blocked"]; derived {
		t.Error("is_blocked is derived state and must not be carried")
	}
	if _, empty := got.Legacy["spec_id"]; empty {
		t.Error("an empty column must not be carried")
	}
	if got.Wisp {
		t.Error("a row from the issues table is not a wisp")
	}
	if !issueFromRow(row, true).Wisp {
		t.Error("a row from the wisps table must be marked Wisp")
	}
}

func TestIssueFromRowTreatsNullsAsEmpty(t *testing.T) {
	t.Parallel()
	got := issueFromRow(map[string]any{"id": "gt-1", "title": "t", "status": "open", "issue_type": "task", "priority": nil, "description": nil, "created_at": ts("2026-10-01T10:00:00Z"), "updated_at": ts("2026-10-01T10:00:00Z")}, false)
	if got.Description != "" || got.Priority != 0 || got.Assignee != "" {
		t.Errorf("nulls should read as empty values: %+v", got)
	}
}

func TestDepFromRowResolvesTheTarget(t *testing.T) {
	t.Parallel()
	d := depFromRow(map[string]any{
		"id": "u-1", "issue_id": "gt-a", "type": "blocks", "depends_on_issue_id": nil, "depends_on_wisp_id": nil, "depends_on_external": []byte("be-9yi"),
		"created_at": ts("2026-10-01T10:00:00Z"), "created_by": "me", "metadata": nil, "thread_id": nil,
	})
	if d.DependsOn != "be-9yi" || d.Type != "blocks" || d.IssueID != "gt-a" || d.Metadata != "" || d.ThreadID != "" {
		t.Errorf("depFromRow = %+v, want the bare cross-rig id kept as the target", d)
	}
}

func TestEventAndCommentFromRow(t *testing.T) {
	t.Parallel()
	at := ts("2026-10-01T10:00:00Z")
	e := eventFromRow(map[string]any{"id": "e1", "issue_id": "gt-a", "event_type": "closed", "actor": nil, "old_value": nil, "new_value": []byte("x"), "comment": nil, "created_at": at})
	if e.Op != "closed" || e.Actor != "" || e.OldValue != nil || e.NewValue == nil || *e.NewValue != "x" || !e.CreatedAt.Equal(at) {
		t.Errorf("eventFromRow = %+v", e)
	}
	c := commentFromRow(map[string]any{"id": "c1", "issue_id": "gt-a", "author": "me", "text": []byte("hi"), "created_at": at})
	if c.Text != "hi" || c.Author != "me" || c.IssueID != "gt-a" {
		t.Errorf("commentFromRow = %+v", c)
	}
}

func TestOnlyKnownTablesAreRead(t *testing.T) {
	t.Parallel()
	for _, tbl := range []string{"issues", "wisp_events", "child_counters"} {
		if !knownTables[tbl] {
			t.Errorf("%s should be a known table", tbl)
		}
	}
	for _, tbl := range []string{"", "issues; DROP TABLE issues", "dolt_log", "users"} {
		if _, err := readTable(context.Background(), nil, tbl); err == nil {
			t.Errorf("readTable(%q) was not refused before touching the database", tbl)
		}
	}
}

func TestDatetimeReadsTimesTextBytesAndNothing(t *testing.T) {
	t.Parallel()
	want := time.Date(2026, 10, 19, 18, 2, 35, 0, time.UTC)
	frac := time.Date(2026, 10, 19, 18, 2, 35, 123456000, time.UTC)
	for name, tc := range map[string]struct {
		in   any
		want time.Time
	}{
		"a parsed time":      {want, want},
		"datetime bytes":     {[]byte("2026-10-19 18:02:35"), want},
		"datetime string":    {"2026-10-19 18:02:35", want},
		"fractional seconds": {[]byte("2026-10-19 18:02:35.123456"), frac},
		"rfc3339":            {"2026-10-19T18:02:35Z", want},
		"null":               {nil, time.Time{}},
		"empty":              {[]byte(""), time.Time{}},
		"not a time":         {[]byte("yesterday"), time.Time{}},
		"an integer":         {int64(5), time.Time{}},
	} {
		if got := datetime(tc.in); !got.Equal(tc.want) {
			t.Errorf("%s: datetime(%v) = %v, want %v", name, tc.in, got, tc.want)
		}
	}
	row := map[string]any{"created_at": []byte("2026-10-19 18:02:35"), "closed_at": nil}
	if !timeVal(row, "created_at").Equal(want) || timePtr(row, "closed_at") != nil || timePtr(row, "created_at") == nil {
		t.Error("timeVal and timePtr must read text datetimes and give nil for a NULL")
	}
}

func TestValidateRefusesTheZeroTimesOfAConnectionThatDidNotParseDatetimes(t *testing.T) {
	t.Parallel()
	ok := time.Date(2026, 10, 19, 18, 2, 35, 0, time.UTC)
	good := &Dataset{Issues: []IssueRow{{ID: "gt-a", CreatedAt: ok, UpdatedAt: ok}}, Comments: []CommentRow{{CreatedAt: ok}}, Events: []EventRow{{CreatedAt: ok}}}
	if err := Validate(good); err != nil {
		t.Errorf("Validate(good) = %v, want nil", err)
	}
	for name, ds := range map[string]*Dataset{
		"issue with no created_at": {Issues: []IssueRow{{ID: "gt-z", UpdatedAt: ok}}},
		"issue with no updated_at": {Issues: []IssueRow{{ID: "gt-z", CreatedAt: ok}}},
		"comment without a time":   {Comments: []CommentRow{{IssueID: "gt-a"}}},
		"event without a time":     {Events: []EventRow{{IssueID: "gt-a"}}},
	} {
		err := Validate(ds)
		if err == nil || !strings.Contains(err.Error(), "parse datetimes") {
			t.Errorf("%s: Validate = %v, want an error that names the likely cause", name, err)
		}
	}
	if err := Validate(&Dataset{Issues: []IssueRow{{ID: "gt-first"}, {ID: "gt-second"}}}); err == nil || !strings.Contains(err.Error(), "gt-first") || !strings.Contains(err.Error(), "2 issues") {
		t.Errorf("Validate = %v, want the count and the first issue", err)
	}
}

func TestDottedParentIsTheIdBeforeTheLastDot(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{"gt-p.1": "gt-p", "gt-p.1.2": "gt-p.1", "be-1r0.16": "be-1r0"} {
		if got, ok := dottedParent(id); !ok || got != want {
			t.Errorf("dottedParent(%q) = %q, %v; want %q", id, got, ok, want)
		}
	}
	for _, id := range []string{"gt-p", ".1", "gt-p.", ""} {
		if got, ok := dottedParent(id); ok {
			t.Errorf("dottedParent(%q) = %q; want no parent", id, got)
		}
	}
}

func TestRepairedDepsLinksADottedChildWithNoParentRowToItsPrefix(t *testing.T) {
	t.Parallel()
	ds := &Dataset{Issues: []IssueRow{{ID: "be-1r0"}, {ID: "be-1r0.16"}}}
	deps, synth := repairedDeps(ds)
	want := DepRow{IssueID: "be-1r0.16", DependsOn: "be-1r0", Type: "parent-child"}
	if len(synth) != 1 || synth[0] != want {
		t.Fatalf("synthesized = %+v, want exactly %+v", synth, want)
	}
	if len(deps) != 1 || deps[0] != want {
		t.Errorf("deps = %+v, want the synthesized row alone", deps)
	}
	if len(ds.Deps) != 0 {
		t.Errorf("the dataset's own deps changed: %+v", ds.Deps)
	}
}

func TestRepairedDepsSynthesizesOnlyWhereTheRulePlainlyApplies(t *testing.T) {
	t.Parallel()
	gtP := IssueRow{ID: "gt-p"}
	child := IssueRow{ID: "gt-p.1"}
	for name, tc := range map[string]struct {
		issues []IssueRow
		deps   []DepRow
		want   int
	}{
		"a parent-child row already holds the link": {
			issues: []IssueRow{gtP, child},
			deps:   []DepRow{{IssueID: "gt-p.1", DependsOn: "gt-p", Type: "parent-child"}},
		},
		"a parent-child row to another parent is left alone": {
			issues: []IssueRow{gtP, {ID: "gt-q"}, child},
			deps:   []DepRow{{IssueID: "gt-p.1", DependsOn: "gt-q", Type: "parent-child"}},
		},
		"a row of another type to the prefix is never replaced": {
			issues: []IssueRow{gtP, child},
			deps:   []DepRow{{IssueID: "gt-p.1", DependsOn: "gt-p", Type: "blocks"}},
		},
		"a row of another type to another target keeps its own rows": {
			issues: []IssueRow{gtP, child, {ID: "gt-q"}},
			deps:   []DepRow{{IssueID: "gt-p.1", DependsOn: "gt-q", Type: "blocks"}},
		},
		"a parent-child row with no target is no link": {
			issues: []IssueRow{gtP, child},
			deps:   []DepRow{{IssueID: "gt-p.1", DependsOn: "", Type: "parent-child"}},
			want:   1,
		},
		"a prefix that is not an issue of the dataset gets nothing": {
			issues: []IssueRow{child},
		},
		"an id with no dot gets nothing": {
			issues: []IssueRow{gtP},
		},
		"a wisp child with a wisp parent is repaired": {
			issues: []IssueRow{{ID: "gt-wisp-m", Wisp: true}, {ID: "gt-wisp-m.1", Wisp: true}},
			want:   1,
		},
		"a wisp child whose parent is not a wisp is left alone": {
			issues: []IssueRow{gtP, {ID: "gt-p.1", Wisp: true}},
		},
		"a non-wisp child whose parent is a wisp is repaired": {
			issues: []IssueRow{{ID: "gt-wisp-m", Wisp: true}, {ID: "gt-wisp-m.1"}},
			want:   1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps, synth := repairedDeps(&Dataset{Issues: tc.issues, Deps: tc.deps})
			if len(synth) != tc.want {
				t.Errorf("synthesized %d rows %+v, want %d", len(synth), synth, tc.want)
			}
			if len(deps) != len(tc.deps)+len(synth) {
				t.Errorf("deps = %d rows, want the dataset's %d plus %d synthesized", len(deps), len(tc.deps), len(synth))
			}
		})
	}
}
