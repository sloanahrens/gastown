package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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
	// usageRowsLimit caps one poll's read of the usage_records rows. A read that
	// fills it is refused rather than folded: the rows that fell off the cap are
	// the window's oldest, so a total built from them would be short without
	// saying so (the claude_plan rows keep the same cap, gt-u9dsir).
	usageRowsLimit = 20000
	// usageRowsTimeout bounds one poll's read of the usage_records rows, so a bd
	// that hangs cannot hold a poller. It is dashboardSpendTimeout, the bound the
	// dashboard's external command reads share.
	usageRowsTimeout = dashboardSpendTimeout
	// The reasons a panel's row reading is unavailable. Each names the cause in
	// fixed words, so no output of the row store reaches the page.
	usageRowsReadReason     = "the usage_records rows could not be read"
	usageRowsTimedOutReason = "the usage_records read timed out"
	usageRowsCapReason      = "the usage_records read filled its cap, so the window's start is not in the rows"
	usageRowsConflictReason = "two usage_records rows count the same session with different totals"
)

// usageRowsRead reads the town's usage_records rows built at or after since, up
// to limit, newest built first. It is the beads store's ReadUsageRecords; the
// context carries the read's deadline.
type usageRowsRead func(ctx context.Context, since time.Time, limit int) ([]beads.UsageRecordRow, error)

// dashUsageReader reads the usage scan's records. dir is the usage root, so the
// reading and its cache are testable against a temp directory; the cache holds
// each file's last decode, keyed by path, and a file the directory no longer
// holds leaves with it.
//
// The rows are read first and the files fold in beside them: a row outlives the
// file path a later bead removes, so a record filed only as a row still counts.
//
// Two of the hub's pollers read through it - the Usage panel's day and the plan
// line's month - and each runs on its own goroutine, so the cache is guarded by
// mu (gt-d8sem).
type dashUsageReader struct {
	dir string
	// rows reads the usage_records rows. Nil until withRows wires it; with no
	// rows the reader folds the files alone.
	rows usageRowsRead
	// rowsTimeout bounds one row read. newDashUsageReader sets usageRowsTimeout.
	rowsTimeout time.Duration
	mu          sync.Mutex
	// cache is guarded by mu.
	cache map[string]usageCached
}

// usageKey is a record's identity in the records directory: one file per bead
// and kind, so a row for one kind never hides a file of the other.
type usageKey struct {
	bead, kind string
}

// usageKeyOf is the identity of rec.
func usageKeyOf(rec dashboard.UsageRecord) usageKey {
	return usageKey{bead: rec.Bead, kind: rec.Kind}
}

// usageCached is one record file as it was when last read.
type usageCached struct {
	modTime time.Time
	rec     dashboard.UsageRecord
}

func newDashUsageReader(townRoot string) *dashUsageReader {
	return &dashUsageReader{
		dir:         filepath.Join(townRoot, ".runtime", "usage"),
		rowsTimeout: usageRowsTimeout,
		cache:       map[string]usageCached{},
	}
}

// withRows wires the town's usage_records rows into the reader.
func (r *dashUsageReader) withRows(rows usageRowsRead) *dashUsageReader {
	r.rows = rows
	return r
}

// dashboardUsageRows reads the town's usage_records rows through the town's beads
// store, the store the seat-end and om-review writers file them in.
func dashboardUsageRows(townRoot string) usageRowsRead {
	client := beads.New(townRoot)
	return func(ctx context.Context, since time.Time, limit int) ([]beads.UsageRecordRow, error) {
		return client.ReadUsageRecords(ctx, since, limit)
	}
}

// read is the panel's whole reading. A row read that fails, or that cannot be
// taken whole, is an unavailable panel with the reason it could not be, and so is
// a records directory that cannot be listed: "nothing ran today" and "the records
// could not be read" are different things to tell the operator.
func (r *dashUsageReader) read(now time.Time) *dashboard.Usage {
	since := now.Add(-usageStaleRecord)
	rows, reason := r.rowRecords(since)
	if reason != "" {
		return &dashboard.Usage{Unavailable: true, Reason: reason}
	}
	entries, err := r.entries()
	if err != nil {
		return unavailableUsage(err)
	}
	u := dashboard.AggregateUsage(r.merge(rows, entries, since), now, r.unmatched())
	u.AsOf = r.asOf()
	return &u
}

// rowRecords is the usage_records rows built at or after since, as the records
// they hold, newest built first. Nil and no reason when no row store is wired.
// A reason, with nil records, says why the rows cannot be taken whole: the read
// failed or ran past rowsTimeout, it filled usageRowsLimit, or its rows share a
// session. The reason is fixed text, never the store's own output, since that
// can carry a secret.
func (r *dashUsageReader) rowRecords(since time.Time) ([]dashboard.UsageRecord, string) {
	if r.rows == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.rowsTimeout)
	defer cancel()
	rows, err := r.rows(ctx, since, usageRowsLimit)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, usageRowsTimedOutReason
		}
		return nil, usageRowsReadReason
	}
	if len(rows) == usageRowsLimit {
		return nil, usageRowsCapReason
	}
	recs, err := dedupeUsageRows(rows)
	if err != nil {
		return nil, usageRowsConflictReason
	}
	return recs, ""
}

