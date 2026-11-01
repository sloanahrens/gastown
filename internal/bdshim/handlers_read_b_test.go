package bdshim

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

// The board handlers' tests answer each verb through Main, against the fixture
// the goldens were recorded from, and compare what bd printed with what the
// handler prints after the same normalization the differential test applies.
// TestDifferentialGoldens replays every golden; these tests name each verb's
// rows, so a verb whose handler is removed fails here by name.

// TestHandlersReady answers every ready golden row.
func TestHandlersReady(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "ready")
}

// TestHandlersBlocked answers every blocked golden row.
func TestHandlersBlocked(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "blocked")
}

// TestHandlersQuery answers every query golden row.
func TestHandlersQuery(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "query")
}

// TestHandlersSearch answers every search golden row.
func TestHandlersSearch(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "search")
}

// TestHandlersCount answers every count golden row.
func TestHandlersCount(t *testing.T) {
	t.Parallel()
	testHandlerRows(t, "count")
}

// TestReadBHandlersRefuseAnUnknownFlag: a flag a golden row does not cover is
// refused by name, never ignored, so a caller cannot read a filtered answer as
// an unfiltered one.
func TestReadBHandlersRefuseAnUnknownFlag(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		flag string
	}{
		{name: "ready refuses --label", argv: []string{"ready", "--json", "--label", "gt:agent"}, flag: "--label"},
		{name: "blocked refuses --parent", argv: []string{"blocked", "--json", "--parent", "fx-parent"}, flag: "--parent"},
		{name: "query refuses --offset", argv: []string{"query", "--json", "--offset", "2", "status=open"}, flag: "--offset"},
		{name: "search refuses --query", argv: []string{"search", "--json", "--query", "fixture"}, flag: "--query"},
		{name: "count refuses --label", argv: []string{"count", "--label", "gt:agent"}, flag: "--label"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.flag) {
				t.Errorf("%v: stderr = %q, want it to name %s", tc.argv, stderr.String(), tc.flag)
			}
		})
	}
}

// TestReadBHandlersRefuseAnAnswerTheyDoNotPrint: a read the goldens record as
// JSON is refused without --json rather than printed in a shape bd did not
// record.
func TestReadBHandlersRefuseAnAnswerTheyDoNotPrint(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "ready", argv: []string{"ready"}, want: "ready without --json"},
		{name: "blocked", argv: []string{"blocked"}, want: "blocked without --json"},
		{name: "query", argv: []string{"query", "status=open"}, want: "query without --json"},
		{name: "search", argv: []string{"search", "fixture"}, want: "search without --json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestReadBHandlersRefuseOperandsTheyDoNotRead: each verb's usage names its
// operands, and one the handler cannot answer is refused rather than ignored.
func TestReadBHandlersRefuseOperandsTheyDoNotRead(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "ready with an operand", argv: []string{"ready", "--json", "fx-1"}, want: "ready takes no operands"},
		{name: "blocked with an operand", argv: []string{"blocked", "--json", "fx-1"}, want: "blocked takes no operands"},
		{name: "query with no expression", argv: []string{"query", "--json"}, want: "query needs one expression"},
		{name: "query with two expressions", argv: []string{"query", "--json", "status=open", "status=closed"}, want: "query needs one expression"},
		{name: "search with no word", argv: []string{"search", "--json", "--limit", "10"}, want: "search needs a word"},
		{name: "count with an operand", argv: []string{"count", "--status", "open", "fx-1"}, want: "count takes no operands"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestHandlersRefuseACountTheyCannotBuild: the two answers bd count prints are
// named by one flag each, and a combination or a call naming neither is refused
// rather than given one of their meanings.
func TestHandlersRefuseACountTheyCannotBuild(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		argv []string
		want string
	}{
		{name: "by status together with a status", argv: []string{"count", "--by-status", "--status", "open"}, want: "name one of them"},
		{name: "neither flag", argv: []string{"count"}, want: "count without --status or --by-status"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, tc.argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", tc.argv, code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("%v: stderr = %q, want it to hold %q", tc.argv, stderr.String(), tc.want)
			}
		})
	}
}

