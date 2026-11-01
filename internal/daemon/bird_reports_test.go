package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
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
	if err := os.WriteFile(filepath.Join(dir, birdReportsFile), data, 0o644); err != nil {
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
			summary: []string{"dunlin", "missing", "gt birds due"},
		},
		{
			name:    "older than the maximum",
			setup:   func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(-27*time.Hour)) },
			want:    true,
			summary: []string{"dunlin", "27h00m", "gt birds due"},
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
			name:  "a future written_at is fresh",
			setup: func(t *testing.T, town string) { birdReportsWriteDaily(t, town, "dunlin", now.Add(time.Hour)) },
		},
		{
			name: "not JSON",
			setup: func(t *testing.T, town string) {
				birdReportsWriteRaw(t, town, "dunlin", []byte("this is not a report"))
			},
			want:    true,
			summary: []string{"dunlin", "age: unknown", "gt birds due"},
		},
		{
			name:    "no written_at",
			setup:   func(t *testing.T, town string) { birdReportsWriteRaw(t, town, "dunlin", []byte(`{"body":"hello"}`)) },
			want:    true,
			summary: []string{"dunlin", "no readable written_at", "gt birds due"},
		},
		{
			name: "unreadable file",
			setup: func(t *testing.T, town string) {
				path := filepath.Join(town, ".runtime", "dunlin", "reports", birdReportsFile)
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want:    true,
			summary: []string{"dunlin", "could not be read", "gt birds due"},
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

// TestBirdReportsOffOrUnreadableSettingChecksNothing holds the two ways the
// patrol declines to act: a disabled patrol records nothing, and a max_age it
// cannot parse keeps the last check's items instead of clearing them.
func TestBirdReportsOffOrUnreadableSettingChecksNothing(t *testing.T) {
	t.Parallel()

	t.Run("disabled", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
		d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: false, Reporters: []string{"dunlin"}})
		d.runBirdReports()
		if got := d.birdReportStale.observed(); len(got) != 0 {
			t.Errorf("a disabled patrol observed %+v, want nothing", got)
		}
	})

	t.Run("unreadable max_age keeps the last check", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
		cfg := &BirdReportsConfig{Enabled: true, Reporters: []string{"dunlin"}}
		d := birdReportsTestDaemon(town, now, cfg)
		d.runBirdReports()
		if got := d.birdReportStale.observed(); len(got) != 1 {
			t.Fatalf("setup: %+v, want the missing report's item", got)
		}
		// Only a config built in code can carry a value decoding refuses.
		cfg.MaxAgeStr = "soon"
		d.runBirdReports()
		got := d.birdReportStale.observed()
		if len(got) != 1 || got[0].Key != "bird-report-stale:dunlin" {
			t.Errorf("after the unreadable setting: %+v, want the last check's item kept", got)
		}
	})

	t.Run("no town root", func(t *testing.T) {
		t.Parallel()
		d := &Daemon{
			logger:       discardLogger,
			patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{BirdReports: &BirdReportsConfig{Enabled: true}}},
		}
		d.runBirdReports()
		if got := d.birdReportStale.observed(); len(got) != 0 {
			t.Errorf("a daemon with no town root observed %+v, want nothing", got)
		}
	})

	t.Run("an unset zone reads the local clock", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
		hour := 0 // no hour is before midnight, so the gate cannot hide the item
		d := birdReportsTestDaemon(town, now, &BirdReportsConfig{Enabled: true, StartHour: &hour, Reporters: []string{"dunlin"}})
		d.birdReportLoc = nil
		d.runBirdReports()
		if got := d.birdReportStale.observed(); len(got) != 1 {
			t.Errorf("with an unset zone: %+v, want the missing report's item", got)
		}
	})
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

// TestBirdReportsIsRegisteredInTheAttentionQueue drives the collector through
// the queue's collector set, so dropping the registration turns it red: an
// item no collector returns would clear on the next tick.
func TestBirdReportsIsRegisteredInTheAttentionQueue(t *testing.T) {
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

// TestBirdReportsReadsOnlyWrittenAt holds the report shape: written_at is the
// field the check judges, and another field or key beside it changes nothing.
func TestBirdReportsReadsOnlyWrittenAt(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, birdReportsTestZone)
	birdReportsWriteRaw(t, town, "dunlin", []byte(`{"written_at":"`+now.Add(-time.Minute).UTC().Format(time.RFC3339)+`","rest":{"ignored":true}}`))
	if got := birdReportStaleItems(now, birdReportsTestReads(town, 6, 26*time.Hour, "dunlin")); len(got) != 0 {
		t.Errorf("items = %+v, want a fresh report whatever else the file holds", got)
	}
}
