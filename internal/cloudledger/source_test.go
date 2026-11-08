package cloudledger_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/cloudledger/ledgertest"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dashboard"
)

// ledgerNow is the clock every fixture is written against.
var ledgerNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ledgerFixture is one ledger's records, shared by the file fixture and the fake
// Postgres row so the two sources read the same data.
type ledgerFixture struct {
	reportJSON   string
	projectsJSON []byte
	findingsJSON []byte
	heartbeat    time.Time
	releases     map[string][]string
}

func newLedgerFixture(t *testing.T) ledgerFixture {
	t.Helper()
	projects := []string{"alpha", "beta"}
	findings := []map[string]any{
		{"ID": "f1", "Kind": "orphan_resource", "Severity": "urgent", "Project": "alpha", "Resource": "run/api", "Detail": "no stack", "ObservedAt": "2026-11-08T11:58:00Z"},
		{"ID": "f2", "Kind": "state_missing", "Severity": "normal", "Project": "beta", "Resource": "state/db", "Detail": "no checkpoint", "ObservedAt": "2026-11-08T11:58:30Z"},
	}
	report := map[string]any{
		"SchemaVersion": 1,
		"RunID":         "98e841ea48f17737c2bc79a4f1e88839",
		"StartedAt":     "2026-11-08T11:55:00Z",
		"FinishedAt":    "2026-11-08T11:59:00Z",
		"Projects":      projects,
		"Findings":      findings,
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	projectsJSON, err := json.Marshal(projects)
	if err != nil {
		t.Fatal(err)
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	return ledgerFixture{
		reportJSON:   string(raw),
		projectsJSON: projectsJSON,
		findingsJSON: findingsJSON,
		heartbeat:    ledgerNow.Add(-time.Minute),
		releases: map[string][]string{
			"alpha": {
				`{"time":"2026-11-08T10:00:00Z","app":"alpha","action":"deploy","reason":"green","commit":"abc123","version":"v1.2.3"}`,
				`{"time":"2026-11-08T11:30:00Z","app":"alpha","action":"skip","reason":"app_off","commit":"abc124","version":"v1.2.4"}`,
			},
			"beta": {
				`{"time":"2026-11-08T09:00:00Z","app":"beta","action":"deploy","reason":"green","commit":"def456","version":"v0.9.0"}`,
			},
		},
	}
}

// fileDirs writes the fixture as the file source lays it out and returns the
// reports directory, the releases directory and the off markers directory.
func fileDirs(t *testing.T, f ledgerFixture) (reports, releases, off string) {
	t.Helper()
	root := t.TempDir()
	reports, releases, off = filepath.Join(root, "reports"), filepath.Join(root, "releases"), filepath.Join(root, "off")
	writeFile(t, filepath.Join(reports, "report.json"), f.reportJSON)
	writeFile(t, filepath.Join(reports, "heartbeat"), f.heartbeat.Format(time.RFC3339))
	for app, lines := range f.releases {
		writeFile(t, filepath.Join(releases, app+".jsonl"), strings.Join(lines, "\n")+"\n")
	}
	writeFile(t, filepath.Join(off, "alpha.off"), "since=2026-11-08T11:00:00Z by=operator env=prod reason=drill\n")
	return reports, releases, off
}

// pgLedger is the same fixture behind the fake driver.
func pgLedger(t *testing.T, f ledgerFixture) *ledgertest.Ledger {
	t.Helper()
	l := &ledgertest.Ledger{
		Report: &ledgertest.ReportRow{SchemaVersion: 1, Projects: f.projectsJSON, Findings: f.findingsJSON},
	}
	hb := f.heartbeat
	l.Heartbeat = &hb
	l.Releases = map[string][]ledgertest.ReleaseRow{}
	for app, lines := range f.releases {
		for _, line := range lines {
			var entry struct {
				Time string `json:"time"`
			}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				t.Fatal(err)
			}
			at, err := time.Parse(time.RFC3339, entry.Time)
			if err != nil {
				t.Fatal(err)
			}
			l.Releases[app] = append(l.Releases[app], ledgertest.ReleaseRow{At: at, Line: []byte(line)})
		}
	}
	return l
}

// The file source still answers what the dashboard and the plugin findings read
// before it existed: the patrol's report, the heartbeat, and one app's last
// ledger line. A town with no DSN setting opens no database.
//
// It is not parallel: the database calls it counts are process-wide, and a
// parallel test's ledger reads would be counted against this one.
//
//testpolicy:allow parallel — it counts database calls process-wide, so no parallel test may run beside it
func TestCloudLedgerFileSourceIsUnchanged(t *testing.T) {
	f := newLedgerFixture(t)
	reports, releases, _ := fileDirs(t, f)
	calls := ledgertest.Calls()
	// NewSource is the production selection: with no ledger_dsn_file it returns
	// the file source, and only a PostgresSource ever opens a database (it is
	// the only type in the package that imports database/sql).
	src, ok := cloudledger.NewSource(config.CloudSettings{ReportsDir: reports, ReleasesDir: releases}).(*cloudledger.FileSource)
	if !ok {
		t.Fatalf("no ledger_dsn_file selected %T, want the file source", src)
	}
	ctx := context.Background()

	rep, err := src.LatestPatrolReport(ctx)
	if err != nil {
		t.Fatalf("LatestPatrolReport: %v", err)
	}
	if rep.SchemaVersion != 1 || !reflect.DeepEqual(rep.Projects, []string{"alpha", "beta"}) {
		t.Errorf("report = %+v", rep)
	}
	if len(rep.Findings) != 2 || rep.Findings[0].ID != "f1" || rep.Findings[1].Kind != "state_missing" {
		t.Errorf("findings = %+v", rep.Findings)
	}

	hb, err := src.Heartbeat(ctx)
	if err != nil || !hb.Equal(f.heartbeat) {
		t.Errorf("heartbeat = %v, %v; want %v", hb, err, f.heartbeat)
	}

	rel, err := src.LastRelease(ctx, "alpha")
	if err != nil {
		t.Fatalf("LastRelease: %v", err)
	}
	if rel.Action != "skip" || rel.Reason != "app_off" || !rel.At.Equal(time.Date(2026, 11, 8, 11, 30, 0, 0, time.UTC)) {
		t.Errorf("last release = %+v", rel)
	}

	apps, err := src.Apps(ctx)
	if err != nil || !reflect.DeepEqual(apps, []string{"alpha", "beta"}) {
		t.Errorf("apps = %v, %v", apps, err)
	}

	// The same reader the dashboard builds over the directory reads a fresh
	// Report: the file source is the read path, not a copy of it.
	c := dashboard.NewCloudReader(reports).Read(ledgerNow)
	if c.State != dashboard.CloudFresh || c.Total != 2 {
		t.Errorf("dashboard cloud line = %+v, want fresh with 2 findings", c)
	}

	// A town with no ledger_dsn_file made no database call at all.
	if got := ledgertest.Calls(); got != calls {
		t.Errorf("the file-only reads made %d database calls, want none", got-calls)
	}
}

// The file source keeps each named state the dashboard showed before the
// source existed.
func TestCloudLedgerFileSourceNamesItsStates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	empty := t.TempDir()
	if _, err := cloudledger.NewFileSource(filepath.Join(empty, "gone"), "").LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrNoPatrolDir) {
		t.Errorf("a missing reports directory = %v, want ErrNoPatrolDir", err)
	}
	if _, err := cloudledger.NewFileSource(empty, "").LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrNoReport) {
		t.Errorf("an empty reports directory = %v, want ErrNoReport", err)
	}

	bad := t.TempDir()
	writeFile(t, filepath.Join(bad, "report.json"), "{not json")
	if _, err := cloudledger.NewFileSource(bad, "").LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrInvalidReport) {
		t.Errorf("a malformed report = %v, want ErrInvalidReport", err)
	}

	oversize := t.TempDir()
	writeFile(t, filepath.Join(oversize, "report.json"), `{"Pad":"`+strings.Repeat("x", cloudledger.ReportMaxBytes)+`"}`)
	if _, err := cloudledger.NewFileSource(oversize, "").LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrOversize) {
		t.Errorf("an oversized report = %v, want ErrOversize", err)
	}

	if _, err := cloudledger.NewFileSource(bad, "").Heartbeat(ctx); !errors.Is(err, cloudledger.ErrNoHeartbeat) {
		t.Errorf("a missing heartbeat = %v, want ErrNoHeartbeat", err)
	}
	if _, err := cloudledger.NewFileSource(filepath.Join(empty, "gone"), "").Heartbeat(ctx); !errors.Is(err, cloudledger.ErrNoHeartbeat) {
		t.Errorf("no reports directory heartbeat = %v, want ErrNoHeartbeat", err)
	}
	if _, err := cloudledger.NewFileSource("", t.TempDir()).LastRelease(ctx, "../escape"); !errors.Is(err, cloudledger.ErrInvalidApp) {
		t.Errorf("a path-like app name = %v, want ErrInvalidApp", err)
	}

	ledgers := t.TempDir()
	writeFile(t, filepath.Join(ledgers, "app.jsonl"), "{not json\n")
	if _, err := cloudledger.NewFileSource("", ledgers).LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrBadRelease) {
		t.Errorf("a malformed ledger line = %v, want ErrBadRelease", err)
	}
	if _, err := cloudledger.NewFileSource("", ledgers).LastRelease(ctx, "missing"); !errors.Is(err, cloudledger.ErrNoRelease) {
		t.Errorf("an app with no ledger = %v, want ErrNoRelease", err)
	}
	if _, err := cloudledger.NewFileSource("", "").LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrNoRelease) {
		t.Errorf("a source with no releases directory = %v, want ErrNoRelease", err)
	}
	if _, err := cloudledger.NewFileSource("", ledgers).ReleasesSince(ctx, "app", ledgerNow.Add(-24*time.Hour)); !errors.Is(err, cloudledger.ErrBadRelease) {
		t.Errorf("a whole-ledger read with a malformed line = %v, want ErrBadRelease", err)
	}

	// A problem the report's own bytes cannot explain is the read error, not a
	// report read as missing.
	reportDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(reportDir, "report.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := cloudledger.NewFileSource(reportDir, "").LatestPatrolReport(ctx)
	if err == nil || errors.Is(err, cloudledger.ErrNoReport) {
		t.Errorf("a report that is a directory = %v, want a read error", err)
	}
	gone := errors.New("the report went away mid-read")
	readable := t.TempDir()
	writeFile(t, filepath.Join(readable, "report.json"), "{}")
	if _, err := cloudledger.NewFileSourceWithRead(readable, "", func(string) ([]byte, error) { return nil, gone }).LatestPatrolReport(ctx); !errors.Is(err, gone) {
		t.Errorf("an injected read failure = %v, want the failure kept", err)
	}

	// A heartbeat that is not a timestamp, and one past the size cap, are the
	// same named state.
	heartbeats := t.TempDir()
	writeFile(t, filepath.Join(heartbeats, "heartbeat"), "yesterday")
	if _, err := cloudledger.NewFileSource(heartbeats, "").Heartbeat(ctx); !errors.Is(err, cloudledger.ErrBadHeartbeat) {
		t.Errorf("a heartbeat that is not a time = %v, want ErrBadHeartbeat", err)
	}
	writeFile(t, filepath.Join(heartbeats, "heartbeat"), strings.Repeat("x", cloudledger.HeartbeatMaxBytes+1))
	if _, err := cloudledger.NewFileSource(heartbeats, "").Heartbeat(ctx); !errors.Is(err, cloudledger.ErrBadHeartbeat) {
		t.Errorf("an oversized heartbeat = %v, want ErrBadHeartbeat", err)
	}
	if err := os.Remove(filepath.Join(heartbeats, "heartbeat")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(heartbeats, "heartbeat"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = cloudledger.NewFileSource(heartbeats, "").Heartbeat(ctx)
	if err == nil || errors.Is(err, cloudledger.ErrNoHeartbeat) {
		t.Errorf("a heartbeat that is a directory = %v, want a read error", err)
	}
	notADir := filepath.Join(t.TempDir(), "reports")
	writeFile(t, notADir, "x")
	if _, err := cloudledger.NewFileSource(notADir, "").Heartbeat(ctx); err == nil || errors.Is(err, cloudledger.ErrNoHeartbeat) {
		t.Errorf("a reports path that is a file = %v, want a read error", err)
	}

	// The ledger's own read paths: a missing file, a directory, and one past
	// the whole-ledger cap.
	if _, err := cloudledger.NewFileSource("", ledgers).ReleasesSince(ctx, "../escape", ledgerNow); !errors.Is(err, cloudledger.ErrInvalidApp) {
		t.Errorf("a path-like app name to ReleasesSince = %v, want ErrInvalidApp", err)
	}
	if _, err := cloudledger.NewFileSource("", ledgers).ReleasesSince(ctx, "missing", ledgerNow); !errors.Is(err, cloudledger.ErrNoRelease) {
		t.Errorf("a missing ledger in ReleasesSince = %v, want ErrNoRelease", err)
	}
	if err := os.Mkdir(filepath.Join(ledgers, "dir.jsonl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := cloudledger.NewFileSource("", ledgers).ReleasesSince(ctx, "dir", ledgerNow); !errors.Is(err, cloudledger.ErrLedgerRead) {
		t.Errorf("a ledger that is a directory = %v, want ErrLedgerRead", err)
	}
	if _, err := cloudledger.NewFileSource("", ledgers).LastRelease(ctx, "dir"); !errors.Is(err, cloudledger.ErrLedgerRead) {
		t.Errorf("a tail read of a directory = %v, want ErrLedgerRead", err)
	}
	writeFile(t, filepath.Join(ledgers, "empty.jsonl"), "\n \n")
	if _, err := cloudledger.NewFileSource("", ledgers).LastRelease(ctx, "empty"); !errors.Is(err, cloudledger.ErrNoRelease) {
		t.Errorf("an empty ledger = %v, want ErrNoRelease", err)
	}
	writeFile(t, filepath.Join(ledgers, "big.jsonl"), strings.Repeat("x", cloudledger.LedgerMaxBytes+1))
	if _, err := cloudledger.NewFileSource("", ledgers).ReleasesSince(ctx, "big", ledgerNow); !errors.Is(err, cloudledger.ErrLedgerRead) {
		t.Errorf("a whole ledger over the cap = %v, want ErrLedgerRead", err)
	}
	writeFile(t, filepath.Join(ledgers, "long.jsonl"), `{"reason":"`+strings.Repeat("x", 2*cloudledger.LedgerTailBytes)+`"}`+"\n")
	if _, err := cloudledger.NewFileSource("", ledgers).LastRelease(ctx, "long"); !errors.Is(err, cloudledger.ErrLedgerRead) {
		t.Errorf("a last line over the tail = %v, want ErrLedgerRead", err)
	}

	// The app listing's own failures.
	if _, err := cloudledger.NewFileSource("", "").Apps(ctx); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no releases directory = %v, want fs.ErrNotExist", err)
	}
	if _, err := cloudledger.NewFileSource("", notADir).Apps(ctx); err == nil {
		t.Error("a releases path that is a file listed apps")
	}

	// The shared regular-file guard.
	if _, err := cloudledger.ReadRegularFile(filepath.Join(empty, "gone"), 10, cloudledger.ErrOversize); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing guarded read = %v, want fs.ErrNotExist", err)
	}
	if _, err := cloudledger.ReadRegularFile(empty, 10, cloudledger.ErrOversize); !errors.Is(err, cloudledger.ErrNotRegular) {
		t.Errorf("a guarded read of a directory = %v, want ErrNotRegular", err)
	}
	writeFile(t, filepath.Join(ledgers, "wide"), strings.Repeat("x", 11))
	if _, err := cloudledger.ReadRegularFile(filepath.Join(ledgers, "wide"), 10, cloudledger.ErrOversize); !errors.Is(err, cloudledger.ErrOversize) {
		t.Errorf("a guarded read over the cap = %v, want the cap error", err)
	}
	if _, err := cloudledger.OpenRegular(filepath.Join(empty, "gone")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("opening a missing path = %v, want fs.ErrNotExist", err)
	}
	if _, err := cloudledger.OpenRegular(empty); !errors.Is(err, cloudledger.ErrNotRegular) {
		t.Errorf("opening a directory = %v, want ErrNotRegular", err)
	}
}

// The same records through the file source and through Postgres give the same
// dashboard cloud line, app power line and report findings (the fields the
// daemon's plugin findings read).
func TestCloudLedgerSourcesAreEquivalent(t *testing.T) {
	t.Parallel()

	f := newLedgerFixture(t)
	reports, releases, off := fileDirs(t, f)
	fileSrc := cloudledger.NewFileSource(reports, releases)

	l := pgLedger(t, f)
	_, dsnPath := ledgertest.Register(t, l)
	pgSrc := newPostgres(t, dsnPath)

	ctx := context.Background()
	fileReport, err := fileSrc.LatestPatrolReport(ctx)
	if err != nil {
		t.Fatalf("file Report: %v", err)
	}
	pgReport, err := pgSrc.LatestPatrolReport(ctx)
	if err != nil {
		t.Fatalf("postgres Report: %v", err)
	}
	if !reflect.DeepEqual(fileReport, pgReport) {
		t.Errorf("reports differ:\nfile:     %+v\npostgres: %+v", fileReport, pgReport)
	}
	for _, app := range []string{"alpha", "beta"} {
		fileRel, err := fileSrc.LastRelease(ctx, app)
		if err != nil {
			t.Fatalf("%s file release: %v", app, err)
		}
		pgRel, err := pgSrc.LastRelease(ctx, app)
		if err != nil {
			t.Fatalf("%s postgres release: %v", app, err)
		}
		if !reflect.DeepEqual(fileRel, pgRel) {
			t.Errorf("%s releases differ:\nfile:     %+v\npostgres: %+v", app, fileRel, pgRel)
		}
		since := ledgerNow.Add(-4 * time.Hour)
		fileHist, err := fileSrc.ReleasesSince(ctx, app, since)
		if err != nil {
			t.Fatalf("%s file history: %v", app, err)
		}
		pgHist, err := pgSrc.ReleasesSince(ctx, app, since)
		if err != nil {
			t.Fatalf("%s postgres history: %v", app, err)
		}
		if !reflect.DeepEqual(fileHist, pgHist) {
			t.Errorf("%s release history differs:\nfile:     %+v\npostgres: %+v", app, fileHist, pgHist)
		}
		// Nothing after a time later than every line is an empty answer, not
		// an error, through either source.
		fileNone, err := fileSrc.ReleasesSince(ctx, app, ledgerNow)
		if err != nil {
			t.Fatalf("%s file empty history: %v", app, err)
		}
		pgNone, err := pgSrc.ReleasesSince(ctx, app, ledgerNow)
		if err != nil {
			t.Fatalf("%s postgres empty history: %v", app, err)
		}
		if !reflect.DeepEqual(fileNone, pgNone) || len(fileNone) != 0 {
			t.Errorf("%s empty histories differ: file %+v, postgres %+v", app, fileNone, pgNone)
		}
	}

	fileCloud := dashboard.NewCloudReaderFromSource(fileSrc).
		WithApps(dashboard.NewAppPowerReaderFromSource(off, fileSrc)).Read(ledgerNow)
	pgCloud := dashboard.NewCloudReaderFromSource(pgSrc).
		WithApps(dashboard.NewAppPowerReaderFromSource(off, pgSrc)).Read(ledgerNow)
	if !reflect.DeepEqual(fileCloud, pgCloud) {
		t.Errorf("cloud lines differ:\nfile:     %+v\npostgres: %+v", fileCloud, pgCloud)
	}
	if fileCloud.State != dashboard.CloudFresh || fileCloud.Apps == nil || len(fileCloud.Apps.Apps) != 2 {
		t.Fatalf("the equivalence fixture did not read as expected: %+v", fileCloud)
	}
}

// Every way the Postgres source cannot be read is an error the cloud line
// shows as UNKNOWN with the reason, naming the source and never the DSN.
func TestCloudLedgerFailsClosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	t.Run("an unreadable database", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{}
		dsn, path := ledgertest.Register(t, l)
		l.QueryErr = fmt.Errorf("dialing %s: connection refused", dsn)
		src := newPostgres(t, path)

		if _, err := src.LatestPatrolReport(ctx); err == nil {
			t.Fatal("an unreadable database answered a report")
		} else if !strings.Contains(err.Error(), "postgres source") || strings.Contains(err.Error(), dsn) {
			t.Errorf("err = %q, want the source named and the DSN absent", err)
		}
		if _, err := src.Heartbeat(ctx); err == nil {
			t.Fatal("an unreadable database answered a heartbeat")
		}

		c := dashboard.NewCloudReaderFromSource(src).Read(ledgerNow)
		if c.State != dashboard.CloudError {
			t.Errorf("state = %q, want %q", c.State, dashboard.CloudError)
		}
		if c.Note == "" || !strings.Contains(c.Note, "postgres source") {
			t.Errorf("note = %q, want the source named", c.Note)
		}
		if strings.Contains(c.Note, dsn) {
			t.Errorf("the cloud line carries the DSN: %q", c.Note)
		}
	})

	t.Run("a missing DSN file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "gone.dsn")
		src := newPostgres(t, path)
		_, err := src.LatestPatrolReport(ctx)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v, want the missing file named", err)
		}
		c := dashboard.NewCloudReaderFromSource(src).Read(ledgerNow)
		if c.State != dashboard.CloudError || !strings.Contains(c.Note, "DSN file") {
			t.Errorf("cloud line = %+v, want an error naming the DSN file", c)
		}
	})

	t.Run("a DSN file readable by group or others", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{}
		dsn, path := ledgertest.Register(t, l)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		src := newPostgres(t, path)
		_, err := src.LatestPatrolReport(ctx)
		if err == nil || !strings.Contains(err.Error(), "readable by group or others") {
			t.Fatalf("err = %v, want the mode refusal", err)
		}
		if strings.Contains(err.Error(), dsn) {
			t.Errorf("the refusal carries the DSN: %q", err)
		}
		c := dashboard.NewCloudReaderFromSource(src).Read(ledgerNow)
		if c.State != dashboard.CloudError || strings.Contains(c.Note, dsn) {
			t.Errorf("cloud line = %+v, want an error and no DSN", c)
		}
	})

	t.Run("an empty cloud block", func(t *testing.T) {
		t.Parallel()
		src := cloudledger.NewSource(config.CloudSettings{Off: true})
		if _, err := src.LatestPatrolReport(ctx); !errors.Is(err, cloudledger.ErrNotConfigured) {
			t.Fatalf("report err = %v, want ErrNotConfigured", err)
		}
		if _, err := src.Heartbeat(ctx); !errors.Is(err, cloudledger.ErrNotConfigured) {
			t.Errorf("heartbeat err = %v, want ErrNotConfigured", err)
		}
		if _, err := src.ReleasesSince(ctx, "app", ledgerNow); !errors.Is(err, cloudledger.ErrNotConfigured) {
			t.Errorf("releases err = %v, want ErrNotConfigured", err)
		}
		if _, err := src.LastRelease(ctx, "app"); !errors.Is(err, cloudledger.ErrNotConfigured) {
			t.Errorf("last release err = %v, want ErrNotConfigured", err)
		}
		c := dashboard.NewCloudReaderFromSource(src).Read(ledgerNow)
		if c.State != dashboard.CloudError || c.Note != config.CloudNotConfigured {
			t.Errorf("cloud line = %+v, want %q", c, config.CloudNotConfigured)
		}
		// The app power line still draws the markers, and says nothing about a
		// release ledger there is no cloud step to read.
		off := t.TempDir()
		writeFile(t, filepath.Join(off, "alpha.off"), "since=2026-11-08T11:00:00Z by=op env=prod reason=x\n")
		p := dashboard.NewAppPowerReaderFromSource(off, src).Read(ledgerNow)
		if p == nil || len(p.Apps) != 1 {
			t.Fatalf("app power line = %+v, want the marker's row", p)
		}
		if p.Apps[0].Release != nil || p.Apps[0].ReleaseNote != "" {
			t.Errorf("row = %+v, want no release and no note", p.Apps[0])
		}
	})

	t.Run("an empty DSN file", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "empty.dsn")
		if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := newPostgres(t, path).LatestPatrolReport(ctx)
		if err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Fatalf("err = %v, want the empty-file refusal", err)
		}
	})

	t.Run("a DSN path this reader will not open", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "dsn-dir")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := newPostgres(t, path).LatestPatrolReport(ctx)
		if err == nil || !errors.Is(err, cloudledger.ErrNotRegular) {
			t.Fatalf("err = %v, want ErrNotRegular", err)
		}
	})
}

