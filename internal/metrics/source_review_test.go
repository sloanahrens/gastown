package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// reviewTestTown is the town root every review test reads under.
const reviewTestTown = "/town"

// reviewTestEnv serves the review log the test names: one file's content, no
// file at all, or the error a read gets instead.
func reviewTestEnv(t *testing.T, content string, missing bool, tailErr error) Env {
	t.Helper()
	path := land.TownReviewsFile(reviewTestTown).Path
	env := envAt(fixedNow())
	env.Town = reviewTestTown
	env.Tail = func(got string, maxBytes int64) ([]byte, error) {
		if got != path {
			t.Errorf("Tail(%s), want %s", got, path)
		}
		if maxBytes != sourceLogTailBytes {
			t.Errorf("Tail(%s) read %d bytes, want the %d-byte bound", got, maxBytes, sourceLogTailBytes)
		}
		if tailErr != nil {
			return nil, tailErr
		}
		if missing {
			return nil, fs.ErrNotExist
		}
		return []byte(content), nil
	}
	return env
}

// reviewLine is one verdict line in the shape land.ReviewRecord decodes.
func reviewLine(bead, verdict string, attempt int, score float64, rejected bool, at time.Time) string {
	return fmt.Sprintf(`{"bead":%q,"rig":"gastown","attempt":%d,"score":%g,"verdict":%q,"rejected":%t,"findings":2,"bar":7,"at":%q}`,
		bead, attempt, score, verdict, rejected, at.UTC().Format(time.RFC3339))
}

func TestSourceReviewCountsTheLastHour(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-50*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		// An approving verdict the landing refused is a rejection too.
		reviewLine("gt-a3", land.VerdictApprove, 2, 6, true, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a4", land.VerdictApprove, 3, 9, false, now.Add(-20*time.Minute)) + "\n" +
		reviewLine("gt-old", land.VerdictRequestChanges, 1, 1, false, now.Add(-90*time.Minute)) + "\n" + // outside the hour
		reviewLine("gt-future", land.VerdictApprove, 1, 10, false, now.Add(5*time.Minute)) + "\n" + // not yet
		`{"bead":"gt-torn","verdict":` + "\n" + // torn append
		`{"verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + // no bead
		`{"bead":"gt-nv","at":"2026-10-24T12:30:00Z"}` + "\n" // no verdict

	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":         4,
		"om_rejections_1h":      2,
		"om_mean_score_1h":      6.875,
		"om_second_attempts_1h": 2,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
}

func TestSourceReviewMissingLogIsAnHourWithNoReviews(t *testing.T) {
	t.Parallel()
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, "", true, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":         0,
		"om_rejections_1h":      0,
		"om_mean_score_1h":      nil,
		"om_second_attempts_1h": 0,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v (no review is not a score of zero)", cols, want)
	}
}

func TestSourceReviewSkipsEveryLineItCannotUse(t *testing.T) {
	t.Parallel()
	content := "{\n" + `{"bead":"","verdict":"approve","at":"2026-10-24T12:30:00Z"}` + "\n" + `{"bead":"gt-a1"}` + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 0 || cols["om_mean_score_1h"] != nil {
		t.Fatalf("review = %v; want no reviews and no mean from lines it cannot attribute", cols)
	}
}

func TestSourceReviewAnUnreadableLogLeavesTheGroupNull(t *testing.T) {
	t.Parallel()
	env := reviewTestEnv(t, "", false, fs.ErrPermission)
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err == nil || cols != nil {
		t.Fatalf("Collect = %v, %v; want an error and no columns", cols, err)
	}
	rec := Collect(context.Background(), env, []Source{reviewSource{}}, SourceTimeout)
	if rec.Groups["review"] != nil || len(rec.Errors) != 1 || rec.Errors[0].Group != "review" {
		t.Errorf("record review = %v, errors = %v; want null and one review error", rec.Groups["review"], rec.Errors)
	}
}
