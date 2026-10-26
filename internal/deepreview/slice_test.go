package deepreview

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// pathsOf names a slice's files the way slice.txt is read, one path per row.
func pathsOf(s Slice) string {
	paths := make([]string, len(s.Rows))
	for i, c := range s.Rows {
		paths[i] = c.Path
	}
	return strings.Join(paths, " ")
}

func TestBuildSlice(t *testing.T) {
	t.Parallel()
	// One more candidate than the schedule's file cap, so the cap is what
	// stops the slice rather than the byte budget.
	overCap := make([]Candidate, DefaultSliceFiles+1)
	overCapPaths := make([]string, len(overCap))
	for i := range overCap {
		overCap[i] = Candidate{Path: fmt.Sprintf("internal/f%02d.go", i), Bytes: 1}
		overCapPaths[i] = overCap[i].Path
	}
	for _, tc := range []struct {
		name       string
		candidates []Candidate
		maxFiles   int
		maxBytes   int
		paths      string
		records    int
		bytes      int
		skipped    int
		cursor     string
	}{
		{
			name: "under both bounds every candidate is included",
			candidates: []Candidate{
				{"internal/a.go", 10}, {"internal/b.go", 20}, {"internal/c.go", 30},
			},
			maxFiles: 10,
			maxBytes: 60,
			paths:    "internal/a.go internal/b.go internal/c.go",
			records:  3,
			bytes:    60,
			cursor:   "internal/c.go",
		},
		{
			name: "the byte bound cuts before the candidate that would pass it",
			candidates: []Candidate{
				{"internal/a.go", 4}, {"internal/b.go", 4},
				{"internal/c.go", 4}, {"internal/d.go", 4},
			},
			maxFiles: 10,
			maxBytes: 10,
			paths:    "internal/a.go internal/b.go",
			records:  2,
			bytes:    8,
			skipped:  2,
			cursor:   "internal/b.go",
		},
		{
			name: "the file bound cuts even when the bytes fit",
			candidates: []Candidate{
				{"internal/a.go", 1}, {"internal/b.go", 1}, {"internal/c.go", 1},
				{"internal/d.go", 1}, {"internal/e.go", 1},
			},
			maxFiles: 3,
			maxBytes: 1000,
			paths:    "internal/a.go internal/b.go internal/c.go",
			records:  3,
			bytes:    3,
			skipped:  2,
			cursor:   "internal/c.go",
		},
		{
			name: "both bounds at once stop the slice at the same candidate",
			candidates: []Candidate{
				{"internal/a.go", 2}, {"internal/b.go", 2},
				{"internal/c.go", 2}, {"internal/d.go", 2},
			},
			// After two rows both the file cap and the byte budget would
			// be passed by a third.
			maxFiles: 2,
			maxBytes: 5,
			paths:    "internal/a.go internal/b.go",
			records:  2,
			bytes:    4,
			skipped:  2,
			cursor:   "internal/b.go",
		},
		{
			name:       "a first candidate larger than the budget is kept",
			candidates: []Candidate{{"internal/large.go", 500}, {"internal/small.go", 1}},
			maxFiles:   10,
			maxBytes:   10,
			paths:      "internal/large.go",
			records:    1,
			bytes:      500,
			skipped:    1,
			cursor:     "internal/large.go",
		},
		{
			name: "a candidate that does not fit ends the slice; a smaller one behind it is not pulled in",
			candidates: []Candidate{
				{"internal/a.go", 6}, {"internal/b.go", 10}, {"internal/c.go", 1},
			},
			maxFiles: 10,
			maxBytes: 10,
			paths:    "internal/a.go",
			records:  1,
			bytes:    6,
			skipped:  2,
			cursor:   "internal/a.go",
		},
		{
			name:     "an empty rotation is an empty slice",
			maxFiles: DefaultSliceFiles,
			maxBytes: DefaultSliceBytes,
			records:  0,
			bytes:    0,
			skipped:  0,
			cursor:   "",
		},
		{
			name:       "the schedule's default byte budget stops at 250000",
			candidates: []Candidate{{"internal/a.go", 100000}, {"internal/b.go", 100000}, {"internal/c.go", 100000}},
			maxFiles:   DefaultSliceFiles,
			maxBytes:   DefaultSliceBytes,
			paths:      "internal/a.go internal/b.go",
			records:    2,
			bytes:      200000,
			skipped:    1,
			cursor:     "internal/b.go",
		},
		{
			name:       "the schedule's default file cap stops at 40",
			candidates: overCap,
			maxFiles:   DefaultSliceFiles,
			maxBytes:   DefaultSliceBytes,
			paths:      strings.Join(overCapPaths[:DefaultSliceFiles], " "),
			records:    DefaultSliceFiles,
			bytes:      DefaultSliceFiles,
			skipped:    1,
			cursor:     overCapPaths[DefaultSliceFiles-1],
		},
	} {
		got, err := BuildSlice(tc.candidates, tc.maxFiles, tc.maxBytes)
		if err != nil {
			t.Errorf("%s: BuildSlice error: %v", tc.name, err)
			continue
		}
		if paths := pathsOf(got); paths != tc.paths {
			t.Errorf("%s: slice = %q, want %q", tc.name, paths, tc.paths)
		}
		if got.Records != tc.records || got.Bytes != tc.bytes || got.Skipped != tc.skipped {
			t.Errorf("%s: records/bytes/skipped = %d/%d/%d, want %d/%d/%d",
				tc.name, got.Records, got.Bytes, got.Skipped, tc.records, tc.bytes, tc.skipped)
		}
		if got.Cursor != tc.cursor {
			t.Errorf("%s: cursor = %q, want %q", tc.name, got.Cursor, tc.cursor)
		}
	}
}

