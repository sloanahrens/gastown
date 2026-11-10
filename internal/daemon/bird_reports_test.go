package daemon

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/reportdir"
)

// birdReportsTestZone fixes the zone start_hour is read in, so a test's hour
// does not depend on the host's time zone.
var birdReportsTestZone = time.FixedZone("bird-reports-test", -8*60*60)

// birdReportsTestReads is the check's inputs for a town and a reporter list.
func birdReportsTestReads(town string, startHour int, maxAge time.Duration, reporters ...string) birdReportsReads {
	return birdReportsReads{
		townRoot: town, loc: birdReportsTestZone, startHour: startHour, maxAge: maxAge, reporters: reporters,
	}
}

// birdReportsWriteDaily writes a reporter's daily.json with written_at.
func birdReportsWriteDaily(t *testing.T, town, name string, writtenAt time.Time) {
	t.Helper()
	birdReportsWriteRaw(t, town, name, []byte(`{"written_at":"`+writtenAt.UTC().Format(time.RFC3339)+`"}`))
}

// birdReportsWriteRaw writes bytes at a reporter's daily.json path.
func birdReportsWriteRaw(t *testing.T, town, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(town, ".runtime", name, "reports")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reportdir.SidecarName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// birdReportsMkdirReportDir makes a reporter's legacy reports directory with no
// report in it, so the town shows the daemon a bird report directory.
func birdReportsMkdirReportDir(t *testing.T, town, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, ".runtime", name, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// birdReportsWritePublished writes bytes at the sidecar gt report publish
// writes, <town>/.runtime/reports/<name>/daily.json.
func birdReportsWritePublished(t *testing.T, town, name string, data []byte) {
	t.Helper()
	dir, err := reportdir.Published(filepath.Join(town, ".runtime"), name)
	if err != nil {
		t.Fatalf("resolving the published directory for %s: %v", name, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reportdir.SidecarName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// birdReportsTestDaemon is a daemon with only what runBirdReports reads: the
// clock, the town root, the patrol config and the fixed zone.
func birdReportsTestDaemon(town string, now time.Time, br *BirdReportsConfig) *Daemon {
	return &Daemon{
		config:        &Config{TownRoot: town},
		logger:        discardLogger,
		clock:         clockwork.NewFakeClockAt(now),
		patrolConfig:  &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: br}},
		birdReportLoc: birdReportsTestZone,
	}
}

// TestBirdReportsBeforeStartHourRaisesNothing holds the gate: the day's reports
// are not late until start_hour, so a town is quiet overnight.
func TestBirdReportsBeforeStartHourRaisesNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	reads := birdReportsTestReads(town, 6, 26*time.Hour, "dunlin")

	before := time.Date(2026, 11, 1, 5, 59, 0, 0, birdReportsTestZone)
	if got := birdReportStaleItems(before, reads); len(got) != 0 {
		t.Errorf("items at 05:59 = %+v, want none before start_hour", got)
	}

	at := time.Date(2026, 11, 1, 6, 0, 0, 0, birdReportsTestZone)
	got := birdReportStaleItems(at, reads)
	if len(got) != 1 {
		t.Fatalf("items at 06:00 = %+v, want the missing report's item", got)
	}
	if got[0].Key != "bird-report-stale:dunlin" {
		t.Errorf("key = %q, want bird-report-stale:dunlin", got[0].Key)
	}
}

// TestBirdReportsItemShapes judges one reporter's report at 09:00: missing,
// unreadable, late and fresh files, with the boundary at exactly max_age.
func TestBirdReportsItemShapes(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	const maxAge = 26 * time.Hour

	cases := []struct {
		name    string
		setup   func(t *testing.T, town string)
		want    bool
		summary []string
	}{
		{
			name:    "missing",
			want:    true,
			summary: []string{"dunlin", "missing", "send dunlin the daily request"},
		},
		{
			name:    "older than the maximum",
			setup:   func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(-27*time.Hour)) },
			want:    true,
			summary: []string{"dunlin", "27h00m", "send dunlin the daily request"},
		},
		{
			name:  "exactly at the maximum is fresh",
			setup: func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(-maxAge)) },
		},
		{
			name:  "inside the maximum",
			setup: func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(-25*time.Hour)) },
		},
		{
			name:    "a future written_at is an item",
			setup:   func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(time.Hour)) },
			want:    true,
			summary: []string{"dunlin", "1h00m in the future", "send dunlin the daily request"},
		},
		{
			name: "not JSON",
			setup: func(t *testing.T, town string) {
				birdReportsWriteRaw(t, town, "dunlin", []byte("this is not a report"))
			},
			want:    true,
			summary: []string{"dunlin", "age: unknown", "send dunlin the daily request"},
		},
		{
			name:    "no written_at",
			setup:   func(t *testing.T, town string) { birdReportsWriteRaw(t, town, "dunlin", []byte(`{"body":"hello"}`)) },
			want:    true,
			summary: []string{"dunlin", "no readable written_at", "send dunlin the daily request"},
		},
		{
			name: "unreadable file",
			setup: func(t *testing.T, town string) {
				path := filepath.Join(town, ".runtime", "dunlin", "reports", reportdir.SidecarName)
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want:    true,
			summary: []string{"dunlin", "could not be read", "send dunlin the daily request"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, town)
			}
			got := birdReportStaleItems(now, birdReportsTestReads(town, 6, maxAge, "dunlin"))
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("items = %+v, want the report to read as fresh", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("items = %+v, want exactly one", got)
			}
			it := got[0]
			if it.Key != "bird-report-stale:dunlin" || it.Kind != attention.KindBirdReportStale || it.Severity != attention.SeverityLow {
				t.Errorf("item = %+v, want a low bird-report-stale:dunlin item", it)
			}
			for _, want := range tc.summary {
				if !strings.Contains(it.Summary, want) {
					t.Errorf("summary %q lacks %q", it.Summary, want)
				}
			}
		})
	}
}

