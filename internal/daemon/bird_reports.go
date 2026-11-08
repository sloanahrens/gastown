package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/birdreports"
)

// The bird_reports patrol (gt-cbsat): every hour the daemon checks each
// expected reporter's daily report and, from start_hour on, raises one
// attention item, bird-report-stale:<name>, while that report is missing,
// unreadable or older than the configured maximum age. The daily reports are
// what the Birds panel shows; a session cron writes them, and a cron that dies
// with its session is exactly the lapse this patrol catches. A check that
// cannot run raises bird-report-check-failed instead, so a failed check is not
// silence.
//
// The patrol is opt-in (patrols.bird_reports.enabled). For each reporter it
// reads the daily.json sidecar in the directory birdreports.ReportDir resolves
// (the published <town>/.runtime/reports/<name> when it holds a report, else
// the old <town>/.runtime/<name>/reports) and nothing else. Its only write is
// the in-memory snapshot the attention collector reads.

const (
	// birdReportsKeyPrefix is the item key's prefix; the reporter's name
	// follows it, so one reporter has one item however often it is late.
	birdReportsKeyPrefix = "bird-report-stale:"

	// birdReportsCheckEvery is how often the patrol reads the reports. The
	// reports are daily, so an hourly look notices a missed one the same
	// morning without turning the check into a busy loop.
	birdReportsCheckEvery = time.Hour

	// birdReportsCheckFailedKey is the item a check that could not run raises.
	// It does not start with birdReportsKeyPrefix and carries no reporter
	// name, so no reporter's item can collide with it.
	birdReportsCheckFailedKey = "bird-report-check-failed"
)

// birdReportsReads is one check's inputs: the town whose .runtime holds the
// reports, the zone start_hour is read in, the hour the day's reports are
// expected from, how old one may be, and the reporters expected.
type birdReportsReads struct {
	townRoot  string
	loc       *time.Location
	startHour int
	maxAge    time.Duration
	reporters []string
}

// birdReportsReadsOf builds the patrol's reads from the daemon patrol config,
// or the error for a setting it cannot act on.
func birdReportsReadsOf(cfg *DaemonPatrolConfig, townRoot string, loc *time.Location) (birdReportsReads, error) {
	var c *BirdReportsConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.BirdReports
	}
	maxAge, err := c.MaxAgeValue()
	if err != nil {
		return birdReportsReads{}, err
	}
	return birdReportsReads{
		townRoot:  townRoot,
		loc:       loc,
		startHour: c.StartHourValue(),
		maxAge:    maxAge,
		reporters: c.ReportersValue(),
	}, nil
}

// birdReportStaleItems returns one item per expected reporter whose daily
// report is not fresh at now, and none before startHour in the reads' zone: the
// day's reports are not late until they are expected. A report the check cannot
// read is an item too, because an unknown age is not a fresh one.
func birdReportStaleItems(now time.Time, r birdReportsReads) []attention.Item {
	if now.In(r.loc).Hour() < r.startHour {
		return nil
	}
	var out []attention.Item
	for _, name := range r.reporters {
		if it, late := birdReportStaleItem(now, r, name); late {
			out = append(out, it)
		}
	}
	return out
}