// A role that can write any ledger table is refused, naming the table, before
// a single row is read.
func TestCloudLedgerRefusesAWriterRole(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, table := range []string{cloudledger.PatrolReportTable, cloudledger.PatrolHeartbeatTable, cloudledger.ReleaseTable} {
		l := &ledgertest.Ledger{
			Report:        &ledgertest.ReportRow{SchemaVersion: 1, Projects: []byte(`[]`), Findings: []byte(`[]`)},
			WritableTable: table,
		}
		_, path := ledgertest.Register(t, l)
		src := newPostgres(t, path)

		_, err := src.LatestPatrolReport(ctx)
		if !errors.Is(err, cloudledger.ErrWriterRole) {
			t.Fatalf("%s: err = %v, want ErrWriterRole", table, err)
		}
		if !strings.Contains(err.Error(), table) {
			t.Errorf("%s: err = %q, want the table named", table, err)
		}
		if got := l.Statements(); len(got) == 0 || !strings.Contains(got[0], "has_table_privilege") {
			t.Errorf("%s: first statement = %v, want the role probe", table, got)
		}
		for _, q := range l.Statements() {
			if strings.Contains(q, "FROM ") {
				t.Errorf("%s: a ledger read ran before the refusal: %q", table, q)
			}
		}
		if !l.ReadOnly() {
			t.Errorf("%s: the probe did not run in a read-only transaction", table)
		}
	}

	t.Run("a privilege this reader cannot see is a refusal", func(t *testing.T) {
		t.Parallel()
		l := &ledgertest.Ledger{
			Report:    &ledgertest.ReportRow{SchemaVersion: 1, Projects: []byte(`[]`), Findings: []byte(`[]`)},
			ProbeNull: cloudledger.PatrolReportTable,
		}
		_, path := ledgertest.Register(t, l)
		src := newPostgres(t, path)
		_, err := src.LatestPatrolReport(ctx)
		if !errors.Is(err, cloudledger.ErrWriterRole) || !strings.Contains(err.Error(), cloudledger.PatrolReportTable) {
			t.Fatalf("err = %v, want a refusal naming the table (statements %v)", err, l.Statements())
		}
	})
}
