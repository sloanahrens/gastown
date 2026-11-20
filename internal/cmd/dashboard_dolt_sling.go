package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The sling section of the Dolt panel: the dispatcher's own timing, which
// survives only in the daemon log's "spec_dispatch: <entry> (sling took
// <duration>)" lines (internal/daemon/spec_slings.go). The section is one row
// over the hour ending at the read, so an operator sees how long slings are
// taking without digging the log out by hand (gt-nqs8th).

const (
	// doltSlingWindow is the span the section reads: the hour ending at the
	// read's clock.
	doltSlingWindow = time.Hour
	// doltSlingTailBytes bounds the log a poll reads, so an hour's reading
	// never loads a whole rotated-in log.
	doltSlingTailBytes = 1 << 20
	// doltSlingStampLayout is the daemon logger's own line prefix
	// (log.LstdFlags), which the daemon writes in the host's local zone.
	doltSlingStampLayout = "2006/01/02 15:04:05"
	// doltSlingNoValue is the cell a row draws for a figure the hour has none
	// of, so a quiet hour reads as empty rather than as zero.
	doltSlingNoValue = "—"
	// doltSlingClockLayout is how the row names the worst sling's line: the
	// time of day, since the window is the last hour.
	doltSlingClockLayout = "15:04:05"
)

func init() {
	registerDoltSection("sling", readDoltSlingSection)
}

// doltSlingLineRe matches one line the dispatcher writes after a sling. The
// entry text is lazy, so a line that mentions the phrase twice still matches at
// the duration that closes it.
var doltSlingLineRe = regexp.MustCompile(`^(\d{4}/\d\d/\d\d \d\d:\d\d:\d\d) spec_dispatch: (.+?) \(sling took (\S+)\)$`)

