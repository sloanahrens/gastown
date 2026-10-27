package metrics

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"sync"
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

// reviewBdFake answers `bd show --json` from a table of beads and records the
// argv it was asked, so a test can see which rig each bead was read in.
type reviewBdFake struct {
	beads map[string]string // bead id -> the JSON object bd show prints for it
	fail  map[string]bool

	mu              sync.Mutex
	calls           [][]string
	deadlineMissing bool
}

func (f *reviewBdFake) run(ctx context.Context, argv []string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		f.mu.Lock()
		f.deadlineMissing = true
		f.mu.Unlock()
	}
	f.mu.Lock()
	f.calls = append(f.calls, argv)
	f.mu.Unlock()
	if len(argv) != 7 || argv[0] != "bd" || argv[1] != "-C" || argv[3] != "show" || argv[4] != "--json" || argv[5] != "--" {
		return nil, fmt.Errorf("unexpected argv %v", argv)
	}
	bead := argv[6]
	if f.fail[bead] {
		return nil, fmt.Errorf("bd show %s: no Dolt server", bead)
	}
	entry, ok := f.beads[bead]
	if !ok {
		return []byte("[]"), nil // a bead bd no longer holds
	}
	return []byte("[" + entry + "]"), nil
}

// reviewBeadJSON is one bead in the shape `bd show --json` prints it.
func reviewBeadJSON(id, branch, assignee string) string {
	notes := `"READY TO LAND\nHead: abc\n"`
	if branch != "" {
		notes = fmt.Sprintf("%q", "READY TO LAND\nBranch: "+branch+"\nHead: abc\n")
	}
	return fmt.Sprintf(`{"id":%q,"notes":%s,"assignee":%q}`, id, notes, assignee)
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

	bdfake := &reviewBdFake{beads: map[string]string{
		"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1+mv1", "gastown/polecats/opal"),
		"gt-a2": reviewBeadJSON("gt-a2", "crew/merlin/gt-a2", "gastown/crew/merlin"),
		"gt-a3": reviewBeadJSON("gt-a3", "crew/merlin/gt-a3", "town/crew/overseer"),
		"gt-a4": reviewBeadJSON("gt-a4", "", "gastown/polecats/robin"),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          4,
		"om_rejections_1h":       2,
		"om_mean_score_1h":       6.875,
		"om_second_attempts_1h":  2,
		"om_polecat_reviews_1h":  1,
		"om_crew_reviews_1h":     1,
		"om_overseer_reviews_1h": 1,
		"om_unknown_reviews_1h":  1,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v", cols, want)
	}
	if bdfake.deadlineMissing {
		t.Error("a bd read ran without a deadline")
	}
	// Each review bead is read once, in its review's rig, with the id after --.
	for _, call := range bdfake.calls {
		if call[2] != filepath.Join(reviewTestTown, "gastown") {
			t.Errorf("bd read in %q, want the review's rig", call[2])
		}
	}
	if len(bdfake.calls) != 4 {
		t.Errorf("bd reads = %d, want one per reviewed bead", len(bdfake.calls))
	}
}

// A bead read that fails leaves the kind columns null, because the hour's
// reviews were not attributed; the counts the source can still make stand.
func TestSourceReviewAFailedBeadReadLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n" +
		reviewLine("gt-a2", land.VerdictApprove, 1, 8.5, false, now.Add(-20*time.Minute)) + "\n"
	env := reviewTestEnv(t, content, false, nil)
	env.Run = (&reviewBdFake{
		beads: map[string]string{"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1", "gastown/polecats/opal")},
		fail:  map[string]bool{"gt-a2": true},
	}).run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 2 {
		t.Errorf("om_reviews_1h = %v, want the 2 reviews it could count", cols["om_reviews_1h"])
	}
	for _, column := range []string{"om_polecat_reviews_1h", "om_crew_reviews_1h", "om_overseer_reviews_1h", "om_unknown_reviews_1h"} {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null: a review it could not attribute is not an unknown author's", column, value)
		}
	}
}

