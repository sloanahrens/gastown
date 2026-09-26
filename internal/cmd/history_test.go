package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubHistoryQuerier supplies the four facts without a Dolt server, so the
// verdict and the disclosure it drives can be exercised in a plain unit test.
type stubHistoryQuerier struct {
	floorFact   historyFloor
	floorErr    error
	countFact   int64
	countErr    error
	createdFact historySnapshot
	spanFact    historyEventSpan
	spanErr     error
	// ignoredTable stands in for a dolt_ignore entry: set it to the table the
	// createdFact came from to make the stub report that table unversioned.
	ignoredTable string
	versionedErr error
}

func (s stubHistoryQuerier) floor(context.Context, string) (historyFloor, error) {
	return s.floorFact, s.floorErr
}

func (s stubHistoryQuerier) commitCount(context.Context, string) (int64, error) {
	return s.countFact, s.countErr
}

func (s stubHistoryQuerier) beadCreated(context.Context, string, string) (historySnapshot, error) {
	return s.createdFact, nil
}

func (s stubHistoryQuerier) eventSpan(context.Context, string, string) (historyEventSpan, error) {
	if s.spanErr != nil {
		// Mirrors doltQuerier: a failed read returns no span, not a partial one.
		return historyEventSpan{}, s.spanErr
	}
	return s.spanFact, nil
}

func (s stubHistoryQuerier) tableVersioned(_ context.Context, _, table string) (bool, error) {
	if s.versionedErr != nil {
		return false, s.versionedErr
	}
	return table != s.ignoredTable, nil
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}

// gt-de6b's actual numbers: a bead created 2026-09-09 whose retained commits
// start at the 2026-09-19 flatten of the gt database.
func gtDe6bStub(t *testing.T) stubHistoryQuerier {
	t.Helper()
	return stubHistoryQuerier{
		createdFact: historySnapshot{
			Created: mustTime(t, "2026-09-09T19:46:58Z"),
			Table:   "issues",
			Found:   true,
		},
		floorFact: historyFloor{
			At:      mustTime(t, "2026-09-19T08:03:13Z"),
			Commit:  "abcdef1234567890",
			Message: "maintain: flatten gt history",
			Found:   true,
		},
		countFact: 5138,
		spanFact: historyEventSpan{
			Count:    8,
			Earliest: mustTime(t, "2026-09-09T19:46:58Z"),
			Latest:   mustTime(t, "2026-09-22T15:09:35Z"),
			Found:    true,
		},
	}
}

func TestHistoryReportTruncatedByFloor(t *testing.T) {
	t.Parallel()
	report, err := buildHistoryReport(context.Background(), gtDe6bStub(t), "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}

	if !report.Truncated {
		t.Error("want Truncated: the bead predates the dolt_log floor")
	}
	if want := "2026-09-19 08:03:13 UTC"; report.MissingFrom != want {
		t.Errorf("MissingFrom = %q, want %q", report.MissingFrom, want)
	}
	if want := "9d12h"; report.MissingSpan != want {
		t.Errorf("MissingSpan = %q, want %q", report.MissingSpan, want)
	}
	// The events table is the point: the pre-floor span is still readable.
	if !report.EventsReachFloor {
		t.Error("want EventsReachFloor: events reach back to the bead's creation")
	}
	if report.Events != 8 {
		t.Errorf("Events = %d, want 8", report.Events)
	}
	if want := "bd history gt-de6b --events"; report.RecoveryCommand != want {
		t.Errorf("RecoveryCommand = %q, want %q", report.RecoveryCommand, want)
	}
	if report.FloorMessage != "maintain: flatten gt history" {
		t.Errorf("FloorMessage = %q", report.FloorMessage)
	}
	if report.Commits != 5138 {
		t.Errorf("Commits = %d, want 5138", report.Commits)
	}
}

// A bead created after the floor loses nothing: every commit since its
// creation is retained, so bd history covers its whole life.
func TestHistoryReportCompleteInsideWindow(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.createdFact.Created = mustTime(t, "2026-09-20T00:00:00Z")
	stub.spanFact = historyEventSpan{Count: 2, Earliest: mustTime(t, "2026-09-20T00:00:00Z"), Found: true}

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-new")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if report.Truncated {
		t.Error("want not Truncated: the bead was created after the floor")
	}
	if report.MissingSpan != "" || report.MissingFrom != "" {
		t.Errorf("a complete history must not report a missing span: %q / %q",
			report.MissingFrom, report.MissingSpan)
	}
	// The floor is still reported: it is a property of the database, and a
	// caller comparing two beads' verdicts needs both floors.
	if report.FloorAt == "" {
		t.Error("want FloorAt reported even when this bead is inside the window")
	}
}

