package doctor

import (
	"fmt"
	"io"
	"os"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/util"
)

// reviewsLogWarnBytes is the point at which the monthly archive is shaped:
// completed months move to reviews.YYYY-MM.jsonl beside the live log, and the
// readers of the whole history read both (gt-rq2q4w).
const reviewsLogWarnBytes int64 = 100 << 20

// ReviewsLogCheck reports the size and line count of the town's review log,
// which nothing prunes; the monthly archive is the step that bounds it.
type ReviewsLogCheck struct {
	BaseCheck

	// stat is a seam: nil means os.Stat, and a test fakes the size so the
	// boundary rows need no 100 MiB file.
	stat func(string) (os.FileInfo, error)
}

// NewReviewsLogCheck creates a new review log size check.
func NewReviewsLogCheck() *ReviewsLogCheck {
	return &ReviewsLogCheck{
		BaseCheck: BaseCheck{
			CheckName:        "reviews-log-size",
			CheckDescription: "Check .runtime/landings/reviews.jsonl is bounded by the monthly archive",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// Run stats the town's reviews.jsonl and counts its lines.
func (c *ReviewsLogCheck) Run(ctx *CheckContext) *CheckResult {
	path := land.TownReviewsFile(ctx.TownRoot).Path
	stat := c.stat
	if stat == nil {
		stat = os.Stat
	}
	info, err := stat(path)
	if os.IsNotExist(err) {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "no reviews log yet"}
	}
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusWarning, Message: fmt.Sprintf("cannot stat reviews log: %v", err)}
	}
	lines, err := countReviewsLogLines(path)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusWarning, Message: fmt.Sprintf("cannot read reviews log: %v", err)}
	}

	size := util.FormatBytesHuman(uint64(info.Size())) //nolint:gosec // G115: file size is non-negative
	message := fmt.Sprintf("%s is %s in %s", land.ReviewsFileName, size, lineCount(lines))
	if info.Size() >= reviewsLogWarnBytes {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: message,
			Details: []string{
				fmt.Sprintf("The monthly archive point is %s.", util.FormatBytesHuman(uint64(reviewsLogWarnBytes))),
				"Nothing prunes the log, and the nightly backup copies it whole.",
			},
			FixHint: "Shape the monthly archive: move completed months to reviews.YYYY-MM.jsonl beside the live log; whole-history readers read the live file plus the archives (gt-rq2q4w).",
		}
	}
	return &CheckResult{Name: c.Name(), Status: StatusOK, Message: message}
}

// lineCount renders n as a count with a matching noun.
func lineCount(n int) string {
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// countReviewsLogLines counts the log's lines without loading it whole: a file
// with millions of lines is read in fixed chunks, and a final line that ends
// without a newline counts too.
func countReviewsLogLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return countLogLines(f)
}

func countLogLines(r io.Reader) (int, error) {
	buf := make([]byte, 64<<10)
	var last byte
	lines := 0
	read := false
	for {
		n, err := r.Read(buf)
		for _, b := range buf[:n] {
			if b == '\n' {
				lines++
			}
		}
		if n > 0 {
			read = true
			last = buf[n-1]
		}
		if err == io.EOF {
			if read && last != '\n' {
				lines++
			}
			return lines, nil
		}
		if err != nil {
			return 0, err
		}
	}
}