// TestBuildSliceCursorStopsAtTheLastIncludedRecord is the row the acceptance
// asks for: the cut's own record is checked, not just the count, so a cursor
// written one row too far cannot pass.
func TestBuildSliceCursorStopsAtTheLastIncludedRecord(t *testing.T) {
	t.Parallel()
	candidates := []Candidate{
		{"internal/a.go", 1}, {"internal/b.go", 1},
		{"internal/c.go", 1}, {"internal/d.go", 1},
	}
	got, err := BuildSlice(candidates, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cursor != "internal/b.go" {
		t.Errorf("cursor = %q, want the last included record", got.Cursor)
	}
	if next := candidates[got.Records].Path; next != "internal/c.go" {
		t.Fatalf("record after the cut = %q, want internal/c.go", next)
	}
	if got.Cursor == candidates[got.Records].Path {
		t.Error("cursor names the record after the cut, which was not reviewed")
	}
	if got.Skipped != 2 {
		t.Errorf("skipped = %d, want the two records after the cut", got.Skipped)
	}
}

func TestBuildSliceRefusesBoundsAndSizesItCannotHonor(t *testing.T) {
	t.Parallel()
	one := []Candidate{{"internal/a.go", 1}}
	for _, tc := range []struct {
		name       string
		candidates []Candidate
		maxFiles   int
		maxBytes   int
		want       string
	}{
		{name: "a zero file bound", candidates: one, maxFiles: 0, maxBytes: 1, want: "file bound must be positive"},
		{name: "a negative file bound", candidates: one, maxFiles: -1, maxBytes: 1, want: "file bound must be positive"},
		{name: "a zero byte bound", candidates: one, maxFiles: 1, maxBytes: 0, want: "byte bound must be positive"},
		{name: "a negative byte bound", candidates: one, maxFiles: 1, maxBytes: -1, want: "byte bound must be positive"},
		{
			name:       "a candidate with a negative size",
			candidates: []Candidate{{"internal/a.go", 1}, {"internal/b.go", -1}},
			maxFiles:   1, maxBytes: 100,
			want: "negative size",
		},
	} {
		got, err := BuildSlice(tc.candidates, tc.maxFiles, tc.maxBytes)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: BuildSlice error = %v, want one naming %q", tc.name, err, tc.want)
		}
		if !reflect.DeepEqual(got, Slice{}) {
			t.Errorf("%s: slice = %+v beside an error, want the zero slice", tc.name, got)
		}
	}
}