// wispStub is a wisp bead as the database reports one: the row came from wisps,
// which dolt_ignore keeps out of every commit, and its trail is in wisp_events.
func wispStub(t *testing.T) stubHistoryQuerier {
	t.Helper()
	stub := gtDe6bStub(t)
	stub.createdFact.Table = "wisps"
	stub.ignoredTable = "wisps"
	return stub
}

// A wisp has no snapshot in any commit, so no floor can make its history
// complete: bd history reports nothing for it. Reading the floor as the snapshot
// start turns "nothing was ever recorded" into "everything was kept", which is
// the wrong verdict this pins (gt-ivlh).
func TestHistoryReportWispIsNotComplete(t *testing.T) {
	t.Parallel()
	stub := wispStub(t)
	// Created after the floor: the case the pre-fix verdict called COMPLETE.
	stub.createdFact.Created = mustTime(t, "2026-09-20T00:00:00Z")

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-wisp-qnu1")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if report.Versioned {
		t.Error("want Versioned false for a row in the dolt_ignored wisps table")
	}
	if report.Truncated {
		t.Error("a wisp is not truncated: no commit ever held a snapshot of it")
	}
	if report.MissingFrom != "" || report.MissingSpan != "" {
		t.Errorf("an unversioned bead must not report a missing span: %q / %q",
			report.MissingFrom, report.MissingSpan)
	}
	if report.BeadTable != "wisps" {
		t.Errorf("BeadTable = %q, want wisps", report.BeadTable)
	}
	// The floor stays a reported fact: it is a property of the database, and a
	// caller comparing beads needs it whichever table they live in.
	if report.FloorAt == "" {
		t.Error("want FloorAt reported for a wisp too")
	}

	var out bytes.Buffer
	if err := renderHistoryReport(&out, report); err != nil {
		t.Fatalf("renderHistoryReport: %v", err)
	}
	rendered := out.String()
	if strings.Contains(rendered, "COMPLETE") {
		t.Errorf("wisp report claims COMPLETE:\n%s", rendered)
	}
	for _, want := range []string{"NOT VERSIONED", "wisps", "bd history gt-wisp-qnu1 --events"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("wisp report does not mention %q:\n%s", want, rendered)
		}
	}
}

// The other direction: a wisp older than the floor loses nothing to the flatten
// either — the span the floor cut was never inside a commit — so reporting it as
// truncated invents a loss and points at a recovery that recovers nothing.
func TestHistoryReportWispOlderThanFloorIsNotTruncated(t *testing.T) {
	t.Parallel()
	// gtDe6bStub's bead predates its floor by 9d12h.
	report, err := buildHistoryReport(context.Background(), wispStub(t), "gt", "gt-wisp-old")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if report.Truncated {
		t.Errorf("want not Truncated: the floor cannot truncate rows no commit holds (invented loss %s)",
			report.MissingSpan)
	}

	var out bytes.Buffer
	if err := renderHistoryReport(&out, report); err != nil {
		t.Fatalf("renderHistoryReport: %v", err)
	}
	if rendered := out.String(); strings.Contains(rendered, "TRUNCATED") {
		t.Errorf("wisp report claims TRUNCATED:\n%s", rendered)
	}
}

// Table versioning is a premise of the verdict, so an unreadable dolt_ignore
// fails the command rather than falling through to the issues answer for a row
// that may not be in issues.
func TestHistoryReportUnreadableDoltIgnoreFails(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.versionedErr = errStubVersioning

	if _, err := buildHistoryReport(context.Background(), stub, "gt", "gt-de6b"); err == nil {
		t.Fatal("want an error when dolt_ignore cannot be read")
	}
}

// Events that stop at the floor do not rescue the missing span, and the
// command must not claim they do.
func TestHistoryReportEventsStopAtFloor(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.spanFact = historyEventSpan{
		Count:    3,
		Earliest: mustTime(t, "2026-09-19T08:03:13Z"),
		Latest:   mustTime(t, "2026-09-19T09:00:00Z"),
		Found:    true,
	}
	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if !report.Truncated {
		t.Error("want Truncated")
	}
	if report.EventsReachFloor {
		t.Error("events starting at the floor do not cover the span before it")
	}
}

// An unreadable events table must not suppress the floor disclosure: the
// truncation is the defect, and it is knowable without the events table.
func TestHistoryReportSurvivesUnreadableEvents(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.spanErr = errStubEvents

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if !report.EventsUnavailable {
		t.Error("want EventsUnavailable when the events query fails")
	}
	if !report.Truncated {
		t.Error("want Truncated even when the events table cannot be read")
	}
	if report.EventsReachFloor {
		t.Error("must not claim the events reach the floor when they were not read")
	}

	var out bytes.Buffer
	if err := renderHistoryReport(&out, report); err != nil {
		t.Fatalf("renderHistoryReport: %v", err)
	}
	if !strings.Contains(out.String(), "TRUNCATED") {
		t.Errorf("rendered report drops the truncation when events are unreadable:\n%s", out.String())
	}
}

