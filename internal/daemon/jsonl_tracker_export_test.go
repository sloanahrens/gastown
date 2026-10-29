package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/tracker/api"
)

// trackerTown writes a town whose route table puts hq- and al- on the tracker
// and leaves gt- on Dolt, each workspace's metadata.json naming its database.
func trackerTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	routes := strings.Join([]string{
		`{"prefix":"hq-","path":".","backend":"tracker"}`,
		`{"prefix":"al-","path":"al/mayor/rig","backend":"tracker"}`,
		`{"prefix":"gt-","path":"gt/mayor/rig"}`,
	}, "\n") + "\n"
	writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"), routes)
	for dir, db := range map[string]string{
		filepath.Join(townRoot, ".beads"):                       "hq",
		filepath.Join(townRoot, "al", "mayor", "rig", ".beads"): "al",
		filepath.Join(townRoot, "gt", "mayor", "rig", ".beads"): "gt",
	} {
		writePath(t, filepath.Join(dir, "metadata.json"), `{"dolt_database":"`+db+`"}`+"\n")
	}
	return townRoot
}

// writePath writes content to path, creating its directory.
func writePath(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadTrackerRoutesReadsTheRouteTable: the backup tells which databases
// are read through the tracker from the route table alone.
func TestReadTrackerRoutesReadsTheRouteTable(t *testing.T) {
	t.Parallel()
	routes := readTrackerRoutes(trackerTown(t))
	if len(routes.problems) > 0 || len(routes.refused) > 0 {
		t.Fatalf("routes = %+v, want no problems and nothing refused", routes)
	}
	if len(routes.rigs) != 2 || routes.rigs["al"] != "al" || routes.rigs["hq"] != "hq" {
		t.Fatalf("rigs = %v, want exactly al and hq on the tracker", routes.rigs)
	}
	if _, onTracker := routes.rigs["gt"]; onTracker {
		t.Errorf("rigs = %v: gt is on Dolt and must not be on the tracker", routes.rigs)
	}
}

// TestReadTrackerRoutesPlacesWhatItCannotInNoDatabase: a tracker route the
// table cannot place is reported, and places no database on the tracker; the
// rest of the table is still read, so one unplaceable route does not refuse
// every rig the table did place.
func TestReadTrackerRoutesPlacesWhatItCannotInNoDatabase(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	if err := os.Remove(filepath.Join(townRoot, "al", "mayor", "rig", ".beads", "metadata.json")); err != nil {
		t.Fatal(err)
	}
	routes := readTrackerRoutes(townRoot)
	if len(routes.problems) != 1 || !strings.Contains(routes.problems[0], "al- is on the tracker but") {
		t.Fatalf("problems = %v, want the unplaceable al- route named", routes.problems)
	}
	if _, onTracker := routes.rigs["al"]; onTracker {
		t.Errorf("rigs = %v: no database of an unplaced route may be read through the tracker", routes.rigs)
	}
	if routes.rigs["hq"] != "hq" {
		t.Errorf("rigs = %v, want hq still placed: one unplaceable route must not unplace another", routes.rigs)
	}
}

// TestReadTrackerRoutesReportsATableItCannotOpen: a table that exists but
// cannot be opened is a problem, not an empty table — no database is placed, so
// none is read from Dolt on the strength of a routing the backup never read.
func TestReadTrackerRoutesReportsATableItCannotOpen(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	routes := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	// A symlink to itself: opening the table fails with an error that is not
	// "no such file", so the read cannot pass as a town with no routes.
	if err := os.Symlink("routes.jsonl", routes); err != nil {
		t.Fatal(err)
	}

	got := readTrackerRoutes(townRoot)
	if len(got.problems) != 1 || !strings.Contains(got.problems[0], "reading the route table") {
		t.Fatalf("problems = %v, want the table's open failure", got.problems)
	}
	if len(got.placed) != 0 {
		t.Errorf("placed = %v, want no database placed from a table that would not open", got.placed)
	}
}

// TestReadTrackerRoutesStopsAtALineItCannotRead: a malformed line fails the
// read rather than being skipped — a skipped line could be the tracker route of
// the database it named — and the lines before it still place their rigs.
func TestReadTrackerRoutesStopsAtALineItCannotRead(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"),
		`{"prefix":"gt-","path":"gt/mayor/rig"}`+"\n"+
			`{"prefix":"al-","path":"al/mayor/rig","backend":`+"\n")
	routes := readTrackerRoutes(townRoot)
	if len(routes.problems) != 1 || !strings.Contains(routes.problems[0], "routes.jsonl:2") {
		t.Fatalf("problems = %v, want the malformed line named by its place in the table", routes.problems)
	}
	if !routes.placed["gt"] {
		t.Errorf("placed = %v, want the rig the lines before the bad one named", routes.placed)
	}
	if routes.placed["al"] {
		t.Errorf("placed = %v: a database no readable line names must stay unplaced", routes.placed)
	}
}

