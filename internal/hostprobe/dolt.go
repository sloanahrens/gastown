package hostprobe

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// doltBucketWidth is the span one DoltBucket counts; 144 of them are a day.
const doltBucketWidth = 10 * time.Minute

// doltUnknownDB is the ByDB key a conflict counts under when the line names no
// database or names an empty one, so every counted line lands in ByDB and Total
// is the sum of them.
const doltUnknownDB = "unknown"

// doltConflictMsg and doltConflictError are the two strings a line the server
// logged for a lost commit holds.
const (
	doltConflictMsg   = `msg="error running query"`
	doltConflictError = "serialization failure"
)

// doltMaxLine bounds one log line. A longer line is an error, not a skip: the
// line may be a conflict, and a count that silently drops it reads low.
const doltMaxLine = 1 << 20

var (
	// doltLineTime matches the stamp of a logrus text line.
	doltLineTime = regexp.MustCompile(`\btime="([^"]*)"`)
	// doltLineDB matches connectionDb's value, quoted as logrus writes a value
	// that holds a space and bare otherwise.
	doltLineDB = regexp.MustCompile(`\bconnectionDb=(?:"([^"]*)"|([^\s"]+))`)
)

// DoltConflicts is the Dolt server's lost commits, bucketed by ten minutes and
// by database.
type DoltConflicts struct {
	// Buckets are the ten-minute buckets, oldest first, ending with the one
	// holding the parse's now.
	Buckets []DoltBucket `json:"buckets"`
}

// DoltBucket is the lost commits of one ten minutes: their number in Total and
// their per-database count in ByDB, which is empty rather than nil for a bucket
// with none.
type DoltBucket struct {
	At    time.Time      `json:"at"`
	Total int            `json:"total"`
	ByDB  map[string]int `json:"by_db"`
}

// ParseDoltConflicts reads Dolt server log lines and counts each conflict in
// the UTC ten-minute bucket of its stamp under its connectionDb, over a window
// of buckets ending with the bucket that holds now.
//
// A line of another message, a line whose time cannot be read, and a line
// outside the window are skipped; a failed read or a bucket count below one is
// an error, never a result of zeros that a reader would take for a quiet host.
func ParseDoltConflicts(r io.Reader, now time.Time, buckets int) (DoltConflicts, error) {
	if buckets < 1 {
		return DoltConflicts{}, fmt.Errorf("parse dolt conflicts: %d buckets, want at least 1", buckets)
	}
	first := now.UTC().Truncate(doltBucketWidth).Add(-time.Duration(buckets-1) * doltBucketWidth)
	out := DoltConflicts{Buckets: make([]DoltBucket, buckets)}
	for i := range out.Buckets {
		out.Buckets[i] = DoltBucket{
			At:   first.Add(time.Duration(i) * doltBucketWidth),
			ByDB: map[string]int{},
		}
	}

	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 64<<10), doltMaxLine)
	for s.Scan() {
		line := s.Text()
		if !strings.Contains(line, doltConflictMsg) || !strings.Contains(line, doltConflictError) {
			continue
		}
		at, ok := doltLineStamp(line)
		if !ok {
			continue
		}
		// A stamp before the first bucket truncates to bucket 0 in an integer
		// division, so the bound is checked before the index is computed.
		if at.Before(first) {
			continue
		}
		i := int(at.Sub(first) / doltBucketWidth)
		if i >= buckets {
			continue
		}
		db := doltUnknownDB
		if m := doltLineDB.FindStringSubmatch(line); m != nil {
			if v := m[1] + m[2]; v != "" {
				db = v
			}
		}
		out.Buckets[i].Total++
		out.Buckets[i].ByDB[db]++
	}
	if err := s.Err(); err != nil {
		return DoltConflicts{}, fmt.Errorf("read dolt log: %w", err)
	}
	return out, nil
}

// doltLineStamp reads a conflict line's own stamp. A stamp that does not parse
// reports false, so the caller skips the line rather than counting it in the
// bucket of whatever time it could guess.
func doltLineStamp(line string) (time.Time, bool) {
	m := doltLineTime.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, m[1])
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}
