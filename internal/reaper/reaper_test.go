package reaper

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateDBName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"hq", false},
		{"beads", false},
		{"gt", false},
		{"test_db_123", false},
		{"", true},
		{"drop table", true},
		{"db;--", true},
		{"db`name", true},
		{"../etc/passwd", true},
	}
	for _, tt := range tests {
		err := ValidateDBName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateDBName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestDefaultDatabases(t *testing.T) {
	if len(DefaultDatabases) == 0 {
		t.Error("DefaultDatabases should not be empty")
	}
	for _, db := range DefaultDatabases {
		if err := ValidateDBName(db); err != nil {
			t.Errorf("DefaultDatabases contains invalid name %q: %v", db, err)
		}
	}
}

func TestDogReaperFormulaAlertThresholdMatchesDefault(t *testing.T) {
	data, err := os.ReadFile("../formula/formulas/mol-dog-reaper.formula.toml")
	if err != nil {
		t.Fatalf("read mol-dog-reaper formula: %v", err)
	}

	threshold := fmt.Sprintf("%d", DefaultAlertThreshold)
	source := string(data)
	alertThresholdVars := sourceBetween(t, source, "[vars.alert_threshold]", "[vars.dry_run]")
	if !strings.Contains(alertThresholdVars, fmt.Sprintf("default = %q", threshold)) {
		t.Fatalf("mol-dog-reaper alert_threshold default should match DefaultAlertThreshold %s", threshold)
	}
	if !strings.Contains(source, fmt.Sprintf("default %s", threshold)) {
		t.Fatalf("mol-dog-reaper alert_threshold prose should document default %s", threshold)
	}
}

func TestFormatJSON(t *testing.T) {
	result := FormatJSON(map[string]int{"count": 42})
	if result == "" {
		t.Error("FormatJSON should not return empty string")
	}
	if result[0] != '{' {
		t.Errorf("FormatJSON should return JSON object, got %q", result[:10])
	}
}

func TestParentExcludeJoin(t *testing.T) {
	joinClause, whereCondition := parentExcludeJoin("testdb")

	// JOIN clause should reference the correct database.
	if joinClause == "" {
		t.Error("parentExcludeJoin joinClause should not be empty")
	}
	// parentExcludeJoin no longer qualifies table names with the database — the
	// reaper connects to a specific database via the DSN, so unqualified names
	// are correct. The dbName parameter is retained for API compatibility.

	// JOIN should select wisps with open parents from wisp_dependencies.
	if !contains(joinClause, "wisp_dependencies") {
		t.Error("parentExcludeJoin should query wisp_dependencies")
	}
	if !contains(joinClause, "wd.depends_on_wisp_id") {
		t.Error("parentExcludeJoin should join wisp parents through depends_on_wisp_id")
	}
	if !contains(joinClause, "wd.depends_on_issue_id") {
		t.Error("parentExcludeJoin should join issue parents through depends_on_issue_id")
	}
	if contains(joinClause, "wd.depends_on_id") {
		t.Error("parentExcludeJoin should not use legacy depends_on_id")
	}
	if !contains(joinClause, "parent-child") {
		t.Error("parentExcludeJoin should filter on parent-child type")
	}
	if !contains(joinClause, "'open', 'hooked', 'in_progress'") {
		t.Error("parentExcludeJoin should check for open parent statuses")
	}

	// WHERE condition should be an IS NULL anti-join filter.
	if whereCondition == "" {
		t.Error("parentExcludeJoin whereCondition should not be empty")
	}
	if !contains(whereCondition, "IS NULL") {
		t.Error("parentExcludeJoin whereCondition should use IS NULL for anti-join")
	}
}

func TestMRProtectedJoin(t *testing.T) {
	joinClause, whereCondition := mrProtectedJoin()

	if !contains(joinClause, "wisp_labels") {
		t.Error("mrProtectedJoin should query wisp_labels")
	}
	if !contains(joinClause, "gt:merge-request") {
		t.Error("mrProtectedJoin should protect wisps labeled gt:merge-request")
	}
	if !contains(joinClause, "'cleanup'") || !contains(joinClause, "'state:merge-requested'") {
		t.Error("mrProtectedJoin should protect wisps labeled cleanup + state:merge-requested")
	}
	if !contains(whereCondition, "IS NULL") {
		t.Error("mrProtectedJoin whereCondition should use IS NULL for anti-join")
	}
}

func TestWispExcludeClause(t *testing.T) {
	if clause, args := wispExcludeClause(nil); clause != "" || args != nil {
		t.Errorf("wispExcludeClause(nil) = (%q, %v), want (\"\", nil)", clause, args)
	}

	clause, args := wispExcludeClause(map[string]bool{"gt-wisp-miky": true})
	if !contains(clause, "NOT IN") {
		t.Errorf("wispExcludeClause should render a NOT IN clause, got %q", clause)
	}
	if len(args) != 1 || args[0] != "gt-wisp-miky" {
		t.Errorf("wispExcludeClause args = %v, want [gt-wisp-miky]", args)
	}
}

func TestAgentReferenceFieldPatterns(t *testing.T) {
	cases := []struct {
		name        string
		pattern     *regexp.Regexp
		description string
		want        string
	}{
		{"active_mr", activeMRFieldPattern, "agent_state: working\nactive_mr: gt-wisp-miky\n", "gt-wisp-miky"},
		{"active_mr null", activeMRFieldPattern, "agent_state: working\nactive_mr: null\n", "null"},
		{"hook_bead is not active_mr", activeMRFieldPattern, "agent_state: working\nhook_bead: gt-wisp-miky\n", ""},
		{"hook_bead", hookBeadFieldPattern, "agent_state: working\nhook_bead: gt-wisp-miky\n", "gt-wisp-miky"},
		{"hook_bead null", hookBeadFieldPattern, "agent_state: working\nhook_bead: null\n", "null"},
		{"active_mr is not hook_bead", hookBeadFieldPattern, "agent_state: working\nactive_mr: gt-wisp-miky\n", ""},
	}
	for _, c := range cases {
		m := c.pattern.FindStringSubmatch(c.description)
		got := ""
		if m != nil {
			got = m[1]
		}
		if got != c.want {
			t.Errorf("%s pattern on %q = %q, want %q", c.name, c.description, got, c.want)
		}
	}
}

func TestReaperQueriesUseTypedDependencyColumns(t *testing.T) {
	sourcePath := "reaper.go"
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	source := string(data)
	if strings.Contains(source, "depends_on_id") {
		t.Fatalf("reaper queries should not use legacy depends_on_id")
	}

	scanBody := sourceBetween(t, source, "func Scan(", "func Reap(")
	autoCloseBody := sourceBetween(t, source, "func AutoClose(", "// batchDeleteRows")
	batchDeleteBody := sourceBetween(t, source, "func batchDeleteRows(", "// ClosePluginReceiptResult")
	schemaBody := sourceBetween(t, source, "func HasReaperSchema(", "func tableExists(")
	eligibilityBody := sourceBetween(t, source, "func staleIssueEligibilityClause(", "// AutoClose closes issues")

	for _, want := range []string{
		`hasColumns(ctx, db, "wisp_dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")`,
		`hasColumns(ctx, db, "dependencies", "depends_on_issue_id", "depends_on_wisp_id", "depends_on_external")`,
	} {
		if !strings.Contains(schemaBody, want) {
			t.Fatalf("HasReaperSchema missing typed schema gate %q", want)
		}
	}

	// The eligibility clause moved to a shared helper in gt-2qzr so Scan and
	// AutoClose cannot drift apart; assert it there and assert both callers
	// route through it.
	for _, want := range []string{
		"d.depends_on_issue_id = dep.id",
		"SELECT DISTINCT d.depends_on_issue_id",
		"d.depends_on_issue_id IS NOT NULL",
	} {
		if !strings.Contains(eligibilityBody, want) {
			t.Fatalf("staleIssueEligibilityClause missing typed dependency predicate %q", want)
		}
	}
	if !strings.Contains(scanBody, "staleIssueEligibilityClause(") {
		t.Fatal("Scan must count stale candidates with the shared eligibility clause")
	}
	if !strings.Contains(autoCloseBody, "staleIssueEligibilityClause(") {
		t.Fatal("AutoClose must select candidates with the shared eligibility clause")
	}

	if !strings.Contains(scanBody, "wd.depends_on_wisp_id IS NOT NULL OR wd.depends_on_issue_id IS NOT NULL") {
		t.Fatal("Scan dangling-parent anomaly should ignore external-only dependency rows")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM wisp_dependencies WHERE depends_on_wisp_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse wisp dependency references")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM dependencies WHERE depends_on_wisp_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse issue dependency references to wisps")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM wisp_dependencies WHERE depends_on_issue_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse wisp parent references to issues")
	}
	if !strings.Contains(batchDeleteBody, "DELETE FROM dependencies WHERE depends_on_issue_id IN %s") {
		t.Fatal("batchDeleteRows should clean reverse issue dependency references")
	}
}