// TestReadTrackerRoutesRefusesAStoreOrRouteItCannotRead: a route naming a store
// the backup does not know, or naming no prefix or path, is a problem rather
// than a Dolt rig — defaulting it to Dolt would export the stale copy a rig that
// moved left behind.
func TestReadTrackerRoutesRefusesAStoreOrRouteItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want string
	}{
		{"an unknown backend", `{"prefix":"al-","path":"al/mayor/rig","backend":"postgres"}`, `names the backend "postgres"`},
		{"a route with no path", `{"prefix":"al-"}`, "no prefix or no path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := trackerTown(t)
			writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"), tc.line+"\n")
			routes := readTrackerRoutes(townRoot)
			if len(routes.problems) != 1 || !strings.Contains(routes.problems[0], tc.want) {
				t.Fatalf("problems = %v, want %q", routes.problems, tc.want)
			}
			if _, onTracker := routes.rigs["al"]; onTracker {
				t.Errorf("rigs = %v: a route the backup cannot read must place no export", routes.rigs)
			}
		})
	}
}

// TestReadTrackerRoutesRefusesTwoRigsOnOneDatabase: two tracker routes naming
// one database refuse that database rather than let one rig's export be
// written over the other's, and leave the rest of the table placed.
func TestReadTrackerRoutesRefusesTwoRigsOnOneDatabase(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"),
		`{"prefix":"al-","path":"al/mayor/rig","backend":"tracker"}`+"\n"+
			`{"prefix":"bv-","path":"al/mayor/rig","backend":"tracker"}`+"\n"+
			`{"prefix":"gt-","path":"gt/mayor/rig"}`+"\n")
	routes := readTrackerRoutes(townRoot)
	if why := routes.refused["al"]; !strings.Contains(why, "both name database al") {
		t.Fatalf("refused = %v, want al refused for naming one database twice", routes.refused)
	}
	if _, onTracker := routes.rigs["al"]; onTracker {
		t.Errorf("rigs = %v: a refused database must not be exported", routes.rigs)
	}
	if _, onTracker := routes.rigs["gt"]; onTracker {
		t.Errorf("rigs = %v: gt is on Dolt and must not be on the tracker", routes.rigs)
	}
}

// TestTrackerExportRow pins the backup's line for each record of the tracker's
// export: the file it belongs to and the row written there, keyed by the
// columns of the Dolt export the backup used to write.
func TestTrackerExportRow(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	issue := api.Issue{
		ID: "al-1", Title: "first", Description: "d", Status: "open", Priority: 2, Type: "task",
		Assignee: "alice", CreatedBy: "bob", CreatedAt: at, UpdatedAt: at,
	}
	wisp := api.Issue{
		ID: "al-wisp-1", Title: "wisp", Status: "closed", Type: "molecule",
		CreatedAt: at, UpdatedAt: later, ClosedAt: &later, CloseReason: "done",
		Ephemeral: true, Payload: `{"a":1}`,
	}
	dep := api.ExportDependency{IssueID: "al-1", DependsOn: "al-2", Type: "blocks", CreatedAt: at}
	external := api.ExportDependency{
		IssueID: "al-1", DependsOn: "external:hq:hq-7", Type: "tracks", CreatedAt: at,
		Metadata: json.RawMessage(`{"k":1}`),
	}
	comment := api.Comment{ID: "c1", IssueID: "al-1", Author: "me", Text: "hi", CreatedAt: at}
	markup := api.Comment{ID: "c2", IssueID: "al-1", Author: "me", Text: "<b>a & b</b>", CreatedAt: at}

	cases := []struct {
		name      string
		rec       api.ExportRecord
		wantTable string
		want      string
		wantSkip  bool
		wantErr   string
	}{
		{
			name:      "an issue",
			rec:       api.ExportRecord{Type: api.RecordIssue, Issue: &issue},
			wantTable: "issues",
			want: `{"id":"al-1","title":"first","description":"d","status":"open","priority":2,` +
				`"issue_type":"task","assignee":"alice","created_by":"bob",` +
				`"created_at":"2026-10-29T12:00:00Z","updated_at":"2026-10-29T12:00:00Z","ephemeral":false}`,
		},
		{
			name:     "an ephemeral wisp, which the backup leaves out",
			rec:      api.ExportRecord{Type: api.RecordIssue, Issue: &wisp},
			wantSkip: true,
		},
		{
			name:      "a label",
			rec:       api.ExportRecord{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}},
			wantTable: "labels",
			want:      `{"issue_id":"al-1","label":"task"}`,
		},
		{
			name:      "a dependency",
			rec:       api.ExportRecord{Type: api.RecordDependency, Dependency: &dep},
			wantTable: "dependencies",
			want:      `{"issue_id":"al-1","depends_on_issue_id":"al-2","type":"blocks","created_at":"2026-10-29T12:00:00Z"}`,
		},
		{
			name:      "a dependency on another rig",
			rec:       api.ExportRecord{Type: api.RecordDependency, Dependency: &external},
			wantTable: "dependencies",
			want: `{"issue_id":"al-1","depends_on_external":"external:hq:hq-7","type":"tracks",` +
				`"created_at":"2026-10-29T12:00:00Z","metadata":"{\"k\":1}"}`,
		},
		{
			name:      "a comment",
			rec:       api.ExportRecord{Type: api.RecordComment, Comment: &comment},
			wantTable: "comments",
			want:      `{"id":"c1","issue_id":"al-1","author":"me","text":"hi","created_at":"2026-10-29T12:00:00Z"}`,
		},
		{
			name:      "markup in a comment, left unescaped",
			rec:       api.ExportRecord{Type: api.RecordComment, Comment: &markup},
			wantTable: "comments",
			want:      `{"id":"c2","issue_id":"al-1","author":"me","text":"<b>a & b</b>","created_at":"2026-10-29T12:00:00Z"}`,
		},
		{
			name:    "a record type the backup does not write",
			rec:     api.ExportRecord{Type: "event"},
			wantErr: "not one the backup writes",
		},
		{
			name:    "an issue record with no issue",
			rec:     api.ExportRecord{Type: api.RecordIssue},
			wantErr: "no payload",
		},
		{
			name:    "a label record with no label",
			rec:     api.ExportRecord{Type: api.RecordLabel},
			wantErr: "no payload",
		},
		{
			name:    "a dependency record with no dependency",
			rec:     api.ExportRecord{Type: api.RecordDependency},
			wantErr: "no payload",
		},
		{
			name:    "a comment record with no comment",
			rec:     api.ExportRecord{Type: api.RecordComment},
			wantErr: "no payload",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			table, row, err := trackerExportRow(c.rec)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("trackerExportRow = (%q, %v, %v), want an error naming %q", table, row, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("trackerExportRow: %v", err)
			}
			if c.wantSkip {
				if table != "" || row != nil {
					t.Fatalf("trackerExportRow = (%q, %v), want no row", table, row)
				}
				return
			}
			if table != c.wantTable {
				t.Fatalf("table = %q, want %q", table, c.wantTable)
			}
			// The backup's own encoder, so an escaping difference cannot pass.
			if line := string(trackerLine(row)); line != c.want {
				t.Errorf("line =\n%s\nwant\n%s", line, c.want)
			}
		})
	}
}

