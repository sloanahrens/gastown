package scorecard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// ErrNoScorecard is what Read reports when the cache file is absent: no
// refresh has run yet.
var ErrNoScorecard = errors.New("no scorecard yet")

// ErrCorruptScorecard is what Read reports when the cache file holds something
// other than a scorecard document.
var ErrCorruptScorecard = errors.New("corrupt scorecard")

// CachePath returns the town's cached scorecard document,
// <town>/.runtime/jev/scorecard.json.
func CachePath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "scorecard.json")
}

// CacheDoc is the one document the cache holds: when the refresh ran, the
// window it scored, and one block per battery.
type CacheDoc struct {
	ScoredAt  time.Time      `json:"scored_at"`
	Since     string         `json:"since"`
	Batteries []BatteryBlock `json:"batteries"`
}

// BatteryBlock is one battery's part of a cached scorecard: the report the
// scorecard built, when it was scored, and whether the refresh failed. A
// failed block whose cache held a good report keeps that report with Kept set
// and KeptSince naming when it was scored; one that never scored carries the
// failure alone.
type BatteryBlock struct {
	Name     string    `json:"name"`
	Report   *Report   `json:"report,omitempty"`
	ScoredAt time.Time `json:"scored_at"`
	Failed   bool      `json:"failed,omitempty"`
	Error    string    `json:"error,omitempty"`
	Kept     bool      `json:"kept,omitempty"`
	// KeptSince is the instant the kept report was scored. It is set only
	// with Kept.
	KeptSince *time.Time `json:"kept_since,omitempty"`
	// LogLines and LogBytes fingerprint the battery's log as it was when the
	// report was scored. A refresh reuses the block while both still match
	// the log. A report kept from a failed refresh keeps that report's
	// fingerprint.
	LogLines int   `json:"log_lines,omitempty"`
	LogBytes int64 `json:"log_bytes,omitempty"`
}

// LogFingerprint is what a battery's log looked like when its block was
// scored: its line count and its size in bytes.
type LogFingerprint struct {
	Lines int
	Bytes int64
}

// ReuseAge is the oldest a cached block may be for a refresh to reuse it
// without scoring again. Labels mature as beads close, so a block older than
// this is re-scored even when its log has not changed, which is why the bound
// exists.
const ReuseAge = 72 * time.Hour

// FingerprintLog measures the log at path. An error, as for a log that is
// missing or cannot be read, tells the caller to score the battery: a log it
// cannot read is never treated as an empty one that has not changed.
func FingerprintLog(path string) (LogFingerprint, error) {
	f, err := os.Open(path)
	if err != nil {
		return LogFingerprint{}, fmt.Errorf("opening the battery log %s: %w", path, err)
	}
	defer f.Close()
	lines, size, err := countLines(f)
	if err != nil {
		return LogFingerprint{}, fmt.Errorf("reading the battery log %s: %w", path, err)
	}
	return LogFingerprint{Lines: lines, Bytes: size}, nil
}

// countLines counts the lines in r and the bytes it read, including a last
// line that ends without a newline. It holds at most one line in memory, so no
// line length makes it skip a line or fail.
func countLines(r io.Reader) (int, int64, error) {
	br := bufio.NewReader(r)
	lines, size := 0, int64(0)
	for {
		line, err := br.ReadBytes('\n')
		size += int64(len(line))
		if len(line) > 0 {
			lines++
		}
		if errors.Is(err, io.EOF) {
			return lines, size, nil
		}
		if err != nil {
			return 0, 0, err
		}
	}
}

// ReusableBlock returns the block prev holds for the battery named name when
// a refresh over window may keep it instead of scoring: the block holds a
// report scored over that same window, less than ReuseAge ago, and its
// recorded log fingerprint still matches fp. A report scored over another
// window describes different lines, and a failed or kept block would hide its
// failure if reused, so neither is reused. ok is false when name must be
// scored.
func ReusableBlock(prev CacheDoc, name string, fp LogFingerprint, window string, now time.Time) (BatteryBlock, bool) {
	if prev.Since != window {
		return BatteryBlock{}, false
	}
	for _, b := range prev.Batteries {
		if b.Name != name || b.Report == nil || b.Failed || b.Kept {
			continue
		}
		if b.LogLines != fp.Lines || b.LogBytes != fp.Bytes {
			continue
		}
		if now.Sub(b.ScoredAt) >= ReuseAge {
			continue
		}
		return b, true
	}
	return BatteryBlock{}, false
}

