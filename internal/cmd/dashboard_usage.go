package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	// usageStaleRecord is how old a record file may be and still be read. The
	// Over20 list reaches back seven days, so an older file cannot change one
	// number the page draws, and leaving it unopened is most of what keeps a
	// poll of a 1700-file directory cheap.
	usageStaleRecord = 8 * 24 * time.Hour
)

// dashUsageReader reads the usage scan's records. dir is the usage root, so the
// reading and its cache are testable against a temp directory; the cache holds
// each file's last decode, keyed by path, and is swapped whole on every read so
// a removed file leaves with it.
type dashUsageReader struct {
	dir   string
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
	u := dashboard.AggregateUsage(r.records(entries, now), now, r.unmatched())
	u.AsOf = r.asOf()
	return &u
}

// recordsDir is where the scan writes one record per bead and kind.
func (r *dashUsageReader) recordsDir() string {
	return filepath.Join(r.dir, usageRecordsSubdir)
}

// records decodes the directory's record files, leaving the ones too old for
// any list unopened. A file the scan wrote badly becomes the zero record the
// aggregate counts as Skipped, so it costs the panel a count and never its
// page.
func (r *dashUsageReader) records(entries []os.DirEntry, now time.Time) []dashboard.UsageRecord {
	dir := r.recordsDir()
	stale := now.Add(-usageStaleRecord)
	recs := make([]dashboard.UsageRecord, 0, len(entries))
	kept := make(map[string]usageCached, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), usageRecordsExt) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		modTime := info.ModTime()
		if modTime.Before(stale) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		rec, ok := r.cached(path, modTime)
		if !ok {
			rec = r.decode(path)
		}
		kept[path] = usageCached{modTime: modTime, rec: rec}
		recs = append(recs, rec)
	}
	r.cache = kept
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