// TestExportDatabaseToJsonlTrackerRigReadsThroughTheTrackerExport: a rig the
// route table puts on the tracker is exported through the tracker export, and
// never through the Dolt SQL command.
func TestExportDatabaseToJsonlTrackerRigReadsThroughTheTrackerExport(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var gotRig string
	d.jsonlTrackerExportFn = func(_ context.Context, rig string, emit func(api.ExportRecord) error) error {
		gotRig = rig
		records := []api.ExportRecord{
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}},
			// A wisp the export carries and the backup leaves out.
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-wisp-1", Title: "wisp", Status: "open", Type: "task", Ephemeral: true, CreatedAt: at, UpdatedAt: at}},
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}},
			{Type: api.RecordDependency, Dependency: &api.ExportDependency{IssueID: "al-1", DependsOn: "al-2", Type: "blocks", CreatedAt: at}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c1", IssueID: "al-1", Author: "me", Text: "hi", CreatedAt: at}},
		}
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	}
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		t.Errorf("the Dolt export ran for table %q of a tracker rig", table)
		return 0, nil
	}

	gitRepo := t.TempDir()
	n, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true)
	if err != nil {
		t.Fatalf("exportDatabaseToJsonl: %v", err)
	}
	if gotRig != "al" {
		t.Errorf("the tracker export read rig %q, want al", gotRig)
	}
	if n != 4 {
		t.Errorf("exportDatabaseToJsonl = %d records, want 4", n)
	}
	for _, table := range trackerExportTables {
		lines, err := readLines(filepath.Join(gitRepo, "al", table+".jsonl"))
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if len(lines) != 1 {
			t.Errorf("%s.jsonl holds %d lines, want 1: %q", table, len(lines), lines)
		}
	}
	// The wisp the export carried is not in the file the pollution firewall
	// would strip it from.
	issues, err := readLines(filepath.Join(gitRepo, "al", "issues.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0], `"id":"al-1"`) {
		t.Errorf("issues.jsonl = %q, want the durable issue al-1 alone", issues)
	}
}

// TestExportDatabaseToJsonlDoltRigKeepsTheSQLExport: a rig with no tracker
// backend in the route table still exports through the Dolt SQL command.
func TestExportDatabaseToJsonlDoltRigKeepsTheSQLExport(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}

	d.jsonlTrackerExportFn = func(_ context.Context, rig string, _ func(api.ExportRecord) error) error {
		t.Errorf("the tracker export ran for Dolt rig %q", rig)
		return nil
	}
	var tables []string
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		tables = append(tables, table)
		return 1, nil
	}

	if _, err := d.exportDatabaseToJsonl("gt", t.TempDir(), t.TempDir(), true); err != nil {
		t.Fatalf("exportDatabaseToJsonl: %v", err)
	}
	if len(tables) == 0 || tables[0] != "issues" {
		t.Fatalf("tables exported = %v, want the issues table first", tables)
	}
}

