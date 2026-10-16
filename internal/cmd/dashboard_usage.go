package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Usage panel's reader: the per-record files the usage scan writes under
// <townRoot>/.runtime/usage, folded by AggregateUsage into the payload the page
// draws. The panel only reads - no child process, no bead - which is what lets
// the hub poll it on the same minute its other slow readers use.
//
// The records directory holds about 1700 files, one per bead and kind, so a
// poll must not re-open them: a record is decoded once per mtime, and the files
// no list on the page can reach are never opened at all.

const (
	// usageRecordsSubdir holds the scan's per-record files: <bead>.work.json
	// and <bead>.om.json, plus one per deep-review run.
	usageRecordsSubdir = "beads"
	// usageStateFile is the scan's own state. Its mtime is when the scan last
	// ran, which is the panel's AsOf.
	usageStateFile = "state.json"
	// usageUnmatchedFile holds one line per transcript session the scan matched
	// to no bead.
	usageUnmatchedFile = "unmatched.jsonl"
	// usageRecordsExt is the only extension the reader opens.
	usageRecordsExt = ".json"
	// usageStaleRecord is how old a record file may be and still be read for the
	// panel's own lists. The Over20 list reaches back seven days, so an older
	// file cannot change one number the page draws, and leaving it unopened is
	// most of what keeps a poll of a 1700-file directory cheap.
	usageStaleRecord = 8 * 24 * time.Hour
	// usageMonthRecord is how old a record file may be and still be read for the
	// Claude plan's API-equivalent figures, whose month reaches back to the
	// first of the billing month: a calendar month is at most 31 days, and the
	// extra day covers a record that ends on the boundary itself.
	usageMonthRecord = 32 * 24 * time.Hour
)

// dashUsageReader reads the usage scan's records. dir is the usage root, so the
// reading and its cache are testable against a temp directory; the cache holds
// each file's last decode, keyed by path, and a file the directory no longer
// holds leaves with it.
//
// Two of the hub's pollers read through it - the Usage panel's day and the plan
// line's month - and each runs on its own goroutine, so the cache is guarded by
// mu (gt-d8sem).
type dashUsageReader struct {
	dir string
	mu  sync.Mutex
	// cache is guarded by mu.
	cache map[string]usageCached
}

// usageCached is one record file as it was when last read.
type usageCached struct {
	modTime time.Time
	rec     dashboard.UsageRecord
}

func newDashUsageReader(townRoot string) *dashUsageReader {
	return &dashUsageReader{
		dir:   filepath.Join(townRoot, ".runtime", "usage"),
		cache: map[string]usageCached{},
	}
}

// read is the panel's whole reading. A records directory that cannot be listed
// is an unavailable panel and never an empty day: "nothing ran today" and "the
// records could not be read" are different things to tell the operator.
func (r *dashUsageReader) read(now time.Time) *dashboard.Usage {
	entries, err := os.ReadDir(r.recordsDir())
	if err != nil {
		return &dashboard.Usage{Unavailable: true}
	}
	u := dashboard.AggregateUsage(r.records(entries, now.Add(-usageStaleRecord)), now, r.unmatched())
	u.AsOf = r.asOf()
	return &u
}

// monthRecords is the record set the Claude plan's API-equivalent figures are
// computed from: the same files the panel reads, reaching back far enough to
// hold the whole billing month. A records directory that cannot be listed is
// nil, which the plan reader draws as no figure at all rather than as a month
// of no spend.
func (r *dashUsageReader) monthRecords(now time.Time) []dashboard.UsageRecord {
	entries, err := os.ReadDir(r.recordsDir())
	if err != nil {
		return nil
	}
	return r.records(entries, now.Add(-usageMonthRecord))
}

// recordsDir is where the scan writes one record per bead and kind.
func (r *dashUsageReader) recordsDir() string {
	return filepath.Join(r.dir, usageRecordsSubdir)
}

// records decodes the directory's record files whose mtime is at or after since,
// leaving older ones unopened. A file the scan wrote badly becomes the zero
// record the aggregate counts as Skipped, so it costs the panel a count and
// never its page.
//
// Two readers reach into this directory at once - the panel's day and the plan's
// month - and they share the cache rather than each keeping their own: a file
// either has decoded is served from the cache to the other even when it is out
// of that one's reach, so the wider reader costs the narrower one no re-decodes.
// The cache is dropped only for files the directory no longer holds.
func (r *dashUsageReader) records(entries []os.DirEntry, since time.Time) []dashboard.UsageRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := r.recordsDir()
	recs := make([]dashboard.UsageRecord, 0, len(entries))
	live := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), usageRecordsExt) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		live[path] = true
		info, err := e.Info()
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		rec, ok := r.cached(path, modTime)
		if !ok {
			if modTime.Before(since) {
				continue
			}
			rec = r.decode(path)
			r.cache[path] = usageCached{modTime: modTime, rec: rec}
		}
		if modTime.Before(since) {
			continue
		}
		recs = append(recs, rec)
	}
	for path := range r.cache {
		if !live[path] {
			delete(r.cache, path)
		}
	}
	return recs
}

// cached is the file's last decode, when the file is still the one at this
// mtime. A decode that failed is cached like any other, so a corrupt file is
// opened once rather than on every poll.
func (r *dashUsageReader) cached(path string, modTime time.Time) (dashboard.UsageRecord, bool) {
	c, ok := r.cache[path]
	return c.rec, ok && c.modTime.Equal(modTime)
}

// decode reads and parses one record file. A read or parse failure is the zero
// record - no bead, no end time - which the aggregate counts as Skipped.
func (r *dashUsageReader) decode(path string) dashboard.UsageRecord {
	b, err := os.ReadFile(path)
	if err != nil {
		return dashboard.UsageRecord{}
	}
	var rec dashboard.UsageRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return dashboard.UsageRecord{}
	}
	return rec
}

// unmatched counts the lines of unmatched.jsonl: one per transcript session the
// scan matched to no bead. A file that is not there counts none, since the
// count annotates the day's payload rather than being it.
func (r *dashUsageReader) unmatched() int {
	b, err := os.ReadFile(filepath.Join(r.dir, usageUnmatchedFile))
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// asOf is the scan's last run, as the page draws it: the mtime of the scan's
// state file, empty when that file is not there. Only the value is provided;
// the page decides when it has gone stale.
func (r *dashUsageReader) asOf() string {
	info, err := os.Stat(filepath.Join(r.dir, usageStateFile))
	if err != nil {
		return ""
	}
	return info.ModTime().UTC().Format(time.RFC3339)
}