// TestBirdReportsClearsWhenTheReportBecomesFresh drives the patrol and the
// collector: a late report is an item, and the next check after the report
// lands returns none, which is what clears the queue's item.
func TestBirdReportsClearsWhenTheReportBecomesFresh(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsWriteDaily(t, town, "dunlin", now.Add(-27*time.Hour))
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin"}})

	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 1 {
		t.Fatalf("after a late report: %+v, want one item", got)
	}

	birdReportsWriteDaily(t, town, "dunlin", now.Add(-time.Minute))
	d.runBirdReports()
	observed := d.birdReportStale.observed()
	if len(observed) != 0 {
		t.Fatalf("after a fresh report: %+v, want no item", observed)
	}

	s := &attentionSources{birdReports: d.birdReportStale.observed}
	got, err := s.collectBirdReports(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("collector = %+v, %v; want no items once the report is fresh", got, err)
	}
}

// TestBirdReportsRemovedReporterLosesItsItem holds the config-is-the-list rule:
// the next check replaces the snapshot, so a reporter taken out of the list
// loses its item.
func TestBirdReportsRemovedReporterLosesItsItem(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsMkdirReportDir(t, town, "dipper")
	cfg := &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin", "dipper"}}
	d := birdReportsTestDaemon(town, now, cfg)

	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 2 {
		t.Fatalf("two late reports: %+v, want two items", got)
	}

	cfg.Reporters = []string{"dipper"}
	d.runBirdReports()
	got := d.birdReportStale.observed()
	if len(got) != 1 || got[0].Key != "bird-report-stale:dipper" {
		t.Errorf("after removing dunlin: %+v, want only dipper's item", got)
	}
}

// TestBirdReportsOverseerIsTheTenthReporter holds the default list: with no
// reporters configured, the overseer's own report is expected beside the nine
// birds.
func TestBirdReportsOverseerIsTheTenthReporter(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsMkdirReportDir(t, town, "overseer")
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true})

	d.runBirdReports()
	got := d.birdReportStale.observed()
	if len(got) != 10 {
		t.Fatalf("items = %d, want the ten default reporters", len(got))
	}
	keys := make(map[string]bool, len(got))
	for _, it := range got {
		keys[it.Key] = true
	}
	if !keys["bird-report-stale:overseer"] {
		t.Errorf("keys = %v, want bird-report-stale:overseer", keys)
	}
}

// TestBirdReportsDisabledRecordsNothing holds the opt-in switch: a patrol the
// town did not enable records nothing, so an item never outlives its setting.
func TestBirdReportsDisabledRecordsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: false, Reporters: []string{"dunlin"}})
	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 0 {
		t.Errorf("a disabled patrol observed %+v, want nothing", got)
	}
}