// TestSyncJsonlGitBackupTrackerExportFailureRaisesTheExportAlert: a tracker
// export that fails takes the same alert path as a failed SQL export, so a
// tracker rig whose backup is stale reaches the operator.
func TestSyncJsonlGitBackupTrackerExportFailureRaisesTheExportAlert(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rec := notifyfake.New()
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: townRoot},
		notifier: rec,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, Databases: []string{"al"}},
		}},
	}
	useGitfake(t, d)
	d.jsonlTrackerExportFn = func(context.Context, string, func(api.ExportRecord) error) error {
		return errors.New("tracker: rig al unavailable")
	}

	d.syncJsonlGitBackup()

	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyJSONLExport {
		t.Fatalf("escalations = %+v, want one under %s", got, alertKeyJSONLExport)
	}
	// The phrase the failed export's error line holds: matching "al" alone
	// would pass on any word that contains it, such as "failed".
	if reason := got[0].Escalation.Reason; !strings.Contains(reason, "al: tracker export: tracker: rig al unavailable") {
		t.Errorf("escalation reason = %q, want the failed database's line naming the rig and the tracker's error", reason)
	}
}

// TestExportDatabaseToJsonlTrackerRouteTableUnreadable: a route table the
// backup cannot read places no database, so a database it cannot attribute is
// refused — it may be a tracker rig whose route did not read, and its Dolt copy
// would be the stale one — and no export of it runs.
func TestExportDatabaseToJsonlTrackerRouteTableUnreadable(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	// A directory where the table should be: every read of it fails.
	routes := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(routes, 0o755); err != nil {
		t.Fatal(err)
	}
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(context.Context, string, func(api.ExportRecord) error) error {
		t.Error("the tracker export ran with no readable route table")
		return nil
	}
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		t.Errorf("the Dolt export ran table %q for a database the unreadable route table cannot place", table)
		return 0, nil
	}

	_, err := d.exportDatabaseToJsonl("al", t.TempDir(), t.TempDir(), true)
	if err == nil || !strings.Contains(err.Error(), "reading the route table") || !strings.Contains(err.Error(), "al") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the unreadable table and al named", err)
	}
}

// TestExportDatabaseToJsonlTrackerRouteTablePartlyUnreadable: a line the table
// cannot give does not refuse the rigs the lines before it placed, and the
// database the lost line may have named is not exported from Dolt.
func TestExportDatabaseToJsonlTrackerRouteTablePartlyUnreadable(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"),
		`{"prefix":"gt-","path":"gt/mayor/rig"}`+"\n"+
			`{"prefix":"al-","path":"al/mayor/rig","backend":`+"\n")
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(context.Context, string, func(api.ExportRecord) error) error {
		t.Error("the tracker export ran for a rig the unreadable table cannot place")
		return nil
	}
	var tables []string
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		tables = append(tables, table)
		return 1, nil
	}

	if _, err := d.exportDatabaseToJsonl("gt", t.TempDir(), t.TempDir(), true); err != nil {
		t.Fatalf("exportDatabaseToJsonl(gt) = %v, want the rig the table placed exported from Dolt", err)
	}
	if len(tables) == 0 || tables[0] != "issues" {
		t.Fatalf("tables exported = %v, want the issues table first", tables)
	}
	if _, err := d.exportDatabaseToJsonl("al", t.TempDir(), t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "reading the route table") || !strings.Contains(err.Error(), "al") {
		t.Fatalf("exportDatabaseToJsonl(al) = %v, want the lost route and al named", err)
	}
}

// TestSyncJsonlGitBackupTrackerRouteTableUnreadableRaisesTheRouteAlert: the
// cycle exports no database the route table could not place, so a rig that may
// be on the tracker is not backed up from the Dolt copy it left behind, and the
// table's failure reaches the operator under its own alert key.
func TestSyncJsonlGitBackupTrackerRouteTableUnreadableRaisesTheRouteAlert(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(townRoot, ".beads", "routes.jsonl")
	if err := os.Remove(routes); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(routes, 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rec := notifyfake.New()
	d := &Daemon{
		logger:   log.New(&logs, "", 0),
		config:   &Config{TownRoot: townRoot},
		notifier: rec,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, Databases: []string{"gt"}},
		}},
	}
	useGitfake(t, d)
	d.jsonlTableExportFn = func(table string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		t.Errorf("the Dolt export ran table %q for a database the unreadable route table cannot place", table)
		return 1, nil
	}

	d.syncJsonlGitBackup()

	byKey := map[string]string{}
	for _, e := range rec.Escalations() {
		byKey[e.Escalation.Fingerprint] = e.Escalation.Reason
	}
	reason, raised := byKey[alertKeyJSONLRoute]
	if !raised || !strings.Contains(reason, "reading the route table") {
		t.Fatalf("escalations = %+v, want the route table's failure under %s", rec.Escalations(), alertKeyJSONLRoute)
	}
	// The refused database is a failed export for the cycle as well, so the
	// operator's export alert names it and its offsite copy's staleness.
	if exportReason, raised := byKey[alertKeyJSONLExport]; !raised || !strings.Contains(exportReason, "gt: tracker routes") {
		t.Errorf("escalations = %+v, want the refused database under %s", rec.Escalations(), alertKeyJSONLExport)
	}
}

