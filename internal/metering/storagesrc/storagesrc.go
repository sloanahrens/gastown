// Package storagesrc reads the nightly size records into the storage samples
// the monthly export charges from.
//
// Two writers produce the records. The tracker size report
// (tracker/cmd/trackersize) writes one line per issue-id prefix to
// <town>/.runtime/metrics/tracker-sizes/<UTC date>.jsonl; that directory is the
// path its -out flag names, and no scheduler sets the flag yet, so the operator
// passes <town>/.runtime/metrics/tracker-sizes. The Forgejo size report
// (internal/dashboard) writes one line per repository to
// <town>/.runtime/metrics/forgejo-sizes/<UTC date>.jsonl.
//
// Samples takes an fs.FS rooted at <town>/.runtime/metrics. Its caller is the
// invoice command bead (gt-ehbgh). Neither nightly writer has a scheduler yet,
// so a month may have missing days; export.Month counts a day with no sample as
// zero.
package storagesrc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

const (
	// trackerDir and forgejoDir are the sample directories under the town's
	// metrics directory, named by the writers in tracker/cmd/trackersize and
	// internal/dashboard.
	trackerDir = "tracker-sizes"
	forgejoDir = "forgejo-sizes"
	// jsonlSuffix names a day file. Both writers publish a day file by rename
	// and may leave a .partial or .tmp file beside it while a run is in flight,
	// so only files with this suffix are read.
	jsonlSuffix = ".jsonl"
	// bytesPerKB is the size of one kilobyte, the unit Forgejo reports a
	// repository's size in.
	bytesPerKB int64 = 1024
)

// SkipReason names why a line became no sample.
type SkipReason string

const (
	// SkipInvalidJSON is a line json.Unmarshal rejected.
	SkipInvalidJSON SkipReason = "invalid JSON"
	// SkipNullSize is a Forgejo line whose size_kb is null: an unmeasured
	// repository, not an empty one.
	SkipNullSize SkipReason = "null size"
	// SkipEmptyWorkspace is a line that names no workspace to bill.
	SkipEmptyWorkspace SkipReason = "empty workspace"
	// SkipUnparsableTS is a line whose ts is not RFC 3339.
	SkipUnparsableTS SkipReason = "unparsable ts"
	// SkipNegativeSize is a line whose byte count or kilobyte count is
	// negative.
	SkipNegativeSize SkipReason = "negative size"
	// SkipSizeOverflow is a Forgejo line whose kilobytes do not fit in the
	// sample's int64 byte count.
	SkipSizeOverflow SkipReason = "size overflows bytes"
)

// Result is what Samples read: the storage samples, the sample directories the
// file system does not hold, and the lines that became no sample.
type Result struct {
	// Samples are the samples the export charges storage from, in file name
	// order: the tracker samples, then the Forgejo samples.
	Samples []export.StorageSample
	// Absent names the sample directories fsys does not hold, in the order
	// Samples looks for them. A missing directory is a town that has not run
	// that writer yet, not an error; export.Month counts its days as zero.
	Absent []string
	// Skipped counts the lines that became no sample, by the reason each was
	// left out.
	Skipped map[SkipReason]int
}

// source is one sample directory and the decoder for its lines.
type source struct {
	dir    string
	decode func(line []byte) (export.StorageSample, SkipReason, bool)
}

// Samples reads the tracker and Forgejo size files under fsys and returns the
// storage samples they hold.
//
// The tracker files are read first, then the Forgejo files, and within each
// directory the day files are read in name order, so the result is the same on
// every run. A directory that does not exist is recorded in Absent and is not
// an error; a directory or day file that exists but cannot be read is an error,
// and no partial Result is returned with it. A line that cannot become a sample
// is skipped and counted in Skipped.
func Samples(fsys fs.FS) (Result, error) {
	res := Result{Skipped: map[SkipReason]int{}}
	for _, src := range []source{
		{dir: trackerDir, decode: decodeTracker},
		{dir: forgejoDir, decode: decodeForgejo},
	} {
		names, err := jsonlFiles(fsys, src.dir)
		if errors.Is(err, fs.ErrNotExist) {
			res.Absent = append(res.Absent, src.dir)
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("storagesrc: listing %s: %w", src.dir, err)
		}
		for _, name := range names {
			p := path.Join(src.dir, name)
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				return Result{}, fmt.Errorf("storagesrc: reading %s: %w", p, err)
			}
			for _, line := range bytes.Split(data, []byte("\n")) {
				// A day file ends with a newline; a line of only whitespace
				// carries no record.
				if len(bytes.TrimSpace(line)) == 0 {
					continue
				}
				sample, reason, ok := src.decode(line)
				if !ok {
					res.Skipped[reason]++
					continue
				}
				res.Samples = append(res.Samples, sample)
			}
		}
	}
	return res, nil
}