// TestBirdReportsFailedCheckIsAnItem holds the alarm path: a setting the check
// cannot act on raises the check-failed item in place of the unverified
// reporter items, and a check that runs again clears it. A failed check that
// left the queue quiet would look exactly like a town whose reports are fresh.
func TestBirdReportsFailedCheckIsAnItem(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsMkdirReportDir(t, town, "dunlin")
	cfg := &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin"}}
	d := birdReportsTestDaemon(town, now, cfg)
	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
		t.Fatalf("setup: %+v, want the missing report's item", got)
	}

	// Only a config built in code can carry a value decoding refuses.
	cfg.MaxAgeStr = "soon"
	d.runBirdReports()
	got := d.birdReportStale.observed()
	if len(got) != 1 {
		t.Fatalf("after the failed check: %+v, want exactly the check-failed item", got)
	}
	if got[0].Key != birdReportsCheckFailedKey || got[0].Kind != attention.KindBirdReportStale || got[0].Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high bird-report-check-failed item", got[0])
	}
	if !strings.Contains(got[0].Summary, "cannot check") || !strings.Contains(got[0].Summary, "max_age") {
		t.Errorf("summary = %q, want the failed check and its setting named", got[0].Summary)
	}

	cfg.MaxAgeStr = ""
	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
		t.Errorf("after the check runs again: %+v, want the reporter's item back", got)
	}
}

// TestBirdReportsNoVisibleDirectoryRaisesOneCheckFailedItem holds the
// not-visible case: a town with no bird report directory under its root raises
// one high check-failed item saying the reports cannot be seen, not one stale
// item per bird, and logs the line that names the root.
func TestBirdReportsNoVisibleDirectoryRaisesOneCheckFailedItem(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin", "dipper", "eagle"}})
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)

	d.runBirdReports()

	got := d.birdReportStale.observed()
	if len(got) != 1 {
		t.Fatalf("items = %+v, want exactly one check-failed item and no stale items", got)
	}
	it := got[0]
	if it.Key != birdReportsCheckFailedKey || it.Kind != attention.KindBirdReportStale || it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high bird-report-check-failed item", it)
	}
	if !strings.Contains(it.Summary, "not visible to this daemon") {
		t.Errorf("summary = %q, want it to say the reports directory is not visible to this daemon", it.Summary)
	}
	if want := "bird_reports: not checking: no bird report directory is visible under " + town + "\n"; logs.String() != want {
		t.Errorf("log = %q, want %q", logs.String(), want)
	}
}

// TestBirdReportsOneVisibleDirectoryJudgesTheRest holds the partial case: once
// one reporter's directory is visible, the check runs as it always has, so the
// reporters whose directories are absent are late items and the visible one is
// judged by its report.
func TestBirdReportsOneVisibleDirectoryJudgesTheRest(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsWriteDaily(t, town, "dunlin", now.Add(-time.Hour))
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin", "dipper", "eagle"}})

	d.runBirdReports()

	got := d.birdReportStale.observed()
	if len(got) != 2 {
		t.Fatalf("items = %+v, want the two absent reporters as late items", got)
	}
	for i, name := range []string{"dipper", "eagle"} {
		if got[i].Key != "bird-report-stale:"+name || got[i].Kind != attention.KindBirdReportStale || got[i].Severity != attention.SeverityLow {
			t.Errorf("item %d = %+v, want a low bird-report-stale:%s item", i, got[i], name)
		}
		if !strings.Contains(got[i].Summary, "missing") {
			t.Errorf("item %d summary = %q, want the report named missing", i, got[i].Summary)
		}
	}
}

// TestBirdReportsPublishedDirectoryAloneIsVisible holds the published location
// as a visible directory: a town whose only reports directory is the one gt
// report publish writes is one the daemon can see, so the check runs.
func TestBirdReportsPublishedDirectoryAloneIsVisible(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	dir, err := reportdir.Published(filepath.Join(town, ".runtime"), "dunlin")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin"}})

	d.runBirdReports()

	got := d.birdReportStale.observed()
	if len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
		t.Errorf("items = %+v, want dunlin's missing report as its one late item", got)
	}
}