// Two reviews of one bead are one bead read, and a review that names no rig is
// read from the town root, where bd routes the bead's prefix to its rig.
func TestSourceReviewReadsEachBeadOnce(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictRequestChanges, 1, 4, false, now.Add(-40*time.Minute)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a1","rig":"gastown","attempt":2,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-20*time.Minute).UTC().Format(time.RFC3339)) + "\n" +
		fmt.Sprintf(`{"bead":"gt-a2","attempt":1,"score":8,"verdict":"approve","at":%q}`,
			now.Add(-10*time.Minute).UTC().Format(time.RFC3339)) + "\n"
	bdfake := &reviewBdFake{beads: map[string]string{
		"gt-a1": reviewBeadJSON("gt-a1", "polecat/opal/gt-a1+mv1", "gastown/polecats/opal"),
		"gt-a2": reviewBeadJSON("gt-a2", "polecat/opal/gt-a2+mv1", "gastown/polecats/opal"),
	}}
	env := reviewTestEnv(t, content, false, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_polecat_reviews_1h"] != 3 {
		t.Errorf("om_polecat_reviews_1h = %v, want all three reviews", cols["om_polecat_reviews_1h"])
	}
	if len(bdfake.calls) != 2 {
		t.Fatalf("bd reads = %v, want one per bead, not one per review", bdfake.calls)
	}
	dirs := map[string]string{bdfake.calls[0][6]: bdfake.calls[0][2], bdfake.calls[1][6]: bdfake.calls[1][2]}
	if dirs["gt-a1"] != filepath.Join(reviewTestTown, "gastown") {
		t.Errorf("gt-a1 read in %q, want its review's rig", dirs["gt-a1"])
	}
	if dirs["gt-a2"] != reviewTestTown {
		t.Errorf("gt-a2 read in %q, want the town root for a review that names no rig", dirs["gt-a2"])
	}
}

// A snapshot with no runner cannot attribute, so its kind columns are null: a
// review whose branch was never read is not a review by an unknown author.
func TestSourceReviewWithoutARunnerLeavesTheKindsNull(t *testing.T) {
	t.Parallel()
	now := fixedNow()
	content := reviewLine("gt-a1", land.VerdictApprove, 1, 8.5, false, now.Add(-30*time.Minute)) + "\n"
	cols, err := reviewSource{}.Collect(context.Background(), reviewTestEnv(t, content, false, nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["om_reviews_1h"] != 1 {
		t.Errorf("om_reviews_1h = %v, want the review it could count", cols["om_reviews_1h"])
	}
	for _, column := range []string{"om_polecat_reviews_1h", "om_crew_reviews_1h", "om_overseer_reviews_1h", "om_unknown_reviews_1h"} {
		if value, ok := cols[column]; !ok || value != nil {
			t.Errorf("%s = %v, want null without a runner", column, value)
		}
	}
}

func TestSourceReviewMissingLogIsAnHourWithNoReviews(t *testing.T) {
	t.Parallel()
	bdfake := &reviewBdFake{}
	env := reviewTestEnv(t, "", true, nil)
	env.Run = bdfake.run
	cols, err := reviewSource{}.Collect(context.Background(), env)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := map[string]any{
		"om_reviews_1h":          0,
		"om_rejections_1h":       0,
		"om_mean_score_1h":       nil,
		"om_second_attempts_1h":  0,
		"om_polecat_reviews_1h":  0,
		"om_crew_reviews_1h":     0,
		"om_overseer_reviews_1h": 0,
		"om_unknown_reviews_1h":  0,
	}
	if !reflect.DeepEqual(cols, want) {
		t.Fatalf("review = %#v, want %#v (no review is not a score of zero)", cols, want)
	}
	if len(bdfake.calls) != 0 {
		t.Errorf("bd reads = %v, want none: an hour of no reviews has no bead to read", bdfake.calls)
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
	// No review is every kind at zero, not an attribution nobody could make:
	// there is no bead to read and no runner is needed.
	for _, column := range []string{"om_polecat_reviews_1h", "om_crew_reviews_1h", "om_overseer_reviews_1h", "om_unknown_reviews_1h"} {
		if value, ok := cols[column]; !ok || value != 0 {
			t.Errorf("%s = %v, want 0: an hour with no reviews", column, value)
		}
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
