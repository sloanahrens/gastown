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
)

// The bird_reports patrol (gt-cbsat): every hour the daemon checks each
// expected reporter's daily report and, from start_hour on, raises one
// attention item, bird-report-stale:<name>, while that report is missing or
// older than the configured maximum age. The daily reports are what the Birds
// panel shows; a session cron writes them, and a cron that dies with its
// session is exactly the lapse this patrol catches.
//
// The patrol is opt-in (patrols.bird_reports.enabled). It reads
// <town>/.runtime/<name>/reports/daily.json and nothing else, and its only
// write is the in-memory snapshot the attention collector reads.

const (
	// birdReportsKeyPrefix is the item key's prefix; the reporter's name
	// follows it, so one reporter has one item however often it is late.
	birdReportsKeyPrefix = "bird-report-stale:"

	// birdReportsCheckEvery is how often the patrol reads the reports. The
	// reports are daily, so an hourly look notices a missed one the same
	// morning without turning the check into a busy loop.
	birdReportsCheckEvery = time.Hour

	// birdReportsFile is the report the Birds panel reads, relative to a
	// reporter's runtime directory. The patrol reads no other file.
	birdReportsFile = "daily.json"
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
// report is missing or older than maxAge at now, and none before startHour in
// the reads' zone: the day's reports are not late until they are expected. A
// report the check cannot read is an item too, because an unknown age is not a
// fresh one.
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
// is missing, unreadable or older than the maximum age, and false when it is
// fresh.
func birdReportStaleItem(now time.Time, r birdReportsReads, name string) (attention.Item, bool) {
	it := attention.Item{
		Key:      birdReportsKeyPrefix + name,
		Kind:     attention.KindBirdReportStale,
		Severity: attention.SeverityLow,
	}
	data, err := os.ReadFile(filepath.Join(r.townRoot, ".runtime", name, "reports", birdReportsFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		it.Summary = fmt.Sprintf("%s's daily report is missing (age: none); run gt birds due to print the request to send", name)
		return it, true
	case err != nil:
		it.Summary = fmt.Sprintf("%s's daily report could not be read (age: unknown): %v; run gt birds due to print the request to send", name, err)
		return it, true
	}
	var daily struct {
		WrittenAt time.Time `json:"written_at"`
	}
	if err := json.Unmarshal(data, &daily); err != nil || daily.WrittenAt.IsZero() {
		it.Summary = fmt.Sprintf("%s's daily report has no readable written_at (age: unknown); run gt birds due to print the request to send", name)
		return it, true
	}
	age := now.Sub(daily.WrittenAt)
	if age <= r.maxAge {
		return attention.Item{}, false
	}
	it.Summary = fmt.Sprintf("%s's daily report is %s old, past the %s maximum; run gt birds due to print the request to send",
		name, birdReportAgeWords(age), birdReportAgeWords(r.maxAge))
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
// it cannot act on keeps the last check's items rather than clearing them: a
// config this patrol cannot read is not a town whose reports are fresh.
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
		d.logger.Printf("bird_reports: not checking: %v", err)
		return
	}
	items := birdReportStaleItems(d.clk().Now(), reads)
	d.birdReportStale.mark(items)
	d.logger.Printf("bird_reports: %d of %d daily reports are late or missing", len(items), len(reads.reporters))
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