// KeepFailed returns block with the previous document's block for the same
// battery substituted when this refresh's scoring failed and that block holds
// a report: the report, its scored_at and the log fingerprint it was scored
// against are the last good ones, Kept is set, and KeptSince names when the
// report was scored. A failed block with no good report before it is returned
// as it is; a block that did not fail is too.
func KeepFailed(prev CacheDoc, block BatteryBlock) BatteryBlock {
	if !block.Failed || block.Kept {
		return block
	}
	for _, old := range prev.Batteries {
		if old.Name != block.Name || old.Report == nil {
			continue
		}
		keptSince := old.ScoredAt
		if old.Kept && old.KeptSince != nil {
			keptSince = *old.KeptSince
		}
		block.Report = old.Report
		block.ScoredAt = old.ScoredAt
		block.LogLines, block.LogBytes = old.LogLines, old.LogBytes
		block.Kept = true
		block.KeptSince = &keptSince
		return block
	}
	return block
}

// Cache reads and writes the scorecard cache file. Its zero value uses the
// real filesystem; a test sets WriteFile or ReadFile to stand in for a write
// that fails or a file that cannot be read.
type Cache struct {
	// WriteFile publishes the document's bytes at path. Nil means
	// atomicfile.WriteFile, which writes a temp file beside path and renames
	// it over, so a failed write leaves the previous file untouched.
	WriteFile func(path string, data []byte, perm os.FileMode) error
	// ReadFile reads the document's bytes at path. Nil means os.ReadFile.
	ReadFile func(path string) ([]byte, error)
	// Lock takes an exclusive lock on the file at path and returns the func
	// that releases it. Nil means a flock on path, which waits until the lock
	// is free.
	Lock func(path string) (unlock func() error, err error)
}

// Write stores doc at path, creating the parent directory if it is missing.
func (c Cache) Write(path string, doc CacheDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the scorecard cache: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the scorecard cache directory for %s: %w", path, err)
	}
	write := c.WriteFile
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(path, data, 0o644); err != nil {
		return fmt.Errorf("writing the scorecard cache %s: %w", path, err)
	}
	return nil
}

// Read loads the document at path. A missing file is ErrNoScorecard and a file
// that does not parse as a scorecard is ErrCorruptScorecard; both errors name
// the path.
func (c Cache) Read(path string) (CacheDoc, error) {
	read := c.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return CacheDoc{}, fmt.Errorf("reading the scorecard cache %s: %w", path, ErrNoScorecard)
		}
		return CacheDoc{}, fmt.Errorf("reading the scorecard cache %s: %w", path, err)
	}
	var doc CacheDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return CacheDoc{}, fmt.Errorf("%w %s: %w", ErrCorruptScorecard, path, err)
	}
	if doc.ScoredAt.IsZero() {
		return CacheDoc{}, fmt.Errorf("%w %s: no scored_at", ErrCorruptScorecard, path)
	}
	for _, b := range doc.Batteries {
		if b.Name == "" {
			return CacheDoc{}, fmt.Errorf("%w %s: a battery block has no name", ErrCorruptScorecard, path)
		}
		if b.ScoredAt.IsZero() {
			return CacheDoc{}, fmt.Errorf("%w %s: battery %q has no scored_at", ErrCorruptScorecard, path, b.Name)
		}
	}
	return doc, nil
}

// ErrNoHistory is what ReadHistory reports when the history file is absent: no
// refresh has run yet.
var ErrNoHistory = errors.New("no scorecard history yet")

// ErrCorruptHistory is what ReadHistory reports when a line of the history file
// does not parse as a history row.
var ErrCorruptHistory = errors.New("corrupt scorecard history")

// HistoryRetention is how long a history row is kept. A row older than this is
// dropped the next time a refresh writes the file.
const HistoryRetention = 90 * 24 * time.Hour

// HistoryPath returns the town's refresh history, one JSON row per line:
// <town>/.runtime/jev/scorecard.history.jsonl.
func HistoryPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirRuntime, "jev", "scorecard.history.jsonl")
}

// HistoryRow is one refresh's line in the history: when it ran, the window it
// scored, and each freshly scored battery's per-question counts and verdicts.
// A battery reused unchanged or kept from a failed refresh has no entry, so a
// reader never takes a reused block for growth.
type HistoryRow struct {
	At        time.Time        `json:"at"`
	Since     string           `json:"since"`
	Batteries []HistoryBattery `json:"batteries"`
}