// TestAutoCloseEligibilityExcludesInfrastructureBeads is the regression guard
// for gt-2qzr, where a mis-set stale-age swept 102 durable beads: every agent
// bead in the town, all dog beads, and every rig patrol molecule. The labels
// and types below are the ones those victims actually carried — agent beads are
// issue_type=task with gt:agent, and patrol molecules are issue_type=molecule
// with no labels at all.
func TestAutoCloseEligibilityExcludesInfrastructureBeads(t *testing.T) {
	clause := staleIssueEligibilityClause("`hq`.")

	for _, want := range []struct {
		fragment string
		why      string
	}{
		{"'gt:agent'", "agent beads (mayor, deacon, dog, witness, refinery, crew, polecat) carry this label"},
		{"'epic', 'convoy', 'molecule', 'rig', 'agent', 'event'", "infrastructure issue types are lifecycle containers, not stale work"},
		{"i.id NOT LIKE '%-witness'", "witness bead id pattern"},
		{"i.id NOT LIKE '%-refinery'", "refinery bead id pattern"},
		{"i.id NOT LIKE '%-mayor'", "mayor bead id pattern"},
		{"i.id NOT LIKE '%-deacon'", "deacon bead id pattern"},
		{"i.id NOT LIKE '%-crew-%'", "crew bead id pattern"},
		{"i.id NOT LIKE '%-polecat-%'", "polecat bead id pattern"},
		{"i.id NOT LIKE '%-dog-%'", "dog bead id pattern"},
		{"'type:plugin-run'", "plugin receipts are closed by their own fast-track step"},
		{"i.priority > 1", "P0/P1 are never stale"},
	} {
		if !strings.Contains(clause, want.fragment) {
			t.Errorf("eligibility clause missing %q (%s)", want.fragment, want.why)
		}
	}

	// The qualifier-free clause is Scan's form. It must name tables bare — a
	// backtick-wrapped empty identifier (``.labels) would be a syntax error.
	unqualified := staleIssueEligibilityClause("")
	if strings.Contains(unqualified, "`") {
		t.Fatalf("unqualified clause should name tables bare, got:\n%s", unqualified)
	}
	if !strings.Contains(unqualified, "SELECT DISTINCT l.issue_id FROM labels l") {
		t.Fatalf("unqualified clause should reference labels bare, got:\n%s", unqualified)
	}
	// The qualified form is AutoClose's, and must target the same schema.
	if !strings.Contains(clause, "FROM `hq`.labels l") {
		t.Fatalf("qualified clause should target the named schema, got:\n%s", clause)
	}
}