// usageKeptRow is a row dedupeUsageRows keeps: its record and the sessions it
// counts.
type usageKeptRow struct {
	rec      dashboard.UsageRecord
	sessions map[string]bool
}

// errUsageRowsConflict is dedupeUsageRows' refusal: two rows count one session
// but are not the same run.
var errUsageRowsConflict = errors.New("usage_records rows share a session but are not the same run")

// dedupeUsageRows is the records of rows, newest built first, each session
// counted once. A row that repeats an earlier row of its bead and kind - the same
// sessions and the same record - is dropped, as the usage record writer drops a
// repeated write. Any other session the row shares with an earlier one is refused,
// since the totals of two rows that count one session cannot be split. A row with
// no sessions claims no session that could overlap, so it is dropped only when an
// earlier sessionless row of its bead and kind is the same record; otherwise it is
// kept.
func dedupeUsageRows(rows []beads.UsageRecordRow) ([]dashboard.UsageRecord, error) {
	kept := make(map[usageKey][]usageKeptRow, len(rows))
	recs := make([]dashboard.UsageRecord, 0, len(rows))
	for _, row := range rows {
		key := usageKeyOf(row.Record)
		sessions := make(map[string]bool, len(row.Sessions))
		for _, s := range row.Sessions {
			sessions[s] = true
		}
		if len(sessions) == 0 {
			if usageRowRepeats(kept[key], row.Record) {
				continue
			}
			kept[key] = append(kept[key], usageKeptRow{rec: row.Record, sessions: sessions})
			recs = append(recs, row.Record)
			continue
		}
		repeat := false
		for _, k := range kept[key] {
			shared := 0
			for s := range sessions {
				if k.sessions[s] {
					shared++
				}
			}
			if shared == 0 {
				continue
			}
			if shared != len(sessions) || len(k.sessions) != len(sessions) || !reflect.DeepEqual(k.rec, row.Record) {
				return nil, errUsageRowsConflict
			}
			repeat = true
			break
		}
		if repeat {
			continue
		}
		kept[key] = append(kept[key], usageKeptRow{rec: row.Record, sessions: sessions})
		recs = append(recs, row.Record)
	}
	return recs, nil
}

// usageRowRepeats says a sessionless row of rec is already kept: an earlier kept
// row of its bead and kind that counts no session and is the same record.
func usageRowRepeats(kept []usageKeptRow, rec dashboard.UsageRecord) bool {
	for _, k := range kept {
		if len(k.sessions) == 0 && reflect.DeepEqual(k.rec, rec) {
			return true
		}
	}
	return false
}

// merge is the record set both readers fold: rows first, then each file record
// whose bead and kind no row holds. A row wins over the file of the same bead
// and kind, so that record is never counted twice.
func (r *dashUsageReader) merge(rows []dashboard.UsageRecord, entries []os.DirEntry, since time.Time) []dashboard.UsageRecord {
	held := make(map[usageKey]bool, len(rows))
	recs := make([]dashboard.UsageRecord, 0, len(rows)+len(entries))
	for _, rec := range rows {
		held[usageKeyOf(rec)] = true
		recs = append(recs, rec)
	}
	for _, rec := range r.records(entries, since) {
		if !held[usageKeyOf(rec)] {
			recs = append(recs, rec)
		}
	}
	return recs
}

// entries lists the records directory. Once a row store is wired a directory
// that is not there is no entries rather than an error, since the rows outlive
// the files; without one, a directory that is not there is the error.
func (r *dashUsageReader) entries() ([]os.DirEntry, error) {
	entries, err := os.ReadDir(r.recordsDir())
	if err != nil && r.rows != nil && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return entries, err
}

// unavailableUsage is the panel when the records directory cannot be listed. A
// directory that is not there is the cluster's absence of the usage scan, named
// by its reason; any other failure, such as a directory the process cannot read,
// is a failed read and carries no reason.
func unavailableUsage(err error) *dashboard.Usage {
	if errors.Is(err, fs.ErrNotExist) {
		return &dashboard.Usage{Unavailable: true, Reason: dashboard.NotInClusterReason}
	}
	return &dashboard.Usage{Unavailable: true}
}

// absent says the records directory is not there at all, the test the Claude
// plan reader makes before it draws its own absence. Any other stat failure is
// not absence, so it is not reported as one.
func (r *dashUsageReader) absent() bool {
	_, err := os.Stat(r.recordsDir())
	return errors.Is(err, fs.ErrNotExist)
}

// monthRecords is the record set the Claude plan's API-equivalent figures are
// computed from: the same rows and files the panel reads, reaching back far
// enough to hold the whole billing month. A row read that cannot be taken whole,
// or a records directory that cannot be listed, is nil, which the plan reader
// draws as no figure at all rather than as a month of no spend.
func (r *dashUsageReader) monthRecords(now time.Time) []dashboard.UsageRecord {
	since := now.Add(-usageMonthRecord)
	rows, reason := r.rowRecords(since)
	if reason != "" {
		return nil
	}
	entries, err := r.entries()
	if err != nil {
		return nil
	}
	return r.merge(rows, entries, since)
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