// HistoryBattery is one battery's part of a history row.
type HistoryBattery struct {
	Name      string            `json:"name"`
	Questions []HistoryQuestion `json:"questions"`
}

// HistoryQuestion is one question's labeled count, positives and verdict as
// the refresh scored them.
type HistoryQuestion struct {
	ID        string `json:"id"`
	Labeled   int    `json:"labeled"`
	Positives int    `json:"positives"`
	Verdict   string `json:"verdict"`
}

// HistoryRowFor returns the row a refresh at at over window appends. Only the
// blocks named in fresh, the batteries this refresh scored, are recorded. A
// block that is kept from a failed refresh, or that has no report, is skipped
// even when named, so a failure never reads as growth.
func HistoryRowFor(at time.Time, window string, doc CacheDoc, fresh map[string]bool) HistoryRow {
	row := HistoryRow{At: at, Since: window, Batteries: []HistoryBattery{}}
	for _, b := range doc.Batteries {
		if !fresh[b.Name] || b.Kept || b.Failed || b.Report == nil {
			continue
		}
		hb := HistoryBattery{Name: b.Name, Questions: make([]HistoryQuestion, 0, len(b.Report.Questions))}
		for _, q := range b.Report.Questions {
			hb.Questions = append(hb.Questions, HistoryQuestion{
				ID:        q.ID,
				Labeled:   q.Labeled,
				Positives: q.Positives,
				Verdict:   q.Verdict,
			})
		}
		row.Batteries = append(row.Batteries, hb)
	}
	return row
}

// AppendHistory appends row to the history at path and drops every row older
// than HistoryRetention at now. The read and the write run under a lock on
// <path>.lock, so two refreshes that finish together cannot each rewrite the
// file from the copy they read and lose the other's row.
//
// A line that does not parse as a dated row is moved to <path>.corrupt rather
// than kept, so it cannot block ReadHistory. The move appends to that file
// before the history is rewritten, so a line is never lost; if the rewrite then
// fails, the same line is copied to the sidecar again on the next refresh.
//
// A failure is written to log as one line and is not returned: the refresh
// that calls this has already written the cache, and a history that cannot be
// written must not fail that refresh. A file that cannot be read is left
// untouched rather than rewritten from nothing.
func (c Cache) AppendHistory(path string, row HistoryRow, now time.Time, log io.Writer) {
	if err := c.appendHistory(path, row, now); err != nil {
		if log != nil {
			fmt.Fprintf(log, "scorecard history not appended: %v\n", err)
		}
	}
}

// corruptHistoryPath is the sidecar that holds the history lines moved aside
// because they did not parse as dated rows.
func corruptHistoryPath(path string) string {
	return path + ".corrupt"
}

func (c Cache) appendHistory(path string, row HistoryRow, now time.Time) (err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the scorecard history directory for %s: %w", path, err)
	}
	lock := c.Lock
	if lock == nil {
		lock = flockFile
	}
	unlock, err := lock(path + ".lock")
	if err != nil {
		return fmt.Errorf("locking the scorecard history %s: %w", path, err)
	}
	defer func() {
		if uerr := unlock(); uerr != nil && err == nil {
			err = fmt.Errorf("unlocking the scorecard history %s: %w", path, uerr)
		}
	}()

	read := c.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("reading the scorecard history %s: %w", path, err)
	}
	cutoff := now.Add(-HistoryRetention)
	var out, corrupt bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		parsed, perr := parseHistoryLine(string(line))
		if perr != nil {
			corrupt.Write(line)
			corrupt.WriteByte('\n')
			continue
		}
		if parsed.At.Before(cutoff) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("encoding the scorecard history row: %w", err)
	}
	out.Write(encoded)
	out.WriteByte('\n')
	if corrupt.Len() > 0 {
		if err := appendBytes(corruptHistoryPath(path), corrupt.Bytes()); err != nil {
			return fmt.Errorf("moving unparseable scorecard history lines to %s: %w", corruptHistoryPath(path), err)
		}
	}
	write := c.WriteFile
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(path, out.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing the scorecard history %s: %w", path, err)
	}
	return nil
}

// appendBytes appends data to the file at path, creating it if it is missing.
func appendBytes(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	return errors.Join(werr, f.Close())
}

