package deepreview

import "fmt"

// DefaultSliceBytes and DefaultSliceFiles are the deep-review-slice schedule's
// bounds: the byte budget is the main limit and the file cap keeps a tree of
// small files from making one run enormous.
const (
	DefaultSliceBytes = 250000
	DefaultSliceFiles = 40
)

// Candidate is one file in the rotation: its repo-relative path and its size at
// the reviewed ref.
type Candidate struct {
	Path  string
	Bytes int
}

// Slice is one run's bounded cut of the rotation.
type Slice struct {
	// Rows are the included candidates, in rotation order: the slice header's
	// records, bytes and skipped describe these rows.
	Rows []Candidate
	// Records counts the rows and Bytes sums their sizes.
	Records int
	Bytes   int
	// Skipped counts the candidates the cut left out; the next run reviews
	// them from Cursor onward.
	Skipped int
	// Cursor is the last included path, never one the cut skipped.
	Cursor string
}

// BuildSlice cuts candidates at the first one that would pass maxFiles or
// maxBytes, always keeping the first so one oversized file cannot empty a
// non-quiet day's slice.
func BuildSlice(candidates []Candidate, maxFiles, maxBytes int) (Slice, error) {
	if maxFiles < 1 {
		return Slice{}, fmt.Errorf("slice file bound must be positive: %d", maxFiles)
	}
	if maxBytes < 1 {
		return Slice{}, fmt.Errorf("slice byte bound must be positive: %d", maxBytes)
	}
	// Checked before the cut so a bad size behind the cut is still refused
	// rather than silently dropped: the caller would pass it to the next run.
	for _, c := range candidates {
		if c.Bytes < 0 {
			return Slice{}, fmt.Errorf("slice candidate %q has a negative size: %d", c.Path, c.Bytes)
		}
	}

	s := Slice{Rows: []Candidate{}}
	for _, c := range candidates {
		if len(s.Rows) >= maxFiles {
			break
		}
		if len(s.Rows) > 0 && s.Bytes+c.Bytes > maxBytes {
			break
		}
		s.Rows = append(s.Rows, c)
		s.Bytes += c.Bytes
	}
	s.Records = len(s.Rows)
	if s.Records > 0 {
		s.Cursor = s.Rows[s.Records-1].Path
	}
	s.Skipped = len(candidates) - s.Records
	return s, nil
}