// jsonlFiles names the day files in dir, sorted, so Samples reads them in the
// same order on every run. Entries that are not .jsonl files are left out: a
// half-written .partial or .tmp file beside a day file is not a day's record.
func jsonlFiles(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), jsonlSuffix) {
			continue
		}
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// trackerRow is one line of the tracker size report: the total bytes of one
// issue-id prefix's issue rows.
type trackerRow struct {
	TS        string `json:"ts"`
	Workspace string `json:"workspace"`
	Prefix    string `json:"prefix"`
	Bytes     int64  `json:"bytes"`
}

// decodeTracker turns one tracker line into a sample of kind tracker:<prefix>.
func decodeTracker(line []byte) (export.StorageSample, SkipReason, bool) {
	var row trackerRow
	if err := json.Unmarshal(line, &row); err != nil {
		return export.StorageSample{}, SkipInvalidJSON, false
	}
	if strings.TrimSpace(row.Workspace) == "" {
		return export.StorageSample{}, SkipEmptyWorkspace, false
	}
	day, ok := parseDay(row.TS)
	if !ok {
		return export.StorageSample{}, SkipUnparsableTS, false
	}
	if row.Bytes < 0 {
		return export.StorageSample{}, SkipNegativeSize, false
	}
	return export.StorageSample{
		Workspace: row.Workspace,
		Kind:      "tracker:" + row.Prefix,
		Bytes:     row.Bytes,
		Day:       day,
	}, "", true
}

// forgejoRow is one line of the Forgejo size report: one repository's size.
type forgejoRow struct {
	TS        string `json:"ts"`
	Workspace string `json:"workspace"`
	Repo      string `json:"repo"`
	// SizeKB is the size Forgejo reported, nil when the report carried none.
	SizeKB *int64 `json:"size_kb"`
}

// decodeForgejo turns one Forgejo line into a sample of kind repo:<repo>.
// size_kb is kilobytes, so the sample's Bytes is size_kb * 1024. A null
// size_kb is skipped rather than read as zero bytes: the report's own comment
// says an unmeasured size is not an empty repository.
func decodeForgejo(line []byte) (export.StorageSample, SkipReason, bool) {
	var row forgejoRow
	if err := json.Unmarshal(line, &row); err != nil {
		return export.StorageSample{}, SkipInvalidJSON, false
	}
	if row.SizeKB == nil {
		return export.StorageSample{}, SkipNullSize, false
	}
	if strings.TrimSpace(row.Workspace) == "" {
		return export.StorageSample{}, SkipEmptyWorkspace, false
	}
	day, ok := parseDay(row.TS)
	if !ok {
		return export.StorageSample{}, SkipUnparsableTS, false
	}
	kb := *row.SizeKB
	if kb < 0 {
		return export.StorageSample{}, SkipNegativeSize, false
	}
	if kb > math.MaxInt64/bytesPerKB {
		return export.StorageSample{}, SkipSizeOverflow, false
	}
	return export.StorageSample{
		Workspace: row.Workspace,
		Kind:      "repo:" + row.Repo,
		Bytes:     kb * bytesPerKB,
		Day:       day,
	}, "", true
}

// parseDay is a record's ts as its UTC date, at midnight. The writers stamp ts
// as UTC at second precision with a trailing Z, which RFC 3339 parses; a ts
// that does not parse leaves the line with no day to count it under.
func parseDay(ts string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC), true
}
