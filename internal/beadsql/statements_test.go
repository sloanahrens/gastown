package beadsql

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestDeclaredQueriesAreReadOnlyBdReads: every declared statement passes
// ReadOnly once its arguments are inlined, and reads a bd table.
func TestDeclaredQueriesAreReadOnlyBdReads(t *testing.T) {
	t.Parallel()
	ids := []string{"mayor/", `rig/o\'malley`}
	for name, q := range map[string]Query{
		"SchemaLevel":                SchemaLevel(),
		"IssueCount":                 IssueCount(),
		"TableRowCount":              TableRowCount("hq", "wisps"),
		"EphemeralIssues":            EphemeralIssues(),
		"UnassignedInProgress":       UnassignedInProgress(),
		"InProgressByAge":            InProgressByAge(),
		"LabelPresent":               LabelPresent("gt-1", "gt:agent"),
		"AllWisps":                   AllWisps(),
		"WispsWithLabels":            WispsWithLabels([]string{"gt:merge-request", "it's"}),
		"IssuesWithLabelsOrStatuses": IssuesWithLabelsOrStatuses([]string{"gt:agent"}, []string{"open", "hooked"}),
		"IssuesWithStatusesOnly":     IssuesWithLabelsOrStatuses(nil, []string{"in_progress"}),
		"MailWisps":                  MailWisps(ids),
		"MailIssues":                 MailIssues(ids),
		"MoleculesAttachedTo":        MoleculesAttachedTo("gt-a_b"),
		"RawDepsDown":                RawDeps("gt-1", "down", "tracks"),
		"RawDepsUp":                  RawDeps("gt-1", "up", ""),
		"BackupIssues":               BackupIssues("hq", false, nil),
		"BackupIssuesScrubbed":       BackupIssues("hq", true, []string{"testdb_"}),
		"BackupTable":                BackupTable("hq", "labels"),
	} {
		s, err := q.Inline()
		if err != nil {
			t.Errorf("%s: Inline: %v", name, err)
			continue
		}
		if !bdTableRead.MatchString(s) {
			t.Errorf("%s reads no bd table: %s", name, s)
		}
		args, err := q.BdArgs("--json")
		if err != nil || len(args) != 3 || args[0] != "sql" || args[1] != "--json" || args[2] != s {
			t.Errorf("%s: BdArgs = %q, %v", name, args, err)
		}
	}
	// TableProbe takes any table name; the probe of a missing one is how
	// callers learn it is missing.
	if s, err := TableProbe("nosuch").Inline(); err != nil || s != "SELECT 1 FROM `nosuch` LIMIT 1" {
		t.Errorf("TableProbe = %q, %v", s, err)
	}
}

func TestQueriesRefuseBadIdentifiers(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]Query{
		"TableProbe":    TableProbe("x`; DROP TABLE issues"),
		"TableRowCount": TableRowCount("hq`", "issues"),
		"BackupIssues":  BackupIssues("a b", true, nil),
		"BackupTable":   BackupTable("hq", "labels;"),
		"no filter":     IssuesWithLabelsOrStatuses(nil, nil),
	} {
		if _, err := q.BdArgs(); err == nil {
			t.Errorf("%s: BdArgs accepted %s", name, q)
		}
		if _, err := (&DB{}).Query(context.Background(), q); err == nil {
			t.Errorf("%s: DB.Query accepted it", name)
		}
	}
}

func TestQuoteEscapesSQLLiterals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		values []string
		want   string
	}{
		{[]string{"mayor"}, `'mayor'`},
		{[]string{"o'brien"}, `'o''brien'`},
		{[]string{`rig\agent`}, `'rig\\agent'`},
		{[]string{`rig\`}, `'rig\\'`},
		{[]string{`rig/o\'malley`}, `'rig/o\\''malley'`},
		{[]string{"mayor/", `rig/o\'malley`}, `'mayor/','rig/o\\''malley'`},
	} {
		if got := quoteList(tt.values, ","); got != tt.want {
			t.Errorf("quoteList(%q) = %s, want %s", tt.values, got, tt.want)
		}
	}
}

func TestMailQueriesMatchEveryIdentityAndItsCC(t *testing.T) {
	t.Parallel()
	s := MailWisps([]string{"mayor/", "mayor", `rig/o\'malley`}).String()
	for _, want := range []string{`'mayor/'`, `'mayor'`, `'cc:mayor/'`, `'cc:mayor'`, `'rig/o\\''malley'`, `'cc:rig/o\\''malley'`} {
		if !strings.Contains(s, want) {
			t.Errorf("MailWisps lacks %s:\n%s", want, s)
		}
	}
}

// The external-target match escapes _ with ! so it stays literal under LIKE.
func TestExternalTargetsMatchLiterally(t *testing.T) {
	t.Parallel()
	if s := MoleculesAttachedTo("gt-a_b").String(); !strings.Contains(s, "depends_on_external LIKE '%:gt-a!_b' ESCAPE '!'") {
		t.Errorf("MoleculesAttachedTo = %s", s)
	}
	if s := RawDeps("gt-a_b", "up", "").String(); !strings.Contains(s, "depends_on_external LIKE '%:gt-a!_b' ESCAPE '!'") {
		t.Errorf("RawDeps(up) = %s", s)
	}
}

func TestInlineBindsPlaceholdersOutsideQuotes(t *testing.T) {
	t.Parallel()
	q := Query{text: "SELECT id FROM issues WHERE title = '?' AND id = ? AND priority = ?", args: []any{"it's", 2}}
	if s, err := q.Inline(); err != nil || s != "SELECT id FROM issues WHERE title = '?' AND id = 'it''s' AND priority = 2" {
		t.Errorf("Inline = %q, %v", s, err)
	}
	for _, args := range [][]any{{"a"}, {"a", 1, "c"}, {"a", 1.5}} {
		if _, err := (Query{text: q.text, args: args}).Inline(); err == nil {
			t.Errorf("Inline with %v arguments succeeded", args)
		}
	}
	if _, err := (Query{text: "DELETE FROM issues"}).Inline(); !errors.Is(err, ErrNotReadOnly) {
		t.Errorf("Inline(DELETE) = %v, want ErrNotReadOnly", err)
	}
}