func TestHistoryReportUnknownBead(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.createdFact = historySnapshot{}

	if _, err := buildHistoryReport(context.Background(), stub, "gt", "gt-nope"); err == nil {
		t.Fatal("want an error for a bead that is not in the database")
	}
}

// The defect this command exists to fix: truncated and complete history were
// indistinguishable to the caller. Pin the disclosure.
func TestRenderHistoryTruncatedDisclosesFloorAndRecovery(t *testing.T) {
	t.Parallel()
	report, err := buildHistoryReport(context.Background(), gtDe6bStub(t), "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}

	var out bytes.Buffer
	if err := renderHistoryReport(&out, report); err != nil {
		t.Fatalf("renderHistoryReport: %v", err)
	}
	rendered := out.String()

	for _, want := range []string{
		"TRUNCATED",
		"2026-09-19 08:03:13 UTC", // the floor itself
		"9d12h",                   // how much of the bead's life is omitted
		"maintain: flatten gt history",
		"not evidence of absence",
		"bd history gt-de6b --events",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("truncated report does not mention %q:\n%s", want, rendered)
		}
	}
}

func TestRenderHistoryCompleteSaysSo(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.createdFact.Created = mustTime(t, "2026-09-20T00:00:00Z")

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}

	var out bytes.Buffer
	if err := renderHistoryReport(&out, report); err != nil {
		t.Fatalf("renderHistoryReport: %v", err)
	}
	rendered := out.String()

	if !strings.Contains(rendered, "COMPLETE") {
		t.Errorf("complete report does not say so:\n%s", rendered)
	}
	if strings.Contains(rendered, "TRUNCATED") {
		t.Errorf("complete report claims truncation:\n%s", rendered)
	}
}

// The floor is the second-oldest commit, not the oldest: a flatten soft-resets
// to the repository root, so the root survives every flatten and is the one
// commit that is never truncated. Reading the root as the floor reports
// "history is complete" on exactly the databases that were flattened.
func TestFloorFromSkipsRepositoryRoot(t *testing.T) {
	t.Parallel()
	root := historyCommit{
		Hash:    "dhlngv9d5fjc05ktipuu0ao10cs221rb",
		At:      mustTime(t, "2026-09-08T01:47:26Z"),
		Message: "Initialize data repository",
	}
	flatten := historyCommit{
		Hash:    "ef23nefj0000000000000000000000",
		At:      mustTime(t, "2026-09-19T08:03:13Z"),
		Message: "maintain: flatten gt history",
	}
	later := historyCommit{
		Hash:    "aaa",
		At:      mustTime(t, "2026-09-20T00:00:00Z"),
		Message: "bd: update gt-de6b",
	}

	tests := []struct {
		name    string
		commits []historyCommit
		want    historyFloor
	}{
		{
			name:    "flattened database reports the flatten commit",
			commits: []historyCommit{root, flatten, later},
			want:    historyFloor{At: flatten.At, Commit: flatten.Hash, Message: flatten.Message, Found: true},
		},
		{
			name:    "root only has no floor to report",
			commits: []historyCommit{root},
			want:    historyFloor{},
		},
		{
			name:    "empty log has no floor to report",
			commits: nil,
			want:    historyFloor{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := floorFrom(tt.commits)
			if got != tt.want {
				t.Errorf("floorFrom() = %+v, want %+v", got, tt.want)
			}
			if got.Found && got.At.Equal(root.At) {
				t.Error("the repository root was reported as the floor")
			}
		})
	}
}

// A bead created in the oldest retained commit is complete: its snapshot is the
// commit itself. The commit is dated after the row it wrote, so the comparison
// needs slack or every such bead reads as truncated.
func TestHistoryReportCreatedInFloorCommitIsComplete(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.createdFact.Created = mustTime(t, "2026-09-19T08:03:13Z") // same write as the floor
	stub.spanFact = historyEventSpan{
		Count:    1,
		Earliest: mustTime(t, "2026-09-19T08:03:13Z"),
		Latest:   mustTime(t, "2026-09-19T08:03:13Z"),
		Found:    true,
	}

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-edge")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if report.Truncated {
		t.Errorf("a bead created in the floor commit must not read as truncated (gap %s)",
			report.MissingSpan)
	}
}

// A database with nothing but the repository root has no retained content, so
// there is no floor to compare against and no truncation to claim.
func TestHistoryReportNoFloorIsNotTruncated(t *testing.T) {
	t.Parallel()
	stub := gtDe6bStub(t)
	stub.floorFact = historyFloor{}

	report, err := buildHistoryReport(context.Background(), stub, "gt", "gt-de6b")
	if err != nil {
		t.Fatalf("buildHistoryReport: %v", err)
	}
	if report.Truncated {
		t.Error("want not Truncated when no floor was found")
	}
	if report.FloorAt != "" {
		t.Errorf("FloorAt = %q, want empty", report.FloorAt)
	}
}