// TestExportDatabaseToJsonlRefusesADatabaseTwoTrackerRoutesName: a database
// the table refuses fails its own export and no other, instead of being
// exported as whichever route came first.
func TestExportDatabaseToJsonlRefusesADatabaseTwoTrackerRoutesName(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"),
		`{"prefix":"al-","path":"al/mayor/rig","backend":"tracker"}`+"\n"+
			`{"prefix":"bv-","path":"al/mayor/rig","backend":"tracker"}`+"\n"+
			`{"prefix":"gt-","path":"gt/mayor/rig"}`+"\n")
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(context.Context, string, func(api.ExportRecord) error) error {
		t.Error("the tracker export ran for a database the table refuses")
		return nil
	}
	d.jsonlTableExportFn = func(_ string, _ beadsql.Query, _, _ string, _ time.Duration) (int, error) {
		return 1, nil
	}

	if _, err := d.exportDatabaseToJsonl("al", t.TempDir(), t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "both name database al") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the doubled route named", err)
	}
	if _, err := d.exportDatabaseToJsonl("gt", t.TempDir(), t.TempDir(), true); err != nil {
		t.Fatalf("exportDatabaseToJsonl(gt) = %v, want the Dolt rig exported", err)
	}
}

// TestExportDatabaseToJsonlTrackerDropsWispOwnedRows: the export carries a
// wisp's labels, dependencies and comments inline, and none of them reaches the
// backup's durable files — a Dolt rig's backup leaves the wisp tables out, and
// the pollution firewall would strip a wisp id from issues.jsonl one step
// later.
func TestExportDatabaseToJsonlTrackerDropsWispOwnedRows(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		records := []api.ExportRecord{
			// A wisp's rows arrive before the wisp itself: its id carries the
			// form the pollution firewall strips, which is the filter's id arm.
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-wisp-1", Label: "wisp"}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c-wisp", IssueID: "al-wisp-1", Author: "me", Text: "noise", CreatedAt: at}},
			{Type: api.RecordDependency, Dependency: &api.ExportDependency{IssueID: "al-wisp-1", DependsOn: "al-1", Type: "blocks", CreatedAt: at}},
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-wisp-1", Title: "wisp", Status: "open", Type: "task", Ephemeral: true, CreatedAt: at, UpdatedAt: at}},
			// A wisp whose id has no such form and whose rows arrive before its
			// issue record: nothing but the ephemeral flag marks it, and that
			// flag is known only once the stream ends.
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-e7", Label: "wisp"}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c-e7", IssueID: "al-e7", Author: "me", Text: "noise", CreatedAt: at}},
			{Type: api.RecordDependency, Dependency: &api.ExportDependency{IssueID: "al-e7", DependsOn: "al-1", Type: "blocks", CreatedAt: at}},
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-e7", Title: "wisp", Status: "open", Type: "task", Ephemeral: true, CreatedAt: at, UpdatedAt: at}},
			// A wisp whose id has no such form, its issue record first: the
			// ephemeral flag is the filter's other arm.
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-e9", Title: "wisp", Status: "open", Type: "task", Ephemeral: true, CreatedAt: at, UpdatedAt: at}},
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-e9", Label: "wisp"}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c-e9", IssueID: "al-e9", Author: "me", Text: "noise", CreatedAt: at}},
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}},
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c1", IssueID: "al-1", Author: "me", Text: "hi", CreatedAt: at}},
			{Type: api.RecordDependency, Dependency: &api.ExportDependency{IssueID: "al-1", DependsOn: "al-wisp-1", Type: "tracks", CreatedAt: at}},
		}
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	}

	gitRepo := t.TempDir()
	n, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true)
	if err != nil {
		t.Fatalf("exportDatabaseToJsonl: %v", err)
	}
	// The durable issue, its label, its comment and its link to the wisp.
	if n != 4 {
		t.Errorf("exportDatabaseToJsonl = %d records, want 4 durable rows", n)
	}
	for _, table := range trackerExportTables {
		lines, err := readLines(filepath.Join(gitRepo, "al", table+".jsonl"))
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, line := range lines {
			if strings.Contains(line, `"al-1"`) {
				continue
			}
			t.Errorf("%s.jsonl holds a row no durable issue owns: %s", table, line)
		}
	}
	// A durable issue's own link to a wisp is the durable issue's row: the
	// Dolt export keeps it, so the tracker export does too.
	deps, err := readLines(filepath.Join(gitRepo, "al", "dependencies.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || !strings.Contains(deps[0], `"depends_on_issue_id":"al-wisp-1"`) {
		t.Errorf("dependencies.jsonl = %q, want the durable issue's link alone", deps)
	}
}