// TestReadBHandlersRefuseALimitTheyCannotRead: a limit that is not a count is
// refused, never read as no limit at all.
func TestReadBHandlersRefuseALimitTheyCannotRead(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	for name, argv := range map[string][]string{
		"ready":  {"ready", "--json", "--limit", "many"},
		"query":  {"query", "--json", "--limit=-1", "status=open"},
		"search": {"search", "--json", "--limit", "all", "fixture"},
	} {
		t.Run(name, func(t *testing.T) {
			env, _, stderr := fixtureEnv(t, town, fixture, argv)
			if code := Main(env); code != 1 {
				t.Errorf("%v: exit code is %d, want 1", argv, code)
			}
			if !strings.Contains(stderr.String(), "--limit") {
				t.Errorf("%v: stderr = %q, want it to name --limit", argv, stderr.String())
			}
		})
	}
}

// TestHandlersRefuseAQueryFormTheTrackerDoesNotAnswer: a query expression
// outside the dialect gt sends comes back as the service's invalid error and
// bd's exit code, and writes no rows, so a caller never reads a wider answer
// than the expression asked for.
func TestHandlersRefuseAQueryFormTheTrackerDoesNotAnswer(t *testing.T) {
	t.Parallel()
	fixture, _ := readFixture(t)
	town := newTown(t, routeFixture)
	tests := []struct {
		name string
		expr string
		want string
	}{
		{name: "an empty expression", expr: ``, want: "empty"},
		{name: "a key outside the dialect", expr: `title="fixture"`, want: "title"},
		{name: "an operator outside the dialect", expr: `status>open`, want: "is not an operator"},
		{name: "a character outside the dialect", expr: `status=$open`, want: "not part of a predicate"},
		{name: "a group over another key", expr: `(label="a" OR label="b")`, want: "only status alternatives"},
		{name: "an empty group", expr: `()`, want: "status predicates"},
		{name: "an unclosed group", expr: `(status="open"`, want: "not closed"},
		{name: "a status named twice", expr: `status="open" AND status="closed"`, want: "more than once"},
		{name: "a status after a group", expr: `(status="open" OR status="closed") AND status="hooked"`, want: "more than once"},
		{name: "a group after a status", expr: `status="open" AND (status="closed" OR status="hooked")`, want: "more than once"},
		{name: "a status with a range in a group", expr: `(status>=open)`, want: "status takes ="},
		{name: "a group status with no value", expr: `(status=)`, want: "status needs a value"},
		{name: "a predicate with no value", expr: `status=`, want: "needs a value"},
		{name: "a quoted value with a bad escape", expr: `status="\q"`, want: "not a quoted string"},
		{name: "an assignee named twice", expr: `assignee="a" AND assignee="b"`, want: "more than once"},
		{name: "an ephemeral that is not a boolean", expr: `ephemeral=maybe`, want: "not a boolean"},
		{name: "an ephemeral with a range", expr: `ephemeral>=true`, want: "ephemeral does not take >="},
		{name: "an assignee with a range", expr: `assignee>=sloan`, want: "assignee does not take >="},
		{name: "a label with a range", expr: `label>=gt:agent`, want: "label does not take >="},
		{name: "a parent with a range", expr: `parent>=fx-parent`, want: "parent does not take >="},
		{name: "a status with a range", expr: `status>=open`, want: "status does not take >="},
		{name: "a priority out of range", expr: `priority=9`, want: "is not a priority"},
		{name: "a priority with a range", expr: `priority>=2`, want: "priority does not take >="},
		{name: "created with equality", expr: `created="2026-01-01T00:00:00Z"`, want: "created takes >="},
		{name: "created that is not a time", expr: `created>=yesterday`, want: "not an RFC 3339 time"},
		{name: "an unquoted value with a quote", expr: `status="open`, want: "not closed"},
		{name: "a bare word", expr: `fixture`, want: "needs = or >="},
		{name: "a quoted word alone", expr: `"open"`, want: "expected a predicate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, stdout, stderr := fixtureEnv(t, town, fixture, []string{"query", "--json", tc.expr})
			if code := Main(env); code != 1 {
				t.Errorf("query %q: exit code is %d, want 1", tc.expr, code)
			}
			if !strings.Contains(stderr.String(), "invalid") || !strings.Contains(stderr.String(), tc.want) {
				t.Errorf("query %q: stderr = %q, want the service's invalid error holding %q", tc.expr, stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("query %q: wrote %q, want no rows", tc.expr, stdout.String())
			}
		})
	}
}

