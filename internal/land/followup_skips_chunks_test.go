package land

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// skippedN is n skipped findings whose files are fileLen bytes, so a line's size
// is set by the test (gt-p1tafj).
func skippedN(n, fileLen int) []FollowupSkipped {
	var out []FollowupSkipped
	for i := 0; i < n; i++ {
		out = append(out, FollowupSkipped{Class: "behaviour", File: fmt.Sprintf("%0*d", fileLen, i), Reason: "doc-nit", Fingerprint: fmt.Sprintf("om-fp:%012d", i)})
	}
	return out
}

// TestFollowupSkipsChunksSplit: a record whose findings fit one line is one line;
// past the limit the findings are cut into lines in order, each under three quarters
// of the limit, and the lines read back as the record's findings, in order. The
// limit is an argument so the test uses a small one.
func TestFollowupSkipsChunksSplit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		n, limit   int
		wantChunks []int
	}{
		{"fits one line", 5, 4096, []int{5}},
		{"five findings, two per line", 5, 400, []int{2, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := skippedN(tc.n, 20)
			chunks, err := skipChunks(in, tc.limit)
			if err != nil {
				t.Fatalf("skipChunks: %v", err)
			}
			var sizes []int
			var joined []FollowupSkipped
			for _, c := range chunks {
				sizes = append(sizes, len(c))
				joined = append(joined, c...)
			}
			if !reflect.DeepEqual(sizes, tc.wantChunks) || !reflect.DeepEqual(joined, in) {
				t.Errorf("chunks of %v with findings %v, want %v holding the record's findings in order", sizes, joined, tc.wantChunks)
			}
		})
	}
	// The real bound: a record of two findings is one line in the file.
	f := reviewsFileIn(t)
	rec := FollowupSkipsRecord{BeadID: "gt-abc", Rig: "gastown", At: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC), Findings: skippedN(2, 20)}
	if err := f.AppendSkipsChunked(rec); err != nil {
		t.Fatalf("AppendSkipsChunked: %v", err)
	}
	raw, err := os.ReadFile(f.Path)
	if err != nil || strings.Count(string(raw), "\n") != 1 {
		t.Errorf("file = %q (%v), want one line", raw, err)
	}
}

// TestFollowupSkipsChunksOversize: a finding that alone does not fit a line is an
// error naming it and nothing of the record is written, and a write that fails is
// returned (gt-p1tafj).
func TestFollowupSkipsChunksOversize(t *testing.T) {
	t.Parallel()
	t.Run("one finding past the bound", func(t *testing.T) {
		t.Parallel()
		f := reviewsFileIn(t)
		rec := FollowupSkipsRecord{BeadID: "gt-abc", Findings: append(skippedN(2, 20), FollowupSkipped{File: strings.Repeat("x", maxReviewLineBytes), Fingerprint: "om-fp:big"})}
		err := f.AppendSkipsChunked(rec)
		if err == nil || !strings.Contains(err.Error(), "does not fit a review line") || !strings.Contains(err.Error(), "om-fp:big") {
			t.Fatalf("AppendSkipsChunked = %v, want the oversize finding named", err)
		}
		if _, statErr := os.Stat(f.Path); !os.IsNotExist(statErr) {
			t.Errorf("the file exists (%v): the findings before the oversize one were written", statErr)
		}
	})
	t.Run("a write that fails", func(t *testing.T) {
		t.Parallel()
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		f := &ReviewsFile{Path: filepath.Join(blocker, "landings", ReviewsFileName)}
		if err := f.AppendSkipsChunked(FollowupSkipsRecord{BeadID: "gt-abc", Findings: skippedN(1, 20)}); err == nil {
			t.Error("AppendSkipsChunked into a path under a file = nil, want the write's error")
		}
	})
}