// flockFile takes an exclusive flock on path, creating the file if it is
// missing, and returns the func that releases it. It blocks until the lock is
// free. gofrs/flock is used rather than internal/lock, whose flock is unix-only
// (flock_unix.go).
func flockFile(path string) (func() error, error) {
	fl := flock.New(path)
	if err := fl.Lock(); err != nil {
		return nil, err
	}
	return fl.Unlock, nil
}

// parseHistoryLine decodes one line of the history. A line is a row only when
// it parses and carries an at, which is the test both ReadHistory and the
// writer apply.
func parseHistoryLine(line string) (HistoryRow, error) {
	var row HistoryRow
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return HistoryRow{}, fmt.Errorf("does not parse: %w", err)
	}
	if row.At.IsZero() {
		return HistoryRow{}, errors.New("has no at")
	}
	return row, nil
}

// ReadHistory loads every row of the history at path, oldest first. A missing
// file is ErrNoHistory; a line that does not parse is ErrCorruptHistory naming
// the line, so the view never shows a history with a row silently missing.
func (c Cache) ReadHistory(path string) ([]HistoryRow, error) {
	read := c.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("reading the scorecard history %s: %w", path, ErrNoHistory)
		}
		return nil, fmt.Errorf("reading the scorecard history %s: %w", path, err)
	}
	var rows []HistoryRow
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		row, err := parseHistoryLine(line)
		if err != nil {
			return nil, fmt.Errorf("%w %s: line %d %w", ErrCorruptHistory, path, i+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// noGrowthRuns is how many trailing refreshes must share a labeled count before
// the history says the question has not grown.
const noGrowthRuns = 3

// WriteHistory prints each question's series from rows, one block per battery
// and question, in the order they first appear. An empty battery or question
// matches every one. A row's date, labeled count, positives and verdict print
// with the change in labeled since the previous row. When the last noGrowthRuns
// or more rows share a labeled count, the block ends by saying how many
// refreshes saw no growth.
func WriteHistory(w io.Writer, rows []HistoryRow, battery, question string) error {
	type point struct {
		at        time.Time
		labeled   int
		positives int
		verdict   string
	}
	type series struct {
		battery, question string
		points            []point
	}
	var order []*series
	index := map[[2]string]*series{}
	for _, row := range rows {
		for _, b := range row.Batteries {
			if battery != "" && b.Name != battery {
				continue
			}
			for _, q := range b.Questions {
				if question != "" && q.ID != question {
					continue
				}
				key := [2]string{b.Name, q.ID}
				s := index[key]
				if s == nil {
					s = &series{battery: b.Name, question: q.ID}
					index[key] = s
					order = append(order, s)
				}
				s.points = append(s.points, point{row.At, q.Labeled, q.Positives, q.Verdict})
			}
		}
	}
	ew := &errWriter{w: w}
	if len(order) == 0 {
		ew.printf("no refresh history for %s\n", historyFilterText(battery, question))
		return ew.err
	}
	for _, s := range order {
		ew.printf("%s %s\n", s.battery, s.question)
		ew.printf("  %-16s  %7s  %9s  %-9s  %7s\n", "date", "labeled", "positives", "verdict", "change")
		for i, p := range s.points {
			change := "-"
			if i > 0 {
				change = fmt.Sprintf("%+d", p.labeled-s.points[i-1].labeled)
			}
			ew.printf("  %-16s  %7d  %9d  %-9s  %7s\n", p.at.Local().Format("2006-01-02 15:04"), p.labeled, p.positives, p.verdict, change)
		}
		if n := trailingSameLabeled(len(s.points), func(i int) int { return s.points[i].labeled }); n >= noGrowthRuns {
			ew.printf("no growth in %d refreshes\n", n)
		}
		ew.printf("\n")
	}
	return ew.err
}

// trailingSameLabeled counts the rows at the end of a series that share the
// last row's labeled count.
func trailingSameLabeled(n int, labeled func(i int) int) int {
	if n == 0 {
		return 0
	}
	run := 1
	for i := n - 1; i > 0 && labeled(i) == labeled(i-1); i-- {
		run++
	}
	return run
}

// historyFilterText names the filter a history view was asked for.
func historyFilterText(battery, question string) string {
	switch {
	case battery != "" && question != "":
		return fmt.Sprintf("battery %s question %s", battery, question)
	case battery != "":
		return "battery " + battery
	case question != "":
		return "question " + question
	default:
		return "any battery or question"
	}
}