// TestParseQueryAcceptsTheFormsGTAggregatesSend pins the dialect's supported
// forms (docs/research/tracker/R4-aggregates.md §4.1) to the store filter they
// name, so a form the tracker can answer is not refused and narrowed.
func TestParseQueryAcceptsTheFormsGTAggregatesSend(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		expr      string
		all       bool
		want      beads.ListOptions
		ephemeral string
	}{
		{
			name:      "a status",
			expr:      `status=open`,
			want:      beads.ListOptions{Status: "open"},
			ephemeral: "both",
		},
		{
			name:      "durable issues in two statuses",
			expr:      `ephemeral=false AND (status="hooked" OR status="in_progress")`,
			want:      beads.ListOptions{Status: "hooked,in_progress"},
			ephemeral: "false",
		},
		{
			name:      "one assignee, both tables",
			expr:      `assignee="gastown/polecats/capable" AND (status="hooked" OR status="in_progress")`,
			want:      beads.ListOptions{Assignee: "gastown/polecats/capable", Status: "hooked,in_progress"},
			ephemeral: "both",
		},
		{
			name:      "wisps by label, a colon in the value",
			expr:      `ephemeral=true AND label="gt:merge-request"`,
			want:      beads.ListOptions{Labels: []string{"gt:merge-request"}, Status: "open"},
			ephemeral: "true",
		},
		{
			name:      "every supported predicate in one expression",
			expr:      `label="fixture-label" AND created>=2026-01-01T00:00:00Z AND priority=2 AND parent="fx-parent"`,
			want:      beads.ListOptions{Labels: []string{"fixture-label"}, Priority: beads.PriorityP2, Parent: "fx-parent", CreatedAfter: created, Status: "open"},
			ephemeral: "both",
		},
		{
			name:      "a quoted value with a space",
			expr:      `assignee="a person"`,
			want:      beads.ListOptions{Assignee: "a person", Status: "open"},
			ephemeral: "both",
		},
		{
			name:      "--all defaults to every status",
			expr:      `assignee="sloan"`,
			all:       true,
			want:      beads.ListOptions{Assignee: "sloan", Status: "all"},
			ephemeral: "both",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseQuery(tc.expr, tc.all)
			if err != nil {
				t.Fatalf("parseQuery(%q) = %v, want the form accepted", tc.expr, err)
			}
			if got.ephemeral != tc.ephemeral {
				t.Errorf("parseQuery(%q).ephemeral = %q, want %q", tc.expr, got.ephemeral, tc.ephemeral)
			}
			if !reflect.DeepEqual(got.opts, tc.want) {
				t.Errorf("parseQuery(%q).opts = %+v, want %+v", tc.expr, got.opts, tc.want)
			}
		})
	}
}

// TestParseQueryReportsTheServiceInvalidError: an unsupported form is the
// service's invalid error, which is what the handler prints and the exit code
// bd answers with.
func TestParseQueryReportsTheServiceInvalidError(t *testing.T) {
	t.Parallel()
	_, err := parseQuery(`notes~"x"`, false)
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != api.KindInvalid {
		t.Fatalf("parseQuery(unsupported) = %v, want an api invalid error", err)
	}
}