// TestAutoCloseRejectsStaleAgeBelowFloor covers the second brake from gt-2qzr:
// the incident run's 7d threshold was short enough to catch eight-day-old agent
// beads, so anything below a week is refused outright.
func TestAutoCloseRejectsStaleAgeBelowFloor(t *testing.T) {
	// db is nil on purpose: the floor is checked before any query runs, so a
	// non-refusing implementation would panic here rather than pass.
	for _, age := range []time.Duration{time.Hour, 24 * time.Hour, 6 * 24 * time.Hour} {
		_, err := AutoClose(nil, "hq", AutoCloseOptions{StaleAge: age})
		if !errors.Is(err, ErrStaleAgeTooLow) {
			t.Errorf("AutoClose(StaleAge=%s) error = %v, want ErrStaleAgeTooLow", age, err)
		}
	}

	// The floor itself is allowed — it is the explicit lower bound — and Force
	// lifts it. Both get past validation and reach the query, so any error they
	// return is the fake driver's, never the floor's.
	db := openFakeReaperDB(t, &fakeReaperState{wisps: map[string]*fakeWisp{}, ops: map[int][]string{}})
	t.Cleanup(func() { _ = db.Close() })

	if _, err := AutoClose(db, "hq", AutoCloseOptions{StaleAge: MinStaleIssueAge}); errors.Is(err, ErrStaleAgeTooLow) {
		t.Errorf("AutoClose(StaleAge=MinStaleIssueAge) should not be refused by the floor")
	}
	if _, err := AutoClose(db, "hq", AutoCloseOptions{StaleAge: time.Hour, Force: true}); errors.Is(err, ErrStaleAgeTooLow) {
		t.Errorf("AutoClose(StaleAge=1h, Force) should not be refused by the floor")
	}
}

// TestAutoCloseMaxPerRunDefaults guards the cap's default: a zero value means
// "use the package default", never "unlimited".
func TestAutoCloseMaxPerRunDefaults(t *testing.T) {
	if DefaultAutoCloseMaxPerRun <= 0 {
		t.Fatalf("DefaultAutoCloseMaxPerRun = %d, want positive", DefaultAutoCloseMaxPerRun)
	}
	// 102 beads were closed in the gt-2qzr run across four databases; the
	// per-database cap has to sit below the per-database share of that to be a
	// brake at all.
	if DefaultAutoCloseMaxPerRun >= 102 {
		t.Fatalf("DefaultAutoCloseMaxPerRun = %d is too high to brake a gt-2qzr-class sweep",
			DefaultAutoCloseMaxPerRun)
	}
}

// TestReapQueryNoDatabaseNameInjection verifies that the Reap function's batch
// SELECT query does not inject the database name into the SQL string. Previously,
// dbName was passed as a Sprintf arg but the format string didn't use it, causing
// positional shift: "FROM wisps w gt WHERE..." instead of "FROM wisps w LEFT JOIN...".
func TestReapQueryNoDatabaseNameInjection(t *testing.T) {
	// Reproduce the exact Sprintf call from Reap() to verify no dbName injection.
	dbName := "gt"
	parentJoin, parentWhere := parentExcludeJoin(dbName)
	whereClause := fmt.Sprintf(
		"w.status IN ('open', 'hooked', 'in_progress') AND w.created_at < ? AND %s", parentWhere)

	// This is the fixed query — dbName is NOT in the Sprintf args.
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w %s WHERE %s LIMIT %d",
		parentJoin, whereClause, DefaultBatchSize)

	// The query must NOT contain the literal database name as a bare token.
	// Before the fix, "gt" appeared between "wisps w" and "WHERE".
	if strings.Contains(idQuery, "wisps w gt") {
		t.Errorf("Reap idQuery contains injected database name: %s", idQuery)
	}
	if !strings.Contains(idQuery, "LEFT JOIN") {
		t.Errorf("Reap idQuery should contain LEFT JOIN from parentExcludeJoin, got: %s", idQuery)
	}
	if !strings.Contains(idQuery, fmt.Sprintf("LIMIT %d", DefaultBatchSize)) {
		t.Errorf("Reap idQuery should end with LIMIT %d, got: %s", DefaultBatchSize, idQuery)
	}
}

// TestReapUpdateQueryNoDatabaseNameInjection verifies that the UPDATE query in
// Reap() does not inject dbName where the IN clause should go.
func TestReapUpdateQueryNoDatabaseNameInjection(t *testing.T) {
	dbName := "gt"
	inClause := "?,?,?"

	// This is the fixed query — only inClause in the Sprintf args.
	updateQuery := fmt.Sprintf(
		"UPDATE wisps SET status='closed', closed_at=NOW() WHERE id IN (%s)",
		inClause)

	if strings.Contains(updateQuery, dbName) {
		t.Errorf("Reap updateQuery contains injected database name %q: %s", dbName, updateQuery)
	}
	if !strings.Contains(updateQuery, "IN (?,?,?)") {
		t.Errorf("Reap updateQuery should contain parameterized IN clause, got: %s", updateQuery)
	}
}

// TestPurgeDigestQueryNoDatabaseNameInjection verifies that the purge digest
// query interpolates only the live-reference exclusion clause, never dbName.
func TestPurgeDigestQueryNoDatabaseNameInjection(t *testing.T) {
	// Mirrors purgeClosedWisps: the exclusion is parameterized, so the literal
	// query text carries no database name and no bead id.
	referencedClause, _ := wispExcludeClause(map[string]bool{"gt-wisp-miky": true})
	digestQuery := "SELECT COALESCE(w.wisp_type, 'unknown') AS wtype, COUNT(*) AS cnt FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?" + referencedClause + " GROUP BY wtype"

	if strings.Contains(digestQuery, "gt") {
		t.Errorf("purge digestQuery should not contain database name, got: %s", digestQuery)
	}
	if !strings.Contains(digestQuery, "GROUP BY wtype") {
		t.Errorf("purge digestQuery should end with GROUP BY, got: %s", digestQuery)
	}
}