// TestBirdReportsRefusedNameIsNotAVisibleDirectory holds the resolver's name
// check on the visibility path: a reporter name the resolver refuses does not
// see a directory, even one a path join would reach, so a town with only that
// directory is not visible.
func TestBirdReportsRefusedNameIsNotAVisibleDirectory(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	// "../dunlin" joined under .runtime is town/dunlin: a directory outside the
	// runtime that a refused name must not reach.
	if err := os.MkdirAll(filepath.Join(town, "dunlin", "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, Reporters: []string{"../dunlin"}})

	d.runBirdReports()

	got := d.birdReportStale.observed()
	if len(got) != 1 || got[0].Key != birdReportsCheckFailedKey {
		t.Errorf("items = %+v, want the check-failed item for a town with no visible directory", got)
	}
}

// TestBirdReportsWithNoDaemonConfigRecordsNothing covers a daemon that never
// loaded a config: the check has no town to read, so it is not run at all.
func TestBirdReportsWithNoDaemonConfigRecordsNothing(t *testing.T) {
	t.Parallel()
	d := &Daemon{
		logger:       discardLogger,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{Enabled: true}}},
	}
	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 0 {
		t.Errorf("a daemon with no config observed %+v, want nothing", got)
	}
}

// TestBirdReportsUnsetZoneUsesLocalClock pins the fallback zone: with no zone
// injected the check reads the host's local hour, so the start-hour gate still
// opens.
func TestBirdReportsUnsetZoneUsesLocalClock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	hour := 0 // no hour is before midnight, so the gate cannot hide the item
	birdReportsMkdirReportDir(t, town, "dunlin")
	d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, StartHour: &hour, Reporters: []string{"dunlin"}})
	d.birdReportLoc = nil
	d.runBirdReports()
	if got := d.birdReportStale.observed(); len(got) != 1 {
		t.Errorf("with an unset zone: %+v, want the missing report's item", got)
	}
}

// TestBirdReportsCollectorReadsTheSnapshot is the wiring: with no patrol the
// collector returns nothing, and with one it returns the snapshot's items as
// they were marked.
func TestBirdReportsCollectorReadsTheSnapshot(t *testing.T) {
	t.Parallel()
	s := &attentionSources{}
	if got, err := s.collectBirdReports(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("without a patrol: %+v, %v; want nothing", got, err)
	}
	s.birdReports = func() []attention.Item {
		return []attention.Item{{Key: "bird-report-stale:dunlin", Kind: attention.KindBirdReportStale}}
	}
	got, err := s.collectBirdReports(context.Background())
	if err != nil || len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
		t.Errorf("collector = %+v, %v; want the snapshot's item", got, err)
	}
}

// TestBirdReportsCollectorIsInTheCollectorSet drives the collector through the
// attention sources' collector set, so dropping the registration turns it red:
// an item no collector returns would clear on the next tick.
func TestBirdReportsCollectorIsInTheCollectorSet(t *testing.T) {
	t.Parallel()
	s := &attentionSources{birdReports: func() []attention.Item {
		return []attention.Item{{Key: "bird-report-stale:dunlin", Kind: attention.KindBirdReportStale}}
	}}
	for _, c := range s.collectors() {
		if c.kind != attention.KindBirdReportStale {
			continue
		}
		got, err := c.collect(context.Background())
		if err != nil || len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
			t.Errorf("registered collector = %+v, %v; want the snapshot's item", got, err)
		}
		return
	}
	t.Fatal("the attention queue has no bird_reports collector")
}

