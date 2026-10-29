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

// TestTrackerRigByDatabaseReadsTheRouteTable: the backup tells which databases
// are read through the tracker from the route table alone, and a route that is
// on the tracker but cannot be placed in a database fails instead of leaving
// that database on the Dolt path.
func TestTrackerRigByDatabaseReadsTheRouteTable(t *testing.T) {
	t.Parallel()
	rigs, err := trackerRigByDatabase(trackerTown(t))
	if err != nil {
		t.Fatalf("trackerRigByDatabase: %v", err)
	}
	if len(rigs) != 2 || rigs["al"] != "al" || rigs["hq"] != "hq" {
		t.Fatalf("trackerRigByDatabase = %v, want exactly al and hq on the tracker", rigs)
	}
	if _, onTracker := rigs["gt"]; onTracker {
		t.Errorf("rigs = %v: gt is on Dolt and must not be on the tracker", rigs)
	}
}

func TestTrackerRigByDatabaseRefusesRoutesItCannotPlace(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, townRoot string)
		want    string
	}{
		{
			name: "a tracker route whose workspace names no database",
			prepare: func(t *testing.T, townRoot string) {
				t.Helper()
				if err := os.Remove(filepath.Join(townRoot, "al", "mayor", "rig", ".beads", "metadata.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "al- is on the tracker but",
		},
		{
			name: "two tracker routes naming one database",
			prepare: func(t *testing.T, townRoot string) {
				t.Helper()
				writePath(t, filepath.Join(townRoot, ".beads", "routes.jsonl"),
					`{"prefix":"al-","path":"al/mayor/rig","backend":"tracker"}`+"\n"+
						`{"prefix":"bv-","path":"al/mayor/rig","backend":"tracker"}`+"\n")
			},
			want: "both name database al",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			townRoot := trackerTown(t)
			c.prepare(t, townRoot)
			rigs, err := trackerRigByDatabase(townRoot)
			if err == nil {
				t.Fatalf("trackerRigByDatabase = %v with no error, want it refused", rigs)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name %q", err, c.want)
			}
		})
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
	if reason := got[0].Escalation.Reason; !strings.Contains(reason, "al") || !strings.Contains(reason, "unavailable") {
		t.Errorf("escalation reason = %q, want the rig and the tracker's error", reason)
	}
}

// TestExportDatabaseToJsonlTrackerRouteTableUnreadable: a route table the
// backup cannot read fails the database's export instead of reading the rig
// through Dolt, so no rig's backend is guessed.
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

	if _, err := d.exportDatabaseToJsonl("gt", t.TempDir(), t.TempDir(), true); err == nil ||
		!strings.Contains(err.Error(), "reading the route table") {
		t.Fatalf("exportDatabaseToJsonl = %v, want the unreadable route table named", err)
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