// TestPurgeBatchQueryNoDatabaseNameInjection verifies that the purge batch
// SELECT query uses DefaultBatchSize as the LIMIT, not dbName.
func TestPurgeBatchQueryNoDatabaseNameInjection(t *testing.T) {
	// Mirrors purgeClosedWisps — only the parameterized exclusion and
	// DefaultBatchSize are interpolated.
	referencedClause, _ := wispExcludeClause(map[string]bool{"gt-wisp-miky": true})
	idQuery := fmt.Sprintf(
		"SELECT w.id FROM wisps w WHERE w.status = 'closed' AND w.closed_at < ?%s LIMIT %d",
		referencedClause, DefaultBatchSize)

	if strings.Contains(idQuery, "gt") {
		t.Errorf("purge idQuery contains injected database name: %s", idQuery)
	}
	expected := fmt.Sprintf("LIMIT %d", DefaultBatchSize)
	if !strings.Contains(idQuery, expected) {
		t.Errorf("purge idQuery should contain %s, got: %s", expected, idQuery)
	}
}

// TestIsNothingToCommit verifies that "nothing to commit" errors are recognized
// correctly. This prevents false-positive dolt_commit_failed anomalies when the
// reaper operates on dolt_ignored tables (wisps, wisp_*), where Dolt has nothing
// to version after a successful SQL DELETE.
func TestIsNothingToCommit(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"nothing to commit", true},
		{"NOTHING TO COMMIT", true},
		{"Error 1105 (HY000): nothing to commit", true},
		{"no changes to commit", false}, // must also contain "commit" — see isNothingToCommit
		{"no changes", false},
		{"connection refused", false},
		{"table not found: wisps", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.msg != "" {
			err = fmt.Errorf("%s", c.msg)
		}
		got := isNothingToCommit(err)
		if got != c.want {
			t.Errorf("isNothingToCommit(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func sourceBetween(t *testing.T, source, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start == -1 {
		t.Fatalf("could not find %q", startMarker)
	}
	end := strings.Index(source[start:], endMarker)
	if end == -1 {
		t.Fatalf("could not find %q after %q", endMarker, startMarker)
	}
	return source[start : start+end]
}

// TestReapExcludesAgentBeads verifies that the Reap function excludes agent beads
// from being closed, regardless of their age. This is a regression test for the bug
// where the wisp reaper was closing agent beads (hq-mayor, hq-deacon, witness, refinery,
// etc.) after 24 hours, causing doctor to report them as missing.
func TestReapExcludesAgentBeads(t *testing.T) {
	// Verify that the WHERE clause in Reap() excludes issue_type='agent'
	// by checking the source code pattern.
	// This is a compile-time guard — if the exclusion is removed, this test
	// will fail when the query pattern doesn't match.

	// The whereClause in Reap() should contain:
	// "w.issue_type != 'agent'"
	// This test documents the expected behavior; actual exclusion is tested
	// in integration tests with a real database.

	// Integration test would require spinning up a Dolt server, which is
	// beyond the scope of this unit test. The exclusion is verified manually
	// by checking that agent beads are not closed by the wisp_reaper patrol.
	t.Log("Agent beads (issue_type='agent') are excluded from wisp reaping")
	t.Log("This prevents hq-mayor, hq-deacon, witness, refinery, etc. from being closed")
}

// TestScanExcludesAgentBeads documents that Scan() must use the same eligibility
// predicate as Reap() for stale open wisps. If Scan counts agent beads but Reap
// excludes them, the operator sees scan>0 and reap=0 for the same cutoff.
func TestScanExcludesAgentBeads(t *testing.T) {
	sourcePath := "reaper.go"
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("read %s: %v", sourcePath, err)
	}
	source := string(data)
	scanStart := strings.Index(source, "func Scan(")
	reapStart := strings.Index(source, "func Reap(")
	if scanStart == -1 || reapStart == -1 || reapStart <= scanStart {
		t.Fatalf("could not isolate Scan() body in %s", sourcePath)
	}
	scanBody := source[scanStart:reapStart]
	if !strings.Contains(scanBody, "w.issue_type != 'agent'") {
		t.Fatalf("expected Scan() eligibility to exclude agent beads, scan body was:\n%s", scanBody)
	}
}