// TestBirdReportsPatrolIsOffUnlessEnabled holds the opt-in default: a town
// that never wrote the block raises nothing, and only enabled:true turns the
// patrol on.
func TestBirdReportsPatrolIsOffUnlessEnabled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     *DaemonPatrolConfig
		disable bool
		want    bool
	}{
		{name: "no patrol config", cfg: nil},
		{name: "no bird_reports entry", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}},
		{name: "enabled unset", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{}}}},
		{name: "enabled", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{Enabled: true}}}, want: true},
		{name: "disabled in the town's list", cfg: &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{Enabled: true}}}, disable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{patrolConfig: tc.cfg}
			if tc.disable {
				d.disabledPatrols = map[string]bool{"bird_reports": true}
			}
			if got := d.isPatrolActive("bird_reports"); got != tc.want {
				t.Errorf("isPatrolActive(bird_reports) = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBirdReportsReadsOfDecodesTheBlock pins the production read builder: the
// config accessors reach the check, and an unreadable max_age is an error
// rather than a silent default.
func TestBirdReportsReadsOfDecodesTheBlock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	hour := 5
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{
		Enabled: true, StartHour: &hour, MaxAgeStr: "30h", Reporters: []string{"eagle"},
	}}}
	reads, err := birdReportsReadsOf(cfg, town, birdReportsTestZone)
	if err != nil {
		t.Fatal(err)
	}
	if reads.townRoot != town || reads.startHour != 5 || reads.maxAge != 30*time.Hour || len(reads.reporters) != 1 || reads.reporters[0] != "eagle" {
		t.Errorf("reads = %+v, want the block as written", reads)
	}

	cfg.Patrols.BirdReports.MaxAgeStr = "soon"
	if _, err := birdReportsReadsOf(cfg, town, birdReportsTestZone); err == nil {
		t.Error("an unparseable max_age was read as a duration")
	}
}

// TestBirdReportsIgnoresFieldsBesideWrittenAt holds the report shape: another
// field or key beside written_at does not change the check's judgment.
func TestBirdReportsIgnoresFieldsBesideWrittenAt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsWriteRaw(t, town, "dunlin", []byte(`{"written_at":"`+now.Add(-time.Minute).UTC().Format(time.RFC3339)+`","rest":{"ignored":true}}`))
	if got := birdReportStaleItems(now, birdReportsTestReads(town, 6, 26*time.Hour, "dunlin")); len(got) != 0 {
		t.Errorf("items = %+v, want a fresh report whatever else the file holds", got)
	}
}

// TestBirdReportsReadsTheVolumeLocation drives the report resolver through the
// patrol: the published sidecar wins when both locations hold one, the old
// location is read when it is the only one, and a reporter in neither is
// missing.
func TestBirdReportsReadsTheVolumeLocation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	const maxAge = 26 * time.Hour
	sidecar := func(age time.Duration) []byte {
		return []byte(`{"written_at":"` + now.Add(-age).UTC().Format(time.RFC3339) + `"}`)
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, town string)
		late  bool
	}{
		{
			name: "the published location wins when both hold a fresh report",
			setup: func(t *testing.T, town string) {
				birdReportsWriteRaw(t, town, "dunlin", sidecar(27*time.Hour))
				birdReportsWritePublished(t, town, "dunlin", sidecar(time.Hour))
			},
		},
		{
			name: "the published location wins even when it is the late one",
			setup: func(t *testing.T, town string) {
				birdReportsWriteRaw(t, town, "dunlin", sidecar(time.Hour))
				birdReportsWritePublished(t, town, "dunlin", sidecar(27*time.Hour))
			},
			late: true,
		},
		{
			name: "the old location is read when it is the only one",
			setup: func(t *testing.T, town string) {
				birdReportsWriteRaw(t, town, "dunlin", sidecar(time.Hour))
			},
		},
		{
			name: "a reporter in neither location is missing",
			late: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, town)
			}
			got := birdReportStaleItems(now, birdReportsTestReads(town, 6, maxAge, "dunlin"))
			if !tc.late {
				if len(got) != 0 {
					t.Fatalf("items = %+v, want the report to read as fresh", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("items = %+v, want exactly one late report", got)
			}
			if tc.name == "a reporter in neither location is missing" && !strings.Contains(got[0].Summary, "missing") {
				t.Errorf("summary = %q, want the report named missing", got[0].Summary)
			}
		})
	}
}

// TestBirdReportsRefusesAConfiguredNameTheResolverRefuses drives the resolver's
// name check through the patrol: a configured reporter name the resolver
// refuses is an item that says the report could not be read, not a fresh one.
func TestBirdReportsRefusesAConfiguredNameTheResolverRefuses(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	got := birdReportStaleItems(now, birdReportsTestReads(t.TempDir(), 6, 26*time.Hour, "../dunlin"))
	if len(got) != 1 {
		t.Fatalf("items = %+v, want the refused name to be one late report", got)
	}
	if !strings.Contains(got[0].Summary, "could not be read") {
		t.Errorf("summary = %q, want it to say the report could not be read", got[0].Summary)
	}
}