// doltSlingBeadRe is the shape of a bead id, so prose that happens to precede
// a colon is not drawn as a bead.
var doltSlingBeadRe = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9][a-z0-9.-]*$`)

// doltSlingCategories are the words the dispatcher prefixes a report entry
// with, before the entry's own "<bead>: ..." text.
var doltSlingCategories = []string{"dispatched: ", "failed: ", "deferred: ", "note: ", "error: "}

// readDoltSlingSection draws the section from the town root the panel hands
// its read. A read without a root fails its section rather than reading the
// empty root.
func readDoltSlingSection(ctx context.Context, now time.Time) (dashboard.DoltSection, error) {
	root, ok := doltTownRoot(ctx)
	if !ok || root == "" {
		return dashboard.DoltSection{}, errors.New("daemon log unreadable: the read carries no town root")
	}
	return doltSlingSection(filepath.Join(root, "daemon", "daemon.log"), now)
}

// doltSlingFile is an open log as the tail reader needs it: its size, its
// seek and its reads. A test drives each unreadable-file branch with a fake,
// so the section's failure path never needs a real file.
type doltSlingFile interface {
	io.ReadSeeker
	Stat() (os.FileInfo, error)
}

// doltSlingSection reads the log at path and draws the hour ending at now.
func doltSlingSection(path string, now time.Time) (dashboard.DoltSection, error) {
	f, err := os.Open(path)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("daemon log unreadable: %w", err)
	}
	defer func() { _ = f.Close() }()
	return doltSlingSectionFrom(f, now)
}

// doltSlingSectionFrom draws the hour ending at now from an open log.
func doltSlingSectionFrom(f doltSlingFile, now time.Time) (dashboard.DoltSection, error) {
	lines, err := readDoltSlingTailFrom(f)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("daemon log unreadable: %w", err)
	}
	samples, err := parseDoltSlingLines(lines, now)
	if err != nil {
		return dashboard.DoltSection{}, fmt.Errorf("daemon log unreadable: %w", err)
	}
	sec := dashboard.DoltSection{
		Title: "Sling duration, last hour",
		Head:  []string{"count", "median", "p90", "worst", "worst bead", "worst at"},
	}
	// The Note is the only sign the hour is drawn from a shorter span than it
	// names.
	if !doltSlingReachesBack(lines, now) {
		sec.Note = "the log did not reach back a full hour"
	}
	sec.Rows = [][]string{doltSlingRow(summarizeDoltSlings(samples))}
	return sec, nil
}

// readDoltSlingTailFrom reads at most the log's last doltSlingTailBytes as
// whole lines. The fragment a window that starts mid-file opens in is not a
// line, and neither is a last line the writer has not finished: the next poll
// reads it whole.
func readDoltSlingTailFrom(f doltSlingFile) ([]string, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var offset int64
	if info.Size() > doltSlingTailBytes {
		offset = info.Size() - doltSlingTailBytes
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, doltSlingTailBytes))
	if err != nil {
		return nil, err
	}
	text := string(b)
	if offset > 0 {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			return nil, nil
		}
		text = text[i+1:]
	}
	i := strings.LastIndexByte(text, '\n')
	if i < 0 {
		return nil, nil
	}
	return strings.Split(strings.TrimSuffix(text[:i+1], "\n"), "\n"), nil
}

// doltSlingSample is one sling line: when the daemon wrote it, how long the
// sling took, and the bead the line names (empty for a line naming none).
type doltSlingSample struct {
	at   time.Time
	took time.Duration
	bead string
}

// parseDoltSlingLines reads the sling lines stamped in the hour ending at now.
// A line the dispatcher's shape does not explain is a line this reader cannot
// trust, so it fails the reading rather than shrinking the count.
func parseDoltSlingLines(lines []string, now time.Time) ([]doltSlingSample, error) {
	start := now.Add(-doltSlingWindow)
	var out []doltSlingSample
	for _, line := range lines {
		if !strings.Contains(line, "spec_dispatch: ") || !strings.Contains(line, "(sling took ") {
			continue
		}
		m := doltSlingLineRe.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if m == nil {
			return nil, errors.New("a sling line is not in the dispatcher's shape")
		}
		at, err := time.ParseInLocation(doltSlingStampLayout, m[1], time.Local)
		if err != nil {
			return nil, errors.New("a sling line's stamp is not a time")
		}
		// A line outside the hour is not part of this reading: its duration is
		// never drawn, so an unreadable one does not cost the hour.
		if at.Before(start) || at.After(now) {
			continue
		}
		took, err := time.ParseDuration(m[3])
		if err != nil {
			return nil, errors.New("a sling line's duration is not a duration")
		}
		out = append(out, doltSlingSample{at: at, took: took, bead: doltSlingBead(m[2])})
	}
	return out, nil
}

// doltSlingReachesBack reports whether the window holds a line stamped at or
// before the hour's start: without one, the log cannot say what the whole hour
// saw.
func doltSlingReachesBack(lines []string, now time.Time) bool {
	start := now.Add(-doltSlingWindow)
	for _, line := range lines {
		at, ok := doltSlingLineStamp(line)
		if ok && !at.After(start) {
			return true
		}
	}
	return false
}

// doltSlingLineStamp reads a log line's own prefix.
func doltSlingLineStamp(line string) (time.Time, bool) {
	if len(line) < len(doltSlingStampLayout) {
		return time.Time{}, false
	}
	at, err := time.ParseInLocation(doltSlingStampLayout, line[:len(doltSlingStampLayout)], time.Local)
	return at, err == nil
}

// doltSlingBead reads the bead id a report entry names. An entry that names
// none reads as empty, which the row draws as a dash.
func doltSlingBead(entry string) string {
	for _, prefix := range doltSlingCategories {
		if rest, ok := strings.CutPrefix(entry, prefix); ok {
			entry = rest
			break
		}
	}
	bead, _, ok := strings.Cut(entry, ":")
	if !ok {
		return ""
	}
	bead = strings.TrimSpace(bead)
	if !doltSlingBeadRe.MatchString(bead) {
		return ""
	}
	return bead
}

// doltSlingSummary is one hour's reading: how many sling lines it held and how
// long they took.
type doltSlingSummary struct {
	count  int
	median time.Duration
	p90    time.Duration
	worst  doltSlingSample
}

// summarizeDoltSlings folds the hour's samples. The worst of equally long
// slings is the latest written, so the row names the line an operator would
// find first scrolling back.
func summarizeDoltSlings(samples []doltSlingSample) doltSlingSummary {
	s := doltSlingSummary{count: len(samples)}
	if s.count == 0 {
		return s
	}
	asc := append([]doltSlingSample(nil), samples...)
	sort.SliceStable(asc, func(i, j int) bool { return asc[i].took < asc[j].took })
	s.median = doltSlingPercentile(asc, 0.5)
	s.p90 = doltSlingPercentile(asc, 0.9)
	for _, x := range samples {
		if x.took > s.worst.took || (x.took == s.worst.took && x.at.After(s.worst.at)) {
			s.worst = x
		}
	}
	return s
}

// doltSlingPercentile is the nearest-rank value at p of durations ascending:
// the smallest value p of the sample does not exceed, so the row names a sling
// that actually ran.
func doltSlingPercentile(asc []doltSlingSample, p float64) time.Duration {
	rank := int(math.Ceil(p*float64(len(asc)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(asc) {
		rank = len(asc) - 1
	}
	return asc[rank].took
}

// doltSlingRow is the section's one row.
func doltSlingRow(s doltSlingSummary) []string {
	if s.count == 0 {
		return []string{"0", doltSlingNoValue, doltSlingNoValue, doltSlingNoValue, doltSlingNoValue, doltSlingNoValue}
	}
	bead := s.worst.bead
	if bead == "" {
		bead = doltSlingNoValue
	}
	return []string{
		strconv.Itoa(s.count),
		doltSlingDuration(s.median),
		doltSlingDuration(s.p90),
		doltSlingDuration(s.worst.took),
		bead,
		s.worst.at.Format(doltSlingClockLayout),
	}
}

// doltSlingDuration is a duration as whole seconds, the precision the daemon
// rounds its line to.
func doltSlingDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}