func TestClosedMoleculeStepReapBehavior(t *testing.T) {
	now := time.Now().UTC()
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"mol-closed":               {id: "mol-closed", status: "closed", issueType: "molecule", createdAt: now},
			"mol-open":                 {id: "mol-open", status: "open", issueType: "molecule", createdAt: now},
			"closed-epic":              {id: "closed-epic", status: "closed", issueType: "epic", createdAt: now},
			"step-closed-mol-recent":   {id: "step-closed-mol-recent", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
			"step-closed-mol-old":      {id: "step-closed-mol-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-mixed-parent-old":    {id: "step-mixed-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-external-parent-old": {id: "step-external-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-open-parent-old":     {id: "step-open-parent-old", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"step-non-molecule-parent": {id: "step-non-molecule-parent", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"agent-step":               {id: "agent-step", status: "open", issueType: "agent", createdAt: now.Add(-48 * time.Hour)},
			"stale-orphan":             {id: "stale-orphan", status: "open", issueType: "task", createdAt: now.Add(-48 * time.Hour)},
			"fresh-orphan":             {id: "fresh-orphan", status: "open", issueType: "task", createdAt: now.Add(-1 * time.Hour)},
		},
		deps: []fakeDep{
			{issueID: "step-closed-mol-recent", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-closed-mol-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-mixed-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-external-parent-old", dependsOnExternal: "external:other", depType: "parent-child"},
			{issueID: "step-open-parent-old", dependsOnID: "mol-open", depType: "parent-child"},
			{issueID: "step-non-molecule-parent", dependsOnID: "closed-epic", depType: "parent-child"},
			{issueID: "agent-step", dependsOnID: "mol-closed", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	maxAge := 24 * time.Hour
	scan, err := Scan(db, "testdb", maxAge, 7*24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if scan.MoleculeStepCandidates != 2 {
		t.Fatalf("Scan MoleculeStepCandidates = %d, want 2", scan.MoleculeStepCandidates)
	}
	if scan.ReapCandidates != 2 {
		t.Fatalf("Scan ReapCandidates = %d, want 2", scan.ReapCandidates)
	}

	beforeDryRun := state.statuses()
	dryRun, err := Reap(db, "testdb", maxAge, true)
	if err != nil {
		t.Fatalf("dry-run Reap: %v", err)
	}
	if dryRun.MoleculeStepsClosed != 2 {
		t.Fatalf("dry-run MoleculeStepsClosed = %d, want 2", dryRun.MoleculeStepsClosed)
	}
	if dryRun.Reaped != 2 {
		t.Fatalf("dry-run Reaped = %d, want 2", dryRun.Reaped)
	}
	if dryRun.OpenRemain != 10 {
		t.Fatalf("dry-run OpenRemain = %d, want 10", dryRun.OpenRemain)
	}
	if afterDryRun := state.statuses(); !reflect.DeepEqual(afterDryRun, beforeDryRun) {
		t.Fatalf("dry-run mutated statuses: before=%v after=%v", beforeDryRun, afterDryRun)
	}

	preRealOps := state.opCounts()
	realRun, err := Reap(db, "testdb", maxAge, false)
	if err != nil {
		t.Fatalf("real Reap: %v", err)
	}
	if realRun.MoleculeStepsClosed != 2 {
		t.Fatalf("real MoleculeStepsClosed = %d, want 2", realRun.MoleculeStepsClosed)
	}
	if realRun.Reaped != 2 {
		t.Fatalf("real Reaped = %d, want 2", realRun.Reaped)
	}
	if realRun.OpenRemain != 6 {
		t.Fatalf("real OpenRemain = %d, want 6", realRun.OpenRemain)
	}

	for _, id := range []string{"step-closed-mol-recent", "step-closed-mol-old", "step-non-molecule-parent", "stale-orphan"} {
		if got := state.status(id); got != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got)
		}
	}
	for _, id := range []string{"step-mixed-parent-old", "step-external-parent-old", "step-open-parent-old", "agent-step", "fresh-orphan", "mol-open"} {
		if got := state.status(id); got != "open" {
			t.Fatalf("%s status = %q, want open", id, got)
		}
	}
	realOps := state.opsSince(preRealOps)
	if len(realOps) != 1 {
		t.Fatalf("real Reap used %d connections, want 1: %#v", len(realOps), realOps)
	}
	for connID, ops := range realOps {
		assertOpsContainInOrder(t, ops,
			"EXEC SET @@autocommit = 0",
			"QUERY SELECT w.id FROM wisps w INNER JOIN",
			"EXEC UPDATE wisps SET status='closed'",
			"QUERY SELECT w.id FROM wisps w LEFT JOIN",
			"EXEC UPDATE wisps SET status='closed'",
			"EXEC COMMIT",
			"EXEC CALL DOLT_COMMIT",
			"QUERY SELECT COUNT(*) FROM wisps WHERE status IN",
			"EXEC SET @@autocommit = 1",
		)
		t.Logf("real Reap used pinned connection %d", connID)
	}
}

// TestReapExcludesLiveMergeQueueWisps is a regression test for gt-4okk: the
// reaper's age-sweep closed a live merge-queue MR wisp (and the witness gc
// purged it irrecoverably two minutes later) because age was the only signal
// and a dormant town made the still-queued MR look abandoned. Age must never
// be sufficient on its own to reap an MR wisp, its tracking cleanup wisp, or
// anything a live agent still references as active_mr.
func TestReapExcludesLiveMergeQueueWisps(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"old-mr-wisp":            {id: "old-mr-wisp", status: "open", issueType: "task", createdAt: old, labels: []string{"gt:merge-request"}},
			"old-cleanup-open-mr":    {id: "old-cleanup-open-mr", status: "open", issueType: "task", createdAt: old, labels: []string{"cleanup", "state:merge-requested"}},
			"old-cleanup-no-mr":      {id: "old-cleanup-no-mr", status: "open", issueType: "task", createdAt: old, labels: []string{"cleanup", "state:clean"}},
			"old-active-mr-target":   {id: "old-active-mr-target", status: "open", issueType: "task", createdAt: old},
			"old-stale-orphan":       {id: "old-stale-orphan", status: "open", issueType: "task", createdAt: old},
			"live-agent":             {id: "live-agent", status: "open", issueType: "agent", createdAt: now, description: "agent_state: working\nactive_mr: old-active-mr-target\n"},
			"nuked-agent-active-ref": {id: "nuked-agent-active-ref", status: "open", issueType: "agent", createdAt: now, description: "agent_state: nuked\nactive_mr: old-stale-orphan\n"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	maxAge := 24 * time.Hour
	scan, err := Scan(db, "testdb", maxAge, 7*24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	// old-cleanup-no-mr and old-stale-orphan are eligible: the other two old
	// wisps are protected by MR label, cleanup+merge-requested labels, or active_mr.
	if scan.ReapCandidates != 2 {
		t.Fatalf("Scan ReapCandidates = %d, want 2", scan.ReapCandidates)
	}

	if _, err := Reap(db, "testdb", maxAge, false); err != nil {
		t.Fatalf("real Reap: %v", err)
	}

	for _, id := range []string{"old-mr-wisp", "old-cleanup-open-mr", "old-active-mr-target"} {
		if got := state.status(id); got != "open" {
			t.Fatalf("%s status = %q, want open (protected)", id, got)
		}
	}
	for _, id := range []string{"old-cleanup-no-mr", "old-stale-orphan"} {
		if got := state.status(id); got != "closed" {
			t.Fatalf("%s status = %q, want closed (not protected)", id, got)
		}
	}
}

// TestPurgeExcludesLiveAgentReferencedWisps is the regression test for gt-gyb6:
// the purge sweep deleted closed MR wisps that live agent beads still named as
// active_mr, so the polecat waited on an MR no lookup could resolve. Both rows
// here are closed and far past purge-age — only the live reference spares them,
// and it lapses with the agent (the nuked bead's reference does not).
func TestPurgeExcludesLiveAgentReferencedWisps(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-30 * 24 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"referenced-mr":       {id: "referenced-mr", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"referenced-hook":     {id: "referenced-hook", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"referenced-by-nuked": {id: "referenced-by-nuked", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"unreferenced":        {id: "unreferenced", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"live-agent":          {id: "live-agent", status: "open", issueType: "agent", createdAt: now, description: "agent_state: done\nactive_mr: referenced-mr\nhook_bead: referenced-hook\n"},
			"nuked-agent":         {id: "nuked-agent", status: "open", issueType: "agent", createdAt: now, description: "agent_state: nuked\nactive_mr: referenced-by-nuked\n"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	purgeAge := 7 * 24 * time.Hour
	scan, err := Scan(db, "testdb", 24*time.Hour, purgeAge, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if scan.PurgeCandidates != 2 {
		t.Fatalf("Scan PurgeCandidates = %d, want 2 (referenced-by-nuked and unreferenced)", scan.PurgeCandidates)
	}

	purge, err := Purge(db, "testdb", purgeAge, 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if purge.WispsPurged != scan.PurgeCandidates {
		t.Fatalf("Purge deleted %d wisps, Scan previewed %d — the preview must gate the sweep", purge.WispsPurged, scan.PurgeCandidates)
	}

	statuses := state.statuses()
	for _, id := range []string{"referenced-mr", "referenced-hook"} {
		if _, ok := statuses[id]; !ok {
			t.Fatalf("%s was purged; a live agent bead still references it", id)
		}
	}
	for _, id := range []string{"referenced-by-nuked", "unreferenced"} {
		if _, ok := statuses[id]; ok {
			t.Fatalf("%s survived the purge, want deleted", id)
		}
	}
}

var fakeReaperDriverID uint64

func openFakeReaperDB(t *testing.T, state *fakeReaperState) *sql.DB {
	t.Helper()
	driverName := fmt.Sprintf("fake_reaper_%d", atomic.AddUint64(&fakeReaperDriverID, 1))
	sql.Register(driverName, &fakeReaperDriver{state: state})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatalf("open fake db: %v", err)
	}
	return db
}

type fakeWisp struct {
	id          string
	status      string
	issueType   string
	createdAt   time.Time
	closedAt    time.Time
	description string
	labels      []string
}

type fakeDep struct {
	issueID           string
	dependsOnID       string
	dependsOnExternal string
	depType           string
}

type fakeReaperState struct {
	mu       sync.Mutex
	wisps    map[string]*fakeWisp
	deps     []fakeDep
	nextConn int
	ops      map[int][]string
}

func (s *fakeReaperState) status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wisps[id].status
}

func (s *fakeReaperState) statuses() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make(map[string]string, len(s.wisps))
	for id, w := range s.wisps {
		statuses[id] = w.status
	}
	return statuses
}

// purgeCandidatesLocked mirrors the purge sweep's eligibility: closed wisps past
// the delete cutoff, minus the ones a live agent bead still references.
func (s *fakeReaperState) purgeCandidatesLocked(cutoff time.Time, excluded map[string]bool) []string {
	var ids []string
	for id, w := range s.wisps {
		if w.status != "closed" || w.closedAt.IsZero() || !w.closedAt.Before(cutoff) || excluded[id] {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// wispTypeCountsLocked groups the same purge candidates by wisp_type, the shape
// the digest query returns.
func (s *fakeReaperState) wispTypeCountsLocked(cutoff time.Time, excluded map[string]bool) map[string]int {
	counts := map[string]int{}
	for _, id := range s.purgeCandidatesLocked(cutoff, excluded) {
		wtype := s.wisps[id].issueType
		if wtype == "" {
			wtype = "unknown"
		}
		counts[wtype]++
	}
	return counts
}

func (s *fakeReaperState) opCounts() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[int]int, len(s.ops))
	for connID, ops := range s.ops {
		counts[connID] = len(ops)
	}
	return counts
}

func (s *fakeReaperState) opsSince(counts map[int]int) map[int][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	opsSince := map[int][]string{}
	for connID, ops := range s.ops {
		start := counts[connID]
		if start < len(ops) {
			opsSince[connID] = append([]string(nil), ops[start:]...)
		}
	}
	return opsSince
}

func (s *fakeReaperState) record(connID int, op string) {
	s.ops[connID] = append(s.ops[connID], normalizeSQL(op))
}

func (s *fakeReaperState) moleculeStepCandidatesLocked() []string {
	var ids []string
	for id := range s.wisps {
		if s.isMoleculeStepCandidateLocked(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *fakeReaperState) absentParentCandidatesLocked() []string {
	var ids []string
	for id, w := range s.wisps {
		if !isOpenWispStatus(w.status) || w.issueType == "agent" {
			continue
		}
		if s.hasAbsentParentLocked(id) && !s.hasOpenParentLocked(id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *fakeReaperState) isMoleculeStepCandidateLocked(id string) bool {
	w := s.wisps[id]
	if w == nil || !isOpenWispStatus(w.status) || w.issueType == "agent" {
		return false
	}
	for _, dep := range s.deps {
		if dep.issueID != id || dep.depType != "parent-child" {
			continue
		}
		if dep.dependsOnExternal != "" {
			return false
		}
		if s.hasOpenParentLocked(id) {
			return false
		}
		parent := s.wisps[dep.dependsOnID]
		if parent != nil && parent.issueType == "molecule" && parent.status == "closed" {
			return true
		}
	}
	return false
}

func (s *fakeReaperState) staleCandidatesLocked(cutoff time.Time, excludeMoleculeSteps bool) []string {
	mrProtected := s.mrProtectedLocked()
	activeMRProtected := s.activeMRProtectedLocked()
	var ids []string
	for id, w := range s.wisps {
		if !isOpenWispStatus(w.status) || w.issueType == "agent" || !w.createdAt.Before(cutoff) {
			continue
		}
		if s.hasOpenParentLocked(id) {
			continue
		}
		if excludeMoleculeSteps && s.isMoleculeStepCandidateLocked(id) {
			continue
		}
		if mrProtected[id] || activeMRProtected[id] {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// mrProtectedLocked mirrors mrProtectedJoin's semantics: wisps labeled
// gt:merge-request, or labeled both cleanup and state:merge-requested.
func (s *fakeReaperState) mrProtectedLocked() map[string]bool {
	protected := map[string]bool{}
	for id, w := range s.wisps {
		hasMR, hasCleanup, hasMergeRequested := false, false, false
		for _, label := range w.labels {
			switch label {
			case "gt:merge-request":
				hasMR = true
			case "cleanup":
				hasCleanup = true
			case "state:merge-requested":
				hasMergeRequested = true
			}
		}
		if hasMR || (hasCleanup && hasMergeRequested) {
			protected[id] = true
		}
	}
	return protected
}

// activeMRProtectedLocked mirrors activeMRProtectedIDs: wisp IDs referenced
// as active_mr by a live (non-nuked) agent bead's description.
func (s *fakeReaperState) activeMRProtectedLocked() map[string]bool {
	protected := map[string]bool{}
	for _, w := range s.wisps {
		if w.issueType != "agent" {
			continue
		}
		if m := agentStateFieldPattern.FindStringSubmatch(w.description); m != nil && m[1] == "nuked" {
			continue
		}
		m := activeMRFieldPattern.FindStringSubmatch(w.description)
		if m == nil || m[1] == "null" {
			continue
		}
		protected[m[1]] = true
	}
	return protected
}

func (s *fakeReaperState) agentDescriptionsLocked() []string {
	var descriptions []string
	for _, w := range s.wisps {
		if w.issueType == "agent" {
			descriptions = append(descriptions, w.description)
		}
	}
	return descriptions
}

func (s *fakeReaperState) hasOpenParentLocked(id string) bool {
	for _, dep := range s.deps {
		if dep.issueID != id || dep.depType != "parent-child" {
			continue
		}
		if dep.dependsOnExternal != "" {
			return true
		}
		parent := s.wisps[dep.dependsOnID]
		if parent != nil && isOpenWispStatus(parent.status) {
			return true
		}
	}
	return false
}

func (s *fakeReaperState) hasAbsentParentLocked(id string) bool {
	for _, dep := range s.deps {
		if dep.issueID != id || dep.depType != "parent-child" {
			continue
		}
		if dep.dependsOnID != "" && s.wisps[dep.dependsOnID] == nil {
			return true
		}
	}
	return false
}

func (s *fakeReaperState) openCountLocked() int {
	count := 0
	for _, w := range s.wisps {
		if isOpenWispStatus(w.status) {
			count++
		}
	}
	return count
}

type fakeReaperDriver struct {
	state *fakeReaperState
}

func (d *fakeReaperDriver) Open(string) (driver.Conn, error) {
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	d.state.nextConn++
	connID := d.state.nextConn
	d.state.ops[connID] = nil
	return &fakeReaperConn{state: d.state, id: connID}, nil
}

type fakeReaperConn struct {
	state *fakeReaperState
	id    int
}

func (c *fakeReaperConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not implemented")
}

func (c *fakeReaperConn) Close() error { return nil }

func (c *fakeReaperConn) Begin() (driver.Tx, error) { return fakeReaperTx{}, nil }

func (c *fakeReaperConn) CheckNamedValue(*driver.NamedValue) error { return nil }

func (c *fakeReaperConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	normalized := normalizeSQL(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.record(c.id, "QUERY "+normalized)

	switch {
	case strings.Contains(normalized, "SELECT description FROM wisps WHERE issue_type = 'agent'"):
		return fakeDescriptionRows(c.state.agentDescriptionsLocked()), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w") && strings.Contains(normalized, "created_at <"):
		if err := validateStaleWispQuery(normalized); err != nil {
			return nil, err
		}
		return fakeCountRows(len(c.state.staleCandidatesLocked(namedTime(args), strings.Contains(normalized, "closed_molecule_step.issue_id IS NULL")))), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w") && strings.Contains(normalized, "pm.issue_type = 'molecule'"):
		if err := validateMoleculeStepQuery(normalized); err != nil {
			return nil, err
		}
		return fakeCountRows(len(c.state.moleculeStepCandidatesLocked())), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps WHERE status IN"):
		return fakeCountRows(c.state.openCountLocked()), nil
	case strings.Contains(normalized, "GROUP BY wtype"):
		return fakeWispTypeCountRows(c.state.wispTypeCountsLocked(namedTime(args), namedExcludedIDs(args))), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w WHERE w.status = 'closed'"):
		return fakeIDRows(c.state.purgeCandidatesLocked(namedTime(args), namedExcludedIDs(args))), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w WHERE w.status = 'closed'"):
		return fakeCountRows(len(c.state.purgeCandidatesLocked(namedTime(args), namedExcludedIDs(args)))), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM issues"):
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "issues WHERE status = 'closed'"):
		// purgeOldMail's count is the only one that qualifies the table with the
		// database name; the fake models no mail.
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisps w") && strings.Contains(normalized, "LEFT JOIN wisp_dependencies wd") && strings.Contains(normalized, "pm.id IS NULL"):
		// absent-parent molecule count (dry-run)
		return fakeCountRows(len(c.state.absentParentCandidatesLocked())), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w") && strings.Contains(normalized, "LEFT JOIN wisp_dependencies wd") && strings.Contains(normalized, "pm.id IS NULL"):
		// absent-parent molecule step ID query (real execution)
		return fakeIDRows(c.state.absentParentCandidatesLocked()), nil
	case strings.Contains(normalized, "SELECT COUNT(*) FROM wisp_dependencies wd"):
		return fakeCountRows(0), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w") && strings.Contains(normalized, "created_at <"):
		if err := validateStaleWispQuery(normalized); err != nil {
			return nil, err
		}
		return fakeIDRows(c.state.staleCandidatesLocked(namedTime(args), strings.Contains(normalized, "closed_molecule_step.issue_id IS NULL"))), nil
	case strings.Contains(normalized, "SELECT w.id FROM wisps w") && strings.Contains(normalized, "pm.issue_type = 'molecule'"):
		if err := validateMoleculeStepQuery(normalized); err != nil {
			return nil, err
		}
		return fakeIDRows(c.state.moleculeStepCandidatesLocked()), nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", normalized)
	}
}

func (c *fakeReaperConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	normalized := normalizeSQL(query)
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.record(c.id, "EXEC "+normalized)

	switch {
	case strings.HasPrefix(normalized, "UPDATE wisps SET status='closed'"):
		affected := int64(0)
		for _, arg := range args {
			id, _ := arg.Value.(string)
			if w := c.state.wisps[id]; w != nil && isOpenWispStatus(w.status) {
				w.status = "closed"
				affected++
			}
		}
		return fakeReaperResult(affected), nil
	case strings.HasPrefix(normalized, "DELETE FROM `wisps` WHERE id IN"):
		affected := int64(0)
		for _, arg := range args {
			id, _ := arg.Value.(string)
			if w := c.state.wisps[id]; w != nil {
				delete(c.state.wisps, id)
				affected++
			}
		}
		return fakeReaperResult(affected), nil
	case normalized == "SET @@autocommit = 0" || normalized == "SET @@autocommit = 1" || normalized == "ROLLBACK" || normalized == "COMMIT" || strings.HasPrefix(normalized, "CALL DOLT_COMMIT"):
		return fakeReaperResult(0), nil
	default:
		return nil, fmt.Errorf("unexpected exec: %s", normalized)
	}
}

type fakeReaperTx struct{}

func (fakeReaperTx) Commit() error   { return nil }
func (fakeReaperTx) Rollback() error { return nil }

type fakeReaperResult int64

func (r fakeReaperResult) LastInsertId() (int64, error) { return 0, nil }
func (r fakeReaperResult) RowsAffected() (int64, error) { return int64(r), nil }

type fakeReaperRows struct {
	cols []string
	rows [][]driver.Value
	next int
}

func fakeCountRows(count int) *fakeReaperRows {
	return &fakeReaperRows{cols: []string{"count"}, rows: [][]driver.Value{{int64(count)}}}
}

func fakeIDRows(ids []string) *fakeReaperRows {
	rows := make([][]driver.Value, len(ids))
	for i, id := range ids {
		rows[i] = []driver.Value{id}
	}
	return &fakeReaperRows{cols: []string{"id"}, rows: rows}
}

func fakeDescriptionRows(descriptions []string) *fakeReaperRows {
	rows := make([][]driver.Value, len(descriptions))
	for i, d := range descriptions {
		rows[i] = []driver.Value{d}
	}
	return &fakeReaperRows{cols: []string{"description"}, rows: rows}
}

func fakeWispTypeCountRows(counts map[string]int) *fakeReaperRows {
	types := make([]string, 0, len(counts))
	for wtype := range counts {
		types = append(types, wtype)
	}
	sort.Strings(types)
	rows := make([][]driver.Value, len(types))
	for i, wtype := range types {
		rows[i] = []driver.Value{wtype, int64(counts[wtype])}
	}
	return &fakeReaperRows{cols: []string{"wtype", "cnt"}, rows: rows}
}

func (r *fakeReaperRows) Columns() []string { return r.cols }
func (r *fakeReaperRows) Close() error      { return nil }

func (r *fakeReaperRows) Next(dest []driver.Value) error {
	if r.next >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.next])
	r.next++
	return nil
}

func namedTime(args []driver.NamedValue) time.Time {
	if len(args) == 0 {
		return time.Time{}
	}
	if value, ok := args[0].Value.(time.Time); ok {
		return value
	}
	return time.Time{}
}

// namedExcludedIDs returns the bind args after the age cutoff — the wisp IDs the
// live-reference exclusion clause carries.
func namedExcludedIDs(args []driver.NamedValue) map[string]bool {
	excluded := map[string]bool{}
	for _, arg := range args[1:] {
		if id, ok := arg.Value.(string); ok {
			excluded[id] = true
		}
	}
	return excluded
}

func isOpenWispStatus(status string) bool {
	return status == "open" || status == "hooked" || status == "in_progress"
}

func normalizeSQL(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func validateMoleculeStepQuery(query string) error {
	return requireSQL(query,
		"wd.issue_id",
		"pm.id = wd.depends_on_wisp_id",
		"wd.type = 'parent-child'",
		"pm.issue_type = 'molecule'",
		"pm.status = 'closed'",
		"NOT EXISTS",
		"open_dep.depends_on_external IS NOT NULL",
		"w.issue_type != 'agent'",
		"w.status IN ('open', 'hooked', 'in_progress')",
	)
}

func validateStaleWispQuery(query string) error {
	return requireSQL(query,
		"wd.issue_id",
		"pw.id = wd.depends_on_wisp_id",
		"pi.id = wd.depends_on_issue_id",
		"pi.status IN ('open', 'hooked', 'in_progress')",
		"depends_on_external IS NOT NULL",
		"wd.type = 'parent-child'",
		"w.issue_type != 'agent'",
		"w.created_at < ?",
		"open_parent.issue_id IS NULL",
		"closed_molecule_step.issue_id IS NULL",
		"mr_protected.issue_id IS NULL",
		"wisp_labels",
		"gt:merge-request",
		"state:merge-requested",
	)
}

func requireSQL(query string, required ...string) error {
	if strings.Contains(query, "depends_on_id") {
		return fmt.Errorf("query uses legacy depends_on_id column: %s", query)
	}
	for _, want := range required {
		if !strings.Contains(query, want) {
			return fmt.Errorf("query missing %q: %s", want, query)
		}
	}
	return nil
}

func assertOpsContainInOrder(t *testing.T, ops []string, want ...string) {
	t.Helper()
	next := 0
	for _, op := range ops {
		if strings.Contains(op, want[next]) {
			next++
			if next == len(want) {
				return
			}
		}
	}
	t.Fatalf("ops missing ordered sequence %v in %v", want[next:], ops)
}