func TestFormatHistoryGap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		gap  time.Duration
		want string
	}{
		{"sub-minute", 30 * time.Second, "<1m"},
		{"hours only", 5 * time.Hour, "5h"},
		{"just under a day", 23 * time.Hour, "23h"},
		{"days and hours", 9*24*time.Hour + 12*time.Hour, "9d12h"},
		{"exact days", 3 * 24 * time.Hour, "3d0h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatHistoryGap(tt.gap); got != tt.want {
				t.Errorf("formatHistoryGap(%v) = %q, want %q", tt.gap, got, tt.want)
			}
		})
	}
}

// writeHistoryTown lays out the minimum a bead→database lookup reads: the
// town routes and one rig whose metadata.json names its database.
func writeHistoryTown(t *testing.T, routes string, rigDB map[string]string) string {
	t.Helper()
	townRoot := t.TempDir()

	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatal(err)
	}
	for rig, db := range rigDB {
		rigBeads := filepath.Join(townRoot, rig, "mayor", "rig", ".beads")
		if err := os.MkdirAll(rigBeads, 0755); err != nil {
			t.Fatal(err)
		}
		meta := `{"dolt_database":"` + db + `"}`
		if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), []byte(meta), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return townRoot
}

func TestHistoryDatabaseForBead(t *testing.T) {
	t.Parallel()
	const routes = `{"prefix":"hq-","path":"."}
{"prefix":"gt-","path":"gastown/mayor/rig"}
{"prefix":"be-","path":"beads/mayor/rig"}
`
	// The gastown rig's database is "gt", not "gastown": the two differ, and a
	// lookup that assumed they matched would query a database that does not
	// exist and report nothing.
	townRoot := writeHistoryTown(t, routes, map[string]string{"gastown": "gt", "beads": "be"})

	tests := []struct {
		name   string
		beadID string
		want   string
	}{
		{"rig bead", "gt-de6b", "gt"},
		{"other rig bead", "be-abc123", "be"},
		{"longest prefix wins", "gt-wisp-3f8d", "gt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := historyDatabaseForBead(townRoot, tt.beadID)
			if err != nil {
				t.Fatalf("historyDatabaseForBead(%s): %v", tt.beadID, err)
			}
			if got != tt.want {
				t.Errorf("historyDatabaseForBead(%s) = %q, want %q", tt.beadID, got, tt.want)
			}
		})
	}

	// An unrouted prefix must not fall back to another database: a verdict
	// about a bead that is not in the database queried is worse than no answer.
	if _, err := historyDatabaseForBead(townRoot, "zz-abc123"); err == nil {
		t.Error("want an error for a prefix that is not routed")
	}
}

func TestHistoryDatabaseForBeadTownLevel(t *testing.T) {
	t.Parallel()
	townRoot := writeHistoryTown(t, `{"prefix":"hq-","path":"."}`, nil)

	// Town-level beads live in the town .beads directory, which carries its own
	// metadata.json naming the hq database.
	meta := `{"dolt_database":"hq"}`
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "metadata.json"), []byte(meta), 0644); err != nil {
		t.Fatal(err)
	}

	got, err := historyDatabaseForBead(townRoot, "hq-abc123")
	if err != nil {
		t.Fatalf("historyDatabaseForBead: %v", err)
	}
	if got != "hq" {
		t.Errorf("got %q, want hq", got)
	}
}

func TestHistoryDatabaseForBeadMissingMetadata(t *testing.T) {
	t.Parallel()
	townRoot := writeHistoryTown(t, `{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n", nil)

	// The route exists but the rig has no metadata.json, so the database name
	// is unknowable. Guessing would be silent misresolution.
	if _, err := historyDatabaseForBead(townRoot, "gt-de6b"); err == nil {
		t.Error("want an error when the rig's metadata.json names no database")
	}
}

func TestHistoryCommandRegistered(t *testing.T) {
	t.Parallel()
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() != "history" {
			continue
		}
		if cmd.GroupID != GroupDiag {
			t.Errorf("expected GroupDiag, got %s", cmd.GroupID)
		}
		if cmd.Flags().Lookup("json") == nil {
			t.Error("expected --json flag")
		}
		return
	}
	t.Fatal("history command not registered on rootCmd")
}

// errStubEvents stands in for a failed events query.
var errStubEvents = errors.New("events table unavailable")

// errStubVersioning stands in for a failed dolt_ignore read.
var errStubVersioning = errors.New("dolt_ignore unavailable")