// birdReportStaleItem judges one reporter's report: the item and true when it
// is missing, unreadable, dated in the future or older than the maximum age,
// and false when it is fresh.
func birdReportStaleItem(now time.Time, r birdReportsReads, name string) (attention.Item, bool) {
	it := attention.Item{
		Key:      birdReportsKeyPrefix + name,
		Kind:     attention.KindBirdReportStale,
		Severity: attention.SeverityLow,
	}
	runtimeDir := filepath.Join(r.townRoot, ".runtime")
	data, err := os.ReadFile(filepath.Join(birdreports.ReportDir(runtimeDir, name), birdreports.SidecarName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		it.Summary = fmt.Sprintf("%s's daily report is missing (age: none); send %s the daily request", name, name)
		return it, true
	case err != nil:
		it.Summary = fmt.Sprintf("%s's daily report could not be read (age: unknown): %v; send %s the daily request", name, err, name)
		return it, true
	}
	var daily struct {
		WrittenAt time.Time `json:"written_at"`
	}
	if err := json.Unmarshal(data, &daily); err != nil || daily.WrittenAt.IsZero() {
		it.Summary = fmt.Sprintf("%s's daily report has no readable written_at (age: unknown); send %s the daily request", name, name)
		return it, true
	}
	// A timestamp after the check's own clock is not a fresh report: the file's
	// clock (or the report) is wrong, and its age cannot be judged. Reading it
	// as fresh would let a bad timestamp hide a stale report indefinitely.
	if daily.WrittenAt.After(now) {
		it.Summary = fmt.Sprintf("%s's daily report says written_at is %s in the future (age: unknown); send %s the daily request",
			name, birdReportAgeWords(daily.WrittenAt.Sub(now)), name)
		return it, true
	}
	age := now.Sub(daily.WrittenAt)
	if age <= r.maxAge {
		return attention.Item{}, false
	}
	it.Summary = fmt.Sprintf("%s's daily report is %s old, past the %s maximum; send %s the daily request",
		name, birdReportAgeWords(age), birdReportAgeWords(r.maxAge), name)
	return it, true
}

// birdReportAgeWords says an age in an item's summary as whole hours and
// minutes, because a daily report's age does not need seconds.
func birdReportAgeWords(d time.Duration) string {
	minutes := int(d.Round(time.Minute) / time.Minute)
	return fmt.Sprintf("%dh%02dm", minutes/60, minutes%60)
}

// birdReportStale is the patrol's last check: the stale items it observed.
// The check replaces it and the attention collector reads it; the mutex keeps
// the copy safe for a caller that runs one from another goroutine.
type birdReportStale struct {
	mu    sync.Mutex
	items []attention.Item
}

// mark replaces the snapshot with items, which are copied so the caller's
// slice cannot change underneath a collector.
func (s *birdReportStale) mark(items []attention.Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append([]attention.Item(nil), items...)
}

// observed returns the snapshot, copied for the same reason.
func (s *birdReportStale) observed() []attention.Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]attention.Item(nil), s.items...)
}

// runBirdReports is one check of the patrol: it reads every expected reporter's
// daily report and replaces the items the attention collector sees. A setting
// it cannot act on raises the check-failed item in their place: a check that
// never ran tells the operator nothing about any report, and silence would
// read as a town whose reports are all fresh.
func (d *Daemon) runBirdReports() {
	if !d.isPatrolActive("bird_reports") || d.config == nil {
		return
	}
	loc := d.birdReportLoc
	if loc == nil {
		loc = time.Local
	}
	reads, err := birdReportsReadsOf(d.patrolConfig, d.config.TownRoot, loc)
	if err != nil {
		d.birdReportStale.mark([]attention.Item{birdReportCheckFailedItem(err)})
		d.logger.Printf("bird_reports: not checking: %v", err)
		return
	}
	items := birdReportStaleItems(d.clk().Now(), reads)
	d.birdReportStale.mark(items)
	d.logger.Printf("bird_reports: %d of %d daily reports are late or missing", len(items), len(reads.reporters))
}

// birdReportCheckFailedItem is the item a check that could not run raises. It
// is high severity: the reports went unjudged, so the queue must not show the
// same thing as a town whose reports were all read and found fresh.
func birdReportCheckFailedItem(err error) attention.Item {
	return attention.Item{
		Key:      birdReportsCheckFailedKey,
		Kind:     attention.KindBirdReportStale,
		Severity: attention.SeverityHigh,
		Summary:  fmt.Sprintf("the bird_reports patrol cannot check the daily reports: %v", err),
	}
}

// collectBirdReports returns the stale-report items the patrol's last check
// observed. The patrol runs on its own hourly ticker, so an item appears or
// clears at that check; between checks the queue holds what the last one
// found.
func (s *attentionSources) collectBirdReports(_ context.Context) ([]attention.Item, error) {
	if s.birdReports == nil {
		return nil, nil
	}
	return s.birdReports(), nil
}