// TestExportDatabaseToJsonlTrackerExportStreamFailureKeepsTheLastExport: a
// stream that breaks part way publishes nothing, so the failed database keeps
// the last verified export the way a failed Dolt export does, and leaves no
// staging temp behind for the commit to carry.
func TestExportDatabaseToJsonlTrackerExportStreamFailureKeepsTheLastExport(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		if err := emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}}); err != nil {
			return err
		}
		return errors.New("tracker: connection reset")
	}

	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "al")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lastExport := `{"id":"al-0","title":"last verified"}` + "\n"
	writePath(t, filepath.Join(dbDir, "issues.jsonl"), lastExport)

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the stream's failure", err)
	}
	lines, err := readLines(filepath.Join(dbDir, "issues.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"al-0"`) {
		t.Errorf("issues.jsonl = %q, want the last verified export untouched", lines)
	}
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "issues.jsonl" {
			t.Errorf("the failed export left %s behind; only the last verified export may be in the directory", e.Name())
		}
	}
}

// TestExportDatabaseToJsonlTrackerStagingFailureIsReported: a record the spool
// cannot write fails the export, so a rig is not published short of rows it
// staged.
func TestExportDatabaseToJsonlTrackerStagingFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}})
	}
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "al")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink that points at itself: opening the staging file fails, which is
	// the open half of the shared staging error.
	if err := os.Symlink("labels.spool.jsonl.tmp", filepath.Join(dbDir, "labels.spool.jsonl.tmp")); err != nil {
		t.Fatalf("making the symlink: %v", err)
	}

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "staging labels") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the staging failure named", err)
	}
}

// TestExportDatabaseToJsonlTrackerStagedOpenFailureIsReported: a staged table
// that cannot be opened fails the export rather than publishing the shards
// that did read.
func TestExportDatabaseToJsonlTrackerStagedOpenFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}})
	}
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "al")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink that points at itself: the export has no label records, so
	// nothing opens this table during the stream, and publish's open fails.
	if err := os.Symlink("labels.spool.jsonl.tmp", filepath.Join(dbDir, "labels.spool.jsonl.tmp")); err != nil {
		t.Fatalf("making the symlink: %v", err)
	}

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "reading the staged export") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the unopenable staged table named", err)
	}
}

// TestExportDatabaseToJsonlTrackerStagedReadFailureIsReported: a staged table
// that cannot be read back fails the export rather than publishing the shards
// that did read.
func TestExportDatabaseToJsonlTrackerStagedReadFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}})
	}
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "al")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The export has no comment records, so nothing opens this table's file;
	// publish reads it and fails on the directory.
	if err := os.Mkdir(filepath.Join(dbDir, "comments.spool.jsonl.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "reading the staged export") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the unreadable staged table named", err)
	}
}

// TestExportDatabaseToJsonlTrackerStagedRowWithoutOwnerIsReported: a staged
// table line that names no owning issue is a spool the export cannot trust, so
// it fails the export rather than publish a row it cannot place.
func TestExportDatabaseToJsonlTrackerStagedRowWithoutOwnerIsReported(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}})
	}
	gitRepo := t.TempDir()
	dbDir := filepath.Join(gitRepo, "al")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The export has no label records, so nothing opens this table's staging
	// file; publish reads the row this one holds, which carries no owner.
	writePath(t, filepath.Join(dbDir, "labels.spool.jsonl.tmp"), `{"issue_id":"al-1","label":"task"}`+"\n")

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "names no owning issue") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the ownerless staged row refused", err)
	}
}

// TestExportDatabaseToJsonlTrackerExportRefusesAStrayRecord: a record the
// backup has no file for fails the export rather than being dropped.
func TestExportDatabaseToJsonlTrackerExportRefusesAStrayRecord(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: "event"})
	}

	if _, err := d.exportDatabaseToJsonl("al", t.TempDir(), t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "not one the backup writes") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the stray record refused", err)
	}
}

// TestExportDatabaseToJsonlTrackerExportWriteFailureIsReported: a shard that
// cannot be written fails the database's export, so the cycle counts it as a
// failed database rather than an empty one.
func TestExportDatabaseToJsonlTrackerExportWriteFailureIsReported(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	d, _ := backupTestDaemon(t)
	d.config = &Config{TownRoot: townRoot}
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		return emit(api.ExportRecord{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}})
	}
	gitRepo := t.TempDir()
	// A directory where the shard's temp belongs: the write cannot happen, so
	// the export is reported failed rather than counted as empty.
	if err := os.MkdirAll(filepath.Join(gitRepo, "al", "issues.jsonl.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := d.exportDatabaseToJsonl("al", gitRepo, t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "issues") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the unwritable issues shard named", err)
	}
}