// TestQueryFilterReadsBothTablesWhenTheExpressionNamesNeither: bd answers an
// expression with no ephemeral predicate from both tables, so the read is two
// lists merged into bd's row order.
func TestQueryFilterReadsBothTablesWhenTheExpressionNamesNeither(t *testing.T) {
	t.Parallel()
	st := stubReadStore{list: func(opts beads.ListOptions) ([]*beads.Issue, error) {
		if opts.Ephemeral {
			return []*beads.Issue{{ID: "wisp-1", Priority: 0, CreatedAt: "2026-01-01T00:00:00Z"}}, nil
		}
		return []*beads.Issue{
			{ID: "fx-2", Priority: 2, CreatedAt: "2026-01-01T00:00:00Z"},
			{ID: "fx-1", Priority: 2, CreatedAt: "2026-01-01T00:00:00Z"},
		}, nil
	}}
	got, err := queryFilter{opts: beads.ListOptions{Status: "open"}, ephemeral: "both"}.read(st)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	ids := make([]string, len(got))
	for i, is := range got {
		ids[i] = is.ID
	}
	if want := []string{"wisp-1", "fx-1", "fx-2"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("read = %v, want bd's row order %v", ids, want)
	}
}

// TestQueryFilterReportsAReadItCannotMake: a table read that fails is the
// caller's error, never the other table's answer read as both.
func TestQueryFilterReportsAReadItCannotMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store stubReadStore
		want  string
	}{
		{
			name: "the durable table",
			store: stubReadStore{list: func(opts beads.ListOptions) ([]*beads.Issue, error) {
				if opts.Ephemeral {
					return nil, nil
				}
				return nil, errors.New("durable refused")
			}},
			want: "durable refused",
		},
		{
			name: "the wisp table",
			store: stubReadStore{list: func(opts beads.ListOptions) ([]*beads.Issue, error) {
				if opts.Ephemeral {
					return nil, errors.New("wisps refused")
				}
				return nil, nil
			}},
			want: "wisps refused",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (queryFilter{ephemeral: "both"}).read(tc.store)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("read = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}

// TestQueryFilterReadsOneTableWhenTheExpressionNamesOne: an expression that
// names ephemeral asks that table only.
func TestQueryFilterReadsOneTableWhenTheExpressionNamesOne(t *testing.T) {
	t.Parallel()
	var asked []bool
	st := stubReadStore{list: func(opts beads.ListOptions) ([]*beads.Issue, error) {
		asked = append(asked, opts.Ephemeral)
		return nil, nil
	}}
	if _, err := (queryFilter{opts: beads.ListOptions{Status: "open"}, ephemeral: "true"}).read(st); err != nil {
		t.Fatalf("read: %v", err)
	}
	if want := []bool{true}; !reflect.DeepEqual(asked, want) {
		t.Errorf("the read asked for Ephemeral %v, want %v", asked, want)
	}
}

// TestBlockedIssuesAppliesTheStoreBlockingRules pins the two rules the store's
// blocked query builds its SQL from: a blocking link to an unresolved target,
// and a child of a blocked issue.
func TestBlockedIssuesAppliesTheStoreBlockingRules(t *testing.T) {
	t.Parallel()
	listed := []*beads.Issue{
		{ID: "held", Status: "open"},
		{ID: "free", Status: "open"},
		{ID: "child", Status: "open"},
		{ID: "closed-target", Status: "open"},
		{ID: "pinned-target", Status: "open"},
		{ID: "conditional", Status: "open"},
	}
	full := map[string]*beads.Issue{
		"held":          {ID: "held", Dependencies: []beads.IssueDep{{ID: "free", DependencyType: "blocks", Status: "open"}}},
		"free":          {ID: "free"},
		"child":         {ID: "child", Parent: "held"},
		"closed-target": {ID: "closed-target", Dependencies: []beads.IssueDep{{ID: "free", DependencyType: "blocks", Status: "closed"}}},
		"pinned-target": {ID: "pinned-target", Dependencies: []beads.IssueDep{{ID: "free", DependencyType: "blocks", Status: "pinned"}}},
		"conditional":   {ID: "conditional", Dependencies: []beads.IssueDep{{ID: "free", DependencyType: "conditional-blocks", Status: "open"}}},
	}
	got := blockedIssues(listed, full)
	want := map[string]bool{"held": true, "child": true, "conditional": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blockedIssues = %v, want %v", got, want)
	}
}

// TestReadyRowsKeepsOpenUnblockedWork: the board keeps open, unblocked work,
// drops the kinds of bead bd's ready leaves out, and honours --unassigned.
func TestReadyRowsKeepsOpenUnblockedWork(t *testing.T) {
	t.Parallel()
	b := board{
		listed: []*beads.Issue{
			{ID: "open", Status: "open"},
			{ID: "held", Status: "open"},
			{ID: "closed", Status: "closed"},
			{ID: "merge", Status: "open", Type: "merge-request"},
			{ID: "assigned", Status: "open", Assignee: "sloan"},
		},
		blocked: map[string]bool{"held": true},
	}
	ids := func(issues []*beads.Issue) []string {
		out := make([]string, len(issues))
		for i, is := range issues {
			out[i] = is.ID
		}
		return out
	}
	if got, want := ids(readyRows(b, false)), []string{"open", "assigned"}; !reflect.DeepEqual(got, want) {
		t.Errorf("readyRows = %v, want %v", got, want)
	}
	if got, want := ids(readyRows(b, true)), []string{"open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("readyRows(--unassigned) = %v, want %v", got, want)
	}
}

// TestSearchMatchesHoldsEveryWord: search keeps an issue only when every word
// appears in its title or description, up to the limit.
func TestSearchMatchesHoldsEveryWord(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		{ID: "both", Title: "Fixture bead", Description: "a note"},
		{ID: "one", Title: "fixture only", Description: "nothing else"},
		{ID: "none", Title: "other", Description: "text"},
	}
	tests := []struct {
		name  string
		words []string
		limit int
		want  []string
	}{
		{name: "one word", words: []string{"FIXTURE"}, want: []string{"both", "one"}},
		{name: "every word", words: []string{"fixture", "note"}, want: []string{"both"}},
		{name: "a limit", words: []string{"fixture"}, limit: 1, want: []string{"both"}},
		{name: "no match", words: []string{"absent"}, want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := searchMatches(issues, tc.words, tc.limit)
			var ids []string
			for _, is := range got {
				ids = append(ids, is.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Errorf("searchMatches(%v, limit %d) = %v, want %v", tc.words, tc.limit, ids, tc.want)
			}
		})
	}
}

// TestReadBoardReportsTheReadsItCannotMake: a board whose list or link read
// failed is the shim's error, never an empty board read as "nothing is ready".
func TestReadBoardReportsTheReadsItCannotMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store stubReadStore
		want  string
	}{
		{
			name:  "the list read",
			store: stubReadStore{list: func(beads.ListOptions) ([]*beads.Issue, error) { return nil, errors.New("list refused") }},
			want:  "reading the board: list refused",
		},
		{
			name: "the link read",
			store: stubReadStore{
				list:         func(beads.ListOptions) ([]*beads.Issue, error) { return []*beads.Issue{{ID: "fx-1"}}, nil },
				showMultiple: func([]string) (map[string]*beads.Issue, error) { return nil, errors.New("links refused") },
			},
			want: "links refused",
		},
		{
			name: "a listed bead the link read did not answer",
			store: stubReadStore{
				list:         func(beads.ListOptions) ([]*beads.Issue, error) { return []*beads.Issue{{ID: "fx-1"}}, nil },
				showMultiple: func([]string) (map[string]*beads.Issue, error) { return map[string]*beads.Issue{}, nil },
			},
			want: "fx-1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readBoard(tc.store, beads.ListOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("readBoard = %v, want it to hold %q", err, tc.want)
			}
		})
	}
}
