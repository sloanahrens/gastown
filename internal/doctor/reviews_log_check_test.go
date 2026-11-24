package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// warnSize is a written-out 100 MiB rather than reviewsLogWarnBytes: the
// boundary rows must keep their input when the constant moves, so halving the
// constant fails a row instead of moving it.
const warnSize int64 = 100 << 20

type fixedFileInfo struct {
	size int64
}

func (f fixedFileInfo) Name() string       { return land.ReviewsFileName }
func (f fixedFileInfo) Size() int64        { return f.size }
func (f fixedFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fixedFileInfo) ModTime() time.Time { return time.Time{} }
func (f fixedFileInfo) IsDir() bool        { return false }
func (f fixedFileInfo) Sys() any           { return nil }

func writeReviewsLog(t *testing.T, town, body string) {
	t.Helper()
	path := land.TownReviewsFile(town).Path
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReviewsLogCheck(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setup       func(t *testing.T, town string) *ReviewsLogCheck
		wantStatus  CheckStatus
		wantMessage []string
		wantHint    []string
		wantNoHint  bool
	}{
		{
			name:        "absent",
			setup:       func(*testing.T, string) *ReviewsLogCheck { return NewReviewsLogCheck() },
			wantStatus:  StatusOK,
			wantMessage: []string{"no reviews log yet"},
			wantNoHint:  true,
		},
		{
			name: "a small file reports its size and exact line count",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				writeReviewsLog(t, town, "a\nb\nc\n")
				return NewReviewsLogCheck()
			},
			wantStatus:  StatusOK,
			wantMessage: []string{"reviews.jsonl is 6 B in 3 lines"},
			wantNoHint:  true,
		},
		{
			name: "a single line takes the singular",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				writeReviewsLog(t, town, "x\n")
				return NewReviewsLogCheck()
			},
			wantStatus:  StatusOK,
			wantMessage: []string{"reviews.jsonl is 2 B in 1 line"},
			wantNoHint:  true,
		},
		{
			name: "a final line without a trailing newline counts",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				writeReviewsLog(t, town, "a\nbcde")
				return NewReviewsLogCheck()
			},
			wantStatus:  StatusOK,
			wantMessage: []string{"reviews.jsonl is 6 B in 2 lines"},
			wantNoHint:  true,
		},
		{
			name: "exactly at the warning size warns with the archive hint",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				writeReviewsLog(t, town, "a\nb\n")
				c := NewReviewsLogCheck()
				c.stat = func(string) (os.FileInfo, error) { return fixedFileInfo{size: warnSize}, nil }
				return c
			},
			wantStatus:  StatusWarning,
			wantMessage: []string{"reviews.jsonl is 100.0 MB in 2 lines"},
			wantHint:    []string{"reviews.YYYY-MM.jsonl", "gt-rq2q4w"},
		},
		{
			name: "one byte under the warning size is OK",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				writeReviewsLog(t, town, "a\nb\n")
				c := NewReviewsLogCheck()
				c.stat = func(string) (os.FileInfo, error) { return fixedFileInfo{size: warnSize - 1}, nil }
				return c
			},
			wantStatus:  StatusOK,
			wantMessage: []string{"reviews.jsonl is 100.0 MB in 2 lines"},
			wantNoHint:  true,
		},
		{
			name: "a stat error warns",
			setup: func(*testing.T, string) *ReviewsLogCheck {
				c := NewReviewsLogCheck()
				c.stat = func(string) (os.FileInfo, error) { return nil, errors.New("stat failed") }
				return c
			},
			wantStatus:  StatusWarning,
			wantMessage: []string{"cannot stat reviews log: stat failed"},
			wantNoHint:  true,
		},
		{
			name: "a directory at the path warns",
			setup: func(t *testing.T, town string) *ReviewsLogCheck {
				if err := os.MkdirAll(land.TownReviewsFile(town).Path, 0o700); err != nil {
					t.Fatal(err)
				}
				return NewReviewsLogCheck()
			},
			wantStatus:  StatusWarning,
			wantMessage: []string{"cannot read reviews log:"},
			wantNoHint:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			check := tt.setup(t, town)

			r := check.Run(&CheckContext{TownRoot: town})

			if r.Status != tt.wantStatus {
				t.Fatalf("status = %v (%s), want %v", r.Status, r.Message, tt.wantStatus)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message %q must contain %q", r.Message, want)
				}
			}
			for _, want := range tt.wantHint {
				if !strings.Contains(r.FixHint, want) {
					t.Errorf("FixHint %q must name %q", r.FixHint, want)
				}
			}
			if tt.wantNoHint && r.FixHint != "" {
				t.Errorf("FixHint = %q, want empty", r.FixHint)
			}
		})
	}
}