// TestVerifyExportCountsRebaselinesARigThatMovesToTheTracker: the tracker
// export counts four tables where Dolt counted seven, so a rig that moves to
// the tracker drops by whatever events, config and metadata held. That drop is
// a new baseline, not a spike — and a tracker count that drops afterwards is
// still a spike, because the two tracker counts are comparable.
func TestVerifyExportCountsRebaselinesARigThatMovesToTheTracker(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	rec := notifyfake.New()
	d.notifier = rec
	gitRepo := t.TempDir()
	initEmptyGitRepo(t, d, gitRepo)
	// The Dolt-era level: 100 records over the seven tables.
	if err := os.MkdirAll(filepath.Join(gitRepo, "al"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitBackup(t, d, gitRepo, "al", 100)

	d.jsonlTrackerDBs = map[string]bool{"al": true}
	spikes, err := d.verifyExportCounts(gitRepo, []string{"al"}, map[string]int{"al": 4}, 0.20)
	if err != nil {
		t.Fatalf("verifyExportCounts: %v", err)
	}
	if len(spikes) != 0 {
		t.Fatalf("spikes = %+v, want the first tracker count rebaselined", spikes)
	}
	// The reset is raised, not only logged, so a short export is seen.
	flipKey := alertKeyJSONLStoreFlip + "al"
	escalations := rec.Escalations()
	if len(escalations) != 1 || escalations[0].Escalation.Fingerprint != flipKey || escalations[0].Escalation.Severity != "low" ||
		!strings.Contains(escalations[0].Escalation.Reason, "4 records (was 100)") {
		t.Fatalf("escalations = %+v, want one low store-flip alert for al naming 4 records (was 100)", escalations)
	}

	// A tracker-measured level and a tracker-measured count are the same
	// measure: a drop is checked, and halts.
	commitAll(t, d, gitRepo, seedSubjectPrefix+"al=100 [TRACKER: al]")
	spikes, err = d.verifyExportCounts(gitRepo, []string{"al"}, map[string]int{"al": 40}, 0.20)
	if err != nil {
		t.Fatalf("verifyExportCounts: %v", err)
	}
	if len(spikes) != 1 || spikes[0].Current != 40 || spikes[0].Previous != 100 {
		t.Fatalf("spikes = %+v, want the tracker count's drop to be caught", spikes)
	}
	// The same-store comparison clears the flip alert.
	cleared := false
	for _, c := range rec.Clears() {
		cleared = cleared || slices.Contains(c.Fingerprints, flipKey)
	}
	if !cleared {
		t.Errorf("clears = %+v, want the store-flip alert cleared once al compares against its own store", rec.Clears())
	}
}

// TestVerifyExportCountsHaltsAnEmptyExportOnAStoreFlip: nothing is comparable
// across a flip, but a database that had records never exports none, so an
// empty tracker export halts like a drop instead of becoming the new level. A
// database that had no records has nothing to lose and is rebaselined.
func TestVerifyExportCountsHaltsAnEmptyExportOnAStoreFlip(t *testing.T) {
	t.Parallel()
	d, _ := backupTestDaemon(t)
	rec := notifyfake.New()
	d.notifier = rec
	gitRepo := t.TempDir()
	initEmptyGitRepo(t, d, gitRepo)
	for _, db := range []string{"al", "empty"} {
		if err := os.MkdirAll(filepath.Join(gitRepo, db), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	commitBackup(t, d, gitRepo, "al", 100)
	commitBackup(t, d, gitRepo, "empty", 0)

	d.jsonlTrackerDBs = map[string]bool{"al": true, "empty": true}
	spikes, err := d.verifyExportCounts(gitRepo, []string{"al", "empty"}, map[string]int{"al": 0, "empty": 0}, 0.20)
	if err != nil {
		t.Fatalf("verifyExportCounts: %v", err)
	}
	if len(spikes) != 1 || spikes[0].DB != "al" || spikes[0].Previous != 100 || spikes[0].Current != 0 {
		t.Fatalf("spikes = %+v, want only al's empty export (was 100) to halt", spikes)
	}
	for _, c := range rec.Escalations() {
		if c.Escalation.Fingerprint == alertKeyJSONLStoreFlip+"al" {
			t.Errorf("al halted and was also rebaselined with a flip alert: %+v", c.Escalation)
		}
	}
}

// TestSyncJsonlGitBackupTrackerFlipCommitsAMarkedCount: the tick a rig flips
// commits its count marked with the store it was measured from, so the next
// tick compares it with the right level, and the accept hint an operator is
// given carries the same mark.
func TestSyncJsonlGitBackupTrackerFlipCommitsAMarkedCount(t *testing.T) {
	t.Parallel()
	townRoot := trackerTown(t)
	if err := os.MkdirAll(filepath.Join(townRoot, ".dolt-data"), 0o755); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	rec := notifyfake.New()
	d, f := backupTestDaemon(t)
	d.logger = log.New(&logs, "", 0)
	root := t.TempDir()
	remote := filepath.Join(root, "origin.git")
	f.InitBare(t, remote)
	f.Commit(t, remote, "main", "seed", map[string]string{"README": "init\n"})
	gitRepo := filepath.Join(root, "repo")
	f.Clone(t, remote, gitRepo)
	d.config = &Config{TownRoot: townRoot}
	d.notifier = rec
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		JsonlGitBackup: &JsonlGitBackupConfig{Enabled: true, GitRepo: gitRepo, Databases: []string{"al"}},
	}}
	// The Dolt-era level the flip tick starts from: 100 records over the seven
	// tables.
	if err := os.MkdirAll(filepath.Join(gitRepo, "al"), 0o755); err != nil {
		t.Fatal(err)
	}
	commitBackup(t, d, gitRepo, "al", 100)

	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	d.jsonlTrackerExportFn = func(_ context.Context, _ string, emit func(api.ExportRecord) error) error {
		for _, rec := range []api.ExportRecord{
			{Type: api.RecordIssue, Issue: &api.Issue{ID: "al-1", Title: "first", Status: "open", Type: "task", CreatedAt: at, UpdatedAt: at}},
			{Type: api.RecordLabel, Label: &api.ExportLabel{IssueID: "al-1", Label: "task"}},
			{Type: api.RecordDependency, Dependency: &api.ExportDependency{IssueID: "al-1", DependsOn: "al-2", Type: "blocks", CreatedAt: at}},
			{Type: api.RecordComment, Comment: &api.Comment{ID: "c1", IssueID: "al-1", Author: "me", Text: "hi", CreatedAt: at}},
		} {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	}

	d.syncJsonlGitBackup()

	// The flip is raised once, at low severity: it informs, it does not halt.
	flipKey := alertKeyJSONLStoreFlip + "al"
	if got := rec.Escalations(); len(got) != 1 || got[0].Escalation.Fingerprint != flipKey || got[0].Escalation.Severity != "low" {
		t.Fatalf("the flip tick escalated %+v, want one low store-flip alert for al", got)
	}
	subjects := backupSubjects(t, d, gitRepo)
	if len(subjects) != 3 {
		t.Fatalf("subjects = %q, want the seed, the Dolt-era level and the flip tick's commit; log:\n%s", subjects, logs.String())
	}
	if want := "al=4 [TRACKER: al]"; !strings.Contains(subjects[0], want) {
		t.Errorf("flip commit subject = %q, want it to contain %q", subjects[0], want)
	}
	if n := strings.Count(logs.String(), "the count's store changed (dolt to tracker)"); n != 1 {
		t.Errorf("rebaseline logged %d time(s), want once; log:\n%s", n, logs.String())
	}

	// The next tick reads a tracker level, so it compares rather than
	// rebaselining again.
	d.syncJsonlGitBackup()
	if got := rec.Escalations(); len(got) != 1 {
		t.Fatalf("the tick after the flip escalated again: %+v", got)
	}
	cleared := false
	for _, c := range rec.Clears() {
		cleared = cleared || slices.Contains(c.Fingerprints, flipKey)
	}
	if !cleared {
		t.Errorf("clears = %+v, want the tick after the flip to clear the store-flip alert", rec.Clears())
	}
	if n := strings.Count(logs.String(), "the count's store changed"); n != 1 {
		t.Errorf("the tick after the flip rebaselined again (%d logs); log:\n%s", n, logs.String())
	}

	d.jsonlTrackerDBs = map[string]bool{"al": true}
	if hint := d.spikeAcceptHint(gitRepo, []string{"al"}, map[string]int{"al": 4}); !strings.Contains(hint, "[TRACKER: al]") {
		t.Errorf("accept hint = %q, want the subject it lands to mark the count's store", hint)
	}
}

// TestTrackerExporterRefusesSettingsItCannotReach: a tracker rig with no
// endpoint, no token or an unusable endpoint fails its export and says what is
// wrong; it never falls back to the Dolt database the rig left behind.
func TestTrackerExporterRefusesSettingsItCannotReach(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"no endpoint", nil, "TRACKER_URL is not set"},
		{"an endpoint with no token", map[string]string{"TRACKER_URL": "https://tracker.example"}, "TRACKER_TOKEN"},
		{
			"an endpoint that is not a URL",
			map[string]string{"TRACKER_URL": "not-a-url", "TRACKER_TOKEN": "secret"},
			"is not a URL with a host",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			export := trackerExporterFromEnv(func(key string) string { return c.env[key] })
			err := export(t.Context(), "al", func(api.ExportRecord) error { return nil })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("export = %v, want an error naming %q", err, c.want)
			}
		})
	}
}

// readLines returns the non-empty lines of path.
func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}
